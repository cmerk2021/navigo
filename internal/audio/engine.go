// Package audio implements Navigo's playback engine: decoding, resampling,
// gapless track transitions, EQ/limiter processing and output through oto.
package audio

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmerk2021/navigo/internal/eq"
)

// SampleRate is the fixed output sample rate.
const SampleRate = 44100

// chunkFrames is the size of one mixer chunk (~23 ms).
const chunkFrames = 1024

// ringChunks bounds decoded audio buffered ahead of the output (~140 ms).
const ringChunks = 6

// State is the playback state.
type State int32

// Playback states.
const (
	Stopped State = iota
	Loading
	Playing
	Paused
	Buffering
)

func (s State) String() string {
	return [...]string{"Stopped", "Loading", "Playing", "Paused", "Buffering"}[s]
}

// EventKind identifies an engine event.
type EventKind int

// Event kinds.
const (
	// EvState: State changed.
	EvState EventKind = iota
	// EvTrackStarted: the output started playing a track (ID, Seq, Auto).
	EvTrackStarted
	// EvEnded: playback reached the end with no next track.
	EvEnded
	// EvError: a track failed (ID, Err); playback continues with the next
	// track if one was prepared.
	EvError
	// EvDeviceError: the output device failed.
	EvDeviceError
)

// Event is published to the UI/controller.
type Event struct {
	Kind  EventKind
	State State
	ID    string
	Seq   int64
	// Auto is true when a track started through a gapless transition.
	Auto bool
	// Open is set on EvError when the track failed to open (as opposed to
	// failing mid-stream).
	Open bool
	Err  error
}

// Media describes a playable item.
type Media struct {
	ID string
	// Gain is a linear ReplayGain factor (0 means 1).
	Gain float32
	// Open returns a decoder positioned at the start. It may block until data
	// is available and must honour ctx cancellation.
	Open func(ctx context.Context) (Decoder, error)
}

type chunk struct {
	gen     int64
	seq     int64
	id      string
	auto    bool
	start   int64 // output-rate frame position of samples[0] within the track
	gain    float32
	samples []float32
	end     bool // end of playback marker
}

type track struct {
	media *Media
	dec   Decoder
	rs    *resampler // nil if no resampling needed
	seq   int64
	pos   int64 // frames produced (output rate)
	auto  bool
}

func (t *track) read(dst []float32) (int, error) {
	if t.rs != nil {
		return t.rs.Read(dst)
	}
	return t.dec.Read(dst)
}

func (t *track) close() {
	if t != nil && t.dec != nil {
		t.dec.Close()
	}
}

type cmdKind int

const (
	cmdLoad cmdKind = iota
	cmdSeek
	cmdStop
	cmdSetNext
)

type command struct {
	kind  cmdKind
	media *Media
	pos   int64 // frames
	gen   int64
	seq   int64
}

// Engine is the playback engine. Its methods are safe for concurrent use.
type Engine struct {
	events chan Event
	cmds   chan command
	ring   chan *chunk

	eqp *eq.Processor
	lim *eq.Limiter

	gen    atomic.Int64 // incremented on load/seek/stop (flushes the ring)
	seqCtr atomic.Int64
	state  atomic.Int32
	paused atomic.Bool
	volume atomic.Uint64 // float64 bits
	muted  atomic.Bool

	// position of the audio currently heard
	posFrames atomic.Int64
	posSeq    atomic.Int64

	mu         sync.Mutex
	interrupts []func() // aborts blocking work in the worker
	waiting    bool
	bufTimer   *time.Timer

	// output stage (only touched by the output goroutine)
	out outStage

	device atomic.Pointer[device]
	closed chan struct{}
	once   sync.Once
}

type outStage struct {
	cur       *chunk
	off       int
	lastSeq   int64
	gain      float32 // pause fade gain
	fadeIn    int     // frames of seek fade-in remaining
	silentFor int     // frames of silence output while paused
	buf       []float32
	endSent   bool
}

