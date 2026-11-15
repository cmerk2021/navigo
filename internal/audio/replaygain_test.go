package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReplayGainFactor(t *testing.T) {
	both := ReplayGain{TrackGain: -6, HasTrack: true, AlbumGain: -3, HasAlbum: true}
	trackOnly := ReplayGain{TrackGain: -6, HasTrack: true}
	albumOnly := ReplayGain{AlbumGain: -3, HasAlbum: true}
	db := func(g float64) float32 { return float32(math.Pow(10, g/20)) }
	cases := []struct {
		name string
		rg   ReplayGain
		mode ReplayGainMode
		want float32
	}{
		{"off", both, ReplayGainOff, 1},
		{"track", both, ReplayGainTrack, db(-6)},
		{"album", both, ReplayGainAlbum, db(-3)},
		{"album falls back to track", trackOnly, ReplayGainAlbum, db(-6)},
		{"track falls back to album", albumOnly, ReplayGainTrack, db(-3)},
		{"no tags", ReplayGain{}, ReplayGainAlbum, 1},
		{"peak limits boost", ReplayGain{TrackGain: 6, TrackPeak: 0.8, HasTrack: true}, ReplayGainTrack, 1 / 0.8},
	}
	for _, c := range cases {
		if got := c.rg.Factor(c.mode); math.Abs(float64(got-c.want)) > 1e-6 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// Server values take precedence; tags fill in what's missing.
	m := trackOnly.Merge(ReplayGain{TrackGain: 1, HasTrack: true, AlbumGain: -2, HasAlbum: true})
	if m.TrackGain != -6 || !m.HasAlbum || m.AlbumGain != -2 {
		t.Errorf("merge: %+v", m)
	}
}

// TestID3UTF16 checks a hand-built ID3v2.3 tag with UTF-16 TXXX frames.
func TestID3UTF16(t *testing.T) {
	utf16 := func(s string) []byte {
		b := []byte{0xFF, 0xFE}
		for _, r := range s {
			b = append(b, byte(r), 0)
		}
		return b
	}
	frame := func(desc, val string) []byte {
		body := append([]byte{1}, utf16(desc)...)
		body = append(body, 0, 0)
		body = append(body, utf16(val)...)
		var f bytes.Buffer
		f.WriteString("TXXX")
		binary.Write(&f, binary.BigEndian, uint32(len(body)))
		f.Write([]byte{0, 0})
		f.Write(body)
		return f.Bytes()
	}
	var frames bytes.Buffer
	frames.Write(frame("replaygain_track_gain", "-7.25 dB"))
	frames.Write(frame("REPLAYGAIN_ALBUM_GAIN", "+1.50 dB"))
	frames.Write(frame("REPLAYGAIN_ALBUM_PEAK", "0.950000"))
	frames.Write(make([]byte, 32)) // padding
	n := frames.Len()
	tag := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	tag = append(tag, frames.Bytes()...)
	rg := readID3ReplayGain(NewMemSource(tag))
	want := ReplayGain{TrackGain: -7.25, HasTrack: true, AlbumGain: 1.5, AlbumPeak: 0.95, HasAlbum: true}
	if rg != want {
		t.Fatalf("got %+v, want %+v", rg, want)
	}
}

// TestReplayGainTags checks that tags written by ffmpeg are read by each
// decoder.
func TestReplayGainTags(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	cases := []struct{ ext, codec string }{
		{"flac", "flac"}, {"mp3", "libmp3lame"}, {"ogg", "libvorbis"},
	}
	for _, c := range cases {
		t.Run(c.ext, func(t *testing.T) {
			path := filepath.Join(dir, "t."+c.ext)
			cmd := exec.Command(ff, "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1:sample_rate=44100",
				"-ac", "2", "-c:a", c.codec,
				"-metadata", "REPLAYGAIN_TRACK_GAIN=-8.40 dB", "-metadata", "REPLAYGAIN_TRACK_PEAK=0.912345",
				"-metadata", "REPLAYGAIN_ALBUM_GAIN=-5.10 dB", "-metadata", "REPLAYGAIN_ALBUM_PEAK=0.998",
				path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("ffmpeg %s: %v %s", c.ext, err, out)
			}
			data, _ := os.ReadFile(path)
			dec, err := Open(NewMemSource(data), c.ext)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			tg, ok := dec.(tagged)
			if !ok {
				t.Fatal("decoder does not expose tags")
			}
			want := ReplayGain{TrackGain: -8.4, TrackPeak: 0.912345, HasTrack: true, AlbumGain: -5.1, AlbumPeak: 0.998, HasAlbum: true}
			if got := tg.ReplayGain(); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			// Decoding must still start at the beginning after tag parsing.
			if all := readAll(t, dec); len(all)/2 < 44100*9/10 {
				t.Fatalf("decoded %d frames", len(all)/2)
			}
		})
	}
}
