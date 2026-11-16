package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

type fakeSource struct {
	similar []subsonic.Song
	simErr  error
	byGenre map[string][]subsonic.Song
	genres  []string
}

func (f *fakeSource) GetSimilarSongs(context.Context, string, int) ([]subsonic.Song, error) {
	return f.similar, f.simErr
}

func (f *fakeSource) GetRandomSongs(_ context.Context, _ int, genre string) ([]subsonic.Song, error) {
	f.genres = append(f.genres, genre)
	return f.byGenre[genre], nil
}

func song(ids ...string) []subsonic.Song {
	out := make([]subsonic.Song, len(ids))
	for i, id := range ids {
		out[i] = subsonic.Song{ID: id}
	}
	return out
}

func TestContinuationFallbacks(t *testing.T) {
	ctx := context.Background()
	seed := subsonic.Song{ID: "s", Genre: "Jazz"}

	// Similar songs that are already queued are skipped; with too few left,
	// random songs of the seed's genre and then of any genre fill up.
	f := &fakeSource{
		similar: song("s", "a", "q"),
		byGenre: map[string][]subsonic.Song{"Jazz": song("j1", "a"), "": song("r1")},
	}
	got, err := continuation(ctx, f, seed, false, map[string]bool{"s": true, "q": true})
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got); ids != "a j1 r1" {
		t.Fatalf("got %q", ids)
	}

	// Errors only surface when nothing could be found.
	f = &fakeSource{simErr: errors.New("no agent"), byGenre: map[string][]subsonic.Song{"": song("r1")}}
	if got, err := continuation(ctx, f, subsonic.Song{ID: "s"}, false, map[string]bool{}); err != nil || idsOf(got) != "r1" {
		t.Fatalf("got %v %v", idsOf(got), err)
	}
	if len(f.genres) != 1 || f.genres[0] != "" {
		t.Fatalf("no genre fallback without a genre: %v", f.genres)
	}
	f = &fakeSource{simErr: errors.New("no agent")}
	if _, err := continuation(ctx, f, seed, false, map[string]bool{}); err == nil {
		t.Fatal("expected error")
	}

	// A library shuffle starts over once every song has been played.
	f = &fakeSource{byGenre: map[string][]subsonic.Song{"": song("x", "y")}}
	if got, _ := continuation(ctx, f, seed, true, map[string]bool{"x": true}); idsOf(got) != "y" {
		t.Fatalf("got %q", idsOf(got))
	}
	if got, _ := continuation(ctx, f, seed, true, map[string]bool{"x": true, "y": true}); idsOf(got) != "x y" {
		t.Fatalf("got %q", idsOf(got))
	}
}

func idsOf(s []subsonic.Song) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x.ID
	}
	return out
}

// TestQueueAndAutoplay checks that enqueued songs play before the rest of
// the album and that autoplay continues once the queue runs out.
func TestQueueAndAutoplay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("XDG_CACHE_HOME", dir+"/cache")
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")

	track := wav(44100/4, 0.5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/ping":
			w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1"}}`))
		case "/rest/getSimilarSongs":
			w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1","similarSongs":{"song":[{"id":"b","title":"B"},{"id":"sim","title":"Similar","suffix":"wav","duration":1}]}}}`))
		case "/rest/stream":
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(track)
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

	wavSong := func(id string) subsonic.Song { return subsonic.Song{ID: id, Title: id, Suffix: "wav", Duration: 1} }
	p.PlayTracks([]subsonic.Song{wavSong("a"), wavSong("b")}, 0, false)
	if n := p.Enqueue([]subsonic.Song{wavSong("q")}, false); n != 1 {
		t.Fatal(n)
	}

	var played []string
	buf := make([]float32, 1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p.eng.Render(buf)
		snap := p.Snapshot()
		if snap.HasSong && (len(played) == 0 || played[len(played)-1] != snap.Song.ID) {
			played = append(played, snap.Song.ID)
		}
		if snap.Song.ID == "sim" && snap.State == audio.Stopped {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// "b" from getSimilarSongs is skipped because it was already queued.
	if got := idsOf(song(played...)); got != "a q b sim" {
		t.Fatalf("played %q", got)
	}
}
