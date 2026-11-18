//go:build windows

// Package elevate checks for admin rights and restarts Duolink with them.
package elevate

import (
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// IsAdmin reports whether the process runs elevated.
func IsAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// ErrCanceled means the user said no to the UAC prompt.
var ErrCanceled = errors.New("administrator rights were not granted")

// Relaunch starts this executable elevated (UAC prompt) with args.
func Relaunch(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(quote(args))
	err = windows.ShellExecute(0, verb, file, params, nil, windows.SW_SHOWNORMAL)
	if err == windows.ERROR_CANCELLED {
		return ErrCanceled
	}
	return err
}

func quote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

// WaitExit waits for process pid to end (used by the elevated instance so
// the old one can release the ports first).
func WaitExit(pid int, timeout time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond))
}
