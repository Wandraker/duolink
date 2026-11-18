// Package web serves the local control panel and its JSON API.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Wandraker/duolink/internal/channel"
	"github.com/Wandraker/duolink/internal/dl"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	Mgr       *channel.Manager
	DL        *dl.Manager
	SocksAddr string
	Version   string

	// Hooks for the desktop shell; all optional.
	OnChange     func()      // merge or channel switches changed: persist, refresh tray
	OnShow       func()      // a second launch asked to bring the window up
	Autostart    func() bool // current autostart state
	SetAutostart func(bool) error
}

type chanJSON struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Desc     string  `json:"desc,omitempty"`
	Enabled  bool    `json:"enabled"`
	Up       bool    `json:"up"`
	RTTms    float64 `json:"rtt_ms"`
	RxRate   float64 `json:"rx_rate"`
	TxRate   float64 `json:"tx_rate"`
	Capacity float64 `json:"capacity"`
	RxTotal  uint64  `json:"rx_total"`
	TxTotal  uint64  `json:"tx_total"`
	Active   int64   `json:"active"`
	V4       string  `json:"v4,omitempty"`
	V6       string  `json:"v6,omitempty"`
}

type dlJSON struct {
	dl.Download
	Done       int64            `json:"done"`
	PerChannel map[string]int64 `json:"per_channel"`
}

func ipStr(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (s *Server) state() any {
	var chans []chanJSON
	for _, c := range s.Mgr.All() {
		rx, tx := c.Rates()
		rt, tt := c.Totals()
		chans = append(chans, chanJSON{
			Name: c.Name, Kind: string(c.Kind), Desc: c.Desc, Enabled: s.Mgr.Enabled(c.Name),
			Up: c.Up(), RTTms: float64(c.RTT().Microseconds()) / 1000,
			RxRate: rx, TxRate: tx, Capacity: c.Capacity(), RxTotal: rt, TxTotal: tt,
			Active: c.Active(), V4: ipStr(c.V4), V6: ipStr(c.V6),
		})
	}
	var dls []dlJSON
	list := s.DL.List()
	for i := len(list) - 1; i >= 0; i-- {
		d := list[i]
		done, per := d.Progress()
		dls = append(dls, dlJSON{Download: d.Snapshot(), Done: done, PerChannel: per})
	}
	auto := false
	if s.Autostart != nil {
		auto = s.Autostart()
	}
	return map[string]any{
		"version": s.Version, "socks": s.SocksAddr, "dir": s.DL.Dir,
		"merge": s.Mgr.Merge(), "autostart": auto, "app": s.OnShow != nil,
		"channels": chans, "downloads": dls,
	}
}

func readOn(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return false, false
	}
	return req.On, true
}

func (s *Server) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// reveal opens Explorer at path (selecting the file when it exists).
func reveal(path string) error {
	if runtime.GOOS != "windows" {
		return errors.New("only on Windows")
	}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return exec.Command("explorer", "/select,", path).Start()
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return exec.Command("explorer", path).Start()
}

// localOnly rejects requests whose Host or Origin is not this machine, so a
// web page cannot drive the panel through the browser (DNS rebinding / CSRF).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet {
			if !strings.HasPrefix(o, "http://127.0.0.1") && !strings.HasPrefix(o, "http://localhost") &&
				!strings.HasPrefix(o, "chrome-extension://") && !strings.HasPrefix(o, "moz-extension://") {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.state())
	})
	mux.HandleFunc("POST /api/downloads", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		d, err := s.DL.Add(req.URL, req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"id": d.ID})
	})
	mux.HandleFunc("POST /api/merge", func(w http.ResponseWriter, r *http.Request) {
		on, ok := readOn(w, r)
		if !ok {
			return
		}
		s.Mgr.SetMerge(on)
		s.changed()
		writeJSON(w, map[string]bool{"merge": on})
	})
	mux.HandleFunc("POST /api/channels/{name}/enabled", func(w http.ResponseWriter, r *http.Request) {
		on, ok := readOn(w, r)
		if !ok {
			return
		}
		s.Mgr.SetEnabled(r.PathValue("name"), on)
		s.changed()
		writeJSON(w, map[string]bool{"enabled": on})
	})
	mux.HandleFunc("POST /api/autostart", func(w http.ResponseWriter, r *http.Request) {
		on, ok := readOn(w, r)
		if !ok {
			return
		}
		if s.SetAutostart == nil {
			http.Error(w, "not available", http.StatusNotImplemented)
			return
		}
		if err := s.SetAutostart(on); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.changed()
		writeJSON(w, map[string]bool{"autostart": on})
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		if s.OnShow != nil {
			s.OnShow()
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		if err := reveal(s.DL.Dir); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/downloads/{id}/reveal", func(w http.ResponseWriter, r *http.Request) {
		d := s.DL.Get(r.PathValue("id"))
		if d == nil {
			http.NotFound(w, r)
			return
		}
		if err := reveal(d.Snapshot().Path); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/downloads/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		d := s.DL.Get(r.PathValue("id"))
		if d == nil {
			http.NotFound(w, r)
			return
		}
		d.Cancel()
		writeJSON(w, map[string]bool{"ok": true})
	})
	return localOnly(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
