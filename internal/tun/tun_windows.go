//go:build windows

// Package tun routes all system traffic through Duolink: a Wintun adapter
// takes the default route, a userspace TCP/IP stack (gVisor, via tun2socks)
// turns packets back into connections, and each connection goes out through
// a channel picked by the balancer. Duolink's own sockets are pinned to the
// physical interfaces, so they never loop back into the adapter.
package tun

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	t2s "github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/Wandraker/duolink/internal/channel"
)

//go:embed wintun/wintun-amd64.dll
var wintunAMD64 []byte

//go:embed wintun/wintun-arm64.dll
var wintunARM64 []byte

// AdapterName is the name of the virtual adapter in Windows.
const AdapterName = "Duolink"

const (
	addr4 = "198.18.0.1"
	mask4 = "255.254.0.0"
	addr6 = "fdd0:d0d0:1::1"
	mtu   = 1500
)

type Tunnel struct {
	Mgr *channel.Manager

	mu    sync.Mutex
	dev   device.Device
	stk   *stack.Stack
	on    bool
	err   string
	since time.Time
	gen   int
	v6    bool // the IPv6 half-default routes are in place
}

// State is shown in the panel.
type State struct {
	On    bool   `json:"on"`
	Error string `json:"error,omitempty"`
	Since int64  `json:"since,omitempty"`
}

func (t *Tunnel) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := State{On: t.on, Error: t.err}
	if t.on {
		s.Since = t.since.Unix()
	}
	return s
}

// ensureDLL puts wintun.dll next to the executable, where the Wintun
// loader looks for it.
func ensureDLL() error {
	data := wintunAMD64
	if runtime.GOARCH == "arm64" {
		data = wintunARM64
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	p := filepath.Join(filepath.Dir(exe), "wintun.dll")
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return os.WriteFile(p, data, 0o644)
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// configure gives the adapter addresses, DNS and the two half-default
// routes (more specific than any 0.0.0.0/0, so they win without touching
// the existing routes). Everything is "active" only: gone after reboot.
func configure() error {
	n := "name=" + AdapterName
	steps := [][]string{
		{"interface", "ipv4", "set", "address", n, "source=static", "address=" + addr4, "mask=" + mask4},
		{"interface", "ipv4", "set", "interface", AdapterName, "metric=1"},
		{"interface", "ipv4", "set", "dnsservers", n, "source=static", "address=1.1.1.1", "register=none", "validate=no"},
		{"interface", "ipv4", "add", "dnsservers", n, "address=8.8.8.8", "index=2", "validate=no"},
		{"interface", "ipv4", "add", "route", "0.0.0.0/1", AdapterName, "nexthop=0.0.0.0", "metric=1", "store=active"},
		{"interface", "ipv4", "add", "route", "128.0.0.0/1", AdapterName, "nexthop=0.0.0.0", "metric=1", "store=active"},
	}
	for _, s := range steps {
		if err := run("netsh", s...); err != nil {
			return err
		}
	}
	// IPv6 is best effort; its routes come and go with syncV6.
	run("netsh", "interface", "ipv6", "add", "address", AdapterName, addr6+"/64", "store=active")
	run("netsh", "interface", "ipv6", "set", "interface", AdapterName, "metric=1")
	return nil
}

// wantV6 reports whether some usable channel can carry IPv6.
func (t *Tunnel) wantV6() bool {
	for _, c := range t.Mgr.Usable() {
		if c.V6 != nil {
			return true
		}
	}
	return false
}

// syncV6 takes IPv6 into the tunnel only while a channel can carry it.
// Without that, the tunnel would accept IPv6 connections it cannot deliver:
// the app sees the connection open and then reset, and never falls back to
// IPv4 (HTTPS sites with IPv6 stop loading when only the cable is left).
// Called with t.mu held.
func (t *Tunnel) syncV6() {
	want := t.wantV6()
	if want == t.v6 {
		return
	}
	verb := "delete"
	if want {
		verb = "add"
	}
	for _, r := range []string{"::/1", "8000::/1"} {
		args := []string{"interface", "ipv6", verb, "route", r, AdapterName}
		if want {
			args = append(args, "metric=1", "store=active")
		}
		run("netsh", args...)
	}
	t.v6 = want
}

// v6Loop follows channels appearing and disappearing.
func (t *Tunnel) v6Loop(gen int) {
	for {
		time.Sleep(2 * time.Second)
		t.mu.Lock()
		if !t.on || t.gen != gen {
			t.mu.Unlock()
			return
		}
		t.syncV6()
		t.mu.Unlock()
	}
}

// waitAdapter waits until Windows lists the new interface.
func waitAdapter() error {
	for i := 0; i < 50; i++ {
		if _, err := net.InterfaceByName(AdapterName); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("the virtual adapter did not appear")
}

// Start creates the adapter and takes over routing. Requires admin.
func (t *Tunnel) Start() (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.on {
		return nil
	}
	defer func() {
		if err != nil {
			t.err = err.Error()
			t.stopLocked()
		} else {
			t.err = ""
		}
	}()
	if err := ensureDLL(); err != nil {
		return fmt.Errorf("wintun.dll: %w", err)
	}
	tunnel.T().SetProxy(&dialer{mgr: t.Mgr})
	tunnel.T().SetUDPTimeout(60 * time.Second)
	dev, err := t2s.Open(AdapterName, mtu)
	if err != nil {
		return fmt.Errorf("create adapter: %w", err)
	}
	t.dev = dev
	stk, err := core.CreateStack(&core.Config{LinkEndpoint: dev, TransportHandler: tunnel.T()})
	if err != nil {
		return fmt.Errorf("network stack: %w", err)
	}
	t.stk = stk
	if err := waitAdapter(); err != nil {
		return err
	}
	if err := configure(); err != nil {
		return err
	}
	t.v6 = false
	t.syncV6()
	t.on, t.since = true, time.Now()
	t.gen++
	go t.watchdog(t.gen)
	go t.v6Loop(t.gen)
	return nil
}

// watchdog turns TUN off if traffic through the adapter stops working while
// the physical links are fine, so a bug here can never leave the PC offline.
func (t *Tunnel) watchdog(gen int) {
	fails := 0
	for {
		time.Sleep(10 * time.Second)
		t.mu.Lock()
		alive := t.on && t.gen == gen
		t.mu.Unlock()
		if !alive {
			return
		}
		if len(t.Mgr.Healthy()) == 0 {
			fails = 0 // no internet at all: not our fault, nothing to undo
			continue
		}
		// An unbound socket follows the system routes, i.e. goes through TUN.
		ok := false
		for _, a := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
			c, err := net.DialTimeout("tcp4", a, 4*time.Second)
			if err == nil {
				c.Close()
				ok = true
				break
			}
		}
		if ok {
			fails = 0
			continue
		}
		if fails++; fails >= 3 {
			t.mu.Lock()
			if t.on && t.gen == gen {
				t.stopLocked()
				t.err = "TUN stopped passing traffic and was turned off automatically; the network is back to normal"
			}
			t.mu.Unlock()
			return
		}
	}
}

// stopLocked gives routing back first, then tears the stack down.
//
// Order matters: removing the adapter deletes every route we added, so the
// internet works again immediately. Waiting on the stack first can block for
// a long time with many open connections, and meanwhile the routes would
// still point into a dead adapter (that once cut the PC off the internet).
func (t *Tunnel) stopLocked() {
	removeRoutes() // explicit, in case closing the adapter is slow
	if t.dev != nil {
		t.dev.Close() // removes the adapter, and with it every route we added
		t.dev = nil
	}
	if t.stk != nil {
		stk := t.stk
		t.stk = nil
		done := make(chan struct{})
		go func() { stk.Close(); stk.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second): // let it finish in the background
		}
	}
	t.on, t.v6 = false, false
}

// removeRoutes deletes our half-default routes. Errors are ignored: the
// routes may already be gone with the adapter.
func removeRoutes() {
	for _, s := range [][]string{
		{"interface", "ipv4", "delete", "route", "0.0.0.0/1", AdapterName},
		{"interface", "ipv4", "delete", "route", "128.0.0.0/1", AdapterName},
		{"interface", "ipv6", "delete", "route", "::/1", AdapterName},
		{"interface", "ipv6", "delete", "route", "8000::/1", AdapterName},
	} {
		run("netsh", s...)
	}
}

// Stop removes the adapter; Windows routing returns to how it was.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	t.stopLocked()
	t.err = ""
	t.mu.Unlock()
}

