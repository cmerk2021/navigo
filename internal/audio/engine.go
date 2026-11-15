// Package audio implements Navigo's playback engine: decoding, resampling,
// gapless track transitions, EQ/limiter processing and output through oto.
package audio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
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

// State is the playback state. It reflects what is audible: Playing is only
// entered once samples of the current track reach the output.
type State int32

// Playback states.
const (
	Stopped State = iota
	// Loading: a track was selected and is being opened/pre-buffered.
	Loading
	// Playing: audio of the current track is being output.
	Playing
	// Paused by the user.
	Paused
	// Buffering: playback ran out of data mid-track (or the gapless
	// successor isn't ready) and resumes automatically.
	Buffering
	// Error: the current track failed to load.
	Error
)

func (s State) String() string {
	return [...]string{"Stopped", "Loading", "Playing", "Paused", "Buffering", "Error"}[s]
}

// Waiting reports whether the state means "play requested, waiting for
// audio" (the UI shows a spinner).
func (s State) Waiting() bool { return s == Loading || s == Buffering }

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

// Event is published to the UI/controller. Every event is tagged with the
// track it belongs to (ID is the Media ID, Seq the engine's sequence number
// of that track instance) so that listeners can drop events of tracks that
// are no longer current.
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
	// ReplayGain holds server-reported values; values missing here are
	// taken from the file's tags.
	ReplayGain ReplayGain
	// Open returns a decoder positioned at the start. It may block until data
	// is available and must honour ctx cancellation.
	Open func(ctx context.Context) (Decoder, error)
	// Duration is the expected length (0 if unknown). Decode errors within
	// the last moments of a track are treated as its end.
	Duration time.Duration
	// Urgent, if set, is called when playback is waiting for this (prepared
	// next) track, so its download can be prioritised.
	Urgent func()
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
	media   *Media
	dec     Decoder
	rs      *resampler // nil if no resampling needed
	seq     int64
	pos     int64 // frames produced (output rate)
	auto    bool
	rg      ReplayGain // server values merged with file tags
	started bool       // produced samples since opening
}

// nearEnd reports whether the track is at (or within a moment of) its end,
// in which case a decode error is treated as the normal end of the track:
// trailing tags/garbage or a truncated last frame shouldn't be reported as
// a playback error (or surface at the start of the next track).
func (t *track) nearEnd() bool {
	if l := t.dec.Length(); l > 0 {
		out := l
		if r := t.dec.SampleRate(); r > 0 && r != SampleRate {
			out = l * SampleRate / int64(r)
		}
		if t.pos >= out-SampleRate/2 {
			return true
		}
	}
	if d := t.media.Duration; d > 0 && t.pos >= durToFrames(d-1500*time.Millisecond) {
		return true
	}
	if ae, ok := t.dec.(interface{ atEnd() bool }); ok && ae.atEnd() {
		return true
	}
	return false
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
	rgMode atomic.Int32 // ReplayGainMode

	// position of the audio currently heard
	posFrames atomic.Int64
	posSeq    atomic.Int64
	// startedSeq is the seq of the last track whose samples were output.
	startedSeq atomic.Int64
	// curSeq is the seq of the worker's current track.
	curSeq atomic.Int64
	// waiting is set while the current track's source has no data (or the
	// gapless successor is not ready yet).
	waiting atomic.Bool
	// active is set while a track is playing or about to continue, i.e.
	// an empty ring means an underrun rather than the end.
	active atomic.Bool

	mu         sync.Mutex
	interrupts []func() // aborts blocking work in the worker

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
	fadeIn    int     // frames of seek/resume fade-in remaining
	fadeLen   int     // length of the current fade-in
	fadeOut   bool    // fade the current chunk out (the source is starving)
	silentFor int     // frames of silence output while paused
	buf       []float32
	endSent   bool
	// underrun counts frames output without data while playing.
	underrun int
	// starved is set after a starvation fade-out: the next underrun enters
	// Buffering immediately.
	starved bool
	// rebuffer holds output after an underrun until the ring has refilled.
	rebuffer     bool
	rebufferWait int
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
	e.startedSeq.Store(-1)
	e.curSeq.Store(-1)
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
	if old := State(e.state.Swap(int32(s))); old != s {
		slog.Debug("audio: state", "from", old, "to", s, "seq", e.posSeq.Load())
		e.emit(Event{Kind: EvState, State: s, Seq: e.posSeq.Load()})
	}
}

