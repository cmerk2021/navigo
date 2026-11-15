// Package cache implements the on-disk track cache with resumable downloads,
// LRU eviction, low-priority prefetching, and a cover-art cache.
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source fetches media from the server.
type Source interface {
	Stream(ctx context.Context, id, format string, maxBitRate int, offset int64) (*http.Response, error)
}

// Request identifies a cached rendition of a track.
type Request struct {
	ID      string
	Format  string // "" = original file, otherwise transcode target (e.g. "mp3")
	BitRate int    // max bitrate for transcodes, 0 for original
}

// Key returns the file name stem used on disk.
func (r Request) Key() string {
	f := r.Format
	if f == "" {
		f = "raw"
	}
	return fmt.Sprintf("%s_%s_%d", sanitize(r.ID), sanitize(f), r.BitRate)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
			b.WriteRune(c)
		default:
			fmt.Fprintf(&b, "_%x", c)
		}
	}
	return b.String()
}

// Priority of a download.
type Priority int

const (
	// Low priority downloads (prefetch) run one at a time and only while the
	// current track's download is healthy (or finished).
	Low Priority = iota
	// High priority downloads (the current track) start immediately.
	High
)

// ErrInterrupted is returned by Reader.Read after Interrupt.
var ErrInterrupted = errors.New("read interrupted")

// Buffering policy defaults.
const (
	// StartSeconds of audio must be downloaded before a track starts.
	StartSeconds = 3.0
	// ResumeSeconds of audio must be available again after an underrun
	// before reads continue (avoids rapid stutter on slow links).
	ResumeSeconds = 6.0
	// MinStartBytes is the minimum start/resume buffer.
	MinStartBytes = 256 << 10
	// maxPrebufferSeconds caps the extra pre-buffer on slow links.
	maxPrebufferSeconds = 60.0
	// healthySeconds of audio buffered ahead of the current track's play
	// position allow a prefetch to run concurrently; below
	// unhealthySeconds a running prefetch yields.
	healthySeconds   = 30.0
	unhealthySeconds = 15.0
	// maxDownloads is the number of concurrent downloads (current + one
	// prefetch).
	maxDownloads = 2
	// defaultBytesPerSec is assumed when a track's bitrate is unknown.
	defaultBytesPerSec = 100_000
)

// Manager owns the cache directory.
type Manager struct {
	dir string

	// StallTimeout aborts (and retries) a download when no bytes arrive for
	// this long. MaxAttempts bounds consecutive failed attempts; BackoffBase
	// and BackoffMax shape the exponential backoff between them. Set them
	// before the first Get.
	StallTimeout time.Duration
	MaxAttempts  int
	BackoffBase  time.Duration
	BackoffMax   time.Duration

	meter meter

	mu       sync.Mutex
	cond     *sync.Cond
	src      Source
	maxBytes int64
	entries  map[string]*Entry
	pinned   map[string]bool
	current  string                   // key of the current track
	writers  map[string]chan struct{} // exit channel of the goroutine writing each key
	active   int                      // running fetches
	lowRun   int                      // running low-priority fetches
	prefetch []Request
	pfCancel context.CancelFunc
	closed   bool
}

