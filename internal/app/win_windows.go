//go:build windows

package app

import (
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	dwmapi = windows.NewLazySystemDLL("dwmapi.dll")
	kernel = windows.NewLazySystemDLL("kernel32.dll")
	ole32  = windows.NewLazySystemDLL("ole32.dll")

	procCreateIconFromResourceEx      = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procIsIconic                      = user32.NewProc("IsIconic")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetUserDefaultUILanguage      = kernel.NewProc("GetUserDefaultUILanguage")
	procDwmSetWindowAttribute         = dwmapi.NewProc("DwmSetWindowAttribute")
	_                                 = ole32
)

const (
	wmSetIcon = 0x0080
	swRestore = 9
	swShow    = 5

	dwmwaUseImmersiveDarkMode = 20
	dwmwaCaptionColor         = 35
	dwmwaTextColor            = 36
)

// Supported reports whether the desktop shell is available on this OS.
func Supported() bool { return true }

func enableDPIAwareness() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	if procSetProcessDpiAwarenessContext.Find() == nil {
		procSetProcessDpiAwarenessContext.Call(uintptr(^uintptr(3)))
	}
}

// iconFromPNG builds an HICON from one PNG image (Vista+ accepts PNG data).
func iconFromPNG(png []byte, size int) uintptr {
	if len(png) == 0 {
		return 0
	}
	h, _, _ := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)),
		1, 0x00030000, uintptr(size), uintptr(size), 0)
	return h
}

func setWindowIcons(hwnd uintptr, small, big uintptr) {
	if small != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, 0, small)
	}
	if big != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, 1, big)
	}
}

// systemDark reports whether apps use the dark theme.
func systemDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err != nil || v == 0
}

func colorref(r, g, b byte) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

// styleTitleBar paints the title bar in the panel's own colour.
func styleTitleBar(hwnd uintptr, dark bool) {
	if procDwmSetWindowAttribute.Find() != nil {
		return
	}
	on := uint32(0)
	cap := colorref(0xEE, 0xF1, 0xF6)
	txt := colorref(0x1B, 0x20, 0x2B)
	if dark {
		on = 1
		cap = colorref(0x12, 0x15, 0x1C)
		txt = colorref(0xE9, 0xED, 0xF5)
	}
	procDwmSetWindowAttribute.Call(hwnd, dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&on)), 4)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaCaptionColor, uintptr(unsafe.Pointer(&cap)), 4)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaTextColor, uintptr(unsafe.Pointer(&txt)), 4)
}

func bringToFront(hwnd uintptr) {
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		procShowWindow.Call(hwnd, swRestore)
	} else {
		procShowWindow.Call(hwnd, swShow)
	}
	procSetForegroundWindow.Call(hwnd)
}

// russianUI reports whether the Windows display language is Russian.
func russianUI() bool {
	if procGetUserDefaultUILanguage.Find() != nil {
		return false
	}
	l, _, _ := procGetUserDefaultUILanguage.Call()
	return l&0x3ff == 0x19 // LANG_RUSSIAN
}

// singleInstance returns false if another Duolink already runs for this user.
func singleInstance() (bool, windows.Handle) {
	name, _ := windows.UTF16PtrFromString(`Local\Duolink.SingleInstance`)
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		return false, h
	}
	return true, h
}
