package eq

import (
	"math"
	"math/cmplx"
)

// BandQ is the Q used for every band. ~1.41 gives roughly one-octave
// bandwidth, which suits octave-spaced graphic EQ bands: neighbouring bands
// overlap enough to sum into smooth curves without excessive interaction.
const BandQ = 1.41

// Coeffs are normalized biquad coefficients (a0 = 1):
//
//	y[n] = B0*x[n] + B1*x[n-1] + B2*x[n-2] - A1*y[n-1] - A2*y[n-2]
type Coeffs struct{ B0, B1, B2, A1, A2 float64 }

// identityCoeffs passes the signal through unchanged.
var identityCoeffs = Coeffs{B0: 1}

// DBToLinear converts decibels to a linear amplitude factor.
func DBToLinear(db float64) float64 { return math.Pow(10, db/20) }

// LinearToDB converts a linear amplitude factor to decibels. Non-positive
// values return -Inf.
func LinearToDB(v float64) float64 {
	if v <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v)
}

// clampFreq keeps a center frequency safely below Nyquist.
func clampFreq(freq, sampleRate float64) float64 {
	if limit := 0.45 * sampleRate; freq > limit {
		return limit
	}
	return freq
}

// PeakingCoeffs returns RBJ audio-EQ-cookbook peaking filter coefficients.
// The center frequency is clamped to 0.45*sampleRate to remain stable.
func PeakingCoeffs(freq, gainDB, q, sampleRate float64) Coeffs {
	if gainDB == 0 {
		return identityCoeffs
	}
	freq = clampFreq(freq, sampleRate)
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * freq / sampleRate
	sin, cos := math.Sincos(w0)
	alpha := sin / (2 * q)

	a0 := 1 + alpha/a
	return Coeffs{
		B0: (1 + alpha*a) / a0,
		B1: (-2 * cos) / a0,
		B2: (1 - alpha*a) / a0,
		A1: (-2 * cos) / a0,
		A2: (1 - alpha/a) / a0,
	}
}

// magnitude returns |H(e^jw)| of the biquad at freq.
func (c Coeffs) magnitude(freq, sampleRate float64) float64 {
	w := 2 * math.Pi * freq / sampleRate
	z1 := cmplx.Exp(complex(0, -w))
	z2 := z1 * z1
	num := complex(c.B0, 0) + complex(c.B1, 0)*z1 + complex(c.B2, 0)*z2
	den := 1 + complex(c.A1, 0)*z1 + complex(c.A2, 0)*z2
	return cmplx.Abs(num) / cmplx.Abs(den)
}

// Response returns the magnitude response in dB (including preamp) at each
// frequency in freqs, computed from the actual biquad coefficients of the
// cascade.
func Response(bands [NumBands]float64, preamp float64, sampleRate float64, freqs []float64) []float64 {
	var cs [NumBands]Coeffs
	for i, g := range bands {
		cs[i] = PeakingCoeffs(Frequencies[i], g, BandQ, sampleRate)
	}
	out := make([]float64, len(freqs))
	for j, f := range freqs {
		db := preamp
		for i, g := range bands {
			if g != 0 {
				db += LinearToDB(cs[i].magnitude(f, sampleRate))
			}
		}
		out[j] = db
	}
	return out
}

// LogFreqs returns n log-spaced frequencies from lo to hi inclusive.
func LogFreqs(n int, lo, hi float64) []float64 {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []float64{lo}
	}
	out := make([]float64, n)
	ratio := math.Log(hi / lo)
	for i := range out {
		out[i] = lo * math.Exp(ratio*float64(i)/float64(n-1))
	}
	out[n-1] = hi
	return out
}

// AutoPreampFor returns a non-positive preamp (dB) that compensates the peak
// of the combined magnitude response, clamped to [-MaxGain, 0].
func AutoPreampFor(bands [NumBands]float64, sampleRate float64) float64 {
	hi := math.Min(20000, 0.49*sampleRate)
	freqs := LogFreqs(512, 20, hi)
	for _, f := range Frequencies {
		if f < hi {
			freqs = append(freqs, f)
		}
	}
	peak := 0.0
	for _, db := range Response(bands, 0, sampleRate, freqs) {
		peak = math.Max(peak, db)
	}
	if peak <= 0 {
		return 0
	}
	return math.Max(-peak, -MaxGain)
}

// EffectivePreamp returns the preamp actually applied: s.Preamp, or
// AutoPreampFor(s.Bands) if s.AutoPreamp is set.
func EffectivePreamp(s Settings, sampleRate float64) float64 {
	if s.AutoPreamp {
		return AutoPreampFor(s.Bands, sampleRate)
	}
	return s.Preamp
}
