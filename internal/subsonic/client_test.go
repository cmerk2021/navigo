package subsonic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestToken(t *testing.T) {
	// Example from the Subsonic API documentation.
	if got := Token("sesame", "c19b2d"); got != "26719a1196d2a940705a59634eb18eab" {
		t.Fatalf("Token = %s", got)
	}
}

func TestNewSaltUnique(t *testing.T) {
	a, b := NewSalt(), NewSalt()
	if a == b || len(a) < 6 {
		t.Fatalf("salts not unique/long enough: %q %q", a, b)
	}
}

func TestParseResponseOK(t *testing.T) {
	body := `{"subsonic-response":{"status":"ok","version":"1.16.1","album":{"id":"a1","name":"Album","artist":"Art","song":[{"id":"s1","title":"One","duration":61,"suffix":"flac","replayGain":{"trackGain":-6.5}}]}}}`
	r, err := ParseResponse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.Album == nil || r.Album.Name != "Album" || len(r.Album.Song) != 1 {
		t.Fatalf("bad album: %+v", r.Album)
	}
	s := r.Album.Song[0]
	if s.Title != "One" || s.DurationTime() != 61*time.Second || s.ReplayGain == nil || s.ReplayGain.TrackGain != -6.5 {
		t.Fatalf("bad song: %+v", s)
	}
}

func TestParseResponseFailed(t *testing.T) {
	body := `{"subsonic-response":{"status":"failed","version":"1.16.1","error":{"code":40,"message":"Wrong username or password"}}}`
	_, err := ParseResponse(strings.NewReader(body))
	var se *Error
	if !errors.As(err, &se) || se.Code != 40 {
		t.Fatalf("expected code 40, got %v", err)
	}
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("unclear message: %v", err)
	}
}

func TestParseResponseGarbage(t *testing.T) {
	if _, err := ParseResponse(strings.NewReader("<html>")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ParseResponse(strings.NewReader(`{"foo":1}`)); err == nil {
		t.Fatal("expected error for non-subsonic JSON")
	}
}

func TestClientRequests(t *testing.T) {
	var lastQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lastQuery = map[string]string{}
		for k := range q {
			lastQuery[k] = q.Get(k)
		}
		if Token("pw", q.Get("s")) != q.Get("t") {
			w.Write([]byte(`{"subsonic-response":{"status":"failed","error":{"code":40}}}`))
			return
		}
		switch r.URL.Path {
		case "/prefix/rest/ping":
			w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1"}}`))
		case "/prefix/rest/getArtists":
			w.Write([]byte(`{"subsonic-response":{"status":"ok","artists":{"index":[{"name":"A","artist":[{"id":"1","name":"Abba"}]},{"name":"B","artist":[{"id":"2","name":"Bjork"}]}]}}}`))
		case "/prefix/rest/search3":
			w.Write([]byte(`{"subsonic-response":{"status":"ok","searchResult3":{"album":[{"id":"al","name":"X"}],"song":[{"id":"s","title":"Y"}]}}}`))
		case "/prefix/rest/stream":
			if r.Header.Get("Range") != "bytes=10-" {
				http.Error(w, "no range", 400)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/prefix/", "user", "pw", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"u", "t", "s", "v", "c", "f"} {
		if lastQuery[k] == "" {
			t.Fatalf("missing auth param %s", k)
		}
	}
	if lastQuery["v"] != APIVersion || lastQuery["c"] != "navigo" || lastQuery["f"] != "json" {
		t.Fatalf("bad params %v", lastQuery)
	}
	arts, err := c.GetArtists(ctx)
	if err != nil || len(arts) != 2 || arts[1].Name != "Bjork" {
		t.Fatalf("GetArtists: %v %v", arts, err)
	}
	sr, err := c.Search3(ctx, "x", 5, 5, 5)
	if err != nil || len(sr.Albums) != 1 || len(sr.Songs) != 1 {
		t.Fatalf("Search3: %+v %v", sr, err)
	}
	resp, err := c.Stream(ctx, "s", "mp3", 320, 10)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if lastQuery["format"] != "mp3" || lastQuery["maxBitRate"] != "320" {
		t.Fatalf("bad stream params %v", lastQuery)
	}
	if _, err := c.GetAlbum(ctx, "nope"); err == nil {
		t.Fatal("expected HTTP error")
	} else {
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != 404 {
			t.Fatalf("expected 404 HTTPError, got %v", err)
		}
	}

	bad, _ := New(srv.URL+"/prefix", "user", "wrong", false)
	if err := bad.Ping(ctx); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "u", "p", false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Ping(ctx)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", "u", "p", false); err == nil {
		t.Fatal("expected error")
	}
	c, err := New("music.example.com", "u", "p", false)
	if err != nil || !strings.HasPrefix(c.BaseURL(), "https://") {
		t.Fatalf("scheme default: %v %v", c, err)
	}
}
