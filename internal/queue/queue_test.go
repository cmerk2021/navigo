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

func ids(q *Queue) string {
	es, _ := q.Entries()
	s := ""
	for _, e := range es {
		s += e.Song.ID
		if e.Queued {
			s += "*"
		}
		s += " "
	}
	return s
}

func named(names ...string) []subsonic.Song {
	s := make([]subsonic.Song, len(names))
	for i, n := range names {
		s[i] = subsonic.Song{ID: n}
	}
	return s
}

func TestInsertUserQueue(t *testing.T) {
	q := New()
	q.Set(songs(4), 1, false) // 0 [1] 2 3
	q.Insert(named("a"), false)
	q.Insert(named("b", "c"), false)
	q.Insert(named("n"), true)
	if got, want := ids(q), "0 1 n* a* b* c* 2 3 "; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	// Without shuffle Tracks stays in play order.
	if q.Tracks[2].ID != "n" || q.Tracks[6].ID != "2" {
		t.Fatal("tracks not in play order", q.Tracks)
	}
	q.Advance(true)
	if c, _ := q.Current(); c.ID != "n" {
		t.Fatal(c.ID)
	}
	q.Insert(named("d"), false) // after the remaining queued tracks
	if got, want := ids(q), "0 1 n a* b* c* d* 2 3 "; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	// Going back keeps queued tracks queued and still appends after them.
	q.Previous()
	q.Insert(named("e"), false)
	if got, want := ids(q), "0 1 n a* b* c* d* e* 2 3 "; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	st := q.Snapshot(0)
	q2 := New()
	q2.Restore(st)
	if ids(q2) != ids(q) {
		t.Fatal("queued flags not restored", ids(q2))
	}
}

func TestInsertShuffleKeepsQueuedNext(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		q := New()
		q.Set(songs(6), 2, true)
		q.Insert(named("a", "b"), false)
		es, pos := q.Entries()
		if es[pos+1].Song.ID != "a" || es[pos+2].Song.ID != "b" {
			t.Fatal("queued tracks must play next", ids(q))
		}
		// Toggling shuffle keeps them right after the current track.
		for _, on := range []bool{false, true} {
			q.SetShuffle(on)
			es, pos = q.Entries()
			if c, _ := q.Current(); c.ID != "2" || es[pos+1].Song.ID != "a" || es[pos+2].Song.ID != "b" {
				t.Fatal("shuffle toggle lost queued order", on, ids(q))
			}
		}
		if len(es) != 8 {
			t.Fatal(len(es))
		}
	}
}

func TestAppendAndEmptyInsert(t *testing.T) {
	q := New()
	q.Insert(named("a", "b"), false)
	if c, _ := q.Current(); c.ID != "a" || q.Remaining() != 1 {
		t.Fatal("insert into empty queue")
	}
	q.Insert(named("x"), false)
	q.Append(named("c"))
	if got, want := ids(q), "a x* b c "; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	q.Remove(1) // x
	if got, want := ids(q), "a b c "; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
}
