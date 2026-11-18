//go:build !windows

package app

import (
	"github.com/Wandraker/duolink/internal/channel"
	"github.com/Wandraker/duolink/internal/web"
)

// Shell is a no-op outside Windows; Duolink runs headless there.
type Shell struct {
	Mgr      *channel.Manager
	Web      *web.Server
	PanelURL string
	Hidden   bool
	Quit     func()
	TunOn    func() bool
}

func (s *Shell) Exit() {}

func AlreadyRunning(string) bool { return false }

func (s *Shell) Run() {}

func Supported() bool { return false }
