package dl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wandraker/duolink/internal/channel"
)

// minSplit is the smallest remainder worth handing to another worker.
const minSplit = 1 << 20

// seg is a byte range [pos, end) being fetched by one worker. Only the owner
// advances pos; another worker may shrink end to steal the tail.
type seg struct {
	pos, end atomic.Int64
	owned    bool // guarded by job.mu
}

func (s *seg) remaining() int64 {
	if r := s.end.Load() - s.pos.Load(); r > 0 {
		return r
	}
	return 0
}

type job struct {
	mu   sync.Mutex
	segs []*seg
}

func (j *job) add(start, end int64, owned bool) *seg {
	s := &seg{owned: owned}
	s.pos.Store(start)
	s.end.Store(end)
	j.segs = append(j.segs, s)
	return s
}

// splitTo pre-splits free ranges so every worker can start at once.
func (j *job) splitTo(n int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for len(j.segs) < n {
		var big *seg
		for _, s := range j.segs {
			if big == nil || s.remaining() > big.remaining() {
				big = s
			}
		}
		if big == nil || big.remaining() < 2*minSplit {
			return
		}
		mid := big.pos.Load() + big.remaining()/2
		end := big.end.Load()
		big.end.Store(mid)
		j.add(mid, end, false)
	}
}

// take hands out a free range, or steals the second half of the biggest
// range another worker is still fetching. nil means nothing is left to do.
func (j *job) take() *seg {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.segs {
		if !s.owned && s.remaining() > 0 {
			s.owned = true
			return s
		}
	}
	var big *seg
	for _, s := range j.segs {
		if s.owned && (big == nil || s.remaining() > big.remaining()) {
			big = s
		}
	}
	if big == nil || big.remaining() < 2*minSplit {
		return nil
	}
	end := big.end.Load()
	mid := big.pos.Load() + big.remaining()/2
	big.end.Store(mid)
	return j.add(mid, end, true)
}

func (j *job) release(s *seg) {
	j.mu.Lock()
	s.owned = false
	j.mu.Unlock()
}

func (j *job) left() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	var n int64
	for _, s := range j.segs {
		n += s.remaining()
	}
	return n
}

func (j *job) pending() [][2]int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	var p [][2]int64
	for _, s := range j.segs {
		if s.remaining() > 0 {
			p = append(p, [2]int64{s.pos.Load(), s.end.Load()})
		}
	}
	return p
}

