package eq

import (
	"math"
	"sync/atomic"
)

const (
	// blockFrames is how often (in frames) smoothed gains and filter
	// coefficients are updated.
	blockFrames = 32
	// smoothTau is the time constant of the one-pole gain smoothing; gains
	// settle (~95%) in about 3*smoothTau.
	smoothTau = 0.012
	// crossfadeTime is the duration of the dry/wet crossfade when toggling
	// Enabled.
	crossfadeTime = 0.050
	// snapDB is the distance below which a smoothed gain snaps to its target.
	snapDB = 0.005
)

// pendingSettings is an immutable snapshot handed from SetSettings to the
// audio goroutine.
type pendingSettings struct {
	s        Settings
	preampDB float64
}

// biquadState is Direct Form I state for one channel of one band.
type biquadState struct{ x1, x2, y1, y2 float64 }

// band holds the smoothing and filter state of one EQ band.
type band struct {
	cur, target float64
	coeffs      Coeffs
	// idle is true when the band has been exactly 0 dB for at least one full
	// block and its filter can be skipped.
	idle bool
	st   [2]biquadState
}

// Processor applies preamp plus 10 cascaded peaking biquads to interleaved
// stereo float32 audio. SetSettings/Settings may be called from any goroutine;
// Process and Reset must be called from a single (audio) goroutine.
type Processor struct {
	sampleRate float64
	pending    atomic.Pointer[pendingSettings]
	applied    *pendingSettings

	bands [NumBands]band

	preampCur, preampTarget float64 // dB
	preampGain              float64 // linear gain at the end of the last block

	wet, wetTarget float64 // dry/wet mix in [0,1]
	wetStep        float64
	bypassed       bool

	fresh      bool          // no audio processed since construction/Reset
	dryHist    [2][2]float64 // [channel][0=x1,1=x2]
	smoothCoef float64
	scratch    [2][blockFrames]float64
}

// NewProcessor creates a disabled, flat processor for the given sample rate.
func NewProcessor(sampleRate int) *Processor {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	fs := float64(sampleRate)
	p := &Processor{
		sampleRate: fs,
		wetStep:    1 / (crossfadeTime * fs),
		smoothCoef: 1 - math.Exp(-blockFrames/(smoothTau*fs)),
		preampGain: 1,
		bypassed:   true,
		fresh:      true,
	}
	for i := range p.bands {
		p.bands[i].coeffs = identityCoeffs
		p.bands[i].idle = true
	}
	p.pending.Store(&pendingSettings{})
	return p
}

// SetSettings sets new target settings. It is safe to call concurrently with
// Process; changes are applied smoothly.
func (p *Processor) SetSettings(s Settings) {
	for i, g := range s.Bands {
		s.Bands[i] = clampGain(g)
	}
	s.Preamp = clampGain(s.Preamp)
	p.pending.Store(&pendingSettings{s: s, preampDB: EffectivePreamp(s, p.sampleRate)})
}

// Settings returns the most recently set settings.
func (p *Processor) Settings() Settings { return p.pending.Load().s }

func clampGain(g float64) float64 {
	if math.IsNaN(g) {
		return 0
	}
	return math.Max(-MaxGain, math.Min(MaxGain, g))
}

// Reset clears all filter state and jumps directly to the current settings.
func (p *Processor) Reset() {
	for i := range p.bands {
		p.bands[i].st = [2]biquadState{}
	}
	p.dryHist = [2][2]float64{}
	p.applied = nil
	p.fresh = true
}

// loadSettings picks up new settings published by SetSettings.
func (p *Processor) loadSettings() {
	ps := p.pending.Load()
	if ps == p.applied {
		return
	}
	p.applied = ps
	for i := range p.bands {
		p.bands[i].target = ps.s.Bands[i]
	}
	p.preampTarget = ps.preampDB
	p.wetTarget = 0
	if ps.s.Enabled {
		p.wetTarget = 1
	}
	if p.fresh {
		p.snapToTargets()
		p.wet = p.wetTarget
		p.bypassed = p.wet == 0
	}
}

// snapToTargets jumps all smoothed values to their targets.
func (p *Processor) snapToTargets() {
	for i := range p.bands {
		b := &p.bands[i]
		if b.cur != b.target {
			b.cur = b.target
			b.coeffs = PeakingCoeffs(Frequencies[i], b.cur, BandQ, p.sampleRate)
		}
		b.idle = b.cur == 0
	}
	if p.preampCur != p.preampTarget {
		p.preampCur = p.preampTarget
		p.preampGain = DBToLinear(p.preampCur)
	}
}

// smooth advances the one-pole gain smoothing by one block.
func (p *Processor) smooth() {
	for i := range p.bands {
		b := &p.bands[i]
		if b.cur == b.target {
			if b.cur == 0 {
				// Exactly 0 dB for a whole block: identity coefficients have
				// flushed any residue, so the filter can be skipped.
				b.idle = true
			}
			continue
		}
		b.idle = false
		b.cur = smoothStep(b.cur, b.target, p.smoothCoef)
		b.coeffs = PeakingCoeffs(Frequencies[i], b.cur, BandQ, p.sampleRate)
	}
	p.preampCur = smoothStep(p.preampCur, p.preampTarget, p.smoothCoef)
}