// casState changes the state from one of the given states only.
func (e *Engine) casState(to State, from ...State) bool {
	for _, f := range from {
		if e.state.CompareAndSwap(int32(f), int32(to)) {
			slog.Debug("audio: state", "from", f, "to", to, "seq", e.posSeq.Load())
			e.emit(Event{Kind: EvState, State: to, Seq: e.posSeq.Load()})
			return true
		}
	}
	return false
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

// SetReplayGainMode selects the ReplayGain value applied to decoded audio.
// The change takes effect for audio decoded from now on.
func (e *Engine) SetReplayGainMode(m ReplayGainMode) { e.rgMode.Store(int32(m)) }

// ReplayGainMode returns the ReplayGain mode.
func (e *Engine) ReplayGainMode() ReplayGainMode { return ReplayGainMode(e.rgMode.Load()) }

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
	e.active.Store(false)
	e.waiting.Store(false)
	slog.Debug("audio: track requested", "id", m.ID, "seq", seq, "pos", pos, "paused", paused)
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
	e.active.Store(false)
	e.waiting.Store(false)
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
	// Playing is entered by the output once audio is actually delivered.
	switch {
	case e.State() != Paused:
	case e.waiting.Load():
		e.casState(Buffering, Paused)
	case e.startedSeq.Load() != e.posSeq.Load():
		e.casState(Loading, Paused)
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
// (true) or flowing again (false). Sources that implement SetWaitHook are
// watched automatically.
func (e *Engine) SetBuffering(w bool) { e.setWaiting(w) }

// setWaiting records whether the current track is starved of data. The
// output stage turns a resulting underrun into the Buffering state.
func (e *Engine) setWaiting(w bool) {
	if e.waiting.Swap(w) != w {
		slog.Debug("audio: source waiting", "waiting", w, "seq", e.curSeq.Load())
		if !w {
			if d := e.device.Load(); d != nil {
				d.wake()
			}
		}
	}
}

// watch installs a wait hook on t's source that reports stalls while t is
// the current track (stale tracks can't affect the state).
func (e *Engine) watch(t *track) {
	e.curSeq.Store(t.seq)
	if h, ok := t.dec.(waitHooker); ok {
		seq := t.seq
		h.SetWaitHook(func(w bool) {
			if e.curSeq.Load() == seq {
				e.setWaiting(w)
			}
		})
	}
}

type waitHooker interface{ SetWaitHook(func(bool)) }

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
		if !isTeardown(p.err) {
			w.e.emit(Event{Kind: EvError, ID: p.media.ID, Seq: -1, Err: p.err, Open: true})
		} else {
			slog.Debug("audio: prepare cancelled", "id", p.media.ID, "err", p.err)
		}
		w.nextMedia = nil
		return
	}
	w.next = p.t
	slog.Debug("audio: next track ready", "id", p.media.ID)
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
		e.curSeq.Store(-1)
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
		e.curSeq.Store(c.seq)
		ctx, cancel := context.WithCancel(context.Background())
		w.setInterrupt(nil, cancel)
		t, err := e.openTrack(ctx, c.media, c.pos)
		superseded := ctx.Err() != nil || e.gen.Load() != c.gen
		cancel()
		if err != nil {
			if superseded || isTeardown(err) {
				slog.Debug("audio: load superseded", "id", c.media.ID, "seq", c.seq, "err", err)
				return
			}
			slog.Debug("audio: open failed", "id", c.media.ID, "seq", c.seq, "err", err)
			e.setState(Error)
			e.emit(Event{Kind: EvError, ID: c.media.ID, Seq: c.seq, Err: err, Open: true})
			return
		}
		if e.gen.Load() != c.gen {
			t.close()
			return
		}
		t.seq, t.pos = c.seq, c.pos
		w.cur = t
		w.setInterrupt(w.cur, nil)
		e.watch(t)
		e.active.Store(true)
		slog.Debug("audio: decoder opened", "id", c.media.ID, "seq", c.seq, "rate", t.dec.SampleRate())
		if e.paused.Load() {
			e.casState(Paused, Loading)
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
				e.watch(t)
			}
		}
		if err != nil {
			if isInterrupt(err) {
				// Superseded by a newer command which will reopen/replace.
				if w.cur == cur {
					cur.close()
					w.cur = &track{media: cur.media, dec: closedDecoder{}, seq: cur.seq, auto: cur.auto, pos: c.pos, rg: cur.rg}
				}
				return
			}
			e.emit(Event{Kind: EvError, ID: cur.media.ID, Seq: cur.seq, Err: err})
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
				e.watch(nt)
				e.setWaiting(false)
				slog.Debug("audio: gapless transition", "id", nt.media.ID, "seq", nt.seq)
			case w.nextMedia == nil:
				w.awaiting = false
				e.setWaiting(false)
				e.active.Store(false)
				w.pending = &chunk{gen: w.endGen, seq: w.endSeq, id: w.endID, end: true}
			default:
				// The successor isn't ready: the output underruns into
				// Buffering instead of pretending to play.
				if !e.waiting.Load() {
					slog.Debug("audio: waiting for the next track", "id", w.nextMedia.ID)
					if w.nextMedia.Urgent != nil {
						go w.nextMedia.Urgent()
					}
				}
				e.setWaiting(true)
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

// IsTeardown reports whether err comes from intentionally stopping a track
// (see isTeardown).
func IsTeardown(err error) bool { return isTeardown(err) }

// isTeardown reports errors caused by intentionally stopping/closing a
// track (cancelled contexts, interrupted or closed sources). They are part
// of the normal track lifecycle, never playback errors.
func isTeardown(err error) bool {
	if err == nil {
		return false
	}
	if isInterrupt(err) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "file already closed") || strings.Contains(msg, "use of closed")
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
		c := &chunk{gen: gen, seq: cur.seq, id: cur.media.ID, auto: cur.auto, start: cur.pos, gain: cur.rg.Factor(e.ReplayGainMode()), samples: buf[:n*2]}
		cur.pos += int64(n)
		cur.started = true
		return c
	}
	if err == nil {
		return nil
	}
	if isTeardown(err) {
		// A command is pending. Wait for it rather than spinning.
		for {
			select {
			case c := <-e.cmds:
				w.handle(c)
			case p := <-w.prepCh:
				w.onPrepared(p)
				continue
			case <-e.closed:
			}
			return nil
		}
	}
	switch {
	case err == io.EOF:
		slog.Debug("audio: track finished", "id", cur.media.ID, "seq", cur.seq)
	case cur.nearEnd():
		// Trailing junk/tags or a truncated last frame: a normal end.
		slog.Debug("audio: decode error at end of track treated as EOF", "id", cur.media.ID, "seq", cur.seq, "err", err)
	default:
		slog.Debug("audio: decode error", "id", cur.media.ID, "seq", cur.seq, "err", err)
		// Failing before producing any audio is an open failure (the
		// controller may re-fetch or transcode).
		e.emit(Event{Kind: EvError, ID: cur.media.ID, Seq: cur.seq, Err: err, Open: !cur.started && !cur.auto})
	}
	cur.close()
	w.cur = nil
	w.awaiting = true
	w.endGen, w.endSeq, w.endID = gen, cur.seq, cur.media.ID
	w.setInterrupt(nil, nil)
	return nil
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
	t := &track{media: m, dec: dec, rg: m.ReplayGain}
	if tg, ok := dec.(tagged); ok {
		t.rg = t.rg.Merge(tg.ReplayGain())
	}
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
		o.cur, o.fadeOut = nil, false
		o.fadeIn, o.fadeLen = seekFadeFrames, seekFadeFrames
		e.post(dst[:n*2], 1)
		clear(dst[n*2:])
		e.post(dst[n*2:], 1)
		return
	}

	if o.rebuffer {
		o.rebufferWait += frames
		if paused || !e.readyToResume(o.rebufferWait) {
			clear(dst)
			e.post(dst, 1)
			if paused && o.gain > 0 {
				o.gain = 0 // nothing was audible; resume with a fade-in
			}
			return
		}
		slog.Debug("audio: buffer refilled, resuming output", "seq", e.posSeq.Load())
		o.rebuffer, o.rebufferWait = false, 0
		o.fadeIn, o.fadeLen = pauseFadeFrames, pauseFadeFrames
	}

	i := 0
	for i < frames {
		if paused && o.gain <= 0 {
			break
		}
		if o.cur == nil || o.off >= len(o.cur.samples) {
			o.cur, o.off, o.fadeOut = nil, 0, false
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
					e.emit(Event{Kind: EvEnded, ID: c.id, Seq: c.seq})
				}
				continue
			}
			o.endSent = false
			o.cur = c
			if len(e.ring) == 0 && e.waiting.Load() && !paused {
				// The source is starving: fade this chunk out instead of
				// cutting off abruptly at the underrun.
				o.fadeOut, o.starved = true, true
			}
			if c.seq != o.lastSeq {
				o.lastSeq = c.seq
				e.posSeq.Store(c.seq)
				e.emit(Event{Kind: EvTrackStarted, ID: c.id, Seq: c.seq, Auto: c.auto})
			}
		}
		c := o.cur
		n := min((len(c.samples)-o.off)/2, frames-i)
		total := len(c.samples) / 2
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
				g *= 1 - float32(o.fadeIn)/float32(max(o.fadeLen, 1))
				o.fadeIn--
			}
			if o.fadeOut {
				g *= float32(total-(o.off/2+k)-1) / float32(total)
			}
			dst[2*(i+k)] = c.samples[o.off+2*k] * g
			dst[2*(i+k)+1] = c.samples[o.off+2*k+1] * g
		}
		o.off += n * 2
		i += n
		e.posFrames.Store(c.start + int64(o.off/2))
		if n > 0 && !paused && c.seq == e.posSeq.Load() {
			e.markPlaying(c)
		}
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
	if i < frames && !paused && e.active.Load() {
		// Out of data mid-track. Short hiccups are tolerated; a real
		// underrun (or one announced by a starvation fade) holds the
		// output until the buffer has refilled.
		o.underrun += frames - i
		if o.starved || o.underrun >= underrunGrace {
			o.starved, o.underrun = false, 0
			o.rebuffer, o.rebufferWait = true, 0
			slog.Debug("audio: underrun", "seq", e.posSeq.Load(), "waiting", e.waiting.Load())
			e.casState(Buffering, Playing)
		}
	} else if i == frames {
		o.underrun = 0
	}
	e.post(dst, 1)
}

// underrunGrace is how long the output may run dry before Buffering.
const underrunGrace = SampleRate / 10

// readyToResume reports whether output can continue after an underrun: the
// ring is (nearly) full, or the source isn't waiting and some data has been
// queued for a moment (e.g. the short tail of a track).
func (e *Engine) readyToResume(waited int) bool {
	n := len(e.ring)
	if n >= ringChunks-1 {
		return true
	}
	if !e.active.Load() {
		return true // ended or stopped: let the end marker through
	}
	return n > 0 && !e.waiting.Load() && waited >= SampleRate/20
}

// markPlaying switches to Playing when audio of the current track reaches
// the output (called from the output stage).
func (e *Engine) markPlaying(c *chunk) {
	if e.startedSeq.Swap(c.seq) != c.seq {
		slog.Debug("audio: first samples output", "id", c.id, "seq", c.seq)
	}
	if e.State() == Playing {
		return
	}
	if e.casState(Playing, Loading, Buffering, Paused) && e.paused.Load() {
		// Raced with Pause.
		e.casState(Paused, Playing)
	}
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
