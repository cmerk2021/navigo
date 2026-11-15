package audio

import (
	"io"
	"math"
)

// resampler converts a stereo Decoder's output to a different rate using a
// Kaiser-windowed sinc kernel with a polyphase table and linear
// interpolation between phases. It has zero group delay: output frame k
// corresponds exactly to input time k*inRate/outRate, and the stream is
// flushed at EOF so concatenated tracks stay length-exact (gapless).
type resampler struct {
	src     Decoder
	step    float64 // input frames per output frame
	half    int     // kernel half width (taps per side)
	phases  int
	table   []float32 // (phases+1) * 2*half
	buf     []float32 // stereo input frames; buf[0] is absolute frame base
	base    int64     // absolute input frame index of buf[0]
	t       float64   // absolute input position of the next output frame
	eof     bool
	padded  int // zero frames appended after EOF
	readBuf []float32
	err     error
}

const resamplerPhases = 256

func newResampler(src Decoder, outRate int) *resampler {
	inRate := src.SampleRate()
	r := &resampler{src: src, step: float64(inRate) / float64(outRate), phases: resamplerPhases}
	cut := 1.0
	if outRate < inRate {
		cut = float64(outRate) / float64(inRate)
	}
	cut *= 0.97
	r.half = int(math.Ceil(16 / cut))
	taps := 2 * r.half
	r.table = make([]float32, (r.phases+1)*taps)
	const beta = 8.6
	i0b := besselI0(beta)
	for p := 0; p <= r.phases; p++ {
		frac := float64(p) / float64(r.phases)
		for k := 0; k < taps; k++ {
			// tap k multiplies input frame floor(t) - half + 1 + k
			x := float64(k-r.half+1) - frac
			v := cut * sinc(x*cut)
			w := x / float64(r.half)
			if w <= -1 || w >= 1 {
				v = 0
			} else {
				v *= besselI0(beta*math.Sqrt(1-w*w)) / i0b
			}
			r.table[p*taps+k] = float32(v)
		}
	}
	// Left context: half zero frames before input frame 0.
	r.buf = make([]float32, 2*r.half)
	r.base = -int64(r.half)
	r.readBuf = make([]float32, 2048)
	return r
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 50; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}

func (r *resampler) SampleRate() int { return 0 } // not used
func (r *resampler) frames() int64   { return int64(len(r.buf) / 2) }

// fill reads more input; returns false when no more input can be obtained.
func (r *resampler) fill() bool {
	if r.eof {
		if r.padded >= r.half+1 {
			return false
		}
		r.buf = append(r.buf, make([]float32, 2*(r.half+1))...)
		r.padded += r.half + 1
		return true
	}
	n, err := r.src.Read(r.readBuf)
	if n > 0 {
		r.buf = append(r.buf, r.readBuf[:2*n]...)
	}
	if err == io.EOF {
		r.eof = true
		// The total number of real input frames determines the output length.
		return true
	}
	if err != nil {
		r.err = err
		return n > 0
	}
	return true
}

// Read produces output frames into dst.
func (r *resampler) Read(dst []float32) (int, error) {
	taps := 2 * r.half
	out := 0
	want := len(dst) / 2
	for out < want {
		i := int64(math.Floor(r.t))
		// End of stream: stop once t passes the last real input frame.
		if r.eof {
			realEnd := r.base + r.frames() - int64(r.padded)
			if r.t >= float64(realEnd) {
				break
			}
		}
		need := i + int64(r.half) // last input frame index required
		if need >= r.base+r.frames() {
			if !r.fill() {
				break
			}
			continue
		}
		frac := r.t - float64(i)
		pf := frac * float64(r.phases)
		p := int(pf)
		a := float32(pf - float64(p))
		c0 := r.table[p*taps : (p+1)*taps]
		c1 := r.table[(p+1)*taps : (p+2)*taps]
		start := int(i-int64(r.half)+1-r.base) * 2
		var sl, sr float32
		for k := 0; k < taps; k++ {
			c := c0[k] + a*(c1[k]-c0[k])
			sl += c * r.buf[start+2*k]
			sr += c * r.buf[start+2*k+1]
		}
		dst[2*out], dst[2*out+1] = sl, sr
		out++
		r.t += r.step
		// Drop input no longer needed (keep half frames of history).
		drop := int64(math.Floor(r.t)) - int64(r.half) + 1 - r.base
		if drop > 4096 {
			copy(r.buf, r.buf[drop*2:])
			r.buf = r.buf[:len(r.buf)-int(drop*2)]
			r.base += drop
		}
	}
	if out == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	return out, nil
}
