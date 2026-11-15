// Package cache implements the on-disk track cache with resumable downloads,
// LRU eviction, low-priority prefetching, and a cover-art cache.
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// Low priority downloads (prefetch) run one at a time and only when no
	// high priority download is active.
	Low Priority = iota
	// High priority downloads (the current track) start immediately.
	High
)

// ErrInterrupted is returned by Reader.Read after Interrupt.
var ErrInterrupted = errors.New("read interrupted")

// Manager owns the cache directory.
type Manager struct {
	dir string

	mu       sync.Mutex
	cond     *sync.Cond
	src      Source
	maxBytes int64
	entries  map[string]*Entry
	pinned   map[string]bool
	highBusy int
	prefetch []Request
	pfCancel context.CancelFunc
	pfGen    int
	closed   bool
}

// New creates a manager storing tracks in dir.
func New(dir string, maxBytes int64) *Manager {
	os.MkdirAll(dir, 0o700)
	m := &Manager{
		dir:      dir,
		maxBytes: maxBytes,
		entries:  map[string]*Entry{},
		pinned:   map[string]bool{},
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

func (m *Manager) finalPath(key string) string { return filepath.Join(m.dir, key) }
func (m *Manager) partPath(key string) string  { return filepath.Join(m.dir, key+".part") }

// Cached reports whether the request is fully downloaded.
func (m *Manager) Cached(r Request) bool {
	_, err := os.Stat(m.finalPath(r.Key()))
	return err == nil
}

// Get returns the entry for r, starting a download if necessary.
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
	e := &Entry{m: m, req: r, key: key, total: -1, prio: prio, doneCh: make(chan struct{})}
	e.cond = sync.NewCond(&e.mu)
	if fi, err := os.Stat(m.finalPath(key)); err == nil {
		e.path = m.finalPath(key)
		e.written = fi.Size()
		e.total = fi.Size()
		e.done = true
		close(e.doneCh)
		now := time.Now()
		os.Chtimes(e.path, now, now)
		m.entries[key] = e
		return e
	}
	e.path = m.partPath(key)
	if f, err := os.OpenFile(e.path, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		if fi, err := f.Stat(); err == nil {
			e.written = fi.Size()
		}
		f.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	m.entries[key] = e
	go e.download(ctx)
	return e
}

// Prefetch replaces the prefetch list. Downloads run sequentially at low
// priority; in-flight prefetches that are no longer wanted are cancelled.
func (m *Manager) Prefetch(reqs []Request) {
	m.mu.Lock()
	want := map[string]bool{}
	for _, r := range reqs {
		want[r.Key()] = true
	}
	for k, e := range m.entries {
		if !e.isDone() && e.prio == Low && !want[k] {
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
	for _, r := range reqs {
		if ctx.Err() != nil {
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

// Forget drops a finished or failed entry from the in-memory table so that
// a later Get retries (used after errors).
func (m *Manager) Forget(r Request) {
	m.mu.Lock()
	if e, ok := m.entries[r.Key()]; ok && e.isDone() {
		delete(m.entries, r.Key())
	}
	m.mu.Unlock()
}

// Close cancels all downloads.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, e := range m.entries {
		if e.cancel != nil {
			e.cancel()
		}
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
		if !e.isDone() || m.pinned[k] {
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
	m      *Manager
	req    Request
	key    string
	cancel context.CancelFunc
	doneCh chan struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	path    string
	written int64
	total   int64
	done    bool
	err     error
	prio    Priority
	readers int
}

// Request returns the request for this entry.
func (e *Entry) Request() Request { return e.req }

// Done is closed when the download finished (successfully or not).
func (e *Entry) Done() <-chan struct{} { return e.doneCh }

func (e *Entry) inUse() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.done || e.readers > 0
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

// Wait blocks until at least n bytes are available, the download finishes,
// or ctx is cancelled.
func (e *Entry) Wait(ctx context.Context, n int64) error {
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.written < n && !e.done {
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

func (e *Entry) setErr(err error) {
	e.mu.Lock()
	e.err = err
	e.done = true
	e.cond.Broadcast()
	e.mu.Unlock()
	close(e.doneCh)
}

func (e *Entry) download(ctx context.Context) {
	m := e.m
	// Low-priority downloads wait until no high-priority download is active.
	m.mu.Lock()
	for e.prio == Low && m.highBusy > 0 && ctx.Err() == nil && !m.closed {
		waitCond(ctx, m.cond)
	}
	high := e.prio == High
	if high {
		m.highBusy++
	}
	src := m.src
	m.mu.Unlock()
	defer func() {
		if high {
			m.mu.Lock()
			m.highBusy--
			m.cond.Broadcast()
			m.mu.Unlock()
		}
	}()
	if ctx.Err() != nil {
		e.setErr(ctx.Err())
		return
	}
	if src == nil {
		e.setErr(errors.New("not connected to a server"))
		return
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				e.setErr(ctx.Err())
				return
			}
		}
		err := e.fetch(ctx, src)
		if err == nil {
			final := m.finalPath(e.key)
			if err := os.Rename(e.path, final); err != nil {
				e.setErr(err)
				return
			}
			e.mu.Lock()
			e.path = final
			e.done = true
			e.total = e.written
			e.cond.Broadcast()
			e.mu.Unlock()
			close(e.doneCh)
			go m.evict()
			return
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		var he interface{ Error() string }
		if errors.As(err, &he) && strings.Contains(err.Error(), "HTTP error: 4") {
			break // client errors won't get better by retrying
		}
	}
	e.setErr(lastErr)
}

func waitCond(ctx context.Context, c *sync.Cond) {
	stop := context.AfterFunc(ctx, func() {
		c.L.Lock()
		c.Broadcast()
		c.L.Unlock()
	})
	c.Wait()
	stop()
}

// fetch downloads (or resumes) into the .part file.
func (e *Entry) fetch(ctx context.Context, src Source) error {
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
	resp, err := src.Stream(ctx, e.req.ID, format, e.req.BitRate, offset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		offset = 0 // server ignored Range; restart
	}
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	e.mu.Lock()
	e.written = offset
	e.total = total
	e.cond.Broadcast()
	e.mu.Unlock()

	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			e.mu.Lock()
			e.written += int64(n)
			e.cond.Broadcast()
			e.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	e.mu.Lock()
	short := e.total >= 0 && e.written < e.total
	e.mu.Unlock()
	if short {
		return io.ErrUnexpectedEOF
	}
	return f.Sync()
}

// Open returns a reader over the entry. Reads past the downloaded part block
// until data arrives (or the download ends).
func (e *Entry) Open() (*Reader, error) {
	e.mu.Lock()
	path := e.path
	e.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		// The .part file may have been renamed between reading path and open.
		e.mu.Lock()
		path = e.path
		e.mu.Unlock()
		if f, err = os.Open(path); err != nil {
			return nil, err
		}
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

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	e := r.e
	e.mu.Lock()
	waited := false
	for r.pos >= e.written && !e.done && !r.interrupted && !r.closed {
		if !waited && r.WaitHook != nil {
			waited = true
			e.mu.Unlock()
			r.WaitHook(true)
			e.mu.Lock()
			continue
		}
		e.cond.Wait()
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
