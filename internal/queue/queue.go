// Package queue implements the play queue: ordered tracks, a current index,
// true shuffle without immediate repeats and repeat modes, plus persistence.
package queue

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

// Repeat mode.
type Repeat int

// Repeat modes.
const (
	RepeatOff Repeat = iota
	RepeatAll
	RepeatOne
)

func (r Repeat) String() string {
	return [...]string{"off", "all", "one"}[r]
}

// Next cycles Off → All → One → Off.
func (r Repeat) Next() Repeat { return (r + 1) % 3 }

// Queue is a play queue. It is not safe for concurrent use.
type Queue struct {
	Tracks  []subsonic.Song
	order   []int // play order (indices into Tracks)
	pos     int   // position in order, -1 if empty
	shuffle bool
	repeat  Repeat
}

// New returns an empty queue.
func New() *Queue { return &Queue{pos: -1} }

// Len returns the number of tracks.
func (q *Queue) Len() int { return len(q.Tracks) }

// Shuffle reports whether shuffle is on.
func (q *Queue) Shuffle() bool { return q.shuffle }

// Repeat returns the repeat mode.
func (q *Queue) Repeat() Repeat { return q.repeat }

// SetRepeat sets the repeat mode.
func (q *Queue) SetRepeat(r Repeat) { q.repeat = r }

// Index returns the index in Tracks of the current track, or -1.
func (q *Queue) Index() int {
	if q.pos < 0 || q.pos >= len(q.order) {
		return -1
	}
	return q.order[q.pos]
}

// Current returns the current track.
func (q *Queue) Current() (subsonic.Song, bool) {
	i := q.Index()
	if i < 0 {
		return subsonic.Song{}, false
	}
	return q.Tracks[i], true
}

// Set replaces the queue and makes start the current track. With shuffle
// the start track plays first and the rest are shuffled.
func (q *Queue) Set(tracks []subsonic.Song, start int, shuffle bool) {
	q.Tracks = append([]subsonic.Song(nil), tracks...)
	q.shuffle = shuffle
	if len(tracks) == 0 {
		q.order, q.pos = nil, -1
		return
	}
	if start < 0 || start >= len(tracks) {
		start = 0
		if shuffle {
			start = rand.IntN(len(tracks))
		}
	}
	q.rebuild(start)
}

// rebuild recomputes the play order so that track index cur is current.
func (q *Queue) rebuild(cur int) {
	n := len(q.Tracks)
	q.order = make([]int, n)
	if !q.shuffle {
		for i := range q.order {
			q.order[i] = i
		}
		q.pos = cur
		return
	}
	q.order[0] = cur
	k := 1
	for i := 0; i < n; i++ {
		if i != cur {
			q.order[k] = i
			k++
		}
	}
	rest := q.order[1:]
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	q.pos = 0
}

// SetShuffle toggles shuffle, keeping the current track.
func (q *Queue) SetShuffle(on bool) {
	if on == q.shuffle {
		return
	}
	q.shuffle = on
	if len(q.Tracks) == 0 {
		return
	}
	cur := q.Index()
	if cur < 0 {
		cur = 0
	}
	q.rebuild(cur)
}

// peek returns the order position after p, honouring repeat-all. Repeat-one
// is handled by callers (it only applies to automatic advance).
func (q *Queue) peek(p int) int {
	if len(q.order) == 0 {
		return -1
	}
	if p+1 < len(q.order) {
		return p + 1
	}
	if q.repeat == RepeatAll {
		return 0
	}
	return -1
}

// reshuffleCycle builds a new random cycle for repeat-all + shuffle, making
// sure the new first track differs from the last one played.
func (q *Queue) reshuffleCycle() {
	if !q.shuffle || len(q.order) < 2 {
		return
	}
	last := q.order[len(q.order)-1]
	rand.Shuffle(len(q.order), func(i, j int) { q.order[i], q.order[j] = q.order[j], q.order[i] })
	if q.order[0] == last {
		j := 1 + rand.IntN(len(q.order)-1)
		q.order[0], q.order[j] = q.order[j], q.order[0]
	}
}

// PeekNext returns the track that would play next automatically (repeat-one
// repeats the current track).
func (q *Queue) PeekNext() (subsonic.Song, bool) {
	if q.pos < 0 {
		return subsonic.Song{}, false
	}
	if q.repeat == RepeatOne {
		return q.Current()
	}
	p := q.peek(q.pos)
	if p < 0 {
		return subsonic.Song{}, false
	}
	return q.Tracks[q.order[p]], true
}

