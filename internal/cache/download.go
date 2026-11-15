package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// errStalled is returned when no bytes arrived within StallTimeout.
	errStalled = errors.New("download stalled")
	// errShort is returned when the body ended before Content-Length bytes.
	errShort = errors.New("download ended early")
	// errYield is returned when a prefetch pauses for the current track.
	errYield = errors.New("download yielded to the current track")
)

// statusCoder is implemented by HTTP errors from the Source (e.g.
// *subsonic.HTTPError).
type statusCoder interface{ HTTPStatus() int }

// permanent is implemented by errors that retrying won't fix.
type permanent interface{ Permanent() bool }

// transient reports whether a failed attempt is worth retrying: timeouts,
// stalls, connection resets, short reads and 5xx/408/429 responses are;
// 4xx responses (401/403/404...) and server API errors are not.
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var sc statusCoder
	if errors.As(err, &sc) {
		c := sc.HTTPStatus()
		return c >= 500 || c == http.StatusRequestTimeout || c == http.StatusTooManyRequests
	}
	var p permanent
	if errors.As(err, &p) && p.Permanent() {
		return false
	}
	return true
}

// backoff returns the delay before retry attempt n (1-based): exponential
// with ±50% jitter, capped.
func (m *Manager) backoff(n int) time.Duration {
	d := m.BackoffBase << (n - 1)
	if d > m.BackoffMax || d <= 0 {
		d = m.BackoffMax
	}
	return d/2 + rand.N(d)
}

// acquire blocks until e may download (see mayRunLocked) and takes a slot.
func (m *Manager) acquire(ctx context.Context, e *Entry) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if m.closed || ctx.Err() != nil {
			return false
		}
		if m.mayRunLocked(e) {
			m.active++
			if e.prio == Low {
				e.lowSlot = true
				m.lowRun++
			}
			return true
		}
		waitCondTimeout(ctx, m.cond, 500*time.Millisecond)
	}
}

func (m *Manager) release(e *Entry) {
	m.mu.Lock()
	m.active--
	if e.lowSlot {
		e.lowSlot = false
		m.lowRun--
	}
	m.cond.Broadcast()
	m.mu.Unlock()
}

// mayRunLocked: the current (high priority) track always downloads; a
// prefetch only runs alone, within the concurrency limit, and while the
// current track is healthy.
func (m *Manager) mayRunLocked(e *Entry) bool {
	if e.prio == High {
		return true
	}
	return m.lowRun == 0 && m.active < maxDownloads && m.currentHealthyLocked(healthySeconds)
}

// shouldYield reports whether a running prefetch must pause because it was
// demoted below the current track, or the current track's buffer dropped.
func (m *Manager) shouldYield(e *Entry) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.prio == High {
		return false
	}
	return !m.currentHealthyLocked(unhealthySeconds)
}

// currentHealthyLocked reports whether the current track is downloaded,
// or buffered at least secs ahead of its read position on a link that keeps
// up with its bitrate.
func (m *Manager) currentHealthyLocked(secs float64) bool {
	cur := m.entries[m.current]
	if cur == nil {
		return true
	}
	cur.mu.Lock()
	defer cur.mu.Unlock()
	if cur.done || (cur.total >= 0 && cur.written >= cur.total) {
		return true
	}
	bps := cur.bpsLocked()
	if rate := m.meter.rate(); rate > 0 && rate < bps {
		return false
	}
	return float64(cur.written-cur.readPos) >= secs*bps
}

// waitCondTimeout waits on c (whose lock is held) until signalled, ctx is
// done or d elapsed (health depends on readers and throughput, which don't
// signal the manager).
func waitCondTimeout(ctx context.Context, c *sync.Cond, d time.Duration) {
	var fired atomic.Bool
	wake := func() {
		if fired.CompareAndSwap(false, true) {
			c.L.Lock()
			c.Broadcast()
			c.L.Unlock()
		}
	}
	stop := context.AfterFunc(ctx, wake)
	t := time.AfterFunc(d, wake)
	c.Wait()
	fired.Store(true)
	stop()
	t.Stop()
}