// New creates an engine. Call StartDevice to begin audio output.
func New() *Engine {
	e := &Engine{
		events: make(chan Event, 128),
		cmds:   make(chan command, 16),
		ring:   make(chan *chunk, ringChunks),
		eqp:    eq.NewProcessor(SampleRate),
		lim:    eq.NewLimiter(SampleRate),
		closed: make(chan struct{}),
	}
	e.volume.Store(math.Float64bits(1))
	e.out.lastSeq = -1
	e.posSeq.Store(-1)
	go e.worker()
	return e
}

// Events returns the event channel.
func (e *Engine) Events() <-chan Event { return e.events }

// EQ returns the equalizer processor.
func (e *Engine) EQ() *eq.Processor { return e.eqp }

func (e *Engine) emit(ev Event) {
	select {
	case e.events <- ev:
	default: // never block the audio path
	}
}

// State returns the current state.
func (e *Engine) State() State { return State(e.state.Load()) }

func (e *Engine) setState(s State) {
	if State(e.state.Swap(int32(s))) != s {
		e.emit(Event{Kind: EvState, State: s})
	}
}

// Position returns the playback position of the current track.
func (e *Engine) Position() time.Duration {
	return framesToDur(e.posFrames.Load())
}

func framesToDur(f int64) time.Duration {
	return time.Duration(f) * time.Second / SampleRate
}

func durToFrames(d time.Duration) int64 {
	return int64(d) * SampleRate / int64(time.Second)
}

// SetVolume sets the volume in [0,1].
func (e *Engine) SetVolume(v float64) {
	e.volume.Store(math.Float64bits(math.Max(0, math.Min(1, v))))
}

// Volume returns the volume.
func (e *Engine) Volume() float64 { return math.Float64frombits(e.volume.Load()) }

// SetMuted mutes or unmutes.
func (e *Engine) SetMuted(m bool) { e.muted.Store(m) }

// Muted reports the mute state.
func (e *Engine) Muted() bool { return e.muted.Load() }

