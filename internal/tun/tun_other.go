//go:build !windows

package tun

import (
	"errors"

	"github.com/Wandraker/duolink/internal/channel"
)

type Tunnel struct{ Mgr *channel.Manager }

type State struct {
	On    bool   `json:"on"`
	Error string `json:"error,omitempty"`
	Since int64  `json:"since,omitempty"`
}

func (t *Tunnel) State() State { return State{} }
func (t *Tunnel) Start() error { return errors.New("TUN mode is only available on Windows") }
func (t *Tunnel) Stop()        {}