// New creates a manager storing tracks in dir.
func New(dir string, maxBytes int64) *Manager {
	os.MkdirAll(dir, 0o700)
	m := &Manager{
		dir:          dir,
		maxBytes:     maxBytes,
		entries:      map[string]*Entry{},
		pinned:       map[string]bool{},
		writers:      map[string]chan struct{}{},
		StallTimeout: 10 * time.Second,
		MaxAttempts:  5,
		BackoffBase:  500 * time.Millisecond,
		BackoffMax:   8 * time.Second,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Dir returns the cache directory.
func (m *Manager) Dir() string { return m.dir }

// SetSource sets the server source used for downloads.
func (m *Manager) SetSource(src Source) {
	m.mu.Lock()
	m.src = src
	m.mu.Unlock()
}

// SetMaxBytes changes the size cap and evicts if needed.
func (m *Manager) SetMaxBytes(n int64) {
	m.mu.Lock()
	m.maxBytes = n
	m.mu.Unlock()
	go m.evict()
}

// Pin marks keys as non-evictable (current and prefetched tracks); it
// replaces the previous pin set.
func (m *Manager) Pin(reqs ...Request) {
	m.mu.Lock()
	m.pinned = map[string]bool{}
	for _, r := range reqs {
		m.pinned[r.Key()] = true
	}
	m.mu.Unlock()
}

// Throughput returns the moving-average download rate in bytes/s (0 when
// unknown).
func (m *Manager) Throughput() float64 { return m.meter.rate() }

// SetCurrent marks r as the track being played. Its download always has
// priority: other high-priority downloads are demoted to prefetch priority
// (and yield while the current download is unhealthy).
func (m *Manager) SetCurrent(r Request) {
	key := r.Key()
	m.mu.Lock()
	m.current = key
	for k, e := range m.entries {
		if k == key {
			e.prio = High
		} else if e.prio == High {
			e.prio = Low
		}
	}
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *Manager) finalPath(key string) string { return filepath.Join(m.dir, key) }
func (m *Manager) partPath(key string) string  { return filepath.Join(m.dir, key+".part") }

// Cached reports whether the request is fully downloaded.
func (m *Manager) Cached(r Request) bool {
	_, err := os.Stat(m.finalPath(r.Key()))
	return err == nil
}

// Get returns the entry for r, starting a download if necessary. A Low
// request never downgrades an existing entry; a High request upgrades it.
func (m *Manager) Get(r Request, prio Priority) *Entry {
	key := r.Key()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		if _, _, done, err := e.Progress(); done && err != nil {
			delete(m.entries, key) // retry failed downloads
		} else {
			if prio == High && e.prio == Low {
				e.prio = High
				m.cond.Broadcast()
			}
			return e
		}
	}
	e := &Entry{m: m, req: r, key: key, total: -1, prio: prio, cancel: func() {}, doneCh: make(chan struct{}), exited: make(chan struct{})}
	e.cond = sync.NewCond(&e.mu)
	if fi, err := os.Stat(m.finalPath(key)); err == nil {
		e.path = m.finalPath(key)
		e.written = fi.Size()
		e.total = fi.Size()
		e.done = true
		e.fromDisk = true
		close(e.doneCh)
		close(e.exited)
		now := time.Now()
		os.Chtimes(e.path, now, now)
		m.entries[key] = e
		return e
	}
	e.path = m.partPath(key)
	prev := m.writers[key]
	m.writers[key] = e.exited
	if prev == nil {
		// Nobody is writing the .part file: what's on disk can be read
		// right away (and is resumed by the download).
		if fi, err := os.Stat(e.path); err == nil {
			e.written = fi.Size()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	m.entries[key] = e
	slog.Debug("cache: download requested", "key", key, "prio", prio)
	go e.download(ctx, prev)
	return e
}

// Prefetch replaces the prefetch list. Downloads run sequentially at low
// priority, the next track first; in-flight prefetches that are no longer
// wanted (and not being read) are cancelled. On a link slower than the
// current track's bitrate only the first request is prefetched.
func (m *Manager) Prefetch(reqs []Request) {
	m.mu.Lock()
	want := map[string]bool{}
	for _, r := range reqs {
		want[r.Key()] = true
	}
	for k, e := range m.entries {
		if k != m.current && e.prio == Low && !want[k] && !e.isDone() && !e.hasReaders() {
			slog.Debug("cache: prefetch cancelled", "key", k)
			e.cancel()
			delete(m.entries, k)
		}
	}
	if m.pfCancel != nil {
		m.pfCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.pfCancel = cancel
	m.prefetch = append([]Request(nil), reqs...)
	m.mu.Unlock()
	go m.runPrefetch(ctx, reqs)
}

func (m *Manager) runPrefetch(ctx context.Context, reqs []Request) {
	for i, r := range reqs {
		if ctx.Err() != nil {
			return
		}
		if i > 0 && m.slowLink() {
			slog.Debug("cache: slow link, only prefetching the next track", "rate", m.Throughput())
			return
		}
		if m.Cached(r) {
			continue
		}
		e := m.Get(r, Low)
		select {
		case <-e.Done():
		case <-ctx.Done():
			return
		}
	}
}

// slowLink reports whether the measured throughput is below the current
// track's bitrate.
func (m *Manager) slowLink() bool {
	rate := m.Throughput()
	if rate <= 0 {
		return false
	}
	m.mu.Lock()
	cur := m.entries[m.current]
	m.mu.Unlock()
	bps := float64(defaultBytesPerSec)
	if cur != nil {
		cur.mu.Lock()
		bps = cur.bpsLocked()
		cur.mu.Unlock()
	}
	return rate < bps
}

// Forget drops a finished or failed entry from the in-memory table so that
// a later Get retries (used after errors).
func (m *Manager) Forget(r Request) {
	m.mu.Lock()
	if e, ok := m.entries[r.Key()]; ok && e.isDone() {
		delete(m.entries, r.Key())
	}
	m.mu.Unlock()
}

// Invalidate cancels and deletes a cached track (e.g. after the decoder
// found it corrupt) so that the next Get downloads it from scratch.
func (m *Manager) Invalidate(r Request) {
	key := r.Key()
	m.mu.Lock()
	e := m.entries[key]
	delete(m.entries, key)
	m.mu.Unlock()
	if e != nil {
		e.cancel()
		<-e.exited
	}
	os.Remove(m.finalPath(key))
	os.Remove(m.partPath(key))
	slog.Debug("cache: invalidated", "key", key)
}

// Close cancels all downloads.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, e := range m.entries {
		e.cancel()
	}
	if m.pfCancel != nil {
		m.pfCancel()
	}
	m.cond.Broadcast()
	m.mu.Unlock()
}

// Size returns the total bytes of complete cached tracks.
func (m *Manager) Size() int64 {
	var n int64
	for _, f := range m.files() {
		n += f.size
	}
	return n
}

type fileInfo struct {
	key   string
	path  string
	size  int64
	atime time.Time
}

func (m *Manager) files() []fileInfo {
	des, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var out []fileInfo
	for _, de := range des {
		if de.IsDir() || strings.HasSuffix(de.Name(), ".part") || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfo{key: de.Name(), path: filepath.Join(m.dir, de.Name()), size: fi.Size(), atime: fi.ModTime()})
	}
	return out
}

// Clear removes all cached tracks except active/pinned ones.
func (m *Manager) Clear() error {
	m.mu.Lock()
	keep := map[string]bool{}
	for k := range m.pinned {
		keep[k] = true
	}
	for k, e := range m.entries {
		if e.inUse() || m.pinned[k] {
			keep[k] = true
		} else {
			delete(m.entries, k)
		}
	}
	m.mu.Unlock()
	des, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		k := strings.TrimSuffix(de.Name(), ".part")
		if keep[k] {
			continue
		}
		os.Remove(filepath.Join(m.dir, de.Name()))
	}
	return nil
}

