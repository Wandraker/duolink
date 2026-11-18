// Package channel discovers internet uplinks (Ethernet, USB-tethered phone,
// Wi-Fi...) and dials connections pinned to one of them.
package channel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Channel is one internet uplink bound to a network interface.
type Channel struct {
	Name  string
	Index int
	V4    net.IP
	V6    net.IP

	up     atomic.Bool
	rttNs  atomic.Int64
	rx, tx atomic.Uint64
	active atomic.Int64
	fails  atomic.Int32

	mu             sync.Mutex
	rxRate, txRate float64 // bytes/s, smoothed
	peak           float64 // decaying peak of rx+tx, bytes/s
	lastRx, lastTx uint64

	resolver *net.Resolver
}

func newChannel(name string, index int, v4, v6 net.IP) *Channel {
	c := &Channel{Name: name, Index: index, V4: v4, V6: v6}
	c.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var lastErr error
			for _, srv := range c.dnsServers() {
				conn, err := c.dialRaw(ctx, "udp", srv)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return c
}

func (c *Channel) dnsServers() []string {
	var s []string
	if c.V4 != nil {
		s = append(s, "1.1.1.1:53", "8.8.8.8:53")
	}
	if c.V6 != nil {
		s = append(s, "[2606:4700:4700::1111]:53")
	}
	return s
}

// Up reports whether the last health probes succeeded.
func (c *Channel) Up() bool { return c.up.Load() }

// RTT is the last measured TCP connect time to a probe host.
func (c *Channel) RTT() time.Duration { return time.Duration(c.rttNs.Load()) }

// Active is the number of open connections through this channel.
func (c *Channel) Active() int64 { return c.active.Load() }

// Totals returns bytes received and sent through this channel.
func (c *Channel) Totals() (rx, tx uint64) { return c.rx.Load(), c.tx.Load() }

// Rates returns smoothed receive/send rates in bytes per second.
func (c *Channel) Rates() (rx, tx float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rxRate, c.txRate
}

// Capacity is the decaying peak throughput, used as the balancing weight.
func (c *Channel) Capacity() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// AddRx / AddTx account traffic that does not go through a wrapped conn (UDP relay).
func (c *Channel) AddRx(n int) { c.rx.Add(uint64(n)) }
func (c *Channel) AddTx(n int) { c.tx.Add(uint64(n)) }

func (c *Channel) sample(dt float64) {
	rx, tx := c.rx.Load(), c.tx.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	r := float64(rx-c.lastRx) / dt
	t := float64(tx-c.lastTx) / dt
	c.lastRx, c.lastTx = rx, tx
	const a = 0.5
	c.rxRate = a*r + (1-a)*c.rxRate
	c.txRate = a*t + (1-a)*c.txRate
	c.peak *= 0.995 // half-life ~2 min
	if cur := r + t; cur > c.peak {
		c.peak = cur
	}
}

func (c *Channel) localFor(ip net.IP) net.IP {
	if ip.To4() != nil {
		return c.V4
	}
	return c.V6
}

// dialRaw dials an IP:port (no DNS) through this channel.
func (c *Channel) dialRaw(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("dialRaw needs an IP, got %q", host)
	}
	local := c.localFor(ip)
	if local == nil {
		return nil, fmt.Errorf("%s: no %s address", c.Name, familyName(ip))
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: bindControl(c.Index)}
	if network == "udp" {
		d.LocalAddr = &net.UDPAddr{IP: local}
	} else {
		d.LocalAddr = &net.TCPAddr{IP: local}
	}
	return d.DialContext(ctx, network+familySuffix(ip), addr)
}

// Resolve looks up host using DNS servers reached through this channel.
func (c *Channel) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := c.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var v4, v6 []net.IP
	for _, a := range addrs {
		if a.IP.To4() != nil {
			if c.V4 != nil {
				v4 = append(v4, a.IP)
			}
		} else if c.V6 != nil {
			v6 = append(v6, a.IP)
		}
	}
	ips := append(v4, v6...) // IPv4 first: usually the more reliable path on mobile
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s: %s has no reachable address family", c.Name, host)
	}
	return ips, nil
}

// DialContext dials host:port over TCP through this channel. Traffic is counted.
func (c *Channel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if _, err := strconv.Atoi(portStr); err != nil {
		return nil, fmt.Errorf("bad port %q", portStr)
	}
	ips, err := c.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := c.dialRaw(ctx, "tcp", net.JoinHostPort(ip.String(), portStr))
		if err == nil {
			c.active.Add(1)
			return &countConn{Conn: conn, ch: c}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no addresses")
	}
	return nil, lastErr
}

// ListenUDP opens an unconnected UDP socket bound to this channel for the
// family of ip.
func (c *Channel) ListenUDP(ip net.IP) (*net.UDPConn, error) {
	local := c.localFor(ip)
	if local == nil {
		return nil, fmt.Errorf("%s: no %s address", c.Name, familyName(ip))
	}
	lc := net.ListenConfig{Control: bindControl(c.Index)}
	pc, err := lc.ListenPacket(context.Background(), "udp"+familySuffix(ip), net.JoinHostPort(local.String(), "0"))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

func (c *Channel) probe() {
	targets := []string{}
	if c.V4 != nil {
		targets = append(targets, "1.1.1.1:443", "8.8.8.8:443")
	}
	if c.V6 != nil {
		targets = append(targets, "[2606:4700:4700::1111]:443")
	}
	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		start := time.Now()
		conn, err := c.dialRaw(ctx, "tcp", t)
		cancel()
		if err == nil {
			conn.Close()
			c.rttNs.Store(int64(time.Since(start)))
			c.fails.Store(0)
			c.up.Store(true)
			return
		}
	}
	if c.fails.Add(1) >= 2 {
		c.up.Store(false)
	}
}

type countConn struct {
	net.Conn
	ch   *Channel
	once sync.Once
}

func (cc *countConn) Read(b []byte) (int, error) {
	n, err := cc.Conn.Read(b)
	cc.ch.rx.Add(uint64(n))
	return n, err
}

func (cc *countConn) Write(b []byte) (int, error) {
	n, err := cc.Conn.Write(b)
	cc.ch.tx.Add(uint64(n))
	return n, err
}

func (cc *countConn) Close() error {
	cc.once.Do(func() { cc.ch.active.Add(-1) })
	return cc.Conn.Close()
}

// CloseWrite half-closes TCP connections so proxied streams end cleanly.
func (cc *countConn) CloseWrite() error {
	if tc, ok := cc.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return nil
}

func familySuffix(ip net.IP) string {
	if ip.To4() != nil {
		return "4"
	}
	return "6"
}

func familyName(ip net.IP) string {
	if ip.To4() != nil {
		return "IPv4"
	}
	return "IPv6"
}
