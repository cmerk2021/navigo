package config

import (
	"os"
	"testing"
)

func TestReplayGainMigration(t *testing.T) {
	cases := map[string]string{
		`{"replayGain":true}`:        ReplayGainTrack,
		`{"replayGain":false}`:       ReplayGainOff,
		`{}`:                         ReplayGainOff,
		`{"replayGainMode":"album"}`: ReplayGainAlbum,
		`{"replayGainMode":"bogus"}`: ReplayGainOff,
	}
	for in, want := range cases {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if err := os.MkdirAll(Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(Path(), []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.ReplayGainMode != want || c.ReplayGain {
			t.Errorf("%s: mode %q legacy %v, want %q", in, c.ReplayGainMode, c.ReplayGain, want)
		}
	}
}
