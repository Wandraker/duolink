package channel

import "strings"

// Kind is what the user would call the connection.
type Kind string

const (
	KindCable Kind = "cable"
	KindPhone Kind = "phone"
	KindWiFi  Kind = "wifi"
	KindOther Kind = "other"
)

type adapter struct {
	desc string
	kind Kind
}

var phoneHints = []string{"remote ndis", "rndis", "apple mobile", "iphone", "android", "usb modem", "mobile broadband", "wwan", "cdc ncm", "cdc ethernet"}

func classify(desc string, ifType uint32) Kind {
	d := strings.ToLower(desc)
	for _, h := range phoneHints {
		if strings.Contains(d, h) {
			return KindPhone
		}
	}
	switch ifType {
	case 71: // IF_TYPE_IEEE80211
		return KindWiFi
	case 243, 244: // WWAN
		return KindPhone
	case 6: // IF_TYPE_ETHERNET_CSMACD
		return KindCable
	}
	return KindOther
}