// dialer hands tunnelled connections to the balancer.
type dialer struct{ mgr *channel.Manager }

func (d *dialer) pick(m *M.Metadata) (*channel.Channel, net.IP, error) {
	ip := net.IP(m.DstIP.AsSlice())
	ch := d.mgr.PickFor(m.DstIP.String(), ip)
	if ch == nil {
		return nil, ip, errors.New("no channel can reach " + m.DstIP.String())
	}
	return ch, ip, nil
}

func (d *dialer) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	ch, ip, err := d.pick(m)
	if err != nil {
		return nil, err
	}
	addr := m.DestinationAddress()
	conn, err := ch.DialContext(ctx, "tcp", addr)
	if err == nil {
		return conn, nil
	}
	// One retry on another channel of the same family.
	for _, o := range d.mgr.Usable() {
		if o != ch && o.HasFamily(ip) {
			if c, e := o.DialContext(ctx, "tcp", addr); e == nil {
				return c, nil
			}
		}
	}
	return nil, err
}

func (d *dialer) DialUDP(m *M.Metadata) (net.PacketConn, error) {
	ch, ip, err := d.pick(m)
	if err != nil {
		return nil, err
	}
	pc, err := ch.ListenUDP(ip)
	if err != nil {
		return nil, err
	}
	ch.OpenUDP()
	return &countPC{UDPConn: pc, ch: ch}, nil
}

type countPC struct {
	*net.UDPConn
	ch   *channel.Channel
	once sync.Once
}

func (p *countPC) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := p.UDPConn.ReadFrom(b)
	p.ch.AddRx(n)
	return n, a, err
}

func (p *countPC) WriteTo(b []byte, a net.Addr) (int, error) {
	n, err := p.UDPConn.WriteTo(b, a)
	p.ch.AddTx(n)
	return n, err
}

func (p *countPC) Close() error {
	p.once.Do(p.ch.CloseUDP)
	return p.UDPConn.Close()
}