func smoothStep(cur, target, coef float64) float64 {
	cur += (target - cur) * coef
	if math.Abs(target-cur) < snapDB {
		return target
	}
	return cur
}

// Process equalizes interleaved stereo samples in place. Filter state carries
// across calls so consecutive buffers are processed seamlessly.
func (p *Processor) Process(buf []float32) {
	frames := len(buf) / 2
	for start := 0; start < frames; start += blockFrames {
		n := min(blockFrames, frames-start)
		p.processBlock(buf[start*2:(start+n)*2], n)
	}
}

func (p *Processor) processBlock(buf []float32, n int) {
	p.loadSettings()
	p.fresh = false

	if p.bypassed {
		if p.wetTarget == 0 {
			// Fully bypassed: pass through, keep parameters current so a
			// later re-enable starts from the right curve.
			p.snapToTargets()
			p.updateDryHist(buf, n)
			return
		}
		p.leaveBypass()
	}
	p.updateDryHist(buf, n)

	p.smooth()
	g0 := p.preampGain
	g1 := DBToLinear(p.preampCur)
	p.preampGain = g1

	// Preamp with a linear gain ramp across the block.
	gStep := (g1 - g0) / float64(n)
	for i := 0; i < n; i++ {
		g := g0 + gStep*float64(i+1)
		p.scratch[0][i] = float64(buf[2*i]) * g
		p.scratch[1][i] = float64(buf[2*i+1]) * g
	}

	for bi := range p.bands {
		b := &p.bands[bi]
		for ch := 0; ch < 2; ch++ {
			x := p.scratch[ch][:n]
			if b.idle {
				b.st[ch].passThrough(x)
			} else {
				b.st[ch].run(b.coeffs, x)
			}
		}
	}

	// Mix dry/wet, ramping the mix when crossfading.
	if p.wet == 1 && p.wetTarget == 1 {
		for i := 0; i < n; i++ {
			buf[2*i] = float32(p.scratch[0][i])
			buf[2*i+1] = float32(p.scratch[1][i])
		}
	} else {
		for i := 0; i < n; i++ {
			if p.wet < p.wetTarget {
				p.wet = math.Min(p.wetTarget, p.wet+p.wetStep)
			} else if p.wet > p.wetTarget {
				p.wet = math.Max(p.wetTarget, p.wet-p.wetStep)
			}
			dl, dr := float64(buf[2*i]), float64(buf[2*i+1])
			buf[2*i] = float32(dl + (p.scratch[0][i]-dl)*p.wet)
			buf[2*i+1] = float32(dr + (p.scratch[1][i]-dr)*p.wet)
		}
		if p.wet == 0 && p.wetTarget == 0 {
			p.bypassed = true
		}
	}
}

// leaveBypass primes the filters as if they had been running in steady state
// on the recent dry signal, so the crossfade-in starts without a transient.
func (p *Processor) leaveBypass() {
	p.bypassed = false
	g := p.preampGain
	for i := range p.bands {
		for ch := 0; ch < 2; ch++ {
			x1, x2 := p.dryHist[ch][0]*g, p.dryHist[ch][1]*g
			p.bands[i].st[ch] = biquadState{x1: x1, x2: x2, y1: x1, y2: x2}
		}
	}
}

// updateDryHist records the last two input frames of buf.
func (p *Processor) updateDryHist(buf []float32, n int) {
	for ch := 0; ch < 2; ch++ {
		h := &p.dryHist[ch]
		if n >= 2 {
			h[1] = float64(buf[2*(n-2)+ch])
			h[0] = float64(buf[2*(n-1)+ch])
		} else {
			h[1] = h[0]
			h[0] = float64(buf[ch])
		}
	}
}

// run filters x in place with Direct Form I, which tolerates coefficient
// changes between blocks well.
func (s *biquadState) run(c Coeffs, x []float64) {
	x1, x2, y1, y2 := s.x1, s.x2, s.y1, s.y2
	for i, in := range x {
		out := c.B0*in + c.B1*x1 + c.B2*x2 - c.A1*y1 - c.A2*y2
		x2, x1 = x1, in
		y2, y1 = y1, out
		x[i] = out
	}
	// Flush denormals.
	const tiny = 1e-30
	if math.Abs(y1) < tiny {
		y1 = 0
	}
	if math.Abs(y2) < tiny {
		y2 = 0
	}
	s.x1, s.x2, s.y1, s.y2 = x1, x2, y1, y2
}

// passThrough updates the state as an identity filter would, keeping it
// consistent so the band can be re-activated seamlessly.
func (s *biquadState) passThrough(x []float64) {
	n := len(x)
	if n >= 2 {
		s.x1, s.x2 = x[n-1], x[n-2]
	} else if n == 1 {
		s.x1, s.x2 = x[0], s.x1
	}
	s.y1, s.y2 = s.x1, s.x2
}
