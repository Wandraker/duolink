//go:build !windows

package autostart

import "errors"

func Enabled() bool { return false }

func Set(bool) error { return errors.New("autostart is only supported on Windows") }
