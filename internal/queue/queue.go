// Package queue implements the play queue: ordered tracks, a current index,
// true shuffle without immediate repeats and repeat modes, plus persistence.
//
// Tracks added with Insert form a Spotify-style "user queue": they play
// right after the current track, ahead of the rest of the album/playlist,
// in the order they were added.
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
	Tracks []subsonic.Song
	// queued[i] marks Tracks[i] as added by the user and not played yet.
	queued  []bool
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
	q.queued = make([]bool, len(tracks))
	q.shuffle = shuffle
	q.order, q.pos = nil, -1
	if len(tracks) == 0 {
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
// Upcoming user-queued tracks keep their order right after cur.
func (q *Queue) rebuild(cur int) {
	n := len(q.Tracks)
	var up []int
	if q.pos >= 0 && q.pos < len(q.order) {
		for _, i := range q.order[q.pos+1:] {
			if q.queued[i] && i != cur {
				up = append(up, i)
			}
		}
	}
	isUp := make([]bool, n)
	for _, i := range up {
		isUp[i] = true
	}
	q.order = make([]int, 0, n)
	if !q.shuffle {
		for i := 0; i < n; i++ {
			if isUp[i] {
				continue
			}
			q.order = append(q.order, i)
			if i == cur {
				q.pos = len(q.order) - 1
				q.order = append(q.order, up...)
			}
		}
		return
	}
	q.order = append(q.order, cur)
	q.order = append(q.order, up...)
	k := len(q.order)
	for i := 0; i < n; i++ {
		if i != cur && !isUp[i] {
			q.order = append(q.order, i)
		}
	}
	rest := q.order[k:]
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

// setPos makes order position p current; a user-queued track stops being
// "queued" once it plays.
func (q *Queue) setPos(p int) {
	q.pos = p
	if p >= 0 && p < len(q.order) {
		q.queued[q.order[p]] = false
	}
}

// Insert adds tracks to the user queue: after the current track and any
// tracks queued before them (FIFO), or right after the current track when
// next is true. Into an empty queue the tracks are added with the first one
// current. It returns the number of tracks added.
func (q *Queue) Insert(tracks []subsonic.Song, next bool) int {
	if len(tracks) == 0 {
		return 0
	}
	if q.pos < 0 || len(q.order) == 0 {
		q.Set(tracks, 0, q.shuffle)
		return len(tracks)
	}
	at := q.pos + 1
	if !next {
		for p := q.pos + 1; p < len(q.order); p++ {
			if q.queued[q.order[p]] {
				at = p + 1
			}
		}
	}
	q.insertAt(at, tracks, true)
	return len(tracks)
}

// Append adds tracks at the end of the play order (e.g. autoplay).
func (q *Queue) Append(tracks []subsonic.Song) {
	if len(tracks) == 0 {
		return
	}
	if q.pos < 0 || len(q.order) == 0 {
		q.Set(tracks, 0, q.shuffle)
		return
	}
	q.insertAt(len(q.order), tracks, false)
}

// insertAt inserts tracks so that they play at order position at (> pos).
// In Tracks they go right after the track that precedes them in play order,
// so without shuffle Tracks stays in play order.
func (q *Queue) insertAt(at int, tracks []subsonic.Song, queued bool) {
	k := len(tracks)
	ti := q.order[at-1] + 1
	q.Tracks = append(q.Tracks[:ti], append(append([]subsonic.Song(nil), tracks...), q.Tracks[ti:]...)...)
	flags := make([]bool, k)
	for i := range flags {
		flags[i] = queued
	}
	q.queued = append(q.queued[:ti], append(flags, q.queued[ti:]...)...)
	for p, i := range q.order {
		if i >= ti {
			q.order[p] = i + k
		}
	}
	ins := make([]int, k)
	for j := range ins {
		ins[j] = ti + j
	}
	q.order = append(q.order[:at], append(ins, q.order[at:]...)...)
}

// Entry is a track in play order.
type Entry struct {
	Song subsonic.Song
	// Index is the track's index for Jump and Remove.
	Index int
	// Queued is set for upcoming tracks added by the user.
	Queued bool
}

// Entries returns the tracks in play order and the position of the current
// one (-1 if none).
func (q *Queue) Entries() ([]Entry, int) {
	out := make([]Entry, len(q.order))
	for p, i := range q.order {
		out[p] = Entry{Song: q.Tracks[i], Index: i, Queued: q.queued[i] && p > q.pos}
	}
	if q.pos >= len(q.order) {
		return out, -1
	}
	return out, q.pos
}

// Remaining returns the number of tracks after the current one in play
// order (ignoring repeat).
func (q *Queue) Remaining() int {
	if q.pos < 0 {
		return 0
	}
	return len(q.order) - q.pos - 1
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
			q.setPos(0)
			return true
		}
		q.reshuffleCycle()
		q.setPos(0)
		return true
	}
	q.setPos(q.pos + 1)
	return true
}

// Previous moves back one track, wrapping to the end.
func (q *Queue) Previous() bool {
	if q.pos < 0 {
		return false
	}
	if q.pos > 0 {
		q.setPos(q.pos - 1)
		return true
	}
	q.setPos(len(q.order) - 1)
	return true
}

// Jump makes track index i current.
func (q *Queue) Jump(i int) bool {
	if i < 0 || i >= len(q.Tracks) {
		return false
	}
	for p, idx := range q.order {
		if idx == i {
			q.setPos(p)
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
	q.queued = append(q.queued[:i], q.queued[i+1:]...)
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
	if removedCurrent && q.pos >= 0 {
		q.setPos(q.pos)
	}
	return removedCurrent
}

// Clear empties the queue.
func (q *Queue) Clear() {
	q.Tracks, q.queued, q.order, q.pos = nil, nil, nil, -1
}

// State is the persisted queue state.
type State struct {
	Tracks   []subsonic.Song `json:"tracks"`
	Order    []int           `json:"order"`
	Pos      int             `json:"pos"`
	Shuffle  bool            `json:"shuffle"`
	Repeat   Repeat          `json:"repeat"`
	Position time.Duration   `json:"position"`
	// Queued flags user-queued tracks (parallel to Tracks).
	Queued []bool `json:"queued,omitempty"`
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
		Queued:   append([]bool(nil), q.queued...),
	}
}

// Restore loads a snapshot, validating it.
func (q *Queue) Restore(s State) {
	q.Tracks = s.Tracks
	q.queued = make([]bool, len(s.Tracks))
	if len(s.Queued) == len(s.Tracks) {
		copy(q.queued, s.Queued)
	}
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
		q.order, q.pos = nil, -1
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
