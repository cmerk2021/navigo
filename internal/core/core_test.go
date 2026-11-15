package core

import (
	"bytes"
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

func wav(frames int, level float64) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	binary.Write(&b, le, uint32(36+frames*4))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(2), uint32(44100), uint32(44100 * 4), uint16(4), uint16(16)} {
		binary.Write(&b, le, v)
	}
	b.WriteString("data")
	binary.Write(&b, le, uint32(frames*4))
	for i := 0; i < frames; i++ {
		v := int16(level * 32767 * math.Sin(float64(i)/10))
		binary.Write(&b, le, v)
		binary.Write(&b, le, v)
	}
	return b.Bytes()
}

// TestPlaybackFlow checks queue advance across gapless transitions and the
// transcode fallback for files the local decoders can't handle.
func TestPlaybackFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("XDG_CACHE_HOME", dir+"/cache")
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")

	track := wav(44100/2, 0.5)
	var transcodes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/ping":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1"}}`))
		case "/rest/stream":
			id, format := r.URL.Query().Get("id"), r.URL.Query().Get("format")
			w.Header().Set("Content-Type", "audio/wav")
			if id == "bad" && format == "raw" {
				w.Write([]byte("this is not audio at all, definitely not"))
				return
			}
			if format == "mp3" {
				transcodes++
			}
			w.Write(track) // the fake "transcode" is WAV; decoders sniff magic bytes
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.ServerURL, cfg.Username, cfg.Password = srv.URL, "u", "p"
	cfg.Scrobble = false
	p := New(cfg)
	p.Start(nil, nil, nil)
	defer p.Close()

	songs := []subsonic.Song{
		{ID: "a", Title: "A", Suffix: "wav", Duration: 1},
		{ID: "bad", Title: "Bad", Suffix: "flac", Duration: 1},
		{ID: "c", Title: "C", Suffix: "opus", Duration: 1},
	}
	p.PlayTracks(songs, 0, false)

	seen := map[string]bool{}
	buf := make([]float32, 1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p.eng.Render(buf)
		snap := p.Snapshot()
		seen[snap.Song.ID] = true
		if snap.Song.ID == "c" && snap.State == audio.Stopped && seen["bad"] {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !seen["a"] || !seen["bad"] || !seen["c"] {
		t.Fatalf("did not play through queue: %v", seen)
	}
	if transcodes < 2 { // "bad" (fallback) and "c" (opus)
		t.Fatalf("expected transcode requests, got %d", transcodes)
	}
	if st := p.Snapshot(); st.State != audio.Stopped {
		t.Fatalf("expected Stopped at end of queue, got %v", st.State)
	}
}