func (m *Manager) evict() {
	m.mu.Lock()
	max := m.maxBytes
	protected := map[string]bool{}
	for k := range m.pinned {
		protected[k] = true
	}
	if m.current != "" {
		protected[m.current] = true
	}
	for k, e := range m.entries {
		if e.inUse() {
			protected[k] = true
		}
	}
	m.mu.Unlock()
	if max <= 0 {
		return
	}
	files := m.files()
	var total int64
	for _, f := range files {
		total += f.size
	}
	if total <= max {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].atime.Before(files[j].atime) })
	for _, f := range files {
		if total <= max {
			break
		}
		if protected[f.key] {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			m.mu.Lock()
			delete(m.entries, f.key)
			m.mu.Unlock()
		}
	}
	// Stale partial files of downloads that aren't running anymore are kept
	// for resuming, but drop very old ones.
	des, _ := os.ReadDir(m.dir)
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".part") {
			continue
		}
		k := strings.TrimSuffix(de.Name(), ".part")
		if protected[k] {
			continue
		}
		if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
			os.Remove(filepath.Join(m.dir, de.Name()))
		}
	}
}

// Entry is a (possibly in-progress) cached track.
type Entry struct {
	m       *Manager
	req     Request
	key     string
	cancel  context.CancelFunc
	doneCh  chan struct{}
	exited  chan struct{} // closed when the download goroutine returns
	prio    Priority      // guarded by m.mu
	lowSlot bool          // holds a low-priority download slot; guarded by m.mu

	mu       sync.Mutex
	cond     *sync.Cond
	path     string
	written  int64
	total    int64
	done     bool
	err      error
	readers  int
	readPos  int64         // furthest position read by any reader
	dur      time.Duration // track duration hint (for bitrate estimates)
	fromDisk bool          // complete file found in the cache at Get
}

// Request returns the request for this entry.
func (e *Entry) Request() Request { return e.req }

