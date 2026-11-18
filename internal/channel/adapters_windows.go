//go:build windows

package channel

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// adapterInfo maps interface index to adapter description and kind.
func adapterInfo() map[int]adapter {
	out := map[int]adapter{}
	size := uint32(15000)
	for tries := 0; tries < 3; tries++ {
		buf := make([]byte, size)
		aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER, 0, aa, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return out
		}
		for a := aa; a != nil; a = a.Next {
			desc := windows.UTF16PtrToString(a.Description)
			idx := int(a.IfIndex)
			if idx == 0 {
				idx = int(a.Ipv6IfIndex)
			}
			out[idx] = adapter{desc: desc, kind: classify(desc, a.IfType)}
		}
		return out
	}
	return out
}
