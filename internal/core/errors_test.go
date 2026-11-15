package core

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

func newTestPlayer(t *testing.T) *Player {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("XDG_CACHE_HOME", dir+"/cache")
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	p := New(config.Default())
	t.Cleanup(p.eng.Close)
	return p
}

func drainToasts(p *Player) []Toast {
	var out []Toast
	for {
		select {
		case t := <-p.toasts:
			out = append(out, t)
		default:
			return out
		}
	}
}

// A gapless transition: the finished track's resources are closed (which
// may surface closed/EOF/cancelled errors and late errors for its token)
// and none of that may toast. A real error of the new track still does.
func TestGaplessTransitionNoStaleErrorToast(t *testing.T) {
	p := newTestPlayer(t)
	a := subsonic.Song{ID: "a", Title: "gas station cowboy hats"}
	b := subsonic.Song{ID: "b", Title: "time flies when you're having fun"}
	p.q.Set([]subsonic.Song{a, b}, 0, false)
	p.curToken, p.curSong = "1:a", a
	p.nextToken, p.nextSong = "2:b", b

	// Teardown of the finished track, before the next one is announced.
	for _, err := range []error{os.ErrClosed, context.Canceled, io.ErrClosedPipe, errors.New("read /x: file already closed")} {
		p.handleEvent(audio.Event{Kind: audio.EvError, ID: "1:a", Err: err})
	}
	p.handleEvent(audio.Event{Kind: audio.EvTrackStarted, ID: "2:b", Auto: true})
	if p.curToken != "2:b" {
		t.Fatalf("current token = %q after gapless start", p.curToken)
	}
	// A late (genuine-looking) error of the old track.
	p.handleEvent(audio.Event{Kind: audio.EvError, ID: "1:a", Err: errors.New("invalid sync-code")})
	if ts := drainToasts(p); len(ts) != 0 {
		t.Fatalf("toasts for the previous track: %+v", ts)
	}

	// A genuine error of the current track is still shown.
	p.handleEvent(audio.Event{Kind: audio.EvError, ID: "2:b", Err: errors.New("corrupt frame")})
	ts := drainToasts(p)
	if len(ts) != 1 || !ts[0].Error {
		t.Fatalf("toasts = %+v, want one error toast", ts)
	}
}

func TestMPRISStatusIsTruthful(t *testing.T) {
	for st, want := range map[audio.State]string{
		audio.Playing:   "Playing",
		audio.Loading:   "Paused",
		audio.Buffering: "Paused",
		audio.Paused:    "Paused",
		audio.Stopped:   "Stopped",
		audio.Error:     "Stopped",
	} {
		if got := mprisStatus(st); got != want {
			t.Errorf("mprisStatus(%v) = %q, want %q", st, got, want)
		}
	}
}