func (e *Entry) finish(err error) {
	e.mu.Lock()
	e.err = err
	e.done = true
	e.cond.Broadcast()
	e.mu.Unlock()
	close(e.doneCh)
	e.m.mu.Lock()
	e.m.cond.Broadcast()
	e.m.mu.Unlock()
}

// complete renames the .part file and marks the entry as cached.
func (e *Entry) complete() {
	m := e.m
	final := m.finalPath(e.key)
	if err := os.Rename(e.path, final); err != nil {
		e.finish(err)
		return
	}
	e.mu.Lock()
	e.path = final
	e.done = true
	e.total = e.written
	e.cond.Broadcast()
	e.mu.Unlock()
	close(e.doneCh)
	m.mu.Lock()
	m.cond.Broadcast()
	m.mu.Unlock()
	slog.Debug("cache: download complete", "key", e.key, "bytes", e.written)
	go m.evict()
}

// Written returns the number of bytes downloaded so far.
func (e *Entry) Written() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.written
}

// download runs attempts until the file is complete, a permanent error
// occurs, MaxAttempts consecutive attempts failed, or ctx is cancelled.
// prev is the exit channel of an earlier goroutine writing the same file.
func (e *Entry) download(ctx context.Context, prev chan struct{}) {
	m := e.m
	defer func() {
		m.mu.Lock()
		if m.writers[e.key] == e.exited {
			delete(m.writers, e.key)
		}
		m.mu.Unlock()
		close(e.exited)
	}()
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			e.finish(ctx.Err())
			return
		}
	}
	attempts := 0
	lastShort := int64(-1)
	for {
		if !m.acquire(ctx, e) {
			err := ctx.Err()
			if err == nil {
				err = errors.New("cache closed")
			}
			e.finish(err)
			return
		}
		m.mu.Lock()
		src := m.src
		m.mu.Unlock()
		if src == nil {
			m.release(e)
			e.finish(errors.New("not connected to a server"))
			return
		}
		before := e.Written()
		slog.Debug("cache: downloading", "key", e.key, "offset", before, "attempt", attempts+1)
		err := e.fetch(ctx, src)
		m.release(e)
		if err == nil {
			e.complete()
			return
		}
		if ctx.Err() != nil {
			e.finish(ctx.Err())
			return
		}
		if errors.Is(err, errYield) {
			slog.Debug("cache: prefetch paused for the current track", "key", e.key)
			continue // acquire waits until the current track is healthy
		}
		if !transient(err) {
			slog.Debug("cache: download failed permanently", "key", e.key, "err", err)
			e.finish(err)
			return
		}
		w := e.Written()
		if errors.Is(err, errShort) {
			// A server that consistently ends at the same size (e.g. an
			// over-estimated Content-Length for a transcode) is done.
			if w == lastShort {
				slog.Debug("cache: accepting consistently short download", "key", e.key, "bytes", w)
				e.complete()
				return
			}
			lastShort = w
		}
		if w-before >= MinStartBytes {
			attempts = 0 // made progress; only count consecutive failures
		}
		attempts++
		if attempts >= m.MaxAttempts {
			slog.Debug("cache: giving up", "key", e.key, "err", err)
			e.finish(err)
			return
		}
		d := m.backoff(attempts)
		slog.Debug("cache: retrying", "key", e.key, "err", err, "in", d)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			e.finish(ctx.Err())
			return
		}
	}
}

// contentRange parses "bytes start-end/total".
func contentRange(h string) (start, total int64, ok bool) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes ") {
		return 0, -1, false
	}
	h = h[len("bytes "):]
	rng, tot, found := strings.Cut(h, "/")
	if !found {
		return 0, -1, false
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, -1, false
	}
	start, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, -1, false
	}
	total = -1
	if tot != "*" {
		if t, err := strconv.ParseInt(tot, 10, 64); err == nil {
			total = t
		}
	}
	return start, total, true
}

