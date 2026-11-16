package picker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/cmerk2021/navigo/internal/subsonic"
)

const (
	pageSize       = 60
	requestTimeout = 30 * time.Second

	searchArtists = 12
	searchAlbums  = 30
	searchSongs   = 40
)

// loadState tracks one asynchronously loaded collection. seq is bumped on
// every reset so that responses of superseded requests are discarded.
type loadState struct {
	seq     int
	loading bool
	loaded  bool
	done    bool
	err     string
}

type albumList struct {
	typ    string
	albums []subsonic.Album
	loadState
}

type artistView struct {
	id     string
	name   string
	albums []subsonic.Album
	loadState
}

type searchState struct {
	query   string
	res     *subsonic.SearchResult
	loading bool
	err     string
}

// data is everything written by background goroutines; guarded by Panel.mu.
type data struct {
	// version is bumped on every change so the UI can cache derived rows.
	version   int
	client    *subsonic.Client
	ctx       context.Context
	cancel    context.CancelFunc
	albums    [numTabs]albumList
	playlists []subsonic.Playlist
	plState   loadState
	artists   []subsonic.Artist
	arState   loadState
	artist    *artistView
	search    searchState
	busy      int
	closeReq  bool
}

func albumListType(t tab) string {
	switch t {
	case tabRandom:
		return "random"
	case tabRecent:
		return "recent"
	default:
		return "alphabeticalByName"
	}
}

// resetLocked drops all loaded data and cancels in-flight requests.
func (p *Panel) resetLocked(c *subsonic.Client) {
	if p.d.cancel != nil {
		p.d.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	old := p.d
	p.d = data{
		version: old.version + 1,
		client:  c,
		ctx:     ctx,
		cancel:  cancel,
		plState: loadState{seq: old.plState.seq + 1},
		arState: loadState{seq: old.arState.seq + 1},
	}
	for i := range p.d.albums {
		p.d.albums[i] = albumList{typ: albumListType(tab(i)), loadState: loadState{seq: old.albums[i].seq + 1}}
	}
}

// resetAlbumListLocked forgets one album list so it is reloaded on demand.
func (p *Panel) resetAlbumListLocked(t tab) {
	l := &p.d.albums[t]
	*l = albumList{typ: l.typ, loadState: loadState{seq: l.seq + 1}}
	p.d.version++
}

func (p *Panel) requestCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(p.d.ctx, requestTimeout)
}

func canceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// loadAlbumPage fetches the next page of an album-list tab if needed.
func (p *Panel) loadAlbumPage(t tab) {
	p.mu.Lock()
	l := &p.d.albums[t]
	c := p.d.client
	if c == nil || l.loading || l.done || l.err != "" {
		p.mu.Unlock()
		return
	}
	l.loading = true
	seq, typ, offset := l.seq, l.typ, len(l.albums)
	ctx, cancel := p.requestCtx()
	p.d.version++
	p.mu.Unlock()

	go func() {
		defer cancel()
		page, err := c.GetAlbumList2(ctx, typ, pageSize, offset)
		p.mu.Lock()
		l := &p.d.albums[t]
		if l.seq != seq || p.d.client != c {
			p.mu.Unlock()
			return
		}
		l.loading, l.loaded = false, true
		if err != nil {
			l.err = err.Error()
		} else {
			l.albums = append(l.albums[:len(l.albums):len(l.albums)], page...)
			// Random lists ignore the offset, so a single page is all there is.
			if len(page) < pageSize || t == tabRandom {
				l.done = true
			}
		}
		p.d.version++
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

func (p *Panel) loadPlaylists() {
	p.mu.Lock()
	s := &p.d.plState
	c := p.d.client
	if c == nil || s.loading || s.loaded {
		p.mu.Unlock()
		return
	}
	s.loading = true
	seq := s.seq
	ctx, cancel := p.requestCtx()
	p.d.version++
	p.mu.Unlock()

	go func() {
		defer cancel()
		pls, err := c.GetPlaylists(ctx)
		p.mu.Lock()
		s := &p.d.plState
		if s.seq != seq || p.d.client != c {
			p.mu.Unlock()
			return
		}
		s.loading, s.loaded, s.done = false, true, true
		if err != nil {
			s.err = err.Error()
		} else {
			p.d.playlists = pls
		}
		p.d.version++
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

func (p *Panel) loadArtists() {
	p.mu.Lock()
	s := &p.d.arState
	c := p.d.client
	if c == nil || s.loading || s.loaded {
		p.mu.Unlock()
		return
	}
	s.loading = true
	seq := s.seq
	ctx, cancel := p.requestCtx()
	p.d.version++
	p.mu.Unlock()

	go func() {
		defer cancel()
		artists, err := c.GetArtists(ctx)
		p.mu.Lock()
		s := &p.d.arState
		if s.seq != seq || p.d.client != c {
			p.mu.Unlock()
			return
		}
		s.loading, s.loaded, s.done = false, true, true
		if err != nil {
			s.err = err.Error()
		} else {
			p.d.artists = artists
		}
		p.d.version++
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

// openArtist switches to the album sub-view of an artist.
func (p *Panel) openArtist(id, name string) {
	p.mu.Lock()
	c := p.d.client
	if c == nil {
		p.mu.Unlock()
		return
	}
	seq := 1
	if p.d.artist != nil {
		seq = p.d.artist.seq + 1
	}
	av := &artistView{id: id, name: name, loadState: loadState{seq: seq, loading: true}}
	p.d.artist = av
	ctx, cancel := p.requestCtx()
	p.d.version++
	p.mu.Unlock()

	go func() {
		defer cancel()
		a, err := c.GetArtist(ctx, id)
		p.mu.Lock()
		if p.d.artist != av {
			p.mu.Unlock()
			return
		}
		av.loading, av.loaded, av.done = false, true, true
		if err != nil {
			av.err = err.Error()
		} else {
			av.albums = a.Album
			if a.Name != "" {
				av.name = a.Name
			}
		}
		p.d.version++
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

// closeArtist leaves the artist sub-view; it reports whether one was open.
func (p *Panel) closeArtist() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.d.artist == nil {
		return false
	}
	p.d.artist = nil
	p.d.version++
	return true
}

// startSearch runs search3 for q, cancelling any previous search.
func (p *Panel) startSearch(q string) {
	p.cancelSearch()
	p.mu.Lock()
	c := p.d.client
	if c == nil {
		p.mu.Unlock()
		return
	}
	ctx, cancel := p.requestCtx()
	p.searchCancel = cancel
	prev := p.d.search.res
	p.d.search = searchState{query: q, res: prev, loading: true}
	p.d.version++
	p.mu.Unlock()

	go func() {
		res, err := c.Search3(ctx, q, searchArtists, searchAlbums, searchSongs)
		p.mu.Lock()
		if p.d.search.query != q || canceled(ctx.Err()) {
			p.mu.Unlock()
			return
		}
		p.d.search.loading = false
		if err != nil {
			p.d.search.err = err.Error()
			p.d.search.res = nil
		} else {
			p.d.search.res = res
		}
		p.d.version++
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

func (p *Panel) cancelSearch() {
	if p.searchCancel != nil {
		p.searchCancel()
		p.searchCancel = nil
	}
}

// clearSearchData forgets the current search results.
func (p *Panel) clearSearchData() {
	p.cancelSearch()
	p.searchPending = false
	p.mu.Lock()
	p.d.search = searchState{}
	p.d.version++
	p.mu.Unlock()
}

// beginBusy marks a play fetch in progress and returns the client to use.
func (p *Panel) beginBusy() (*subsonic.Client, context.Context, context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.d.client == nil {
		return nil, nil, nil
	}
	p.d.busy++
	p.d.version++
	ctx, cancel := p.requestCtx()
	return p.d.client, ctx, cancel
}

// finishPlay ends a play fetch: on success it starts playback and asks the
// UI goroutine to close the panel.
func (p *Panel) finishPlay(songs []subsonic.Song, start int, shuffle bool, err error) {
	p.mu.Lock()
	p.d.busy--
	p.d.version++
	p.mu.Unlock()
	switch {
	case err != nil:
		if !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
	case len(songs) == 0:
		p.e.Toast("Nothing to play", true)
	default:
		if shuffle {
			// A negative start lets the queue pick a random first track.
			start = -1
		}
		p.e.Player.PlayTracks(songs, start, shuffle)
		p.mu.Lock()
		p.d.closeReq = true
		p.mu.Unlock()
	}
	p.e.Invalidate()
}

// songsFor fetches the songs of an album, a playlist or an artist (all
// albums, oldest first).
func songsFor(ctx context.Context, c *subsonic.Client, it item) ([]subsonic.Song, error) {
	switch it.kind {
	case kindAlbum:
		a, err := c.GetAlbum(ctx, it.id)
		if err != nil {
			return nil, err
		}
		return a.Song, nil
	case kindPlaylist:
		pl, err := c.GetPlaylist(ctx, it.id)
		if err != nil {
			return nil, err
		}
		return pl.Entry, nil
	default:
		return artistSongs(ctx, c, it.id)
	}
}

// artistSongs fetches the songs of all albums of an artist.
func artistSongs(ctx context.Context, c *subsonic.Client, id string) ([]subsonic.Song, error) {
	a, err := c.GetArtist(ctx, id)
	if err != nil {
		return nil, err
	}
	albums := a.Album
	year := func(y int) int {
		if y <= 0 {
			return math.MaxInt
		}
		return y
	}
	sort.SliceStable(albums, func(i, j int) bool { return year(albums[i].Year) < year(albums[j].Year) })
	res := make([][]subsonic.Song, len(albums))
	errs := make([]error, len(albums))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, al := range albums {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if a, err := c.GetAlbum(ctx, al.ID); err != nil {
				errs[i] = err
			} else {
				res[i] = a.Song
			}
		}()
	}
	wg.Wait()
	var out []subsonic.Song
	for _, s := range res {
		out = append(out, s...)
	}
	if len(out) == 0 {
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// playItem plays an album, playlist or artist, replacing the queue.
func (p *Panel) playItem(it item, shuffle bool) {
	c, ctx, cancel := p.beginBusy()
	if c == nil {
		return
	}
	go func() {
		defer cancel()
		songs, err := songsFor(ctx, c, it)
		p.finishPlay(songs, 0, shuffle, err)
	}()
}

// enqueue adds an item's songs (or a single song when it is nil) to the
// play queue without closing the picker.
func (p *Panel) enqueue(it *item, song subsonic.Song, next bool) {
	c, ctx, cancel := p.beginBusy()
	if c == nil {
		return
	}
	go func() {
		defer cancel()
		songs, err := []subsonic.Song{song}, error(nil)
		if it != nil {
			songs, err = songsFor(ctx, c, *it)
		}
		p.mu.Lock()
		p.d.busy--
		p.d.version++
		p.mu.Unlock()
		switch {
		case err != nil:
			if !canceled(err) {
				p.e.Toast(err.Error(), true)
			}
		case len(songs) == 0:
			p.e.Toast("Nothing to add", true)
		default:
			p.e.Player.Enqueue(songs, next)
			p.e.Toast(enqueueMessage(songs, next), false)
		}
		p.e.Invalidate()
	}()
}

func enqueueMessage(songs []subsonic.Song, next bool) string {
	what := fmt.Sprintf("%d songs", len(songs))
	if len(songs) == 1 {
		what = "“" + songs[0].Title + "”"
	}
	if next {
		return what + " will play next"
	}
	return "Added " + what + " to the queue"
}

// shuffleLibrary plays the whole library shuffled.
func (p *Panel) shuffleLibrary() {
	c, ctx, cancel := p.beginBusy()
	if c == nil {
		return
	}
	go func() {
		defer cancel()
		err := p.e.Player.ShuffleLibrary(ctx)
		p.mu.Lock()
		p.d.busy--
		p.d.version++
		if err == nil {
			p.d.closeReq = true
		}
		p.mu.Unlock()
		if err != nil && !canceled(err) {
			p.e.Toast(err.Error(), true)
		}
		p.e.Invalidate()
	}()
}

// playSong plays s followed by the rest of its album.
func (p *Panel) playSong(s subsonic.Song) {
	c, ctx, cancel := p.beginBusy()
	if c == nil {
		return
	}
	go func() {
		defer cancel()
		if s.AlbumID != "" {
			if a, err := c.GetAlbum(ctx, s.AlbumID); err == nil {
				for i, t := range a.Song {
					if t.ID == s.ID {
						p.finishPlay(a.Song, i, false, nil)
						return
					}
				}
			} else if canceled(err) {
				p.finishPlay(nil, 0, false, err)
				return
			}
		}
		p.finishPlay([]subsonic.Song{s}, 0, false, nil)
	}()
}
