//go:build !windows

package elevate

import (
	"errors"
	"os"
	"time"
)

var ErrCanceled = errors.New("administrator rights were not granted")

func IsAdmin() bool { return os.Geteuid() == 0 }

func Relaunch(...string) error { return errors.New("not supported") }

func WaitExit(int, time.Duration) {}
