package cache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusErr mimics subsonic.HTTPError.
type statusErr int

func (e statusErr) Error() string   { return fmt.Sprintf("HTTP error: %d", int(e)) }
func (e statusErr) HTTPStatus() int { return int(e) }

// httpSource streams from an httptest server like subsonic.Client does.
type httpSource struct{ url string }

func (s httpSource) Stream(ctx context.Context, id, format string, br int, offset int64) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/"+id, nil)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, statusErr(resp.StatusCode)
	}
	return resp, nil
}

// behaviour decides how request n (0-based) is served.
type behaviour func(n int, w http.ResponseWriter, r *http.Request, data []byte, offset int64) bool

type testServer struct {
	*httptest.Server
	mu      sync.Mutex
	offsets []int64
}

func (ts *testServer) requests() []int64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]int64(nil), ts.offsets...)
}

func newServer(t *testing.T, data []byte, honourRange bool, b behaviour) *testServer {
	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var off int64
		if h := r.Header.Get("Range"); h != "" && honourRange {
			off, _ = strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(h, "bytes="), "-"), 10, 64)
		}
		ts.mu.Lock()
		n := len(ts.offsets)
		ts.offsets = append(ts.offsets, off)
		ts.mu.Unlock()
		if b != nil && b(n, w, r, data, off) {
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(off)))
		if off > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(data[off:])
	}))
	t.Cleanup(ts.Close)
	return ts
}

// sendThenReset writes the headers for the full remaining range and part of
// the body, then kills the connection.
func sendThenReset(w http.ResponseWriter, data []byte, off int64, n int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(off)))
	if off > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Write(data[off : off+int64(n)])
	w.(http.Flusher).Flush()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func testData(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func newTestManager(t *testing.T, src Source) *Manager {
	m := New(t.TempDir(), 1<<30)
	m.StallTimeout = 300 * time.Millisecond
	m.BackoffBase = 5 * time.Millisecond
	m.BackoffMax = 20 * time.Millisecond
	m.SetSource(src)
	t.Cleanup(m.Close)
	return m
}

func download(t *testing.T, m *Manager, id string) *Entry {
	t.Helper()
	e := m.Get(Request{ID: id}, High)
	select {
	case <-e.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("download did not finish")
	}
	return e
}

func checkFile(t *testing.T, m *Manager, id string, data []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.Dir(), Request{ID: id}.Key()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, data) {
		t.Fatalf("file differs: got %d bytes, want %d", len(b), len(data))
	}
}

func TestDownloadSlowBody(t *testing.T) {
	data := testData(400 << 10)
	ts := newServer(t, data, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		for i := 0; i < len(data); i += 20 << 10 {
			w.Write(data[i:min(len(data), i+20<<10)])
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond) // slow, but below the stall timeout
		}
		return true
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	checkFile(t, m, "a", data)
	if got := ts.requests(); len(got) != 1 {
		t.Fatalf("slow body should not be retried, requests %v", got)
	}
	if m.Throughput() <= 0 {
		t.Fatal("throughput not measured")
	}
}

func TestDownloadResumesAfterReset(t *testing.T) {
	data := testData(300 << 10)
	ts := newServer(t, data, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		if n < 2 {
			sendThenReset(w, data, off, 50<<10)
			return true
		}
		return false
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	checkFile(t, m, "a", data)
	got := ts.requests()
	if len(got) != 3 || got[0] != 0 || got[1] != 50<<10 || got[2] != 100<<10 {
		t.Fatalf("expected range resumes at 0, 50K, 100K; got %v", got)
	}
}

func TestDownloadServerIgnoresRange(t *testing.T) {
	data := testData(300 << 10)
	ts := newServer(t, data, false, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		if n == 0 {
			sendThenReset(w, data, 0, 100<<10)
			return true
		}
		return false // 200 with the whole file despite the Range header
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	checkFile(t, m, "a", data) // restarted from zero, not appended
	if n := len(ts.requests()); n != 2 {
		t.Fatalf("requests %d", n)
	}
}

func TestDownloadStallRetries(t *testing.T) {
	data := testData(200 << 10)
	ts := newServer(t, data, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		if n == 0 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:70<<10])
			w.(http.Flusher).Flush()
			select { // stall without closing the connection
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return true
		}
		return false
	})
	m := newTestManager(t, httpSource{ts.URL})
	start := time.Now()
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stall was not detected")
	}
	checkFile(t, m, "a", data)
	if got := ts.requests(); len(got) != 2 || got[1] != 70<<10 {
		t.Fatalf("expected resume from 70K after the stall, got %v", got)
	}
}

