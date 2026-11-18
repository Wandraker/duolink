// Duolink combines several internet connections on one PC.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Wandraker/duolink/internal/channel"
	"github.com/Wandraker/duolink/internal/dl"
	"github.com/Wandraker/duolink/internal/socks"
	"github.com/Wandraker/duolink/internal/web"
)

var version = "0.1.0-dev"

func defaultDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "Downloads", "Duolink")
	}
	return "downloads"
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "get" {
		cmdGet(os.Args[2:])
		return
	}
	uiAddr := flag.String("ui", "127.0.0.1:7878", "control panel address")
	socksAddr := flag.String("socks", "127.0.0.1:1080", "SOCKS5 proxy address")
	dir := flag.String("dir", defaultDir(), "download folder")
	workers := flag.Int("workers", dl.WorkersPerChannel, "parallel range requests per channel")
	noBrowser := flag.Bool("no-browser", false, "do not open the control panel")
	flag.Parse()
	dl.WorkersPerChannel = *workers

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logger := log.New(os.Stderr, "", log.LstdFlags)

	mgr := channel.NewManager()
	mgr.Start(ctx)
	for _, c := range mgr.All() {
		state := "down"
		if c.Up() {
			state = fmt.Sprintf("up, %d ms", c.RTT().Milliseconds())
		}
		logger.Printf("channel %-22s v4=%v v6=%v  %s", c.Name, c.V4, c.V6, state)
	}

	dlm := &dl.Manager{Mgr: mgr, Dir: *dir}
	ws := &web.Server{Mgr: mgr, DL: dlm, SocksAddr: *socksAddr, Version: version}
	ss := &socks.Server{Mgr: mgr}

	errc := make(chan error, 2)
	go func() { errc <- ss.ListenAndServe(ctx, *socksAddr) }()
	go func() { errc <- ws.ListenAndServe(ctx, *uiAddr) }()
	panel := "http://" + *uiAddr
	logger.Printf("Duolink %s  panel %s  socks5 %s", version, panel, *socksAddr)
	if !*noBrowser {
		openBrowser(panel)
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			logger.Fatal(err)
		}
	}
}

// cmdGet: duolink get [-o dir] URL — download from the command line.
func cmdGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	dir := fs.String("o", ".", "output folder")
	workers := fs.Int("workers", dl.WorkersPerChannel, "parallel range requests per channel")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: duolink get [-o dir] [-workers n] URL")
		os.Exit(2)
	}
	dl.WorkersPerChannel = *workers
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mgr := channel.NewManager()
	mgr.Start(ctx)
	for _, c := range mgr.Healthy() {
		fmt.Printf("channel %s (%d ms)\n", c.Name, c.RTT().Milliseconds())
	}
	m := &dl.Manager{Mgr: mgr, Dir: *dir}
	d, err := m.Add(fs.Arg(0), "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go func() { <-ctx.Done(); d.Cancel() }()
	start := time.Now()
	var last int64
	for {
		time.Sleep(time.Second)
		s := d.Snapshot()
		done, per := d.Progress()
		line := fmt.Sprintf("\r%6.1f Mbit/s  %8.1f MB", float64(done-last)*8/1e6, float64(done)/1e6)
		if s.Size > 0 {
			line += fmt.Sprintf(" / %.1f MB", float64(s.Size)/1e6)
		}
		for n, v := range per {
			line += fmt.Sprintf("  %s %.0f MB", n, float64(v)/1e6)
		}
		fmt.Print(line + "   ")
		last = done
		if s.State == dl.Done || s.State == dl.Failed || s.State == dl.Canceled {
			el := time.Since(start).Seconds()
			fmt.Printf("\n%s in %.1fs, avg %.1f Mbit/s  %s\n", s.State, el, float64(done)*8/1e6/el, s.Path)
			if s.Err != "" {
				fmt.Println(s.Err)
				os.Exit(1)
			}
			return
		}
	}
}
