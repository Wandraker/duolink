package channel

import (
	"net"
	"testing"
)

func testManager() (*Manager, *Channel, *Channel) {
	m := NewManager()
	a := newChannel("A", 1, net.IPv4(10, 0, 0, 1), nil)
	b := newChannel("B", 2, net.IPv4(10, 0, 1, 1), nil)
	a.up.Store(true)
	b.up.Store(true)
	m.channels = map[string]*Channel{"A": a, "B": b}
	m.order = []string{"A", "B"}
	return m, a, b
}

// Single connections to a host stay on one channel (logins, banking).
func TestPickSticky(t *testing.T) {
	m, _, _ := testManager()
	first := m.Pick("bank.example")
	for i := 0; i < 2; i++ {
		if c := m.Pick("bank.example"); c != first {
			t.Fatalf("connection %d moved from %s to %s", i+2, first.Name, c.Name)
		}
	}
}

// Many parallel connections to one host (Steam CDN) use every channel.
func TestPickBurstSpreads(t *testing.T) {
	m, a, b := testManager()
	seen := map[*Channel]int{}
	for i := 0; i < 64; i++ {
		seen[m.Pick("199.232.215.82")]++
	}
	if seen[a] == 0 || seen[b] == 0 {
		t.Fatalf("burst stuck to one channel: A=%d B=%d", seen[a], seen[b])
	}
}
