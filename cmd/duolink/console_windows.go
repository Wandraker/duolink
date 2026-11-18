//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// attachConsole lets the GUI-subsystem build print to the terminal it was
// started from (for `duolink get` and -headless).
func attachConsole() {
	k := windows.NewLazySystemDLL("kernel32.dll")
	if r, _, _ := k.NewProc("AttachConsole").Call(uintptr(^uint32(0))); r == 0 { // ATTACH_PARENT_PROCESS
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}
