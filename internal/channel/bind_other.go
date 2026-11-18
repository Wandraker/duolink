//go:build !windows

package channel

import "syscall"

// bindControl is a no-op outside Windows for now: the source address bind
// is used alone. Linux support (SO_BINDTODEVICE) is planned.
func bindControl(int) func(network, address string, c syscall.RawConn) error {
	return nil
}

func adapterInfo() map[int]adapter { return map[int]adapter{} }