// Upcoming returns up to n upcoming tracks for prefetching.
func (q *Queue) Upcoming(n int) []subsonic.Song {
	var out []subsonic.Song
	if q.pos < 0 {
		return nil
	}
	if q.repeat == RepeatOne {
		return nil
	}
	p := q.pos
	for len(out) < n {
		p = q.peek(p)
		if p < 0 || q.order[p] == q.Index() {
			break
		}
		out = append(out, q.Tracks[q.order[p]])
	}
	return out
}

// Advance moves to the next track. auto is true for an automatic transition
// at the end of a track (where repeat-one applies). It returns false at the
// end of the queue.
func (q *Queue) Advance(auto bool) bool {
	if q.pos < 0 {
		return false
	}
	if auto && q.repeat == RepeatOne {
		return true
	}
	if q.pos+1 >= len(q.order) {
		if q.repeat != RepeatAll && auto {
			return false
		}
		if q.repeat != RepeatAll && !auto {
			// Manual next at the end wraps around like most players.
			q.pos = 0
			return true
		}
		q.reshuffleCycle()
		q.pos = 0
		return true
	}
	q.pos++
	return true
}

// Previous moves back one track, wrapping to the end.
func (q *Queue) Previous() bool {
	if q.pos < 0 {
		return false
	}
	if q.pos > 0 {
		q.pos--
		return true
	}
	q.pos = len(q.order) - 1
	return true
}

// Jump makes track index i current.
func (q *Queue) Jump(i int) bool {
	if i < 0 || i >= len(q.Tracks) {
		return false
	}
	for p, idx := range q.order {
		if idx == i {
			q.pos = p
			return true
		}
	}
	return false
}

// Remove deletes track index i. It reports whether the current track was
// removed (the new current is the following track).
func (q *Queue) Remove(i int) (removedCurrent bool) {
	if i < 0 || i >= len(q.Tracks) {
		return false
	}
	cur := q.Index()
	removedCurrent = cur == i
	q.Tracks = append(q.Tracks[:i], q.Tracks[i+1:]...)
	newOrder := q.order[:0]
	newPos := -1
	for p, idx := range q.order {
		if idx == i {
			if p == q.pos {
				newPos = len(newOrder) // next element takes its slot
			}
			continue
		}
		if idx > i {
			idx--
		}
		if p == q.pos {
			newPos = len(newOrder)
		}
		newOrder = append(newOrder, idx)
	}
	q.order = newOrder
	switch {
	case len(q.order) == 0:
		q.pos = -1
	case newPos >= len(q.order):
		q.pos = len(q.order) - 1
	case newPos < 0:
		q.pos = 0
	default:
		q.pos = newPos
	}
	return removedCurrent
}

// Clear empties the queue.
func (q *Queue) Clear() {
	q.Tracks, q.order, q.pos = nil, nil, -1
}

// State is the persisted queue state.
type State struct {
	Tracks   []subsonic.Song `json:"tracks"`
	Order    []int           `json:"order"`
	Pos      int             `json:"pos"`
	Shuffle  bool            `json:"shuffle"`
	Repeat   Repeat          `json:"repeat"`
	Position time.Duration   `json:"position"`
}

// Snapshot returns the persistable state.
func (q *Queue) Snapshot(position time.Duration) State {
	return State{
		Tracks:   append([]subsonic.Song(nil), q.Tracks...),
		Order:    append([]int(nil), q.order...),
		Pos:      q.pos,
		Shuffle:  q.shuffle,
		Repeat:   q.repeat,
		Position: position,
	}
}

// Restore loads a snapshot, validating it.
func (q *Queue) Restore(s State) {
	q.Tracks = s.Tracks
	q.shuffle = s.Shuffle
	q.repeat = s.Repeat
	if q.repeat < RepeatOff || q.repeat > RepeatOne {
		q.repeat = RepeatOff
	}
	n := len(s.Tracks)
	valid := len(s.Order) == n
	seen := make([]bool, n)
	for _, i := range s.Order {
		if !valid || i < 0 || i >= n || seen[i] {
			valid = false
			break
		}
		seen[i] = true
	}
	if n == 0 {
		q.order, q.pos = nil, -1
		return
	}
	if !valid {
		q.order = nil
		q.shuffle = false
		q.rebuild(0)
		return
	}
	q.order = s.Order
	q.pos = s.Pos
	if q.pos < 0 || q.pos >= n {
		q.pos = 0
	}
}

// StatePath returns the queue state file path.
func StatePath() string { return filepath.Join(config.StateDir(), "queue.json") }

// SaveState writes the state atomically.
func SaveState(s State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(StatePath(), b, 0o600)
}

// LoadState reads the persisted state (zero State if missing).
func LoadState() (State, error) {
	b, err := os.ReadFile(StatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return State{Pos: -1}, nil
		}
		return State{Pos: -1}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{Pos: -1}, err
	}
	return s, nil
}