// Done is closed when the download finished (successfully or not).
func (e *Entry) Done() <-chan struct{} { return e.doneCh }

// FromDisk reports whether the entry was already completely cached when it
// was created (as opposed to downloaded in this session).
func (e *Entry) FromDisk() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fromDisk
}

// SetDuration sets the track duration, used to estimate its bitrate for
// the buffering thresholds.
func (e *Entry) SetDuration(d time.Duration) {
	e.mu.Lock()
	e.dur = d
	e.mu.Unlock()
}

func (e *Entry) inUse() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.done || e.readers > 0
}

func (e *Entry) hasReaders() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readers > 0
}

func (e *Entry) isDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// Progress reports the download state. total is -1 when unknown.
func (e *Entry) Progress() (written, total int64, done bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.written, e.total, e.done, e.err
}

// Err returns the download error, if any.
func (e *Entry) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// bpsLocked estimates the track's bytes per second of audio.
func (e *Entry) bpsLocked() float64 {
	switch {
	case e.dur > 0 && e.total > 0:
		return float64(e.total) / e.dur.Seconds()
	case e.req.BitRate > 0:
		return float64(e.req.BitRate) * 125
	}
	return defaultBytesPerSec
}

// needLocked returns how many bytes past pos must be available before
// playback (re)starts: secs of audio (at least MinStartBytes), raised on
// links slower than the track's bitrate so that the rest of the download
// finishes before playback catches up.
func (e *Entry) needLocked(pos int64, secs float64) int64 {
	bps := e.bpsLocked()
	need := int64(secs * bps)
	if need < MinStartBytes {
		need = MinStartBytes
	}
	if rate := e.m.Throughput(); rate > 0 && rate < bps && e.total > 0 {
		rem := e.total - pos
		extra := int64(float64(rem) * (1 - rate/bps))
		extra = min(extra, int64(maxPrebufferSeconds*bps))
		need = max(need, extra)
	}
	return need
}

func (e *Entry) bufferedLocked(pos int64, secs float64) bool {
	if e.done || (e.total >= 0 && e.written >= e.total) {
		return true
	}
	return e.written-pos >= e.needLocked(pos, secs)
}

// Wait blocks until at least n bytes are available, the download finishes,
// or ctx is cancelled.
func (e *Entry) Wait(ctx context.Context, n int64) error {
	return e.waitFor(ctx, func() bool { return e.written >= n || e.done }, n)
}

// WaitStart blocks until enough of the track is downloaded to start
// playback without stuttering (see StartSeconds), the download finishes, or
// ctx is cancelled.
func (e *Entry) WaitStart(ctx context.Context) error {
	return e.waitFor(ctx, func() bool { return e.bufferedLocked(0, StartSeconds) }, 1)
}

func (e *Entry) waitFor(ctx context.Context, ok func() bool, n int64) error {
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()
	defer e.pollWhileWaiting()()
	e.mu.Lock()
	defer e.mu.Unlock()
	for !ok() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.cond.Wait()
	}
	if e.err != nil && e.written < n {
		return e.err
	}
	return nil
}

// pollWhileWaiting wakes cond waiters periodically, because thresholds
// that depend on the measured throughput change without a signal. It
// returns the function that stops polling.
func (e *Entry) pollWhileWaiting() func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				e.mu.Lock()
				e.cond.Broadcast()
				e.mu.Unlock()
			}
		}
	}()
	return func() { close(stop) }
}

// Open returns a reader over the entry. Reads past the downloaded part block
// until data arrives (or the download ends).
func (e *Entry) Open() (*Reader, error) {
	e.mu.Lock()
	path := e.path
	e.mu.Unlock()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// The .part file may have been renamed between reading path and
		// open, or not created yet.
		e.mu.Lock()
		path = e.path
		e.mu.Unlock()
		f, err = os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			e.mu.Lock()
			path = e.path
			e.mu.Unlock()
			f, err = os.Open(path)
		}
	}
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.readers++
	e.mu.Unlock()
	return &Reader{e: e, f: f}, nil
}

// Reader is an io.ReadSeekCloser over a growing cache file.
type Reader struct {
	e           *Entry
	f           *os.File
	pos         int64
	interrupted bool
	closed      bool
	// WaitHook, if set, is called with true when a read starts waiting for
	// data and false when it stops waiting.
	WaitHook func(waiting bool)
}

