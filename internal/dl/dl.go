// Package dl downloads one file over several channels at once by splitting it
// into HTTP Range pieces. Faster channels simply take more pieces.
package dl

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wandraker/duolink/internal/channel"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Duolink"

// WorkersPerChannel is how many parallel range requests each channel runs.
var WorkersPerChannel = 4

type State string

const (
	Queued     State = "queued"
	Connecting State = "connecting" // asking the server for size and range support
	Running    State = "running"
	Paused     State = "paused"
	Done       State = "done"
	Failed     State = "failed"
	Canceled   State = "canceled"
)

// Active reports whether the download is still in progress.
func (s State) Active() bool { return s == Queued || s == Connecting || s == Running }

type Download struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Ranged   bool      `json:"ranged"`
	State    State     `json:"state"`
	Err      string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`

	done    atomic.Int64
	mu      sync.Mutex
	per     map[string]*atomic.Int64
	cancel  context.CancelFunc
	pausing atomic.Bool
	gen     int // bumps on every (re)start so a stale run cannot overwrite state
}

// Progress returns bytes done and bytes per channel name.
func (d *Download) Progress() (int64, map[string]int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := make(map[string]int64, len(d.per))
	for k, v := range d.per {
		m[k] = v.Load()
	}
	return d.done.Load(), m
}

func (d *Download) counter(ch string) *atomic.Int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.per[ch]
	if !ok {
		c = &atomic.Int64{}
		d.per[ch] = c
	}
	return c
}

func (d *Download) setState(s State, err error) {
	d.mu.Lock()
	d.State = s
	if err != nil {
		d.Err = err.Error()
	}
	if s == Running || s == Connecting {
		d.Err = ""
	}
	if s == Done || s == Failed || s == Canceled {
		d.Finished = time.Now()
	}
	d.mu.Unlock()
}

// Snapshot is a copy safe to serialise.
func (d *Download) Snapshot() Download {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Download{ID: d.ID, URL: d.URL, Path: d.Path, Size: d.Size, Ranged: d.Ranged,
		State: d.State, Err: d.Err, Started: d.Started, Finished: d.Finished}
}

func (d *Download) Cancel() {
	d.mu.Lock()
	c := d.cancel
	d.mu.Unlock()
	if c != nil {
		c()
	}
}

type Manager struct {
	Mgr *channel.Manager
	Dir string
	// StateFile, if set, keeps the download list across restarts.
	StateFile string

	mu   sync.Mutex
	list []*Download
	seq  int
}

type savedDownload struct {
	ID    string           `json:"id"`
	URL   string           `json:"url"`
	Path  string           `json:"path"`
	Size  int64            `json:"size"`
	State State            `json:"state"`
	Done  int64            `json:"done"`
	Per   map[string]int64 `json:"per_channel,omitempty"`
	Start time.Time        `json:"started"`
}

// persist writes the list (newest 50) to StateFile.
func (m *Manager) persist() {
	if m.StateFile == "" {
		return
	}
	var out []savedDownload
	for _, d := range m.List() {
		s := d.Snapshot()
		done, per := d.Progress()
		st := s.State
		if st.Active() {
			st = Paused // after a restart it waits for the user
		}
		out = append(out, savedDownload{ID: s.ID, URL: s.URL, Path: s.Path, Size: s.Size, State: st,
			Done: done, Per: per, Start: s.Started})
	}
	if len(out) > 50 {
		out = out[len(out)-50:]
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	os.MkdirAll(filepath.Dir(m.StateFile), 0o755)
	tmp := m.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, m.StateFile)
	}
}

// Load restores the list saved by an earlier run. Unfinished downloads come
// back paused.
func (m *Manager) Load() {
	if m.StateFile == "" {
		return
	}
	b, err := os.ReadFile(m.StateFile)
	if err != nil {
		return
	}
	var in []savedDownload
	if json.Unmarshal(b, &in) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range in {
		d := &Download{ID: s.ID, URL: s.URL, Path: s.Path, Size: s.Size, State: s.State, Started: s.Start,
			Ranged: true, per: map[string]*atomic.Int64{}}
		if d.State.Active() {
			d.State = Paused
		}
		d.done.Store(s.Done)
		for k, v := range s.Per {
			c := &atomic.Int64{}
			c.Store(v)
			d.per[k] = c
		}
		m.list = append(m.list, d)
		if n, err := strconv.Atoi(s.ID); err == nil && n > m.seq {
			m.seq = n
		}
	}
}

// ScanPartial adds unfinished downloads found in Dir (a .part with its
// .part.duolink progress file) that are not in the list yet, as paused. This
// recovers downloads from versions that did not save the list.
func (m *Manager) ScanPartial() {
	matches, _ := filepath.Glob(filepath.Join(m.Dir, "*.part.duolink"))
	if len(matches) == 0 {
		return
	}
	known := map[string]bool{}
	for _, d := range m.List() {
		known[strings.ToLower(d.Snapshot().Path)] = true
	}
	added := false
	for _, sp := range matches {
		path := strings.TrimSuffix(sp, ".part.duolink")
		if known[strings.ToLower(path)] {
			continue
		}
		if _, err := os.Stat(path + ".part"); err != nil {
			continue
		}
		b, err := os.ReadFile(sp)
		if err != nil {
			continue
		}
		var sc sidecar
		if json.Unmarshal(b, &sc) != nil || sc.URL == "" || sc.Size <= 0 {
			continue
		}
		var left int64
		for _, r := range sc.Pending {
			left += r[1] - r[0]
		}
		fi, _ := os.Stat(sp)
		m.mu.Lock()
		m.seq++
		d := &Download{ID: strconv.Itoa(m.seq), URL: sc.URL, Path: path, Size: sc.Size, Ranged: true,
			State: Paused, Started: fi.ModTime(), per: map[string]*atomic.Int64{}}
		d.done.Store(sc.Size - left)
		m.list = append(m.list, d)
		m.mu.Unlock()
		added = true
	}
	if added {
		m.persist()
	}
}

// Pause stops a download but keeps what was fetched.
func (m *Manager) Pause(id string) error {
	d := m.Get(id)
	if d == nil {
		return errors.New("no such download")
	}
	if !d.Snapshot().State.Active() {
		return nil
	}
	d.pausing.Store(true)
	d.Cancel()
	return nil
}

// Resume continues a paused or failed download from where it stopped.
func (m *Manager) Resume(id string) error {
	d := m.Get(id)
	if d == nil {
		return errors.New("no such download")
	}
	st := d.Snapshot().State
	if st != Paused && st != Failed {
		return nil
	}
	m.start(d, "")
	return nil
}

// Remove cancels a download (if running), deletes its unfinished file and
// drops it from the list. Finished files stay on disk.
func (m *Manager) Remove(id string) error {
	d := m.Get(id)
	if d == nil {
		return errors.New("no such download")
	}
	d.Cancel()
	s := d.Snapshot()
	if s.State != Done && s.Path != "" {
		go func() { // give the workers a moment to close the file
			time.Sleep(500 * time.Millisecond)
			os.Remove(s.Path + ".part")
			os.Remove(s.Path + ".part.duolink")
		}()
	}
	m.mu.Lock()
	for i, x := range m.list {
		if x == d {
			m.list = append(m.list[:i], m.list[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	m.persist()
	return nil
}

func (m *Manager) start(d *Download, name string) {
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.gen++
	gen := d.gen
	d.State, d.Err = Queued, ""
	d.mu.Unlock()
	d.pausing.Store(false)
	go func() {
		err := m.run(ctx, d, name)
		d.mu.Lock()
		stale := d.gen != gen
		d.mu.Unlock()
		if stale {
			return
		}
		switch {
		case err == nil:
			d.setState(Done, nil)
		case d.pausing.Load():
			d.setState(Paused, nil)
		case ctx.Err() != nil:
			d.setState(Canceled, nil)
		default:
			d.setState(Failed, err)
		}
		m.persist()
	}()
}

func (m *Manager) List() []*Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Download(nil), m.list...)
}

func (m *Manager) Get(id string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// Add starts a download in the background. name may be empty.
func (m *Manager) Add(rawURL, name string) (*Download, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("need an http(s) URL")
	}
	m.mu.Lock()
	m.seq++
	d := &Download{ID: strconv.Itoa(m.seq), URL: u.String(), State: Queued, Started: time.Now(),
		per: map[string]*atomic.Int64{}}
	m.list = append(m.list, d)
	m.mu.Unlock()
	m.start(d, name)
	m.persist()
	return d, nil
}

func client(ch *channel.Channel) *http.Client {
	tr := &http.Transport{
		DialContext:           ch.DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1: one conn per worker
		MaxIdleConnsPerHost:   WorkersPerChannel,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	return &http.Client{Transport: tr}
}

func newReq(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

type probeResult struct {
	finalURL string
	size     int64
	ranged   bool
	name     string
}

func probe(ctx context.Context, ch *channel.Channel, u string) (probeResult, error) {
	req, err := newReq(ctx, u)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client(ch).Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	r := probeResult{finalURL: resp.Request.URL.String(), size: -1}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Content-Range: bytes 0-0/12345
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				r.size, r.ranged = n, true
			}
		}
	case http.StatusOK:
		r.size = resp.ContentLength
	default:
		return r, fmt.Errorf("server answered %s", resp.Status)
	}
	if _, p, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		r.name = p["filename"]
	}
	if r.name == "" {
		r.name = path.Base(resp.Request.URL.Path)
	}
	return r, nil
}

func safeName(n string) string {
	n = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, n)
	n = strings.Trim(n, ". ")
	if n == "" || n == "/" {
		n = "download"
	}
	return n
}

func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if _, err := os.Stat(p + ".part"); os.IsNotExist(err) {
				return p
			}
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

// sidecar is saved next to the .part file so a download can resume.
type sidecar struct {
	URL     string     `json:"url"`
	Size    int64      `json:"size"`
	Pending [][2]int64 `json:"pending"` // [start, end) ranges still missing
}

func (m *Manager) run(ctx context.Context, d *Download, name string) error {
	d.setState(Connecting, nil)
	best := m.Mgr.Best()
	if best == nil {
		return errors.New("no working internet channel")
	}
	// The first request may fail on one channel; try the others before giving up.
	var pr probeResult
	var err error
	tried := []*channel.Channel{best}
	for _, c := range m.Mgr.Usable() {
		if c != best {
			tried = append(tried, c)
		}
	}
	for _, c := range tried {
		if pr, err = probe(ctx, c, d.URL); err == nil {
			best = c
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	if name == "" {
		name = pr.name
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	// Resume: reuse an existing .part with a matching sidecar. A paused
	// download already knows its file; a new one tries the plain name.
	d.mu.Lock()
	known := d.Path
	d.mu.Unlock()
	target := known
	if target == "" {
		target = filepath.Join(m.Dir, safeName(name))
	}
	var sc sidecar
	if b, err := os.ReadFile(target + ".part.duolink"); err == nil && json.Unmarshal(b, &sc) == nil &&
		sc.URL == d.URL && sc.Size == pr.size && pr.ranged {
		// resuming
	} else {
		if known == "" {
			target = uniquePath(m.Dir, safeName(name))
		}
		sc = sidecar{}
	}
	d.mu.Lock()
	d.Path, d.Size, d.Ranged = target, pr.size, pr.ranged
	d.mu.Unlock()
	d.done.Store(0)
	d.setState(Running, nil)
	m.persist()

	if !pr.ranged || pr.size <= 0 || len(m.Mgr.Usable()) == 0 {
		return single(ctx, d, best, pr.finalURL, target)
	}
	return m.ranged(ctx, d, pr, target, sc)
}

func single(ctx context.Context, d *Download, ch *channel.Channel, u, target string) error {
	req, err := newReq(ctx, u)
	if err != nil {
		return err
	}
	resp, err := client(ch).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	f, err := os.Create(target + ".part")
	if err != nil {
		return err
	}
	cnt := d.counter(ch.Name)
	_, err = io.Copy(f, &progressReader{r: resp.Body, d: d, c: cnt})
	f.Close()
	if err != nil {
		return err
	}
	return os.Rename(target+".part", target)
}

type progressReader struct {
	r io.Reader
	d *Download
	c *atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.d.done.Add(int64(n))
	p.c.Add(int64(n))
	return n, err
}
