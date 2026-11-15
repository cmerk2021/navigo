package eq

import (
	"math"
	"math/rand"
	"testing"
)

const fs = 48000

func sine(frames int, freq, amp float64) []float32 {
	buf := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(amp * math.Sin(2*math.Pi*freq*float64(i)/fs))
		buf[2*i], buf[2*i+1] = v, v
	}
	return buf
}

func TestResponseFlat(t *testing.T) {
	var flat [NumBands]float64
	for i, db := range Response(flat, 0, fs, LogFreqs(200, 20, 20000)) {
		if math.Abs(db) > 1e-9 {
			t.Fatalf("flat response %d = %v dB", i, db)
		}
	}
}

func TestResponseBandCenter(t *testing.T) {
	for i, f := range Frequencies {
		var b [NumBands]float64
		b[i] = 6
		got := Response(b, 0, fs, []float64{f})[0]
		if math.Abs(got-6) > 0.5 {
			t.Errorf("band %v Hz: got %.2f dB, want ~6", f, got)
		}
	}
}

func TestAutoPreamp(t *testing.T) {
	var flat [NumBands]float64
	if got := AutoPreampFor(flat, fs); got != 0 {
		t.Errorf("flat: got %v, want 0", got)
	}
	var b [NumBands]float64
	b[5] = 6
	if got := AutoPreampFor(b, fs); math.Abs(got+6) > 0.3 {
		t.Errorf("+6 band: got %v, want ~-6", got)
	}
	for i := range b {
		b[i] = MaxGain
	}
	if got := AutoPreampFor(b, fs); got != -MaxGain {
		t.Errorf("all +12: got %v, want %v", got, -MaxGain)
	}
	s := Settings{Bands: b, Preamp: -2}
	if got := EffectivePreamp(s, fs); got != -2 {
		t.Errorf("EffectivePreamp manual = %v", got)
	}
}

func TestPeakingStableLowRate(t *testing.T) {
	for _, sr := range []float64{22050, 32000, 44100, 48000} {
		c := PeakingCoeffs(16000, 12, BandQ, sr)
		// Stability of a 2nd order polynomial: |A2| < 1 and |A1| < 1 + A2.
		if math.Abs(c.A2) >= 1 || math.Abs(c.A1) >= 1+c.A2 {
			t.Errorf("unstable at %v Hz: %+v", sr, c)
		}
	}
}

func TestBuiltinPresets(t *testing.T) {
	ps := BuiltinPresets()
	if len(ps) != 13 || ps[0].Name != "Flat" || ps[12].Name != "Small Speakers" {
		t.Fatalf("unexpected presets: %d", len(ps))
	}
	for _, p := range ps {
		for _, g := range p.Bands {
			if math.Abs(g) > MaxGain {
				t.Errorf("%s out of range", p.Name)
			}
		}
	}
	if p, ok := FindBuiltin("rock"); !ok || p.Name != "Rock" {
		t.Error("FindBuiltin(rock) failed")
	}
	if _, ok := FindBuiltin("nope"); ok {
		t.Error("FindBuiltin(nope) succeeded")
	}
}

func TestProcessorFlatIdentity(t *testing.T) {
	p := NewProcessor(fs)
	p.SetSettings(Settings{Enabled: true})
	in := sine(fs/2, 440, 0.5)
	out := append([]float32(nil), in...)
	p.Process(out)
	for i := range in {
		if math.Abs(float64(in[i]-out[i])) > 1e-6 {
			t.Fatalf("sample %d: %v != %v", i, out[i], in[i])
		}
	}
}

