package audio

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

// gateSource is a Source whose data becomes available progressively, like a
// downloading cache file.
type gateSource struct {
	mu          sync.Mutex
	cond        *sync.Cond
	data        []byte
	avail       int
	pos         int64
	interrupted bool
	closed      bool
	hook        func(bool)
}

func newGateSource(data []byte, avail int) *gateSource {
	g := &gateSource{data: data, avail: avail}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gateSource) release(n int) {
	g.mu.Lock()
	g.avail = min(len(g.data), n)
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gateSource) Read(p []byte) (int, error) {
	g.mu.Lock()
	waited := false
	for int(g.pos) >= g.avail && g.avail < len(g.data) && !g.interrupted && !g.closed {
		if !waited && g.hook != nil {
			waited = true
			h := g.hook
			g.mu.Unlock()
			h(true)
			g.mu.Lock()
			continue
		}
		g.cond.Wait()
	}
	h := g.hook
	defer func() {
		if waited && h != nil {
			h(false)
		}
	}()
	defer g.mu.Unlock()
	if g.closed {
		return 0, os.ErrClosed
	}
	if g.interrupted {
		return 0, errInterrupted
	}
	if int(g.pos) >= len(g.data) {
		return 0, io.EOF
	}
	n := copy(p, g.data[g.pos:g.avail])
	g.pos += int64(n)
	return n, nil
}

func (g *gateSource) Seek(off int64, whence int) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch whence {
	case io.SeekStart:
		g.pos = off
	case io.SeekCurrent:
		g.pos += off
	case io.SeekEnd:
		g.pos = int64(len(g.data)) + off
	}
	return g.pos, nil
}

func (g *gateSource) Close() error {
	g.mu.Lock()
	g.closed = true
	g.cond.Broadcast()
	g.mu.Unlock()
	return nil
}

func (g *gateSource) Interrupt() {
	g.mu.Lock()
	g.interrupted = true
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gateSource) Complete() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.avail >= len(g.data)
}

func (g *gateSource) WaitComplete(ctx context.Context) error { return nil }

func (g *gateSource) SetWaitHook(h func(bool)) {
	g.mu.Lock()
	g.hook = h
	g.mu.Unlock()
}

// player drives the output stage roughly in real time (like a device) and
// records state changes.
type player struct {
	t      *testing.T
	e      *Engine
	states []State
	events []Event
	heard  int // frames with audible output
}

func (p *player) run(d time.Duration, until func() bool) {
	p.t.Helper()
	buf := make([]float32, 441*2) // 10 ms
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.e.Render(buf)
		for i := 0; i < len(buf)/2; i++ {
			if math.Abs(float64(buf[2*i])) > 1e-4 {
				p.heard++
			}
		}
	drain:
		for {
			select {
			case ev := <-p.e.events:
				p.events = append(p.events, ev)
				if ev.Kind == EvState {
					p.states = append(p.states, ev.State)
				}
			default:
				break drain
			}
		}
		if until != nil && until() {
			return
		}
		time.Sleep(2 * time.Millisecond) // 10 ms of audio per ~2 ms: faster than real time
	}
	p.t.Fatalf("timeout; state %v, states %v", p.e.State(), p.states)
}

