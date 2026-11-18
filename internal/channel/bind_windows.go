//go:build windows

package channel

import (
	"encoding/binary"
	"syscall"
)

const (
	ipUnicastIf   = 31 // IP_UNICAST_IF
	ipv6UnicastIf = 31 // IPV6_UNICAST_IF
)

// bindControl pins the socket's outgoing interface. Binding the source
// address alone is not enough on Windows when several interfaces have a
// default route, so IP_UNICAST_IF / IPV6_UNICAST_IF is set as well.
func bindControl(index int) func(network, address string, c syscall.RawConn) error {
	return func(network, _ string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			h := syscall.Handle(fd)
			if network[len(network)-1] == '6' {
				opErr = syscall.SetsockoptInt(h, syscall.IPPROTO_IPV6, ipv6UnicastIf, index)
				return
			}
			// IPv4 expects the index in network byte order.
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(index))
			opErr = syscall.SetsockoptInt(h, syscall.IPPROTO_IP, ipUnicastIf, int(binary.LittleEndian.Uint32(b[:])))
		})
		if err != nil {
			return err
		}
		return opErr
	}
}
