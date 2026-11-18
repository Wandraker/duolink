// Package socks is a SOCKS5 server (CONNECT + UDP ASSOCIATE, no auth) that
// spreads outgoing connections across channels.
package socks

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/Wandraker/duolink/internal/channel"
)

type Server struct {
	Mgr *channel.Manager
	Log *log.Logger
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

const (
	cmdConnect = 1
	cmdUDP     = 3

	atypV4     = 1
	atypDomain = 3
	atypV6     = 4

	repOK          = 0
	repFail        = 1
	repNetUnreach  = 3
	repHostUnreach = 4
	repCmdUnsup    = 7
)

func (s *Server) logf(f string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(f, a...)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	defer func() {
		if r := recover(); r != nil {
			s.logf("socks handler panic: %v", r)
		}
	}()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	c.Write([]byte{5, 0}) // no auth

	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return
	}
	host, port, err := readAddr(c, req[3])
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	switch req[1] {
	case cmdConnect:
		s.connect(c, host, port)
	case cmdUDP:
		s.udpAssociate(c)
	default:
		reply(c, repCmdUnsup, nil)
	}
}

func readAddr(r io.Reader, atyp byte) (string, int, error) {
	var host string
	switch atyp {
	case atypV4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", 0, err
		}
		host = net.IP(b).String()
	case atypV6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", 0, err
		}
		host = net.IP(b).String()
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", 0, err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", 0, err
		}
		host = string(b)
	default:
		return "", 0, errors.New("bad atyp")
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(p[:])), nil
}

func reply(w io.Writer, rep byte, bound *net.UDPAddr) {
	b := []byte{5, rep, 0, atypV4, 0, 0, 0, 0, 0, 0}
	if bound != nil {
		if ip4 := bound.IP.To4(); ip4 != nil {
			copy(b[4:8], ip4)
		}
		binary.BigEndian.PutUint16(b[8:], uint16(bound.Port))
	}
	w.Write(b)
}

func (s *Server) connect(c net.Conn, host string, port int) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	tried := map[*channel.Channel]bool{}
	var out net.Conn
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ch := s.Mgr.Pick(host)
		if ch == nil {
			break
		}
		if tried[ch] {
			// sticky choice failed before: try any other healthy channel
			for _, o := range s.Mgr.Healthy() {
				if !tried[o] {
					ch = o
					break
				}
			}
			if tried[ch] {
				break
			}
		}
		tried[ch] = true
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		out, lastErr = ch.DialContext(ctx, "tcp", addr)
		cancel()
		if lastErr == nil {
			break
		}
		s.logf("connect %s via %s: %v", addr, ch.Name, lastErr)
	}
	if out == nil {
		if lastErr == nil {
			reply(c, repNetUnreach, nil)
		} else {
			reply(c, repHostUnreach, nil)
		}
		return
	}
	defer out.Close()
	reply(c, repOK, nil)
	pipe(c, out)
}

type closeWriter interface{ CloseWrite() error }

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// --- UDP ASSOCIATE ---

type udpAssoc struct {
	s      *Server
	relay  *net.UDPConn
	mu     sync.Mutex
	client *net.UDPAddr
	outs   map[string]*net.UDPConn // channel name + family -> socket
	chans  map[*net.UDPConn]*channel.Channel
	done   chan struct{}
}

func (s *Server) udpAssociate(c net.Conn) {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		reply(c, repFail, nil)
		return
	}
	a := &udpAssoc{s: s, relay: relay, outs: map[string]*net.UDPConn{}, chans: map[*net.UDPConn]*channel.Channel{}, done: make(chan struct{})}
	reply(c, repOK, relay.LocalAddr().(*net.UDPAddr))
	go a.fromClient()
	// The association lives as long as the TCP control connection.
	io.Copy(io.Discard, c)
	close(a.done)
	relay.Close()
	a.mu.Lock()
	for _, o := range a.outs {
		o.Close()
	}
	a.mu.Unlock()
}

func (a *udpAssoc) fromClient() {
	buf := make([]byte, 65535)
	for {
		n, src, err := a.relay.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.client == nil {
			a.client = src
		}
		ok := a.client.String() == src.String()
		a.mu.Unlock()
		if !ok || n < 4 || buf[2] != 0 { // fragmentation unsupported
			continue
		}
		r := &byteReader{b: buf[4:n]}
		host, port, err := readAddr(r, buf[3])
		if err != nil {
			continue
		}
		payload := r.b
		a.send(host, port, payload)
	}
}

func (a *udpAssoc) send(host string, port int, payload []byte) {
	ch := a.s.Mgr.Pick(host)
	if ch == nil {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := ch.Resolve(ctx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			return
		}
		ip = ips[0]
	}
	out, err := a.outFor(ch, ip)
	if err != nil {
		return
	}
	n, err := out.WriteToUDP(payload, &net.UDPAddr{IP: ip, Port: port})
	if err == nil {
		ch.AddTx(n)
	}
}

func (a *udpAssoc) outFor(ch *channel.Channel, ip net.IP) (*net.UDPConn, error) {
	key := ch.Name + "/4"
	if ip.To4() == nil {
		key = ch.Name + "/6"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if o, ok := a.outs[key]; ok {
		return o, nil
	}
	o, err := ch.ListenUDP(ip)
	if err != nil {
		return nil, err
	}
	a.outs[key] = o
	a.chans[o] = ch
	go a.fromRemote(o, ch)
	return o, nil
}

func (a *udpAssoc) fromRemote(o *net.UDPConn, ch *channel.Channel) {
	buf := make([]byte, 65535)
	for {
		n, src, err := o.ReadFromUDP(buf)
		if err != nil {
			return
		}
		ch.AddRx(n)
		a.mu.Lock()
		client := a.client
		a.mu.Unlock()
		if client == nil {
			continue
		}
		pkt := make([]byte, 0, n+22)
		if ip4 := src.IP.To4(); ip4 != nil {
			pkt = append(pkt, 0, 0, 0, atypV4)
			pkt = append(pkt, ip4...)
		} else {
			pkt = append(pkt, 0, 0, 0, atypV6)
			pkt = append(pkt, src.IP.To16()...)
		}
		pkt = binary.BigEndian.AppendUint16(pkt, uint16(src.Port))
		pkt = append(pkt, buf[:n]...)
		a.relay.WriteToUDP(pkt, client)
	}
}

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
