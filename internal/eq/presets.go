package eq

import "strings"

// BuiltinPresets returns the built-in presets. The returned slice is a fresh
// copy and may be modified by the caller.
func BuiltinPresets() []Preset {
	return []Preset{
		{Name: "Flat"},
		{Name: "Bass Boost", Bands: [NumBands]float64{6, 5.5, 4.5, 2.5, 0.5, 0, 0, 0, 0, 0}, Preamp: -3},
		{Name: "Bass Reducer", Bands: [NumBands]float64{-6, -5, -4, -2, -0.5, 0, 0, 0, 0, 0}},
		{Name: "Treble Boost", Bands: [NumBands]float64{0, 0, 0, 0, 0, 0.5, 2, 4, 5.5, 6}, Preamp: -3},
		{Name: "Vocal", Bands: [NumBands]float64{-2, -1.5, -1, 0.5, 2, 3, 3, 2, 0.5, -0.5}, Preamp: -1.5},
		{Name: "Rock", Bands: [NumBands]float64{4.5, 3.5, 2, 0.5, -1, -1, 0.5, 2.5, 3.5, 4}, Preamp: -2},
		{Name: "Pop", Bands: [NumBands]float64{-1, 0.5, 2, 3, 3.5, 2.5, 1, 0, -0.5, -1}, Preamp: -2},
		{Name: "Jazz", Bands: [NumBands]float64{3, 2, 1, 1.5, -1, -1, 0, 1, 2, 3}, Preamp: -1.5},
		{Name: "Classical", Bands: [NumBands]float64{3.5, 3, 2, 1, 0, 0, 0, 1.5, 2.5, 3.5}, Preamp: -1.5},
		{Name: "Electronic", Bands: [NumBands]float64{5, 4.5, 1.5, 0, -1.5, 1, 0.5, 1.5, 4, 4.5}, Preamp: -2.5},
		{Name: "Acoustic", Bands: [NumBands]float64{3, 3, 2, 1, 1.5, 1.5, 2.5, 3, 2.5, 1.5}, Preamp: -2},
		{Name: "Loudness", Bands: [NumBands]float64{6, 4, 1, 0, -1, 0, -0.5, 1, 4, 5}, Preamp: -3},
		{Name: "Small Speakers", Bands: [NumBands]float64{-6, -3, 2, 3.5, 2, 0.5, 0, 1, 2, 1}, Preamp: -2},
	}
}

// FindBuiltin looks up a built-in preset by name (case-insensitive).
func FindBuiltin(name string) (Preset, bool) {
	for _, p := range BuiltinPresets() {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Preset{}, false
}
