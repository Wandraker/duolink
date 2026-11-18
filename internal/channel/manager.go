package channel

import (
	"context"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Interfaces that are never uplinks of their own (tunnels, VMs, loopback).
var skipNames = []string{
	"loopback", "vethernet", "vmware", "virtualbox", "hyper-v", "wsl",
	"cloudflarewarp", "warp", "wintun", "wireguard", "tap", "tun",
	"onedpi", "duolink", "bluetooth", "teredo", "isatap", "zerotier", "tailscale",
}

// Manager keeps the live set of channels and picks one per connection.
type Manager struct {
	mu       sync.RWMutex
	channels map[string]*Channel
	order    []string

	stickyMu  sync.Mutex
	sticky    map[string]stickyEntry
	StickyTTL time.Duration

	merge    atomic.Bool
	offMu    sync.RWMutex
	disabled map[string]bool
}

// SetMerge turns combining on or off. When off, everything goes through the
// single best channel, like a normal PC.
func (m *Manager) SetMerge(on bool) { m.merge.Store(on) }
func (m *Manager) Merge() bool      { return m.merge.Load() }

// SetEnabled includes or excludes one channel (by interface name) from use.
func (m *Manager) SetEnabled(name string, on bool) {
	m.offMu.Lock()
	if on {
		delete(m.disabled, name)
	} else {
		m.disabled[name] = true
	}
	m.offMu.Unlock()
}

func (m *Manager) Enabled(name string) bool {
	m.offMu.RLock()
	defer m.offMu.RUnlock()
	return !m.disabled[name]
}

// Disabled lists channel names the user switched off.
func (m *Manager) Disabled() []string {
	m.offMu.RLock()
	defer m.offMu.RUnlock()
	var out []string
	for n := range m.disabled {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Usable returns the channels new traffic may use right now: healthy and
// enabled, or just the best one when combining is off.
func (m *Manager) Usable() []*Channel {
	var out []*Channel
	for _, c := range m.Healthy() {
		if m.Enabled(c.Name) {
			out = append(out, c)
		}
	}
	if !m.Merge() && len(out) > 1 {
		best := out[0]
		for _, c := range out[1:] {
			if c.Capacity() > best.Capacity() || (c.Capacity() == best.Capacity() && c.RTT() < best.RTT()) {
				best = c
			}
		}
		return []*Channel{best}
	}
	return out
}

type stickyEntry struct {
	ch   *Channel
	seen time.Time
}

func NewManager() *Manager {
	m := &Manager{
		channels:  map[string]*Channel{},
		sticky:    map[string]stickyEntry{},
		StickyTTL: 10 * time.Minute,
		disabled:  map[string]bool{},
	}
	m.merge.Store(true)
	return m
}

// Start runs discovery, health probes and rate sampling until ctx ends.
func (m *Manager) Start(ctx context.Context) {
	m.discover()
	m.probeAll()
	go m.loop(ctx)
}

func (m *Manager) loop(ctx context.Context) {
	sample := time.NewTicker(time.Second)
	probe := time.NewTicker(5 * time.Second)
	rediscover := time.NewTicker(10 * time.Second)
	defer sample.Stop()
	defer probe.Stop()
	defer rediscover.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-sample.C:
			dt := now.Sub(last).Seconds()
			last = now
			for _, c := range m.All() {
				c.sample(dt)
			}
		case <-probe.C:
			m.probeAll()
			m.pruneSticky()
		case <-rediscover.C:
			m.discover()
		}
	}
}

func (m *Manager) probeAll() {
	var wg sync.WaitGroup
	for _, c := range m.All() {
		wg.Add(1)
		go func(c *Channel) { defer wg.Done(); c.probe() }(c)
	}
	wg.Wait()
}

func skip(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(name, " ", ""))
	for _, s := range skipNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

func globalUnicast(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast()
}

// discover finds interfaces that are up and have a usable address. Whether a
// candidate really reaches the internet is decided by the health probe.
func (m *Manager) discover() {
	ifs, err := net.Interfaces()
	if err != nil {
		return
	}
	info := adapterInfo()
	found := map[string]bool{}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skip(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var v4, v6 net.IP
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !globalUnicast(ipn.IP) {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil {
				if v4 == nil {
					v4 = ip4
				}
			} else if v6 == nil && !isTemporaryLike(ipn.IP) {
				v6 = ipn.IP
			}
		}
		if v4 == nil && v6 == nil {
			continue
		}
		found[ifc.Name] = true
		m.mu.Lock()
		old, ok := m.channels[ifc.Name]
		if !ok || old.Index != ifc.Index || !old.V4.Equal(v4) || !old.V6.Equal(v6) {
			c := newChannel(ifc.Name, ifc.Index, v4, v6)
			if a, ok := info[ifc.Index]; ok {
				c.Desc, c.Kind = a.desc, a.kind
			} else {
				c.Kind = KindOther
			}
			if ok {
				// keep counters across address changes
				c.rx.Store(old.rx.Load())
				c.tx.Store(old.tx.Load())
				c.lastRx, c.lastTx = c.rx.Load(), c.tx.Load()
			}
			m.channels[ifc.Name] = c
			if !ok {
				go c.probe()
			}
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	for name := range m.channels {
		if !found[name] {
			delete(m.channels, name)
		}
	}
	m.order = m.order[:0]
	for name := range m.channels {
		m.order = append(m.order, name)
	}
	sort.Strings(m.order)
	m.mu.Unlock()
}

// isTemporaryLike filters unique-local (fc00::/7) addresses, which never
// reach the internet.
func isTemporaryLike(ip net.IP) bool {
	return len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc
}

// All returns every known channel, healthy or not, in stable order.
func (m *Manager) All() []*Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Channel, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.channels[n])
	}
	return out
}