func TestProcessorBoostGain(t *testing.T) {
	p := NewProcessor(fs)
	var b [NumBands]float64
	b[5] = 6
	p.SetSettings(Settings{Enabled: true, Bands: b})
	buf := sine(fs, 1000, 0.25)
	p.Process(buf)
	peak := 0.0
	for _, v := range buf[len(buf)/2:] {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if db := LinearToDB(peak / 0.25); math.Abs(db-6) > 0.5 {
		t.Errorf("gain at 1 kHz = %.2f dB, want ~6", db)
	}
}

func TestProcessorStability(t *testing.T) {
	p := NewProcessor(fs)
	var b [NumBands]float64
	for i := range b {
		b[i] = MaxGain
	}
	p.SetSettings(Settings{Enabled: true, Bands: b, Preamp: MaxGain})
	rng := rand.New(rand.NewSource(1))
	buf := make([]float32, 960*2)
	for iter := 0; iter < 500; iter++ {
		for i := range buf {
			buf[i] = float32(rng.Float64()*2 - 1)
		}
		if iter%7 == 0 {
			var s Settings
			s.Enabled = rng.Intn(4) != 0
			s.AutoPreamp = rng.Intn(2) == 0
			s.Preamp = rng.Float64()*24 - 12
			for i := range s.Bands {
				s.Bands[i] = rng.Float64()*24 - 12
			}
			p.SetSettings(s)
		}
		p.Process(buf)
		for i, v := range buf {
			f := float64(v)
			if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e4 {
				t.Fatalf("iter %d sample %d invalid: %v", iter, i, v)
			}
		}
	}
}

func maxJump(buf []float32) float64 {
	m := 0.0
	for i := 2; i < len(buf); i++ {
		m = math.Max(m, math.Abs(float64(buf[i]-buf[i-2])))
	}
	return m
}

func TestProcessorClickFree(t *testing.T) {
	const amp = 0.5
	const freq = 60.0
	p := NewProcessor(fs)
	b := [NumBands]float64{9, 9, 6, 0, -6, 0, 3, 6, 9, 12}
	s := Settings{Enabled: true, Bands: b, Preamp: 6}
	p.SetSettings(s)

	// Natural max slope of the sine at the largest steady-state gain any of
	// the settings used below produce, plus a small margin.
	s2 := s
	s2.Bands[1], s2.Preamp = -s.Bands[1], -s.Preamp
	gain := math.Max(Response(s.Bands, s.Preamp, fs, []float64{freq})[0],
		Response(s2.Bands, s2.Preamp, fs, []float64{freq})[0])
	limit := amp * 2 * math.Pi * freq / fs * DBToLinear(math.Max(gain, 0)) * 1.25
	buf := sine(fs*4, freq, amp)
	chunk := 1024 * 2
	var prev float32
	for i, off := 0, 0; off < len(buf); i, off = i+1, off+chunk {
		end := min(off+chunk, len(buf))
		switch i % 12 {
		case 3:
			s.Enabled = !s.Enabled
			p.SetSettings(s)
		case 8:
			s.Bands[1] = -s.Bands[1]
			s.Preamp = -s.Preamp
			p.SetSettings(s)
		}
		part := buf[off:end]
		p.Process(part)
		if off > 0 {
			if d := math.Abs(float64(part[0] - prev)); d > limit {
				t.Fatalf("jump across chunk %d: %v > %v", i, d, limit)
			}
		}
		if d := maxJump(part); d > limit {
			t.Fatalf("jump in chunk %d: %v > %v", i, d, limit)
		}
		prev = part[len(part)-2]
	}
}

func TestProcessorStateAcrossCalls(t *testing.T) {
	s := Settings{Enabled: true, Bands: [NumBands]float64{6, 0, -3, 0, 2, 0, 0, 4, 0, -5}, Preamp: -3}
	in := sine(fs/2, 97, 0.4)
	for i := range in {
		in[i] += float32(0.1 * math.Sin(float64(i)*0.37))
	}

	a := NewProcessor(fs)
	a.SetSettings(s)
	whole := append([]float32(nil), in...)
	a.Process(whole)

	b := NewProcessor(fs)
	b.SetSettings(s)
	split := append([]float32(nil), in...)
	cut := 2 * 4111 // deliberately not a multiple of the block size
	b.Process(split[:cut])
	b.Process(split[cut:])

	for i := range whole {
		if math.Abs(float64(whole[i]-split[i])) > 1e-6 {
			t.Fatalf("sample %d differs: %v vs %v", i, whole[i], split[i])
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(fs)
	for _, amp := range []float64{1, 1.5, 2, 4} {
		buf := sine(fs/4, 200, amp)
		buf[100] = float32(amp) // instantaneous transient
		l.Process(buf)
		for i, v := range buf {
			if math.Abs(float64(v)) > 1 {
				t.Fatalf("amp %v sample %d = %v", amp, i, v)
			}
		}
	}

	l = NewLimiter(fs)
	in := sine(fs/4, 200, DBToLinear(-6))
	out := append([]float32(nil), in...)
	l.Process(out)
	for i := range in {
		if math.Abs(float64(in[i]-out[i])) > 1e-6 {
			t.Fatalf("not transparent at -6 dBFS: %v vs %v", out[i], in[i])
		}
	}
}

func TestDBConversions(t *testing.T) {
	if math.Abs(LinearToDB(DBToLinear(-7.5))+7.5) > 1e-12 {
		t.Error("round trip failed")
	}
	if !math.IsInf(LinearToDB(0), -1) {
		t.Error("LinearToDB(0) should be -Inf")
	}
}

func TestProcessorConcurrentSettings(t *testing.T) {
	p := NewProcessor(fs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			var s Settings
			s.Enabled = i%2 == 0
			s.Bands[i%NumBands] = float64(i%25 - 12)
			p.SetSettings(s)
			_ = p.Settings()
		}
	}()
	buf := sine(960, 440, 0.5)
	for i := 0; i < 200; i++ {
		p.Process(buf)
	}
	<-done
}
