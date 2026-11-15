package cache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu     sync.Mutex
	data   map[string][]byte
	gate   chan struct{} // if set, the second half is held until closed
	ranges []int64
}

func (f *fakeSource) Stream(ctx context.Context, id, format string, br int, offset int64) (*http.Response, error) {
	f.mu.Lock()
	d, ok := f.data[id]
	f.ranges = append(f.ranges, offset)
	gate := f.gate
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("HTTP error: 404")
	}
	pr, pw := io.Pipe()
	go func() {
		rest := d[offset:]
		half := len(rest) / 2
		pw.Write(rest[:half])
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
				return
			}
		}
		pw.Write(rest[half:])
		pw.Close()
	}()
	status := http.StatusOK
	if offset > 0 {
		status = http.StatusPartialContent
	}
	return &http.Response{StatusCode: status, Body: pr, ContentLength: int64(len(d)) - offset}, nil
}

func TestDownloadAndRead(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("0123456789"), 10000)
	src := &fakeSource{data: map[string][]byte{"a": data}, gate: make(chan struct{})}
	m := New(dir, 1<<30)
	m.SetSource(src)
	e := m.Get(Request{ID: "a"}, High)
	r, err := e.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	waited := make(chan bool, 4)
	r.WaitHook = func(w bool) {
		select {
		case waited <- w:
		default:
		}
	}
	got := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		got <- b
	}()
	time.Sleep(50 * time.Millisecond)
	close(src.gate)
	b := <-got
	if !bytes.Equal(b, data) {
		t.Fatalf("read %d bytes, want %d", len(b), len(data))
	}
	<-e.Done()
	if !m.Cached(Request{ID: "a"}) {
		t.Fatal("not cached after completion")
	}
	if _, err := os.Stat(filepath.Join(dir, Request{ID: "a"}.Key()+".part")); !os.IsNotExist(err) {
		t.Fatal(".part file left behind")
	}
	select {
	case w := <-waited:
		if !w {
			t.Fatal("expected waiting=true first")
		}
	default:
		t.Fatal("reader never waited for data")
	}
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), 5000)
	req := Request{ID: "b", Format: "mp3", BitRate: 320}
	os.WriteFile(filepath.Join(dir, req.Key()+".part"), data[:1000], 0o600)
	src := &fakeSource{data: map[string][]byte{"b": data}}
	m := New(dir, 1<<30)
	m.SetSource(src)
	e := m.Get(req, High)
	<-e.Done()
	if e.Err() != nil {
		t.Fatal(e.Err())
	}
	if src.ranges[0] != 1000 {
		t.Fatalf("expected range from 1000, got %v", src.ranges)
	}
	b, _ := os.ReadFile(filepath.Join(dir, req.Key()))
	if len(b) != 5000 {
		t.Fatalf("size %d", len(b))
	}
}

func TestEvictionKeepsPinned(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, 2500)
	old := time.Now().Add(-time.Hour)
	for i, k := range []string{"k1", "k2", "k3"} {
		p := filepath.Join(dir, Request{ID: k}.Key())
		os.WriteFile(p, make([]byte, 1000), 0o600)
		ts := old.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, ts, ts)
	}
	m.Pin(Request{ID: "k1"})
	m.evict()
	if !m.Cached(Request{ID: "k1"}) {
		t.Fatal("pinned entry evicted")
	}
	if m.Cached(Request{ID: "k2"}) {
		t.Fatal("oldest unpinned entry should be evicted")
	}
	if !m.Cached(Request{ID: "k3"}) {
		t.Fatal("newest entry evicted")
	}
}

func TestInterrupt(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{data: map[string][]byte{"c": make([]byte, 1000)}, gate: make(chan struct{})}
	defer close(src.gate)
	m := New(dir, 1<<30)
	m.SetSource(src)
	e := m.Get(Request{ID: "c"}, High)
	r, _ := e.Open()
	defer r.Close()
	r.Seek(900, io.SeekStart)
	errc := make(chan error)
	go func() {
		_, err := r.Read(make([]byte, 10))
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	r.Interrupt()
	if err := <-errc; err != ErrInterrupted {
		t.Fatalf("got %v", err)
	}
}