// Healthy returns channels whose probes currently succeed.
func (m *Manager) Healthy() []*Channel {
	var out []*Channel
	for _, c := range m.All() {
		if c.Up() {
			out = append(out, c)
		}
	}
	return out
}

// Best returns the usable channel with the highest capacity (ties: lowest
// RTT), falling back to any healthy one if the user disabled them all.
func (m *Manager) Best() *Channel {
	pool := m.Usable()
	if len(pool) == 0 {
		pool = m.Healthy()
	}
	var best *Channel
	for _, c := range pool {
		if best == nil || c.Capacity() > best.Capacity() ||
			(c.Capacity() == best.Capacity() && c.RTT() < best.RTT()) {
			best = c
		}
	}
	return best
}

// LowestRTT returns the healthy channel with the lowest latency.
func (m *Manager) LowestRTT() *Channel {
	var best *Channel
	for _, c := range m.Healthy() {
		if best == nil || c.RTT() < best.RTT() {
			best = c
		}
	}
	return best
}

// InUse reports whether new traffic may go through c right now.
func (m *Manager) InUse(c *Channel) bool { return contains(m.Usable(), c) }

func contains(cs []*Channel, c *Channel) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// minWeight keeps an idle channel in rotation so its capacity gets measured.
const minWeight = 1 << 20 // 1 MB/s

// Pick chooses a channel for a new connection to key (host or IP). The same
// key keeps the same channel while it stays healthy, so sites that tie a
// session to the client IP keep working.
func (m *Manager) Pick(key string) *Channel {
	usable := m.Usable()
	if len(usable) == 0 {
		return nil
	}
	if key != "" {
		m.stickyMu.Lock()
		e, ok := m.sticky[key]
		if ok && m.alive(e.ch) && contains(usable, e.ch) {
			e.seen = time.Now()
			m.sticky[key] = e
			m.stickyMu.Unlock()
			return e.ch
		}
		m.stickyMu.Unlock()
	}
	ch := weighted(usable)
	if key != "" {
		m.stickyMu.Lock()
		m.sticky[key] = stickyEntry{ch: ch, seen: time.Now()}
		m.stickyMu.Unlock()
	}
	return ch
}

func weighted(cs []*Channel) *Channel {
	if len(cs) == 1 {
		return cs[0]
	}
	ws := make([]float64, len(cs))
	var sum float64
	for i, c := range cs {
		w := c.Capacity()
		if w < minWeight {
			w = minWeight
		}
		ws[i] = w
		sum += w
	}
	r := rand.Float64() * sum
	for i, w := range ws {
		if r < w {
			return cs[i]
		}
		r -= w
	}
	return cs[len(cs)-1]
}

func (m *Manager) alive(c *Channel) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.channels[c.Name] == c
}

func (m *Manager) pruneSticky() {
	cut := time.Now().Add(-m.StickyTTL)
	m.stickyMu.Lock()
	for k, e := range m.sticky {
		if e.seen.Before(cut) {
			delete(m.sticky, k)
		}
	}
	m.stickyMu.Unlock()
}