// Read implements io.Reader. A read that catches up with the download
// waits until ResumeSeconds of audio (more on slow links) is available
// again, so playback resumes with a healthy buffer instead of stuttering.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	e := r.e
	e.mu.Lock()
	waited := false
	if r.pos >= e.written && !e.done && !r.interrupted && !r.closed {
		if r.WaitHook != nil {
			e.mu.Unlock()
			r.WaitHook(true)
			e.mu.Lock()
		}
		waited = true
		slog.Debug("cache: reader waiting for data", "key", e.key, "pos", r.pos, "written", e.written)
		stop := e.pollWhileWaiting()
		for !e.bufferedLocked(r.pos, ResumeSeconds) && !r.interrupted && !r.closed {
			e.cond.Wait()
		}
		stop()
	}
	avail := e.written - r.pos
	done, derr := e.done, e.err
	interrupted, closed := r.interrupted, r.closed
	e.mu.Unlock()
	if waited && r.WaitHook != nil {
		r.WaitHook(false)
	}
	if closed {
		return 0, os.ErrClosed
	}
	if interrupted {
		return 0, ErrInterrupted
	}
	if avail <= 0 {
		if done && derr != nil {
			return 0, derr
		}
		return 0, io.EOF
	}
	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := r.f.ReadAt(p, r.pos)
	r.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	e.mu.Lock()
	if r.pos > e.readPos {
		e.readPos = r.pos
	}
	e.mu.Unlock()
	return n, err
}

// Seek implements io.Seeker. SeekEnd blocks until the total size is known.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var npos int64
	switch whence {
	case io.SeekStart:
		npos = offset
	case io.SeekCurrent:
		npos = r.pos + offset
	case io.SeekEnd:
		e := r.e
		e.mu.Lock()
		for e.total < 0 && !e.done && !r.interrupted && !r.closed {
			e.cond.Wait()
		}
		total, interrupted := e.total, r.interrupted
		if e.done {
			total = e.written
		}
		e.mu.Unlock()
		if interrupted {
			return r.pos, ErrInterrupted
		}
		npos = total + offset
	default:
		return r.pos, errors.New("invalid whence")
	}
	if npos < 0 {
		return r.pos, errors.New("negative position")
	}
	r.pos = npos
	e := r.e
	e.mu.Lock()
	e.readPos = npos // seeking resets the play position used for health
	e.mu.Unlock()
	return npos, nil
}

// Size returns the total size if known (-1 otherwise) without blocking.
func (r *Reader) Size() int64 {
	w, t, done, _ := r.e.Progress()
	if done {
		return w
	}
	return t
}

// Complete reports whether the underlying download has finished successfully.
func (r *Reader) Complete() bool {
	_, _, done, err := r.e.Progress()
	return done && err == nil
}

// Entry returns the cache entry being read.
func (r *Reader) Entry() *Entry { return r.e }

// Interrupt makes blocked and future reads return ErrInterrupted.
func (r *Reader) Interrupt() {
	r.e.mu.Lock()
	r.interrupted = true
	r.e.cond.Broadcast()
	r.e.mu.Unlock()
}

// Close implements io.Closer.
func (r *Reader) Close() error {
	r.e.mu.Lock()
	if r.closed {
		r.e.mu.Unlock()
		return nil
	}
	r.closed = true
	r.e.readers--
	r.e.cond.Broadcast()
	r.e.mu.Unlock()
	return r.f.Close()
}

// SetWaitHook sets WaitHook.
func (r *Reader) SetWaitHook(h func(waiting bool)) { r.WaitHook = h }

// WaitComplete blocks until the download has finished, the reader is
// interrupted or closed, or ctx is done.
func (r *Reader) WaitComplete(ctx context.Context) error {
	e := r.e
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	waited := false
	for !e.done && !r.interrupted && !r.closed && ctx.Err() == nil {
		if !waited && r.WaitHook != nil {
			waited = true
			e.mu.Unlock()
			r.WaitHook(true)
			e.mu.Lock()
			continue
		}
		e.cond.Wait()
	}
	if waited && r.WaitHook != nil {
		e.mu.Unlock()
		r.WaitHook(false)
		e.mu.Lock()
	}
	switch {
	case r.interrupted:
		return ErrInterrupted
	case r.closed:
		return os.ErrClosed
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return e.err
}
