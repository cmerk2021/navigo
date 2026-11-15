package audio

import (
	"bytes"
	"context"
	"io"
	"time"
)

// DecodeSection decodes [start, start+length) of a media item into memory at
// the output sample rate.
func DecodeSection(ctx context.Context, m *Media, start, length time.Duration) ([]float32, error) {
	e := &Engine{}
	t, err := e.openTrack(ctx, m, durToFrames(start))
	if err != nil {
		return nil, err
	}
	defer t.close()
	want := int(durToFrames(length))
	out := make([]float32, 0, want*2)
	buf := make([]float32, chunkFrames*2)
	for len(out) < want*2 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n, err := t.read(buf)
		out = append(out, buf[:n*2]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(out) > want*2 {
		out = out[:want*2]
	}
	return out, nil
}

// LoopMedia returns a media item that plays pcm (stereo, output rate) in an
// endless seamless loop. The seam is hidden by crossfading the tail into the
// head over fade.
func LoopMedia(id string, pcm []float32, fade time.Duration) *Media {
	frames := len(pcm) / 2
	f := int(durToFrames(fade))
	if f*3 > frames {
		f = frames / 3
	}
	loopLen := frames - f
	loop := make([]float32, loopLen*2)
	copy(loop, pcm[:loopLen*2])
	for i := 0; i < f; i++ {
		w := float32(i) / float32(f)
		for c := 0; c < 2; c++ {
			head := pcm[2*i+c]
			tail := pcm[2*(loopLen+i)+c]
			loop[2*i+c] = head*w + tail*(1-w)
		}
	}
	return &Media{
		ID: id,
		Open: func(context.Context) (Decoder, error) {
			return &loopDec{pcm: loop}, nil
		},
	}
}

type loopDec struct {
	pcm []float32
	pos int
}

func (l *loopDec) Read(dst []float32) (int, error) {
	if len(l.pcm) == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(dst)/2 {
		k := copy(dst[2*n:], l.pcm[l.pos:])
		k /= 2
		n += k
		l.pos += 2 * k
		if l.pos >= len(l.pcm) {
			l.pos = 0
		}
	}
	return n, nil
}

func (l *loopDec) SampleRate() int { return SampleRate }
func (l *loopDec) SeekFrame(frame int64) error {
	if len(l.pcm) > 0 {
		l.pos = int(frame*2) % len(l.pcm)
	}
	return nil
}
func (l *loopDec) Length() int64 { return -1 }
func (l *loopDec) Close() error  { return nil }

// FromSource builds a Media whose decoder reads from the Source returned by
// open. Blocking reads are aborted when ctx is cancelled. If the source has a
// SetWaitHook method, onWait is installed once the decoder is open, so that
// stalls during playback (not while pre-opening) are reported.
func FromSource(id, hint string, rg ReplayGain, open func(ctx context.Context) (Source, error), onWait func(bool)) *Media {
	return &Media{
		ID:         id,
		ReplayGain: rg,
		Open: func(ctx context.Context) (Decoder, error) {
			src, err := open(ctx)
			if err != nil {
				return nil, err
			}
			stop := context.AfterFunc(ctx, src.Interrupt)
			dec, err := Open(src, hint)
			stop()
			if err != nil {
				src.Close()
				return nil, err
			}
			if h, ok := src.(interface{ SetWaitHook(func(bool)) }); ok && onWait != nil {
				h.SetWaitHook(onWait)
			}
			return dec, nil
		},
	}
}

// MemSource is an in-memory Source (used for tests and previews).
type MemSource struct {
	*bytes.Reader
}

// NewMemSource wraps b.
func NewMemSource(b []byte) *MemSource { return &MemSource{bytes.NewReader(b)} }

// Complete implements Source.
func (*MemSource) Complete() bool { return true }

// WaitComplete implements Source.
func (*MemSource) WaitComplete(context.Context) error { return nil }

// Interrupt implements Source.
func (*MemSource) Interrupt() {}

// Close implements Source.
func (*MemSource) Close() error { return nil }