// fetch runs one download attempt, resuming the .part file with a Range
// request. It aborts with errStalled when no bytes arrive for
// StallTimeout, verifies the size against Content-Length, and restarts
// from zero when the server ignores the Range.
func (e *Entry) fetch(ctx context.Context, src Source) error {
	m := e.m
	f, err := os.OpenFile(e.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	offset := fi.Size()
	format := e.req.Format
	if format == "" {
		format = "raw" // original file, no server-side transcoding
	}

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	stall := m.StallTimeout
	timer := time.AfterFunc(stall, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	wrap := func(err error) error {
		if stalled.Load() && ctx.Err() == nil {
			return fmt.Errorf("%w (no data for %v)", errStalled, stall)
		}
		return err
	}

	resp, err := src.Stream(actx, e.req.ID, format, e.req.BitRate, offset)
	if err != nil {
		var sc statusCoder
		if offset > 0 && errors.As(err, &sc) && sc.HTTPStatus() == http.StatusRequestedRangeNotSatisfiable {
			// The partial file is unusable for resuming; start over.
			f.Truncate(0)
			e.mu.Lock()
			e.written = 0
			e.mu.Unlock()
		}
		return wrap(err)
	}
	defer resp.Body.Close()
	timer.Reset(stall)

	total := int64(-1)
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		start, t, ok := contentRange(resp.Header.Get("Content-Range"))
		if ok && start != offset {
			f.Truncate(0) // unexpected range; resume can't be trusted
			e.mu.Lock()
			e.written = 0
			e.mu.Unlock()
			return fmt.Errorf("server returned range from %d, asked %d", start, offset)
		}
		if ok && t >= 0 {
			total = t
		}
	case offset > 0:
		slog.Debug("cache: server ignored Range, restarting", "key", e.key, "status", resp.StatusCode)
		offset = 0
	}
	if total < 0 && resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	e.mu.Lock()
	e.written = offset
	e.total = total
	e.cond.Broadcast()
	e.mu.Unlock()

	buf := make([]byte, 64<<10)
	lastSignal := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			timer.Reset(stall)
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			m.meter.add(n, time.Now())
			e.mu.Lock()
			e.written += int64(n)
			over := e.total >= 0 && e.written > e.total
			e.cond.Broadcast()
			e.mu.Unlock()
			if over {
				f.Truncate(0)
				e.mu.Lock()
				e.written = 0
				e.mu.Unlock()
				return errors.New("server sent more data than announced")
			}
			if time.Since(lastSignal) > 250*time.Millisecond {
				// Let waiting prefetches re-evaluate the current track's
				// health.
				lastSignal = time.Now()
				m.mu.Lock()
				m.cond.Broadcast()
				m.mu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return wrap(rerr)
		}
		if m.shouldYield(e) {
			return errYield
		}
	}
	e.mu.Lock()
	short := e.total >= 0 && e.written < e.total
	got, want := e.written, e.total
	e.mu.Unlock()
	if short {
		return fmt.Errorf("%w: got %d of %d bytes", errShort, got, want)
	}
	return f.Sync()
}

// meter is a moving average of the download throughput, sampled over
// windows of at least 500 ms while data is flowing.
type meter struct {
	mu    sync.Mutex
	start time.Time
	last  time.Time
	bytes int64
	ewma  float64
}

func (t *meter) add(n int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.start.IsZero() || now.Sub(t.last) > 2*time.Second {
		// New burst after an idle period: idle time isn't throughput.
		t.start, t.bytes = now, 0
	}
	t.last = now
	t.bytes += int64(n)
	if el := now.Sub(t.start); el >= 500*time.Millisecond {
		r := float64(t.bytes) / el.Seconds()
		if t.ewma == 0 {
			t.ewma = r
		} else {
			t.ewma = 0.3*r + 0.7*t.ewma
		}
		t.start, t.bytes = now, 0
	}
}

func (t *meter) rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ewma
}
