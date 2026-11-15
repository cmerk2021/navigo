package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
)

type device struct {
	e      *Engine
	ctx    *oto.Context
	mu     sync.Mutex
	player *oto.Player
	susp   bool
	stop   chan struct{}
	fbuf   []float32
}

// StartDevice opens the audio output. oto supports only one context per
// process, so if this fails the app must be restarted to retry the context;
// player-level errors are retried automatically. Because of that, it first
// waits (up to soundServerWait) for the PulseAudio/PipeWire socket, which may
// not exist yet when Navigo is autostarted at login. It may block; callers
// should run it in a goroutine.
func (e *Engine) StartDevice() error {
	waitSoundServer(soundServerWait)
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      SampleRate,
		ChannelCount:    2,
		Format:          oto.FormatFloat32LE,
		BufferSize:      60 * time.Millisecond,
		ApplicationName: "Navigo",
	})
	if err != nil {
		return fmt.Errorf("open audio device: %w", err)
	}
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("open audio device: timed out")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("open audio device: %w", err)
	}
	d := &device{e: e, ctx: ctx, stop: make(chan struct{})}
	d.newPlayer()
	e.device.Store(d)
	select {
	case <-e.closed:
		if e.device.CompareAndSwap(d, nil) {
			d.close()
		}
		return nil
	default:
	}
	go d.monitor()
	return nil
}

const soundServerWait = 15 * time.Second

// waitSoundServer waits until the PulseAudio-compatible socket exists. It
// returns immediately when there is no session runtime directory or a
// PULSE_SERVER override (oto then decides by itself, falling back to ALSA).
func waitSoundServer(max time.Duration) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || os.Getenv("PULSE_SERVER") != "" {
		return
	}
	sock := filepath.Join(dir, "pulse", "native")
	for deadline := time.Now().Add(max); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if _, err := os.Stat(sock); err == nil {
			return
		}
	}
}

func (d *device) newPlayer() {
	p := d.ctx.NewPlayer(d)
	p.SetBufferSize(2048 * 8) // ~46 ms
	p.Play()
	d.mu.Lock()
	old := d.player
	d.player = p
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// Read implements io.Reader for oto.
func (d *device) Read(p []byte) (int, error) {
	frames := len(p) / 8
	if frames == 0 {
		return 0, nil
	}
	if cap(d.fbuf) < frames*2 {
		d.fbuf = make([]float32, frames*2)
	}
	buf := d.fbuf[:frames*2]
	d.e.Render(buf)
	for i, v := range buf {
		binary.LittleEndian.PutUint32(p[4*i:], math.Float32bits(v))
	}
	return frames * 8, nil
}

func (d *device) wake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.susp {
		d.susp = false
		d.ctx.Resume()
	}
}

func (d *device) monitor() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
		}
		d.mu.Lock()
		p := d.player
		idle := d.e.idle()
		if idle && !d.susp {
			d.susp = true
			d.ctx.Suspend()
		}
		d.mu.Unlock()
		if err := p.Err(); err != nil {
			failures++
			d.e.emit(Event{Kind: EvDeviceError, Err: err})
			time.Sleep(time.Duration(min(failures, 10)) * time.Second)
			d.newPlayer()
		} else {
			failures = 0
		}
	}
}

func (d *device) close() {
	close(d.stop)
	d.mu.Lock()
	if d.player != nil {
		d.player.Pause()
		d.player.Close()
	}
	d.mu.Unlock()
}
