package core

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cmerk2021/navigo/internal/queue"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

const (
	// libraryBatch is the number of random songs fetched per library
	// shuffle batch (the getRandomSongs maximum).
	libraryBatch = 500
	// autoplayBatch is the number of songs autoplay adds at a time.
	autoplayBatch = 25
	contTimeout   = 30 * time.Second
)

// songSource is the part of the Subsonic client used for continuations.
type songSource interface {
	GetRandomSongs(ctx context.Context, size int, genre string) ([]subsonic.Song, error)
	GetSimilarSongs(ctx context.Context, id string, count int) ([]subsonic.Song, error)
}

// continuation fetches the songs that follow when the queue runs out: more
// random songs for a library shuffle, otherwise music similar to seed
// (falling back to random songs of its genre, then of the whole library).
// Songs in exclude are skipped; exclude is modified.
func continuation(ctx context.Context, src songSource, seed subsonic.Song, library bool, exclude map[string]bool) ([]subsonic.Song, error) {
	if library {
		s, err := src.GetRandomSongs(ctx, libraryBatch, "")
		if err != nil {
			return nil, err
		}
		var out []subsonic.Song
		for _, x := range s {
			if !exclude[x.ID] {
				out = append(out, x)
			}
		}
		if len(out) == 0 {
			// Small library and everything has played: start over.
			return s, nil
		}
		return out, nil
	}
	var out []subsonic.Song
	var firstErr error
	add := func(s []subsonic.Song, err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, x := range s {
			if len(out) >= autoplayBatch {
				return
			}
			if x.ID == "" || exclude[x.ID] {
				continue
			}
			exclude[x.ID] = true
			out = append(out, x)
		}
	}
	add(src.GetSimilarSongs(ctx, seed.ID, 2*autoplayBatch))
	if len(out) < autoplayBatch/2 && seed.Genre != "" && ctx.Err() == nil {
		add(src.GetRandomSongs(ctx, 2*autoplayBatch, seed.Genre))
	}
	if len(out) < autoplayBatch/2 && ctx.Err() == nil {
		add(src.GetRandomSongs(ctx, 2*autoplayBatch, ""))
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

// resetContLocked forgets continuation state when the queue is replaced.
func (p *Player) resetContLocked(library bool) {
	p.contGen++
	p.contBusy = false
	p.contTried = ""
	p.libraryMode = library
}

// maybeContinueLocked starts fetching a continuation when the current track
// is the last one and the queue would otherwise stop: always for a library
// shuffle, otherwise if autoplay is enabled.
func (p *Player) maybeContinueLocked() {
	if p.contBusy || p.preview != nil || p.client == nil || p.q.Len() == 0 ||
		p.q.Repeat() != queue.RepeatOff || p.q.Remaining() > 0 ||
		!(p.libraryMode || p.cfg.Autoplay) {
		return
	}
	seed, ok := p.q.Current()
	if !ok || seed.ID == p.contTried {
		return
	}
	p.contBusy = true
	gen, c, library := p.contGen, p.client, p.libraryMode
	exclude := make(map[string]bool, p.q.Len())
	for _, s := range p.q.Tracks {
		exclude[s.ID] = true
	}
	slog.Debug("player: fetching continuation", "seed", seed.ID, "library", library)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), contTimeout)
		defer cancel()
		songs, err := continuation(ctx, c, seed, library, exclude)
		p.finishContinuation(gen, seed.ID, songs, err)
	}()
}

// finishContinuation appends fetched songs; if playback already stopped at
// the end of the queue, it continues with the first of them.
func (p *Player) finishContinuation(gen int, seedID string, songs []subsonic.Song, err error) {
	p.mu.Lock()
	if gen != p.contGen {
		p.mu.Unlock()
		return
	}
	p.contBusy = false
	if len(songs) == 0 {
		p.contTried = seedID
		p.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			p.toast(true, "Autoplay: %v", err)
		}
		return
	}
	p.q.Append(songs)
	if p.ended && p.preview == nil && p.q.Advance(false) {
		p.loadCurrentLocked(0, false, false)
	} else {
		p.updateNextLocked()
	}
	p.dirty.Store(true)
	p.mu.Unlock()
	p.publish()
}
