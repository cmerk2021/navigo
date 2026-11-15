// Package eq implements a 10-band graphic equalizer built from cascaded RBJ
// peaking biquads, plus preamp, presets and a soft limiter.
package eq

// NumBands is the number of graphic EQ bands.
const NumBands = 10

// MaxGain is the maximum absolute gain (dB) of a band or the preamp.
const MaxGain = 12.0

// Frequencies are the center frequencies of the 10 bands in Hz.
var Frequencies = [NumBands]float64{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

// Labels are short display labels for the bands.
var Labels = [NumBands]string{"31", "62", "125", "250", "500", "1k", "2k", "4k", "8k", "16k"}

// Settings is the complete live EQ state.
type Settings struct {
	Enabled    bool              `json:"enabled"`
	Bands      [NumBands]float64 `json:"bands"`
	Preamp     float64           `json:"preamp"`
	AutoPreamp bool              `json:"autoPreamp"`
}

// Preset is a named band/preamp configuration.
type Preset struct {
	Name   string            `json:"name"`
	Bands  [NumBands]float64 `json:"bands"`
	Preamp float64           `json:"preamp"`
}
