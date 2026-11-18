// Package settings stores user choices in %APPDATA%\Duolink\settings.json.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	Merge    bool     `json:"merge"`
	Disabled []string `json:"disabled_channels,omitempty"`
}

var mu sync.Mutex

func Dir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "Duolink")
}

func path() string { return filepath.Join(Dir(), "settings.json") }

// Load returns saved settings, or defaults (combining on) if none exist.
func Load() Settings {
	mu.Lock()
	defer mu.Unlock()
	s := Settings{Merge: true}
	if b, err := os.ReadFile(path()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func Save(s Settings) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path())
}
