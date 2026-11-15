// Package eqassist implements the "Which sounds best?" EQ assistant: the user
// picks the best-sounding of four loudness-matched options in each of three
// frequency regions, and the choices are combined into a smooth EQ curve.
package eqassist

import (
	"math"

	"github.com/cmerk2021/navigo/internal/eq"
)

const (
	// Rounds is the number of listening rounds.
	Rounds = 3
	// OptionsPerRound is the number of options offered per round.
	OptionsPerRound = 4
	// ClampDB limits the absolute gain of any band in a combined curve.
	ClampDB = 8.0
	// NeutralOption is the option index that leaves a region untouched.
	NeutralOption = 1
)

// Round describes one listening round.
type Round struct {
	Title    string
	Subtitle string
	Options  [OptionsPerRound]Option
}

// Option is a single choice within a round.
type Option struct {
	Label string // "1".."4"
	Name  string
	Bands [eq.NumBands]float64
}

// region defines a round's shape (per-band weight) and option amounts (dB).
type region struct {
	title, subtitle string
	names           [OptionsPerRound]string
	amounts         [OptionsPerRound]float64
	shape           [eq.NumBands]float64
}

var regions = [Rounds]region{
	{
		title:    "Low end",
		subtitle: "Bass and kick drum (31–250 Hz)",
		names:    [OptionsPerRound]string{"Less bass", "Neutral", "More bass", "Much more bass"},
		amounts:  [OptionsPerRound]float64{-4, 0, 3, 6},
		shape:    [eq.NumBands]float64{1, 1, 0.8, 0.45, 0.15, 0, 0, 0, 0, 0},
	},
	{
		title:    "Mids / vocals",
		subtitle: "Voices and lead instruments (500 Hz–2 kHz)",
		names:    [OptionsPerRound]string{"Scooped", "Neutral", "Forward", "Very forward"},
		amounts:  [OptionsPerRound]float64{-4, 0, 3, 5.5},
		shape:    [eq.NumBands]float64{0, 0, 0, 0.15, 0.6, 1, 0.6, 0.15, 0, 0},
	},
	{
		title:    "Highs / air",
		subtitle: "Cymbals, detail and sparkle (4–16 kHz)",
		names:    [OptionsPerRound]string{"Rolled off", "Neutral", "Brighter", "Very bright"},
		amounts:  [OptionsPerRound]float64{-5, 0, 3, 6},
		shape:    [eq.NumBands]float64{0, 0, 0, 0, 0, 0, 0.15, 0.5, 0.85, 1},
	},
}

// loudnessWeights approximate the combined energy distribution of typical
// music and ear sensitivity per band.
var loudnessWeights = [eq.NumBands]float64{0.3, 0.6, 0.85, 1, 1, 1, 1, 0.9, 0.6, 0.25}

// AllRounds returns the description of every round and its options.
func AllRounds() [Rounds]Round {
	var out [Rounds]Round
	for r, reg := range regions {
		out[r].Title = reg.title
		out[r].Subtitle = reg.subtitle
		for o := range OptionsPerRound {
			out[r].Options[o] = Option{
				Label: string(rune('1' + o)),
				Name:  reg.names[o],
				Bands: RegionCurve(r, o),
			}
		}
	}
	return out
}

// RegionCurve returns the band offsets for one round/option. Out-of-range
// arguments yield a flat curve.
func RegionCurve(round, option int) [eq.NumBands]float64 {
	var b [eq.NumBands]float64
	if round < 0 || round >= Rounds || option < 0 || option >= OptionsPerRound {
		return b
	}
	reg := regions[round]
	for i, w := range reg.shape {
		b[i] = w * reg.amounts[option]
	}
	return b
}

// Combine sums the chosen regional curves (choices[i] for round i), smooths
// the seams and clamps to ±ClampDB.
func Combine(choices [Rounds]int) [eq.NumBands]float64 {
	var sum [eq.NumBands]float64
	for r, o := range choices {
		c := RegionCurve(r, o)
		for i := range sum {
			sum[i] += c[i]
		}
	}
	return Clamp(Smooth(sum), ClampDB)
}

// Smooth applies a light 0.25/0.5/0.25 kernel, replicating edge values so a
// constant curve stays constant.
func Smooth(b [eq.NumBands]float64) [eq.NumBands]float64 {
	var out [eq.NumBands]float64
	for i := range b {
		prev, next := b[max(i-1, 0)], b[min(i+1, eq.NumBands-1)]
		out[i] = 0.25*prev + 0.5*b[i] + 0.25*next
	}
	return out
}

// Clamp limits every band to [-limit, limit].
func Clamp(b [eq.NumBands]float64, limit float64) [eq.NumBands]float64 {
	for i, g := range b {
		b[i] = math.Max(-limit, math.Min(limit, g))
	}
	return b
}

// PerceivedLoudnessDB estimates the loudness change (dB) caused by a curve
// as a weighted power average of the band gains.
func PerceivedLoudnessDB(b [eq.NumBands]float64) float64 {
	var num, den float64
	for i, g := range b {
		num += loudnessWeights[i] * math.Pow(10, g/10)
		den += loudnessWeights[i]
	}
	return 10 * math.Log10(num/den)
}

// LoudnessCompensation returns the preamp offset (dB) that cancels the
// perceived loudness change of a curve, so "louder" doesn't win.
func LoudnessCompensation(b [eq.NumBands]float64) float64 {
	return -PerceivedLoudnessDB(b)
}

// Preview returns the bands and preamp for auditioning option of round,
// given the earlier choices (choices[0..round-1]); later rounds are treated
// as neutral. The preamp loudness-matches the options against each other.
func Preview(choices [Rounds]int, round, option int) (bands [eq.NumBands]float64, preamp float64) {
	var c [Rounds]int
	for r := range c {
		switch {
		case r < round:
			c[r] = choices[r]
		case r == round:
			c[r] = option
		default:
			c[r] = NeutralOption
		}
	}
	bands = Combine(c)
	return bands, LoudnessCompensation(bands)
}

// Result returns the final curve and a gentle preamp: 60% of the largest
// boost (rounded to 0.5 dB) taken as headroom, clamped to [-8, 0]. The output
// limiter takes care of any remaining peaks.
func Result(choices [Rounds]int) (bands [eq.NumBands]float64, preamp float64) {
	bands = Combine(choices)
	peak := 0.0
	for _, g := range bands {
		peak = math.Max(peak, g)
	}
	preamp = -math.Round(peak*0.6*2) / 2
	preamp = math.Max(-8, preamp)
	if preamp == 0 {
		preamp = 0 // normalize -0
	}
	return bands, preamp
}