func TestStateTransitionsLoadingPlayingBufferingPlaying(t *testing.T) {
	const frames = SampleRate * 2
	wav := makeWAV(SampleRate, frames, func(i int) float64 { return 0.5 })
	src := newGateSource(wav, 0)
	m := FromSource("a", "wav", ReplayGain{}, func(context.Context) (Source, error) { return src, nil })

	e := New()
	defer e.Close()
	p := &player{t: t, e: e}
	e.Load(m, 0, false)
	if e.State() != Loading {
		t.Fatalf("state after Load = %v, want Loading", e.State())
	}

	// No data: the output runs, but nothing is audible and the state must
	// stay Loading.
	end := time.Now().Add(150 * time.Millisecond)
	p.run(time.Second, func() bool { return time.Now().After(end) })
	if p.heard > 0 || e.State() != Loading {
		t.Fatalf("before data: heard %d frames, state %v", p.heard, e.State())
	}

	// Half a second of audio arrives: Playing, and only with audible output.
	src.release(44 + frames)
	p.run(5*time.Second, func() bool {
		if e.State() == Playing && p.heard == 0 {
			t.Fatal("Playing without audible output")
		}
		return e.State() == Playing
	})

	// The download stalls: playback drains the buffer and must report
	// Buffering rather than pretending to play.
	p.run(5*time.Second, func() bool { return e.State() == Buffering })
	heard := p.heard

	// Data resumes: playback continues automatically.
	src.release(len(wav))
	p.run(5*time.Second, func() bool { return e.State() == Playing })
	if p.heard <= heard {
		t.Fatal("resumed Playing without output")
	}
	p.run(10*time.Second, func() bool { return e.State() == Stopped })

	want := []State{Loading, Playing, Buffering, Playing, Stopped}
	got := compact(p.states)
	if !containsSeq(got, want) {
		t.Fatalf("state sequence %v does not contain %v", got, want)
	}
	for _, ev := range p.events {
		if ev.Kind == EvError {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
}

func TestPausedLoadDoesNotPlay(t *testing.T) {
	wav := makeWAV(SampleRate, SampleRate/2, func(i int) float64 { return 0.5 })
	m := wavMedia("a", wav)
	e := New()
	defer e.Close()
	p := &player{t: t, e: e}
	e.Load(m, 0, true)
	end := time.Now().Add(150 * time.Millisecond)
	p.run(time.Second, func() bool { return time.Now().After(end) })
	if e.State() != Paused || p.heard > 0 {
		t.Fatalf("paused load: state %v, heard %d", e.State(), p.heard)
	}
	e.Resume()
	p.run(5*time.Second, func() bool { return e.State() == Playing })
}

func compact(s []State) []State {
	var out []State
	for _, v := range s {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func containsSeq(got, want []State) bool {
	j := 0
	for _, v := range got {
		if j < len(want) && v == want[j] {
			j++
		}
	}
	return j == len(want)
}

// fakeDecoder produces frames of a constant then returns err (after its
// samples, like a FLAC file with a trailing ID3 tag).
type fakeDecoder struct {
	frames, pos int64
	length      int64
	err         error
	closed      *bool
}

func (d *fakeDecoder) Read(dst []float32) (int, error) {
	if *d.closed {
		return 0, os.ErrClosed
	}
	n := min(int64(len(dst)/2), d.frames-d.pos)
	if n <= 0 {
		return 0, d.err
	}
	for i := range dst[:2*n] {
		dst[i] = 0.25
	}
	d.pos += n
	return int(n), nil
}
func (d *fakeDecoder) SampleRate() int         { return SampleRate }
func (d *fakeDecoder) SeekFrame(f int64) error { d.pos = f; return nil }
func (d *fakeDecoder) Length() int64           { return d.length }
func (d *fakeDecoder) Close() error            { *d.closed = true; return nil }

func fakeMedia(id string, frames, length int64, err error) (*Media, *bool) {
	closed := new(bool)
	return &Media{ID: id, Open: func(context.Context) (Decoder, error) {
		*closed = false
		return &fakeDecoder{frames: frames, length: length, err: err, closed: closed}, nil
	}}, closed
}

// A gapless transition closes the finished track's decoder; errors at its
// end (trailing junk, closed file) must not surface as playback errors.
func TestGaplessTransitionEmitsNoError(t *testing.T) {
	const n = SampleRate / 2
	trailing := errors.New("frame.Frame.parseHeader: invalid sync-code")
	a, aClosed := fakeMedia("a", n, n, trailing)
	b, _ := fakeMedia("b", n, -1, io.EOF)

	e := New()
	defer e.Close()
	e.Load(a, 0, false)
	e.SetNext(b)
	_, events := renderAll(t, e, n*4)
	if !*aClosed {
		t.Fatal("finished track was not closed")
	}
	var started []string
	for _, ev := range events {
		switch ev.Kind {
		case EvError:
			t.Fatalf("unexpected error event for %q: %v", ev.ID, ev.Err)
		case EvTrackStarted:
			started = append(started, ev.ID)
		}
	}
	if len(started) != 2 || started[1] != "b" {
		t.Fatalf("tracks started: %v", started)
	}
}

// A genuine mid-track failure of the current track is still reported,
// tagged with that track's sequence number.
func TestCurrentTrackErrorIsReported(t *testing.T) {
	const n = SampleRate * 2
	broken := errors.New("corrupt frame")
	a, _ := fakeMedia("a", n/4, n, broken) // fails a quarter of the way in
	e := New()
	defer e.Close()
	seq := e.Load(a, 0, false)
	_, events := renderAll(t, e, n)
	var errs []Event
	for _, ev := range events {
		if ev.Kind == EvError {
			errs = append(errs, ev)
		}
	}
	if len(errs) != 1 || errs[0].ID != "a" || errs[0].Seq != seq || !errors.Is(errs[0].Err, broken) {
		t.Fatalf("error events = %+v, want one for a/seq %d", errs, seq)
	}
	if errs[0].Open {
		t.Fatal("mid-track error flagged as an open failure")
	}
}

func TestIsTeardown(t *testing.T) {
	for _, err := range []error{context.Canceled, os.ErrClosed, io.ErrClosedPipe, errInterrupted,
		errors.New("read x: file already closed"), errors.New("use of closed network connection")} {
		if !isTeardown(err) {
			t.Errorf("isTeardown(%v) = false", err)
		}
	}
	if isTeardown(errors.New("corrupt")) || isTeardown(io.ErrUnexpectedEOF) {
		t.Error("genuine errors classified as teardown")
	}
}
