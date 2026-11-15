package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// makeWAV creates a 16-bit stereo PCM WAV of f(t) for the given frames.
func makeWAV(rate, frames int, f func(i int) float64) []byte {
	var b bytes.Buffer
	data := frames * 4
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+data))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*4))
	binary.Write(&b, binary.LittleEndian, uint16(4))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(data))
	for i := 0; i < frames; i++ {
		v := int16(math.Round(f(i) * 32767))
		binary.Write(&b, binary.LittleEndian, v)
		binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

func wavMedia(id string, data []byte) *Media {
	return FromSource(id, "wav", ReplayGain{}, func(context.Context) (Source, error) {
		return NewMemSource(data), nil
	}, nil)
}

// renderAll drives the output stage until playback ends, waiting for the
// worker instead of rendering underruns (which would be timing artefacts of
// the test, not gaps in the stream).
func renderAll(t *testing.T, e *Engine, maxFrames int) ([]float32, []Event) {
	t.Helper()
	var out []float32
	var events []Event
	buf := make([]float32, 256*2)
	deadline := time.Now().Add(20 * time.Second)
	ended := false
	for !ended && len(out)/2 < maxFrames {
		if time.Now().After(deadline) {
			t.Fatal("timeout rendering")
		}
		o := &e.out
		if (o.cur == nil || o.off >= len(o.cur.samples)) && len(e.ring) == 0 {
			select {
			case ev := <-e.events:
				events = append(events, ev)
				if ev.Kind == EvEnded {
					ended = true
				}
			case <-time.After(time.Millisecond):
			}
			continue
		}
		// Never render past what is known to be buffered: a Render call that
		// crosses into an empty ring would produce an underrun.
		n := 1
		if o.cur != nil && o.off < len(o.cur.samples) {
			n = min((len(o.cur.samples)-o.off)/2, len(buf)/2)
		}
		e.Render(buf[:2*n])
		out = append(out, buf[:2*n]...)
	drain:
		for {
			select {
			case ev := <-e.events:
				events = append(events, ev)
				if ev.Kind == EvEnded {
					ended = true
				}
			default:
				break drain
			}
		}
	}
	return out, events
}

func TestGaplessNoZeroGap(t *testing.T) {
	const frames = 22050 + 333 // deliberately not a multiple of the chunk size
	sig := func(i int) float64 { return 0.3 + 0.2*math.Sin(2*math.Pi*440*float64(i)/SampleRate) }
	a := wavMedia("a", makeWAV(SampleRate, frames, sig))
	b := wavMedia("b", makeWAV(SampleRate, frames, func(i int) float64 { return sig(i + frames) }))

	e := New()
	defer e.Close()
	e.Load(a, 0, false)
	e.SetNext(b)
	out, events := renderAll(t, e, frames*4)

	// Find the audible region and verify there is no silence inside it.
	first, last := -1, -1
	for i := 0; i < len(out)/2; i++ {
		if math.Abs(float64(out[2*i])) > 1e-3 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatal("no audio rendered")
	}
	for i := first + pauseFadeFrames; i <= last; i++ {
		if math.Abs(float64(out[2*i])) < 0.05 {
			t.Fatalf("gap/dropout at frame %d (value %v)", i, out[2*i])
		}
	}
	// The pause/resume fade-in affects only the very beginning.
	if got := last - first + 1; got < 2*frames-SampleRate/50 || got > 2*frames {
		t.Fatalf("audible length %d, want ~%d", got, 2*frames)
	}
	// Continuity across the boundary: the signal is one continuous waveform.
	boundary := first + frames
	for i := boundary - 3; i < boundary+3; i++ {
		d := math.Abs(float64(out[2*(i+1)] - out[2*i]))
		if d > 0.01 {
			t.Fatalf("discontinuity %.4f at boundary frame %d", d, i)
		}
	}
	var started []Event
	for _, ev := range events {
		if ev.Kind == EvTrackStarted {
			started = append(started, ev)
		}
	}
	if len(started) != 2 || started[0].ID != "a" || started[1].ID != "b" || !started[1].Auto {
		t.Fatalf("unexpected track events: %+v", started)
	}
}

func TestSeekAndPause(t *testing.T) {
	const frames = SampleRate * 3
	m := wavMedia("s", makeWAV(SampleRate, frames, func(i int) float64 { return float64(i%1000) / 2000 }))
	e := New()
	defer e.Close()
	e.Load(m, 0, false)
	buf := make([]float32, 512)
	waitRing := func() {
		for i := 0; i < 2000 && len(e.ring) == 0; i++ {
			time.Sleep(time.Millisecond)
		}
	}
	waitRing()
	e.Render(buf)
	e.Seek(2 * time.Second)
	if p := e.Position(); p != 2*time.Second {
		t.Fatalf("position after seek = %v", p)
	}
	e.Render(buf) // fade-out of the stale chunk
	waitRing()
	e.Render(buf)
	if p := e.Position(); p < 2*time.Second || p > 2*time.Second+50*time.Millisecond {
		t.Fatalf("position after rendering = %v", p)
	}
	e.Pause()
	for i := 0; i < 10; i++ {
		waitRing()
		e.Render(buf)
	}
	pos := e.Position()
	e.Render(buf)
	for _, v := range buf {
		if v != 0 {
			t.Fatal("output not silent while paused")
		}
	}
	if e.Position() != pos {
		t.Fatal("position advanced while paused")
	}
}

func TestResamplerAccuracy(t *testing.T) {
	const in = 48000
	frames := in
	sig := func(i int) float64 { return 0.5 * math.Sin(2*math.Pi*1000*float64(i)/in) }
	dec, err := Open(NewMemSource(makeWAV(in, frames, sig)), "wav")
	if err != nil {
		t.Fatal(err)
	}
	rs := newResampler(dec, SampleRate)
	var out []float32
	buf := make([]float32, 1000)
	for {
		n, err := rs.Read(buf)
		out = append(out, buf[:n*2]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	want := int(math.Ceil(float64(frames) * SampleRate / in))
	if got := len(out) / 2; got < want-1 || got > want+1 {
		t.Fatalf("length %d want %d", got, want)
	}
	var maxErr float64
	for k := 1000; k < len(out)/2-1000; k++ {
		ideal := 0.5 * math.Sin(2*math.Pi*1000*float64(k)/SampleRate)
		maxErr = math.Max(maxErr, math.Abs(float64(out[2*k])-ideal))
	}
	if maxErr > 2e-3 {
		t.Fatalf("max error %v", maxErr)
	}
}

func TestLoopMediaSeamless(t *testing.T) {
	pcm := make([]float32, SampleRate*2*2)
	for i := 0; i < len(pcm)/2; i++ {
		v := float32(0.4 * math.Sin(2*math.Pi*220*float64(i)/SampleRate))
		pcm[2*i], pcm[2*i+1] = v, v
	}
	m := LoopMedia("loop", pcm, 50*time.Millisecond)
	d, _ := m.Open(context.Background())
	buf := make([]float32, SampleRate*2*3)
	n, _ := d.Read(buf)
	if n != SampleRate*3 {
		t.Fatalf("read %d", n)
	}
	maxStep := 2 * math.Pi * 220 / SampleRate * 0.4 * 1.5
	for i := 1; i < n; i++ {
		if math.Abs(float64(buf[2*i]-buf[2*i-2])) > maxStep {
			t.Fatalf("seam discontinuity at %d", i)
		}
	}
}

// TestDecoders checks real codecs against ffmpeg-generated fixtures.
func TestDecoders(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	cases := []struct{ ext, codec string }{
		{"flac", "flac"}, {"mp3", "libmp3lame"}, {"ogg", "libvorbis"}, {"wav", "pcm_s16le"},
	}
	for _, c := range cases {
		t.Run(c.ext, func(t *testing.T) {
			path := filepath.Join(dir, "t."+c.ext)
			cmd := exec.Command(ff, "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=4:sample_rate=44100",
				"-ac", "2", "-c:a", c.codec, path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("ffmpeg %s: %v %s", c.ext, err, out)
			}
			data, _ := os.ReadFile(path)
			dec, err := Open(NewMemSource(data), c.ext)
			if err != nil {
				t.Fatal(err)
			}
			if dec.SampleRate() != 44100 {
				t.Fatalf("rate %d", dec.SampleRate())
			}
			all := readAll(t, dec)
			if n := len(all) / 2; n < 44100*39/10 || n > 44100*42/10 {
				t.Fatalf("decoded %d frames", n)
			}
			target := int64(44100 * 2)
			start := time.Now()
			if err := dec.SeekFrame(target); err != nil {
				t.Fatal(err)
			}
			if el := time.Since(start); el > 500*time.Millisecond {
				t.Fatalf("seek too slow: %v", el)
			}
			buf := make([]float32, 2048)
			n, err := dec.Read(buf)
			if err != nil || n == 0 {
				t.Fatalf("read after seek: %d %v", n, err)
			}
			// Compare against the straight-through decode.
			tol := 1e-4
			if c.ext == "mp3" || c.ext == "ogg" {
				tol = 0.05 // lossy decoders restart mid-stream
			}
			off := int(target)
			var maxDiff float64
			for i := 200; i < n && off+i < len(all)/2; i++ {
				maxDiff = math.Max(maxDiff, math.Abs(float64(buf[2*i]-all[2*(off+i)])))
			}
			if maxDiff > tol {
				t.Fatalf("seek mismatch %.4f", maxDiff)
			}
		})
	}
}

func readAll(t *testing.T, d Decoder) []float32 {
	var out []float32
	buf := make([]float32, 4096)
	for {
		n, err := d.Read(buf)
		out = append(out, buf[:n*2]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWAVMalformedHeader(t *testing.T) {
	// "fmt " chunk whose size field contains the bytes "data" (a broken
	// encoder) must be rejected without allocating ~1.6 GB.
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt data\x00\x00\x00\x00")
	if _, err := Open(NewMemSource(b), "wav"); err == nil {
		t.Fatal("expected error for malformed wav")
	}
}