func TestDownloadRetries5xx(t *testing.T) {
	data := testData(100 << 10)
	ts := newServer(t, data, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		if n < 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	checkFile(t, m, "a", data)
	if n := len(ts.requests()); n != 3 {
		t.Fatalf("requests %d", n)
	}
}

func TestDownloadFailsFastOn404(t *testing.T) {
	ts := newServer(t, nil, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		http.NotFound(w, r)
		return true
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() == nil {
		t.Fatal("expected an error")
	}
	if n := len(ts.requests()); n != 1 {
		t.Fatalf("404 retried: %d requests", n)
	}
}

func TestDownloadGivesUpAfterMaxAttempts(t *testing.T) {
	ts := newServer(t, nil, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		http.Error(w, "boom", http.StatusInternalServerError)
		return true
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() == nil {
		t.Fatal("expected an error")
	}
	if n := len(ts.requests()); n != m.MaxAttempts {
		t.Fatalf("attempts %d, want %d", n, m.MaxAttempts)
	}
}

func TestDownloadShortBodyResumes(t *testing.T) {
	data := testData(100 << 10)
	ts := newServer(t, data, true, func(n int, w http.ResponseWriter, r *http.Request, data []byte, off int64) bool {
		if n == 0 {
			sendThenReset(w, data, 0, 99<<10)
			return true
		}
		return false
	})
	m := newTestManager(t, httpSource{ts.URL})
	e := download(t, m, "a")
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	checkFile(t, m, "a", data)
}

// gatedSource serves "cur" slowly (until released) and anything else
// immediately, recording when each download started.
type gatedSource struct {
	mu      sync.Mutex
	started []string
	release chan struct{}
	data    []byte
}

func (g *gatedSource) Stream(ctx context.Context, id, format string, br int, offset int64) (*http.Response, error) {
	g.mu.Lock()
	g.started = append(g.started, id)
	g.mu.Unlock()
	rec := httptest.NewRecorder()
	body := g.data[offset:]
	if id == "cur" {
		pr, pw := io.Pipe()
		go func() {
			pw.Write(body[:len(body)/4])
			select {
			case <-g.release:
				pw.Write(body[len(body)/4:])
				pw.Close()
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr, ContentLength: int64(len(body))}, nil
	}
	rec.Write(body)
	resp := rec.Result()
	resp.ContentLength = int64(len(body))
	return resp, nil
}

func (g *gatedSource) has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.started {
		if s == id {
			return true
		}
	}
	return false
}

func TestPrefetchWaitsForCurrent(t *testing.T) {
	g := &gatedSource{release: make(chan struct{}), data: testData(4 << 20)}
	m := newTestManager(t, g)
	m.StallTimeout = 10 * time.Second
	cur := Request{ID: "cur"}
	m.SetCurrent(cur)
	ce := m.Get(cur, High)
	ce.SetDuration(4 * time.Minute) // ~17 KB/s: 1 MB buffered is ~60 s ahead
	// Open a reader at the download head so the current buffer is low.
	r, _ := ce.Open()
	defer r.Close()
	for ce.Written() < 1<<20 {
		time.Sleep(5 * time.Millisecond)
	}
	r.Seek(ce.Written(), 0)
	m.Prefetch([]Request{{ID: "next"}, {ID: "after"}})
	time.Sleep(300 * time.Millisecond)
	if g.has("next") {
		t.Fatal("prefetch started while the current track's buffer was empty")
	}
	close(g.release)
	<-ce.Done()
	deadline := time.Now().Add(5 * time.Second)
	for !(m.Cached(Request{ID: "next"}) && m.Cached(Request{ID: "after"})) {
		if time.Now().After(deadline) {
			t.Fatal("prefetches did not run after the current track finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.mu.Lock()
	order := strings.Join(g.started, ",")
	g.mu.Unlock()
	if order != "cur,next,after" {
		t.Fatalf("download order %s", order)
	}
}

func TestWaitStartThreshold(t *testing.T) {
	g := &gatedSource{release: make(chan struct{}), data: testData(4 << 20)}
	m := newTestManager(t, g)
	m.StallTimeout = 10 * time.Second
	e := m.Get(Request{ID: "cur"}, High)
	e.SetDuration(10 * time.Second) // 400 KB/s: 3 s = 1.2 MB > the 1 MB available
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.WaitStart(ctx); err == nil {
		t.Fatal("WaitStart returned before the start threshold")
	}
	close(g.release)
	if err := e.WaitStart(context.Background()); err != nil {
		t.Fatal(err)
	}
}
