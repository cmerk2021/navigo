package eq

import "testing"

func TestFormatDB(t *testing.T) {
	for v, want := range map[float64]string{3.5: "+3.5 dB", -2: "−2.0 dB", 0: "0.0 dB", -0.04: "0.0 dB", 11.96: "+12.0 dB"} {
		if got := FormatDB(v); got != want {
			t.Errorf("FormatDB(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestActiveCaption(t *testing.T) {
	var b [NumSliders - 1]float64
	b[5] = 3.5
	if got := ActiveCaption(5, b, 0); got != "+3.5 dB @ 1k" {
		t.Errorf("band caption = %q", got)
	}
	if got := ActiveCaption(NumSliders-1, b, -2); got != "Preamp −2.0 dB" {
		t.Errorf("preamp caption = %q", got)
	}
	if got := ActiveCaption(-1, b, 0); got != "" {
		t.Errorf("idle caption = %q", got)
	}
}
