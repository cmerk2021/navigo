package eqassist

import (
	"math"
	"testing"

	"github.com/cmerk2021/navigo/internal/eq"
)

func neutral() [Rounds]int { return [Rounds]int{NeutralOption, NeutralOption, NeutralOption} }

func TestNeutral(t *testing.T) {
	if Combine(neutral()) != ([eq.NumBands]float64{}) {
		t.Error("Combine(neutral) not flat")
	}
	for r := range Rounds {
		if RegionCurve(r, NeutralOption) != ([eq.NumBands]float64{}) {
			t.Errorf("round %d neutral not flat", r)
		}
	}
}

func TestClampExtremes(t *testing.T) {
	for a := range OptionsPerRound {
		for b := range OptionsPerRound {
			for c := range OptionsPerRound {
				for i, g := range Combine([Rounds]int{a, b, c}) {
					if math.Abs(g) > ClampDB {
						t.Fatalf("%d,%d,%d band %d = %v", a, b, c, i, g)
					}
				}
			}
		}
	}
	var big [eq.NumBands]float64
	big[0], big[1] = 20, -20
	if got := Clamp(big, ClampDB); got[0] != ClampDB || got[1] != -ClampDB {
		t.Errorf("Clamp = %v", got)
	}
}

func TestSmooth(t *testing.T) {
	var c [eq.NumBands]float64
	for i := range c {
		c[i] = 3
	}
	if Smooth(c) != c {
		t.Error("constant curve changed")
	}
	var spike [eq.NumBands]float64
	spike[5] = 6
	s := Smooth(spike)
	if s[5] >= 6 || s[5] <= 0 || s[4] <= 0 || s[6] <= 0 {
		t.Errorf("spike not smoothed: %v", s)
	}
}

func TestLoudnessCompensation(t *testing.T) {
	var c [eq.NumBands]float64
	for i := range c {
		c[i] = 3
	}
	if got := LoudnessCompensation(c); math.Abs(got+3) > 1e-9 {
		t.Errorf("got %v, want -3", got)
	}
}

func TestPreviewLoudnessMatched(t *testing.T) {
	choices := [Rounds]int{3, 0, NeutralOption}
	for r := range Rounds {
		for o := range OptionsPerRound {
			bands, pre := Preview(choices, r, o)
			if l := PerceivedLoudnessDB(bands) + pre; math.Abs(l) > 1e-9 {
				t.Errorf("round %d option %d loudness %v", r, o, l)
			}
		}
	}
	// Later rounds are neutral and the current option is applied.
	bands, _ := Preview([Rounds]int{0, 3, 3}, 0, 3)
	if bands != Combine([Rounds]int{3, NeutralOption, NeutralOption}) {
		t.Error("Preview should ignore later choices")
	}
}

func TestResult(t *testing.T) {
	for a := range OptionsPerRound {
		for b := range OptionsPerRound {
			for c := range OptionsPerRound {
				_, pre := Result([Rounds]int{a, b, c})
				if pre > 0 || pre < -8 {
					t.Fatalf("preamp %v out of range", pre)
				}
			}
		}
	}
	if _, pre := Result(neutral()); pre != 0 {
		t.Errorf("neutral preamp = %v", pre)
	}
}

func TestAllRounds(t *testing.T) {
	rs := AllRounds()
	for r, round := range rs {
		if round.Title == "" {
			t.Errorf("round %d has no title", r)
		}
		for o, opt := range round.Options {
			if opt.Label != string(rune('1'+o)) || opt.Bands != RegionCurve(r, o) {
				t.Errorf("round %d option %d mismatch: %+v", r, o, opt)
			}
		}
	}
}
