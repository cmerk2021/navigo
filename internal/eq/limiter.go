package eq

import "math"

const (
	limiterThresholdDB = -1.0
	limiterAttack      = 0.001
	limiterRelease     = 0.100
	// limiterKnee is the level above which the final soft clipper engages.
	limiterKnee = 0.9
)

// Limiter is a lookahead-free, stereo-linked soft limiter for the output
// stage. It is transparent below about -1 dBFS; above that an envelope
// follower (fast attack, slow release) reduces gain, and a final tanh soft
// clipper guarantees the output never exceeds ±1.0.
type Limiter struct {
	threshold   float64
	attack, rel float64
	env         float64
}

// NewLimiter creates a limiter for the given sample rate.
func NewLimiter(sampleRate int) *Limiter {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	fs := float64(sampleRate)
	return &Limiter{
		threshold: DBToLinear(limiterThresholdDB),
		attack:    1 - math.Exp(-1/(limiterAttack*fs)),
		rel:       1 - math.Exp(-1/(limiterRelease*fs)),
	}
}

// Process limits interleaved stereo samples in place.
func (l *Limiter) Process(buf []float32) {
	env := l.env
	for i := 0; i+1 < len(buf); i += 2 {
		left, right := float64(buf[i]), float64(buf[i+1])
		peak := math.Max(math.Abs(left), math.Abs(right))
		if math.IsNaN(peak) || math.IsInf(peak, 0) {
			buf[i], buf[i+1] = 0, 0
			continue
		}
		if peak > env {
			env += (peak - env) * l.attack
		} else {
			env += (peak - env) * l.rel
		}
		if env > l.threshold {
			g := l.threshold / env
			left *= g
			right *= g
		} else if peak <= limiterKnee {
			continue // fully transparent: leave samples untouched
		}
		buf[i] = float32(softClip(left))
		buf[i+1] = float32(softClip(right))
	}
	if env < 1e-30 {
		env = 0
	}
	l.env = env
}

// softClip is linear up to limiterKnee and then saturates smoothly towards 1.
func softClip(x float64) float64 {
	a := math.Abs(x)
	if a <= limiterKnee {
		return x
	}
	const r = 1 - limiterKnee
	y := limiterKnee + r*math.Tanh((a-limiterKnee)/r)
	// Guard against float32 rounding up to exactly 1.0 and beyond.
	y = math.Min(y, 0.99999)
	return math.Copysign(y, x)
}
