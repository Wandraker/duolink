//go:build windows

// Package app is the Windows desktop shell: a tray icon and a native window
// that shows the local panel through WebView2.
package app

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
	webview2 "github.com/jchv/go-webview2"

	"github.com/Wandraker/duolink/internal/autostart"
	"github.com/Wandraker/duolink/internal/channel"
	"github.com/Wandraker/duolink/internal/icon"
	"github.com/Wandraker/duolink/internal/settings"
	"github.com/Wandraker/duolink/internal/web"
)

type Shell struct {
	Mgr      *channel.Manager
	Web      *web.Server
	PanelURL string
	Hidden   bool        // start in the tray without opening the window
	Quit     func()      // stops the core
	TunOn    func() bool // whether TUN mode is active

	ru bool

	mu   sync.Mutex
	win  webview2.WebView
	hwnd uintptr

	smallIcon, bigIcon uintptr
	refresh            func()
}

// AlreadyRunning asks a running instance to show its window. It returns
// true when another instance exists (the caller should exit).
func AlreadyRunning(panelURL string) bool {
	if ok, _ := singleInstance(); ok {
		return false
	}
	c := http.Client{Timeout: 3 * time.Second}
	if resp, err := c.Post(panelURL+"/api/show", "application/json", bytes.NewReader([]byte("{}"))); err == nil {
		resp.Body.Close()
	}
	return true
}

func (s *Shell) tr(ru, en string) string {
	if s.ru {
		return ru
	}
	return en
}

func pngOf(size int, active bool) []byte {
	var b bytes.Buffer
	png.Encode(&b, icon.Render(size, active))
	return b.Bytes()
}

// Run blocks until the user quits from the tray.
func (s *Shell) Run() {
	enableDPIAwareness()
	s.ru = russianUI()
	s.smallIcon = iconFromPNG(pngOf(16, true), 16)
	s.bigIcon = iconFromPNG(pngOf(48, true), 48)

	s.Web.OnShow = s.Show
	s.Web.Autostart = autostart.Enabled
	s.Web.SetAutostart = autostart.Set
	s.Web.OnChange = func() {
		settings.Save(settings.Settings{Merge: s.Mgr.Merge(), Disabled: s.Mgr.Disabled()})
		if s.refresh != nil {
			s.refresh()
		}
	}
	systray.Run(s.onReady, func() {
		if s.Quit != nil {
			s.Quit()
		}
	})
}

func (s *Shell) onReady() {
	systray.SetIcon(icon.ICO(s.Mgr.Merge(), 16, 24, 32))
	systray.SetTooltip("Duolink")
	systray.SetOnTapped(s.Show)

	open := systray.AddMenuItem(s.tr("Открыть Duolink", "Open Duolink"), "")
	systray.AddSeparator()
	merge := systray.AddMenuItemCheckbox(s.tr("Объединять каналы", "Combine connections"), "", s.Mgr.Merge())
	tunItem := systray.AddMenuItemCheckbox(s.tr("Все программы через Duolink", "All apps through Duolink"), "", s.tunOn())
	auto := systray.AddMenuItemCheckbox(s.tr("Запускать вместе с Windows", "Start with Windows"), "", autostart.Enabled())
	folder := systray.AddMenuItem(s.tr("Папка загрузок", "Downloads folder"), "")
	systray.AddSeparator()
	quit := systray.AddMenuItem(s.tr("Выйти", "Quit"), "")

	var lastMerge = s.Mgr.Merge()
	s.refresh = func() {
		m := s.Mgr.Merge()
		if m {
			merge.Check()
		} else {
			merge.Uncheck()
		}
		if autostart.Enabled() {
			auto.Check()
		} else {
			auto.Uncheck()
		}
		if s.tunOn() {
			tunItem.Check()
		} else {
			tunItem.Uncheck()
		}
		if m != lastMerge {
			systray.SetIcon(icon.ICO(m, 16, 24, 32))
			lastMerge = m
		}
	}

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-open.ClickedCh:
				s.Show()
			case <-merge.ClickedCh:
				s.Mgr.SetMerge(!s.Mgr.Merge())
				s.Web.OnChange()
			case <-tunItem.ClickedCh:
				if s.Web.TunSet != nil {
					go func(on bool) { s.Web.TunSet(on); s.refresh() }(!s.tunOn())
				}
			case <-auto.ClickedCh:
				autostart.Set(!autostart.Enabled())
				s.refresh()
			case <-folder.ClickedCh:
				dir := s.Web.DL.Dir
				os.MkdirAll(dir, 0o755)
				exec.Command("explorer", dir).Start()
			case <-quit.ClickedCh:
				s.closeWindow()
				systray.Quit()
				return
			case <-t.C:
				systray.SetTooltip(s.tooltip())
			}
		}
	}()

	if !s.Hidden {
		s.Show()
	}
}

func (s *Shell) tunOn() bool { return s.TunOn != nil && s.TunOn() }

// Exit closes the window and the tray; Run then returns.
func (s *Shell) Exit() {
	s.closeWindow()
	systray.Quit()
}

func (s *Shell) tooltip() string {
	var rx float64
	n := 0
	for _, c := range s.Mgr.Usable() {
		r, _ := c.Rates()
		rx += r
		n++
	}
	mode := s.tr("объединение включено", "combining on")
	if !s.Mgr.Merge() {
		mode = s.tr("объединение выключено", "combining off")
	}
	return fmt.Sprintf("Duolink: %.1f %s, %d %s, %s", rx*8/1e6, s.tr("Мбит/с", "Mbit/s"), n,
		s.tr("кан.", "links"), mode)
}

// Show opens the window, or brings it to the front if it is already open.
func (s *Shell) Show() {
	s.mu.Lock()
	if s.win != nil {
		w, h := s.win, s.hwnd
		s.mu.Unlock()
		w.Dispatch(func() { bringToFront(h) })
		return
	}
	s.mu.Unlock()
	ready := make(chan struct{})
	go s.windowLoop(ready)
	<-ready
}

func (s *Shell) closeWindow() {
	s.mu.Lock()
	w := s.win
	s.mu.Unlock()
	if w != nil {
		w.Dispatch(func() { w.Destroy() })
	}
}

func (s *Shell) windowLoop(ready chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(settings.Dir(), "WebView2"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Duolink",
			Width:  1100,
			Height: 760,
			Center: true,
		},
	})
	if w == nil {
		// No WebView2 runtime: fall back to the default browser.
		close(ready)
		exec.Command("rundll32", "url.dll,FileProtocolHandler", s.PanelURL).Start()
		return
	}
	hwnd := uintptr(w.Window())
	setWindowIcons(hwnd, s.smallIcon, s.bigIcon)
	styleTitleBar(hwnd, systemDark())
	w.SetSize(860, 620, webview2.HintMin)
	w.Navigate(s.PanelURL)
	s.mu.Lock()
	s.win, s.hwnd = w, hwnd
	s.mu.Unlock()
	close(ready)
	w.Run()
	s.mu.Lock()
	s.win, s.hwnd = nil, 0
	s.mu.Unlock()
}
