package queue

import (
	"fmt"
	"testing"

	"github.com/cmerk2021/navigo/internal/subsonic"
)

func songs(n int) []subsonic.Song {
	s := make([]subsonic.Song, n)
	for i := range s {
		s[i] = subsonic.Song{ID: fmt.Sprint(i)}
	}
	return s
}

func TestSequentialAndRepeat(t *testing.T) {
	q := New()
	q.Set(songs(3), 1, false)
	if q.Index() != 1 {
		t.Fatal(q.Index())
	}
	if !q.Advance(true) || q.Index() != 2 {
		t.Fatal("advance")
	}
	if q.Advance(true) {
		t.Fatal("should stop at end with repeat off")
	}
	q.SetRepeat(RepeatAll)
	if n, _ := q.PeekNext(); n.ID != "0" {
		t.Fatal("repeat-all peek", n.ID)
	}
	if !q.Advance(true) || q.Index() != 0 {
		t.Fatal("repeat-all wrap")
	}
	q.SetRepeat(RepeatOne)
	if !q.Advance(true) || q.Index() != 0 {
		t.Fatal("repeat-one auto")
	}
	if !q.Advance(false) || q.Index() != 1 {
		t.Fatal("repeat-one manual next")
	}
}

func TestShuffleNoImmediateRepeat(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		q := New()
		q.SetRepeat(RepeatAll)
		q.Set(songs(5), 2, true)
		if q.Index() != 2 {
			t.Fatal("start track must play first")
		}
		seen := map[int]bool{}
		prev := q.Index()
		seen[prev] = true
		for i := 0; i < 4; i++ {
			q.Advance(true)
			seen[q.Index()] = true
		}
		if len(seen) != 5 {
			t.Fatal("shuffle cycle repeated a track", seen)
		}
		last := q.Index()
		q.Advance(true) // new cycle
		if q.Index() == last {
			t.Fatal("immediate repeat across cycles")
		}
	}
}

func TestRemoveAndUpcoming(t *testing.T) {
	q := New()
	q.Set(songs(5), 2, false)
	if up := q.Upcoming(3); len(up) != 2 || up[0].ID != "3" {
		t.Fatal(up)
	}
	q.Remove(0)
	if c, _ := q.Current(); c.ID != "2" {
		t.Fatal("current changed", c.ID)
	}
	if !q.Remove(q.Index()) {
		t.Fatal("expected removedCurrent")
	}
	if c, _ := q.Current(); c.ID != "3" {
		t.Fatal("new current", c.ID)
	}
	st := q.Snapshot(0)
	q2 := New()
	q2.Restore(st)
	if c, _ := q2.Current(); c.ID != "3" || q2.Len() != 3 {
		t.Fatal("restore")
	}
	q2.Restore(State{Tracks: songs(2), Order: []int{0, 0}, Pos: 7})
	if q2.Index() != 0 {
		t.Fatal("invalid state not repaired")
	}
}