func (e *Engine) interruptWorker() {
	e.mu.Lock()
	fns := e.interrupts
	e.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

func (e *Engine) send(c command) {
	select {
	case e.cmds <- c:
	case <-e.closed:
	}
}

// Load starts playing m (paused if paused is true) at pos, flushing
// anything queued. It returns the sequence number of the new track.
func (e *Engine) Load(m *Media, pos time.Duration, paused bool) int64 {
	gen := e.gen.Add(1)
	seq := e.seqCtr.Add(1)
	e.paused.Store(paused)
	e.posFrames.Store(durToFrames(pos))
	e.posSeq.Store(seq)
	e.setState(Loading)
	e.interruptWorker()
	e.send(command{kind: cmdLoad, media: m, pos: durToFrames(pos), gen: gen, seq: seq})
	return seq
}

// SetNext sets the track to continue with gaplessly (nil clears).
func (e *Engine) SetNext(m *Media) {
	e.send(command{kind: cmdSetNext, media: m})
}

// Seek moves within the current track.
func (e *Engine) Seek(pos time.Duration) {
	if pos < 0 {
		pos = 0
	}
	gen := e.gen.Add(1)
	e.posFrames.Store(durToFrames(pos))
	e.interruptWorker()
	e.send(command{kind: cmdSeek, pos: durToFrames(pos), gen: gen})
}

// Stop stops playback and releases the current track.
func (e *Engine) Stop() {
	gen := e.gen.Add(1)
	e.interruptWorker()
	e.send(command{kind: cmdStop, gen: gen})
	e.posFrames.Store(0)
	e.setState(Stopped)
}

// Pause pauses with a short fade.
func (e *Engine) Pause() {
	e.paused.Store(true)
	switch e.State() {
	case Playing, Buffering, Loading:
		e.setState(Paused)
	}
}

// Resume resumes playback.
func (e *Engine) Resume() {
	e.paused.Store(false)
	if d := e.device.Load(); d != nil {
		d.wake()
	}
	if e.State() == Paused {
		e.mu.Lock()
		w := e.waiting
		e.mu.Unlock()
		if w {
			e.setState(Buffering)
		} else {
			e.setState(Playing)
		}
	}
}

// Paused reports whether playback is paused.
func (e *Engine) Paused() bool { return e.paused.Load() }

// Close shuts the engine down.
func (e *Engine) Close() {
	e.once.Do(func() {
		e.interruptWorker()
		close(e.closed)
		if d := e.device.Swap(nil); d != nil {
			d.close()
		}
	})
}

// SetBuffering reports that the current track's data source is stalled
// (true) or flowing again (false).
func (e *Engine) SetBuffering(w bool) { e.setWaiting(w) }

// setWaiting is called when the worker blocks on (or resumes from) data.
// Buffering is only reported if the wait lasts a little while.
func (e *Engine) setWaiting(w bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waiting = w
	if e.bufTimer != nil {
		e.bufTimer.Stop()
		e.bufTimer = nil
	}
	if w {
		e.bufTimer = time.AfterFunc(200*time.Millisecond, func() {
			e.mu.Lock()
			still := e.waiting
			e.mu.Unlock()
			if still && !e.paused.Load() && e.State() == Playing {
				e.setState(Buffering)
			}
		})
	} else if e.State() == Buffering && !e.paused.Load() {
		e.state.Store(int32(Playing))
		e.emit(Event{Kind: EvState, State: Playing})
	}
}

// ---------------- worker ----------------

type prepared struct {
	token int64
	t     *track
	err   error
	media *Media
}

// workerState is owned by the worker goroutine.
type workerState struct {
	e          *Engine
	cur, next  *track
	nextMedia  *Media
	prepToken  int64
	prepCancel context.CancelFunc
	prepCh     chan prepared
	pending    *chunk
	// gen is the generation of the last load/seek/stop handled.
	gen int64
	// awaiting is set when the current track ended and playback should
	// continue with the next one once it's ready.
	awaiting bool
	endGen   int64
	endSeq   int64
	endID    string
}

func (w *workerState) setInterrupt(t *track, cancel context.CancelFunc) {
	e := w.e
	e.mu.Lock()
	e.interrupts = nil
	if cancel != nil {
		e.interrupts = append(e.interrupts, cancel)
	}
	if t != nil {
		if in, ok := t.dec.(interrupter); ok {
			e.interrupts = append(e.interrupts, in.Interrupt)
		}
	}
	e.mu.Unlock()
}

func (w *workerState) startPrepare(m *Media) {
	if w.prepCancel != nil {
		w.prepCancel()
		w.prepCancel = nil
	}
	if w.next != nil {
		w.next.close()
		w.next = nil
	}
	w.nextMedia = m
	w.prepToken++
	if m == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.prepCancel = cancel
	tok := w.prepToken
	e := w.e
	go func() {
		t, err := e.openTrack(ctx, m, 0)
		if ctx.Err() != nil && t != nil {
			t.close()
			t = nil
		}
		select {
		case w.prepCh <- prepared{token: tok, t: t, err: err, media: m}:
		case <-e.closed:
			if t != nil {
				t.close()
			}
		}
	}()
}

func (w *workerState) onPrepared(p prepared) {
	if p.token != w.prepToken {
		if p.t != nil {
			p.t.close()
		}
		return
	}
	w.prepCancel = nil
	if p.err != nil {
		if !isInterrupt(p.err) {
			w.e.emit(Event{Kind: EvError, ID: p.media.ID, Err: p.err, Open: true})
		}
		w.nextMedia = nil
		return
	}
	w.next = p.t
}

func (w *workerState) handle(c command) {
	e := w.e
	switch c.kind {
	case cmdLoad, cmdSeek, cmdStop:
		w.gen = c.gen
	}
	switch c.kind {
	case cmdStop:
		w.cur.close()
		w.cur, w.pending, w.awaiting = nil, nil, false
		w.setInterrupt(nil, nil)
		w.startPrepare(nil)
	case cmdSetNext:
		if w.nextMedia == c.media {
			return
		}
		w.startPrepare(c.media)
	case cmdLoad:
		w.cur.close()
		w.cur, w.pending, w.awaiting = nil, nil, false
		e.setWaiting(false)
		ctx, cancel := context.WithCancel(context.Background())
		w.setInterrupt(nil, cancel)
		t, err := e.openTrack(ctx, c.media, c.pos)
		superseded := ctx.Err() != nil || e.gen.Load() != c.gen
		cancel()
		if err != nil {
			if superseded {
				return // superseded
			}
			e.setState(Stopped)
			e.emit(Event{Kind: EvError, ID: c.media.ID, Err: err, Open: true})
			return
		}
		if e.gen.Load() != c.gen {
			t.close()
			return
		}
		t.seq, t.pos = c.seq, c.pos
		w.cur = t
		w.setInterrupt(w.cur, nil)
		if e.paused.Load() {
			e.setState(Paused)
		} else {
			e.setState(Playing)
		}
	case cmdSeek:
		if w.cur == nil {
			return
		}
		w.pending = nil
		cur := w.cur
		ctx, cancel := context.WithCancel(context.Background())
		w.setInterrupt(cur, cancel)
		err := e.seekTrack(ctx, cur, c.pos)
		cancel()
		if err != nil && isInterrupt(err) && e.gen.Load() == c.gen {
			// The decoder was interrupted while blocked; reopen it.
			cur.close()
			ctx2, cancel2 := context.WithCancel(context.Background())
			w.setInterrupt(nil, cancel2)
			var t *track
			t, err = e.openTrack(ctx2, cur.media, c.pos)
			cancel2()
			if err == nil {
				t.seq, t.auto = cur.seq, cur.auto
				w.cur = t
			}
		}
		if err != nil {
			if isInterrupt(err) {
				// Superseded by a newer command which will reopen/replace.
				if w.cur == cur {
					cur.close()
					w.cur = &track{media: cur.media, dec: closedDecoder{}, seq: cur.seq, auto: cur.auto, pos: c.pos}
				}
				return
			}
			e.emit(Event{Kind: EvError, ID: cur.media.ID, Err: err})
			w.cur.close()
			w.cur = nil
			w.awaiting = true
			w.endGen, w.endSeq, w.endID = e.gen.Load(), cur.seq, cur.media.ID
			w.setInterrupt(nil, nil)
			return
		}
		w.setInterrupt(w.cur, nil)
	}
}

// closedDecoder stands in for a decoder that must be reopened by a pending
// command; reading from it yields an interrupt.
type closedDecoder struct{}

func (closedDecoder) Read([]float32) (int, error) { return 0, errInterrupted }
func (closedDecoder) SampleRate() int             { return SampleRate }
func (closedDecoder) SeekFrame(int64) error       { return errInterrupted }
func (closedDecoder) Length() int64               { return -1 }
func (closedDecoder) Close() error                { return nil }

func (e *Engine) worker() {
	w := &workerState{e: e, prepCh: make(chan prepared, 4)}
	defer func() {
		w.cur.close()
		w.next.close()
		if w.prepCancel != nil {
			w.prepCancel()
		}
	}()
	for {
		// Drain commands first.
	drain:
		for {
			select {
			case c := <-e.cmds:
				w.handle(c)
			case p := <-w.prepCh:
				w.onPrepared(p)
			case <-e.closed:
				return
			default:
				break drain
			}
		}

		if w.cur == nil && w.awaiting {
			switch {
			case w.next != nil:
				nt := w.next
				w.next, w.nextMedia, w.awaiting = nil, nil, false
				nt.seq = e.seqCtr.Add(1)
				nt.auto = true
				w.cur = nt
				w.setInterrupt(nt, nil)
				e.setWaiting(false)
			case w.nextMedia == nil:
				w.awaiting = false
				e.setWaiting(false)
				w.pending = &chunk{gen: w.endGen, seq: w.endSeq, id: w.endID, end: true}
			default:
				e.setWaiting(true) // next track still loading
			}
		}

		if w.pending == nil && w.cur != nil {
			w.pending = w.produce()
		}

		if w.pending == nil {
			if w.cur != nil && !w.awaiting {
				continue // e.g. interrupted read; loop to handle commands
			}
			if w.awaiting && (w.next != nil || w.nextMedia == nil) {
				continue // track just ended; promote next / emit end marker
			}
			select {
			case c := <-e.cmds:
				w.handle(c)
			case p := <-w.prepCh:
				w.onPrepared(p)
			case <-e.closed:
				return
			}
			continue
		}

		if w.pending.gen != e.gen.Load() {
			w.pending = nil
			continue
		}
		select {
		case e.ring <- w.pending:
			w.pending = nil
		case c := <-e.cmds:
			w.handle(c)
		case p := <-w.prepCh:
			w.onPrepared(p)
		case <-e.closed:
			return
		}
	}
}

var errInterrupted = errors.New("interrupted")

func isInterrupt(err error) bool {
	return err != nil && (errors.Is(err, errInterrupted) || err.Error() == "read interrupted" || errors.Is(err, context.Canceled))
}

// produce decodes the next chunk from the current track. At EOF it marks the
// track finished so the loop can continue gaplessly with the next one.
func (w *workerState) produce() *chunk {
	e := w.e
	cur := w.cur
	gen := w.gen
	buf := make([]float32, chunkFrames*2)
	n, err := cur.read(buf)
	if n > 0 {
		c := &chunk{gen: gen, seq: cur.seq, id: cur.media.ID, auto: cur.auto, start: cur.pos, gain: gainOf(cur.media), samples: buf[:n*2]}
		cur.pos += int64(n)
		return c
	}
	if err == nil {
		return nil
	}
	if isInterrupt(err) {
		// A command is pending. Wait for it rather than spinning.
		select {
		case c := <-e.cmds:
			w.handle(c)
		case <-e.closed:
		}
		return nil
	}
	if err != io.EOF {
		e.emit(Event{Kind: EvError, ID: cur.media.ID, Err: err})
	}
	cur.close()
	w.cur = nil
	w.awaiting = true
	w.endGen, w.endSeq, w.endID = gen, cur.seq, cur.media.ID
	w.setInterrupt(nil, nil)
	return nil
}

func gainOf(m *Media) float32 {
	if m.Gain <= 0 {
		return 1
	}
	return m.Gain
}

// openTrack opens a media item and seeks to pos (output-rate frames).
func (e *Engine) openTrack(ctx context.Context, m *Media, pos int64) (*track, error) {
	dec, err := m.Open(ctx)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		dec.Close()
		return nil, ctx.Err()
	}
	if dec.SampleRate() <= 0 {
		dec.Close()
		return nil, errors.New("invalid sample rate")
	}
	t := &track{media: m, dec: dec}
	if dec.SampleRate() != SampleRate {
		t.rs = newResampler(dec, SampleRate)
	}
	if pos > 0 {
		if err := e.seekTrack(ctx, t, pos); err != nil {
			dec.Close()
			return nil, err
		}
	}
	return t, nil
}