func (m *Manager) ranged(ctx context.Context, d *Download, pr probeResult, target string, sc sidecar) error {
	if sc.URL == "" {
		sc = sidecar{URL: d.URL, Size: pr.size, Pending: [][2]int64{{0, pr.size}}}
	}
	j := &job{}
	var left int64
	for _, r := range sc.Pending {
		if r[0] < r[1] && r[1] <= pr.size {
			j.add(r[0], r[1], false)
			left += r[1] - r[0]
		}
	}
	d.done.Store(pr.size - left)

	f, err := os.OpenFile(target+".part", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(pr.size); err != nil {
		return err
	}
	finish := func() error {
		f.Sync()
		f.Close()
		os.Remove(target + ".part.duolink")
		return os.Rename(target+".part", target)
	}
	if left == 0 {
		return finish()
	}
	save := func() {
		b, _ := json.Marshal(sidecar{URL: sc.URL, Size: sc.Size, Pending: j.pending()})
		os.WriteFile(target+".part.duolink", b, 0o644)
	}

	wctx, stop := context.WithCancel(ctx)
	defer stop()

	var (
		live    atomic.Int32
		lastErr atomic.Pointer[error] // atomic.Value would panic on differing error types
		wake    = make(chan struct{}, 1)
		limMu   sync.Mutex
		limits  = map[string]*atomic.Int32{}
	)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	worker := func(ch *channel.Channel, cl *http.Client, limit *atomic.Int32, slot int32) {
		defer func() {
			if r := recover(); r != nil { // one broken worker must not kill the app
				e := fmt.Errorf("internal error: %v", r)
				lastErr.Store(&e)
			}
			live.Add(-1)
			notify()
		}()
		cnt := d.counter(ch.Name)
		fails := 0
		for wctx.Err() == nil {
			if slot >= limit.Load() {
				return // shed after the server throttled us
			}
			if !m.Mgr.InUse(ch) {
				select {
				case <-wctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			s := j.take()
			if s == nil {
				return
			}
			t0, p0 := time.Now(), s.pos.Load()
			err := fetchSeg(wctx, cl, pr.finalURL, f, s, d, cnt)
			j.release(s)
			debugf("%s/%d: %.1f MB in %v err=%v", ch.Name, slot, float64(s.pos.Load()-p0)/1e6,
				time.Since(t0).Round(time.Millisecond), err)
			notify()
			if err == nil {
				fails = 0
				continue
			}
			if wctx.Err() != nil {
				return
			}
			var th *throttled
			if errors.As(err, &th) {
				for {
					cur := limit.Load()
					if cur <= 1 || limit.CompareAndSwap(cur, cur-1) {
						break
					}
				}
				select {
				case <-wctx.Done():
					return
				case <-time.After(th.wait):
				}
				continue
			}
			lastErr.Store(&err)
			if fails++; fails > 8 {
				return
			}
			select {
			case <-wctx.Done():
				return
			case <-time.After(time.Duration(fails) * time.Second):
			}
		}
	}

	// spawn starts workers on channels that do not have them yet, so a phone
	// plugged in mid-download joins in.
	spawn := func() {
		for _, ch := range m.Mgr.Usable() {
			limMu.Lock()
			key := fmt.Sprintf("%p", ch) // a re-detected channel is a new object
			_, ok := limits[key]
			if !ok {
				l := &atomic.Int32{}
				l.Store(int32(WorkersPerChannel))
				limits[key] = l
				cl := client(ch)
				for w := 0; w < WorkersPerChannel; w++ {
					live.Add(1)
					go worker(ch, cl, l, int32(w))
				}
			}
			limMu.Unlock()
		}
	}
	j.splitTo(len(m.Mgr.Usable()) * WorkersPerChannel)
	spawn()

	saveTick := time.NewTicker(2 * time.Second)
	defer saveTick.Stop()
	spawnTick := time.NewTicker(5 * time.Second)
	defer spawnTick.Stop()
	respawns := 0
	for {
		if j.left() == 0 {
			stop()
			return finish()
		}
		if live.Load() == 0 {
			// Workers can all exit while a released range is left over (e.g. the
			// last one was shed after throttling). Restart them a few times.
			if lastErr.Load() == nil && ctx.Err() == nil && respawns < 5 {
				respawns++
				limMu.Lock()
				limits = map[string]*atomic.Int32{}
				limMu.Unlock()
				spawn()
				if live.Load() > 0 {
					continue
				}
			}
			save()
			if e := lastErr.Load(); e != nil {
				return *e
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("all channels stopped")
		}
		select {
		case <-wctx.Done():
			save()
			return ctx.Err()
		case <-wake:
		case <-saveTick.C:
			save()
		case <-spawnTick.C:
			spawn()
		}
	}
}

// fetchSeg downloads s from its current position until it reaches s.end,
// which may shrink while running when another worker steals the tail.
func fetchSeg(ctx context.Context, cl *http.Client, u string, f *os.File, s *seg, d *Download, cnt *atomic.Int64) error {
	start, end := s.pos.Load(), s.end.Load()
	if start >= end {
		return nil
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := newReq(pctx, u)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		wait := 3 * time.Second
		if sec, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && sec > 0 && sec < 120 {
			wait = time.Duration(sec) * time.Second
		}
		return &throttled{status: resp.Status, wait: wait}
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range request answered %s", resp.Status)
	}

	// Stall guard: give up on this connection if nothing arrives for 20 s.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > 20*time.Second {
					cancel()
					return
				}
			}
		}
	}()

	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			last.Store(time.Now().UnixNano())
			p, e := s.pos.Load(), s.end.Load()
			w := int64(n)
			if w > e-p {
				w = e - p
			}
			if w > 0 {
				if _, err := f.WriteAt(buf[:w], p); err != nil {
					return err
				}
				s.pos.Add(w)
				d.done.Add(w)
				cnt.Add(w)
			}
			if s.pos.Load() >= s.end.Load() {
				return nil // reached our (possibly shrunk) end
			}
		}
		if rerr != nil {
			if s.pos.Load() >= s.end.Load() {
				return nil
			}
			if rerr == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return rerr
		}
	}
}

var debug = os.Getenv("DUOLINK_DEBUG") != ""

func debugf(f string, a ...any) {
	if debug {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+f+"\n", a...)
	}
}

type throttled struct {
	status string
	wait   time.Duration
}

func (t *throttled) Error() string { return "server is limiting connections: " + t.status }
