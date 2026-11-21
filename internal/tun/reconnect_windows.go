//go:build windows

package tun

import (
	"errors"
	"net"
	"os"
	"syscall"
	"unsafe"
)

var (
	iphlpapi           = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcp = iphlpapi.NewProc("GetExtendedTcpTable")
	procSetTcpEntry    = iphlpapi.NewProc("SetTcpEntry")
	errReconnectOff    = errors.New("turn on \"all apps\" mode first")
	errReconnectDenied = errors.New("administrator rights are needed to reconnect apps")
)

const (
	afInet                  = 2
	tcpTableOwnerPidConns   = 4 // TCP_TABLE_OWNER_PID_CONNECTIONS
	tcpStateEstablished     = 5
	tcpStateDeleteTCB       = 12
	errorInsufficientBuffer = 122
	errorAccessDenied       = 5
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID; the first five fields are
// MIB_TCPROW, which SetTcpEntry takes.
type mibTCPRowOwnerPID struct {
	State, LocalAddr, LocalPort, RemoteAddr, RemotePort, OwningPID uint32
}

func ipv4(a uint32) net.IP { return net.IPv4(byte(a), byte(a>>8), byte(a>>16), byte(a>>24)) }

// Reconnect closes the IPv4 TCP connections other programs opened straight
// through a physical channel (before TUN mode came on, Windows keeps them on
// the old route). The programs reconnect at once, and the new connections go
// through the Duolink adapter. Duolink's own connections are left alone.
// Windows has no API to close IPv6 connections, so those stay as they are.
func (t *Tunnel) Reconnect() (int, error) {
	if !t.State().On {
		return 0, errReconnectOff
	}
	local := map[string]bool{}
	for _, c := range t.Mgr.All() {
		if c.V4 != nil {
			local[c.V4.String()] = true
		}
	}
	rows, err := tcpTable()
	if err != nil {
		return 0, err
	}
	self := uint32(os.Getpid())
	closed, denied := 0, 0
	for _, r := range rows {
		if r.State != tcpStateEstablished || r.OwningPID == self || !local[ipv4(r.LocalAddr).String()] {
			continue
		}
		row := r
		row.State = tcpStateDeleteTCB
		ret, _, _ := procSetTcpEntry.Call(uintptr(unsafe.Pointer(&row)))
		switch ret {
		case 0:
			closed++
		case errorAccessDenied:
			denied++
		}
	}
	if closed == 0 && denied > 0 {
		return 0, errReconnectDenied
	}
	return closed, nil
}

func tcpTable() ([]mibTCPRowOwnerPID, error) {
	size := uint32(0)
	for i := 0; i < 4; i++ {
		buf := make([]byte, size+4096)
		size = uint32(len(buf))
		ret, _, _ := procGetExtendedTcp.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, afInet, tcpTableOwnerPidConns, 0)
		if ret == errorInsufficientBuffer {
			continue
		}
		if ret != 0 {
			return nil, syscall.Errno(ret)
		}
		n := *(*uint32)(unsafe.Pointer(&buf[0]))
		rows := unsafe.Slice((*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[4])), n)
		return append([]mibTCPRowOwnerPID(nil), rows...), nil
	}
	return nil, errors.New("TCP table keeps growing")
}