func (e *Engine) seekTrack(ctx context.Context, t *track, pos int64) error {
	in := pos
	if t.rs != nil {
		in = int64(float64(pos) * float64(t.dec.SampleRate()) / SampleRate)
	}
	if l := t.dec.Length(); l > 0 && in >= l {
		in = l - 1
	}
	done := make(chan error, 1)
	go func() { done <- t.dec.SeekFrame(in) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		if in, ok := t.dec.(interrupter); ok {
			in.Interrupt()
		}
		<-done
		return errInterrupted
	}
	if err != nil {
		if isInterrupt(err) {
			return errInterrupted
		}
		return err
	}
	if t.rs != nil {
		t.rs = newResampler(t.dec, SampleRate)
	}
	t.pos = pos
	return nil
}

// ---------------- output ----------------

const (
	pauseFadeFrames = SampleRate * 12 / 1000
	seekFadeFrames  = SampleRate * 5 / 1000
)

// Render fills dst (interleaved stereo float32) with the next output audio.
// It never blocks. It is called by the audio device (or tests).
func (e *Engine) Render(dst []float32) {
	o := &e.out
	frames := len(dst) / 2
	paused := e.paused.Load()
	gen := e.gen.Load()

	// A seek/load flushed the ring: fade out what's left of the old chunk.
	if o.cur != nil && o.cur.gen != gen {
		n := 0
		if o.gain > 0 && !o.cur.end {
			rem := (len(o.cur.samples) - o.off) / 2
			n = min(rem, seekFadeFrames, frames)
			for i := 0; i < n; i++ {
				g := o.gain * o.cur.gain * float32(n-i) / float32(n)
				dst[2*i] = o.cur.samples[o.off+2*i] * g
				dst[2*i+1] = o.cur.samples[o.off+2*i+1] * g
			}
		}
		o.cur = nil
		o.fadeIn = seekFadeFrames
		e.post(dst[:n*2], 1)
		clear(dst[n*2:])
		e.post(dst[n*2:], 1)
		return
	}

	i := 0
	for i < frames {
		if paused && o.gain <= 0 {
			break
		}
		if o.cur == nil || o.off >= len(o.cur.samples) {
			o.cur, o.off = nil, 0
			var c *chunk
		next:
			for {
				select {
				case c = <-e.ring:
					if c.gen != gen {
						c = nil
						continue
					}
					break next
				default:
					break next
				}
			}
			if c == nil {
				break // underrun
			}
			if c.end {
				if !o.endSent {
					o.endSent = true
					e.posFrames.Store(0)
					e.setState(Stopped)
					e.emit(Event{Kind: EvEnded, ID: c.id})
				}
				continue
			}
			o.endSent = false
			o.cur = c
			if c.seq != o.lastSeq {
				o.lastSeq = c.seq
				e.posSeq.Store(c.seq)
				e.emit(Event{Kind: EvTrackStarted, ID: c.id, Seq: c.seq, Auto: c.auto})
			}
		}
		c := o.cur
		n := min((len(c.samples)-o.off)/2, frames-i)
		for k := 0; k < n; k++ {
			// pause/resume fade
			target := float32(1)
			if paused {
				target = 0
			}
			if o.gain < target {
				o.gain = min(target, o.gain+1/float32(pauseFadeFrames))
			} else if o.gain > target {
				o.gain = max(target, o.gain-1/float32(pauseFadeFrames))
			}
			g := c.gain * o.gain
			if o.fadeIn > 0 {
				g *= 1 - float32(o.fadeIn)/float32(seekFadeFrames)
				o.fadeIn--
			}
			dst[2*(i+k)] = c.samples[o.off+2*k] * g
			dst[2*(i+k)+1] = c.samples[o.off+2*k+1] * g
		}
		o.off += n * 2
		i += n
		e.posFrames.Store(c.start + int64(o.off/2))
		if paused && o.gain <= 0 {
			break
		}
	}
	clear(dst[i*2:])
	if paused && o.gain <= 0 {
		o.silentFor += frames - i
	} else {
		o.silentFor = 0
	}
	e.post(dst, 1)
}

// post applies EQ, limiter and volume.
func (e *Engine) post(buf []float32, _ int) {
	if len(buf) == 0 {
		return
	}
	e.eqp.Process(buf)
	e.lim.Process(buf)
	v := float32(e.Volume())
	if e.muted.Load() {
		v = 0
	}
	v = v * v // perceptual curve
	if v != 1 {
		for i := range buf {
			buf[i] *= v
		}
	}
}

// idle reports whether the output has been silent (paused) for a while, so
// the device can be suspended to save CPU.
func (e *Engine) idle() bool {
	return e.paused.Load() && e.out.silentFor > SampleRate
}
