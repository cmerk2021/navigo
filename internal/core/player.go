// Package core wires the audio engine, track cache, Subsonic client, queue,
// MPRIS and persistence together. The UI talks only to *Player.
package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/cache"
	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/eq"
	"github.com/cmerk2021/navigo/internal/keyring"
	"github.com/cmerk2021/navigo/internal/mpris"
	"github.com/cmerk2021/navigo/internal/queue"
	"github.com/cmerk2021/navigo/internal/subsonic"
)

// KeyringService is the Secret Service "service" attribute.
const KeyringService = "navigo"

// CoverSize is the cover-art size requested for the player background.
const CoverSize = 600

// prefetchCount is the number of upcoming tracks downloaded ahead.
const prefetchCount = 3

// Toast is a short user-facing message.
type Toast struct {
	Text  string
	Error bool
}

// Snapshot is a consistent view of the player state for drawing.
type Snapshot struct {
	Song     subsonic.Song
	HasSong  bool
	State    audio.State
	Position time.Duration
	Duration time.Duration
	// Buffered is the downloaded fraction of the current track (0..1).
	Buffered   float64
	Shuffle    bool
	Repeat     queue.Repeat
	Volume     float64
	Muted      bool
	Previewing bool
}

type previewState struct {
	token  string
	pos    time.Duration
	paused bool
}

// Player is the application controller. All methods are safe for concurrent
// use; blocking network work happens on background goroutines.
type Player struct {
	mu       sync.Mutex
	cfg      *config.Config
	client   *subsonic.Client
	eng      *audio.Engine
	tracks   *cache.Manager
	covers   *cache.Covers
	q        *queue.Queue
	mpris    *mpris.Server
	onChange func()
	toasts   chan Toast
	quit     func()
	raise    func()

	tokenCtr      int64
	curToken      string
	curSong       subsonic.Song
	curReq        cache.Request
	curEntry      *cache.Entry
	curTranscoded bool
	nextToken     string
	nextSong      subsonic.Song
	nextReq       cache.Request
	nextEntry     *cache.Entry
	failStreak    int
	scrobbled     bool
	artPath       string
	artFor        string
	preview       *previewState
	dirty         atomic.Bool

	closeOnce sync.Once
	done      chan struct{}
}

// New creates the controller. cfg is owned by the Player afterwards.
func New(cfg *config.Config) *Player {
	p := &Player{
		cfg:    cfg,
		eng:    audio.New(),
		q:      queue.New(),
		toasts: make(chan Toast, 16),
		done:   make(chan struct{}),
	}
	p.tracks = cache.New(filepath.Join(config.CacheDir(), "tracks"), int64(cfg.CacheSizeMB)<<20)
	p.covers = cache.NewCovers(filepath.Join(config.CacheDir(), "covers"), 256)
	p.eng.SetVolume(cfg.Volume)
	p.eng.SetMuted(cfg.Muted)
	p.eng.EQ().SetSettings(cfg.EQ.Settings)
	return p
}

// Start opens the audio device, connects (if configured), registers MPRIS and
// restores the persisted queue (paused). Errors are reported as toasts.
func (p *Player) Start(onChange func(), quit, raise func()) {
	p.onChange, p.quit, p.raise = onChange, quit, raise
	go func() {
		if err := p.eng.StartDevice(); err != nil {
			p.toast(true, "Audio device unavailable: %v", err)
		}
	}()
	if p.cfg.HasServer() {
		if c, err := subsonic.New(p.cfg.ServerURL, p.cfg.Username, p.password(), p.cfg.InsecureTLS); err == nil {
			p.setClient(c)
		} else {
			p.toast(true, "%v", err)
		}
	}
	if srv, err := mpris.New(mprisAdapter{p}); err == nil {
		p.mpris = srv
	}
	if st, err := queue.LoadState(); err == nil && len(st.Tracks) > 0 {
		p.mu.Lock()
		p.q.Restore(st)
		if p.client != nil {
			p.loadCurrentLocked(st.Position, true, false)
		} else if s, ok := p.q.Current(); ok {
			p.curSong = s
		}
		p.mu.Unlock()
	}
	go p.loop()
	p.publish()
}

// Close stops playback, saves state and releases resources.
func (p *Player) Close() {
	p.closeOnce.Do(func() {
		p.SaveState()
		close(p.done)
		p.eng.Close()
		p.tracks.Close()
		if p.mpris != nil {
			p.mpris.Close()
		}
	})
}

// Toasts returns the toast channel.
func (p *Player) Toasts() <-chan Toast { return p.toasts }

func (p *Player) toast(isErr bool, format string, args ...any) {
	p.toastLocked(isErr, format, args...)
	p.changed()
}

// toastLocked queues a toast; it doesn't take the lock and is safe anywhere.
func (p *Player) toastLocked(isErr bool, format string, args ...any) {
	select {
	case p.toasts <- Toast{Text: fmt.Sprintf(format, args...), Error: isErr}:
	default:
	}
}

func (p *Player) changed() {
	if p.onChange != nil {
		p.onChange()
	}
}

func (p *Player) password() string {
	if p.cfg.PasswordInKeyring {
		if pw, err := keyring.Get(KeyringService, p.cfg.Username); err == nil {
			return pw
		}
	}
	return p.cfg.Password
}

func (p *Player) setClient(c *subsonic.Client) {
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
	p.tracks.SetSource(c)
	p.covers.SetFetcher(c.GetCoverArt)
}

// Client returns the Subsonic client (nil if not configured).
func (p *Player) Client() *subsonic.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

// Covers returns the cover-art cache.
func (p *Player) Covers() *cache.Covers { return p.covers }

// Engine returns the audio engine.
func (p *Player) Engine() *audio.Engine { return p.eng }

// Config returns a copy of the configuration.
func (p *Player) Config() *config.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Clone()
}

// UpdateConfig applies f to the configuration, applies runtime-relevant
// changes and saves it.
func (p *Player) UpdateConfig(f func(c *config.Config)) {
	p.mu.Lock()
	old := p.cfg.Clone()
	f(p.cfg)
	cfg := p.cfg.Clone()
	p.mu.Unlock()
	if cfg.CacheSizeMB != old.CacheSizeMB {
		p.tracks.SetMaxBytes(int64(cfg.CacheSizeMB) << 20)
	}
	if cfg.EQ.Settings != old.EQ.Settings {
		p.eng.EQ().SetSettings(cfg.EQ.Settings)
	}
	if cfg.InsecureTLS != old.InsecureTLS && cfg.HasServer() {
		if c, err := subsonic.New(cfg.ServerURL, cfg.Username, p.password(), cfg.InsecureTLS); err == nil {
			p.setClient(c)
		}
	}
	if err := cfg.Save(); err != nil {
		p.toast(true, "Could not save settings: %v", err)
	}
	p.changed()
}

// ClearCache deletes cached tracks and covers (except files in use).
func (p *Player) ClearCache() error {
	p.covers.Clear()
	return p.tracks.Clear()
}

// CacheSize returns the bytes used by the track cache.
func (p *Player) CacheSize() int64 { return p.tracks.Size() }

// TestConnection pings a server without changing the configuration.
func TestConnection(ctx context.Context, server, user, pw string, insecure bool) error {
	c, err := subsonic.New(server, user, pw, insecure)
	if err != nil {
		return err
	}
	return c.Ping(ctx)
}

// StoredPassword returns the saved password (for pre-filling settings).
func (p *Player) StoredPassword() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.password()
}

// Connect validates the credentials and stores them (password in the
// keyring when available, otherwise in the 0600 config file).
func (p *Player) Connect(ctx context.Context, server, user, pw string, insecure bool) error {
	c, err := subsonic.New(server, user, pw, insecure)
	if err != nil {
		return err
	}
	if err := c.Ping(ctx); err != nil {
		return err
	}
	inKeyring := keyring.Set(KeyringService, user, pw) == nil
	p.mu.Lock()
	p.cfg.ServerURL = c.BaseURL()
	p.cfg.Username = user
	p.cfg.InsecureTLS = insecure
	p.cfg.PasswordInKeyring = inKeyring
	if inKeyring {
		p.cfg.Password = ""
	} else {
		p.cfg.Password = pw
	}
	cfg := p.cfg.Clone()
	p.mu.Unlock()
	p.setClient(c)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	p.changed()
	return nil
}

// ---------------- media construction ----------------

func (p *Player) requestFor(s subsonic.Song, transcode bool) (cache.Request, bool) {
	if transcode || !audio.NativeFormat(s.Suffix) {
		br := p.cfg.TranscodeBitrate
		if br <= 0 {
			br = 320
		}
		return cache.Request{ID: s.ID, Format: "mp3", BitRate: br}, true
	}
	return cache.Request{ID: s.ID}, false
}

func replayGain(s subsonic.Song) float32 {
	rg := s.ReplayGain
	if rg == nil || (rg.TrackGain == 0 && rg.AlbumGain == 0) {
		return 1
	}
	g, peak := rg.TrackGain, rg.TrackPeak
	if g == 0 {
		g, peak = rg.AlbumGain, rg.AlbumPeak
	}
	lin := math.Pow(10, g/20)
	if peak > 0 && lin*peak > 1 {
		lin = 1 / peak
	}
	return float32(lin)
}

func (p *Player) newToken(id string) string {
	p.tokenCtr++
	return fmt.Sprintf("%d:%s", p.tokenCtr, id)
}

func (p *Player) mediaFor(s subsonic.Song, req cache.Request, transcoded bool, prio cache.Priority, hook bool) (*audio.Media, *cache.Entry, string) {
	tok := p.newToken(s.ID)
	hint := strings.ToLower(s.Suffix)
	if transcoded {
		hint = req.Format
	}
	gain := float32(1)
	if p.cfg.ReplayGain {
		gain = replayGain(s)
	}
	entry := p.tracks.Get(req, prio)
	var onWait func(bool)
	if hook {
		onWait = p.eng.SetBuffering
	}
	m := audio.FromSource(tok, hint, gain, func(ctx context.Context) (audio.Source, error) {
		e := p.tracks.Get(req, cache.High)
		// Start playing once ~1-2 s (or the whole file) is available.
		if err := e.Wait(ctx, 256<<10); err != nil {
			return nil, err
		}
		return e.Open()
	}, onWait)
	return m, entry, tok
}

// loadCurrentLocked loads the queue's current track into the engine.
func (p *Player) loadCurrentLocked(pos time.Duration, paused, transcode bool) {
	s, ok := p.q.Current()
	if !ok || p.client == nil {
		p.eng.Stop()
		p.curToken, p.curSong, p.curEntry = "", subsonic.Song{}, nil
		p.clearNextLocked()
		p.dirty.Store(true)
		return
	}
	req, tc := p.requestFor(s, transcode)
	m, entry, tok := p.mediaFor(s, req, tc, cache.High, true)
	p.curToken, p.curSong, p.curReq, p.curEntry, p.curTranscoded = tok, s, req, entry, tc
	p.scrobbled = false
	p.clearNextLocked()
	p.eng.Load(m, pos, paused)
	p.updateNextLocked()
	p.fetchArtLocked()
	p.dirty.Store(true)
}

func (p *Player) clearNextLocked() {
	if p.nextToken != "" {
		p.nextToken = ""
		p.eng.SetNext(nil)
	}
}

// updateNextLocked prepares the gapless successor and prefetches upcoming
// tracks.
func (p *Player) updateNextLocked() {
	if p.preview != nil {
		return
	}
	next, ok := p.q.PeekNext()
	if !ok || p.curToken == "" {
		p.clearNextLocked()
	} else if p.nextToken == "" || p.nextSong.ID != next.ID {
		req, tc := p.requestFor(next, false)
		m, entry, tok := p.mediaFor(next, req, tc, cache.Low, true)
		p.nextToken, p.nextSong, p.nextReq, p.nextEntry = tok, next, req, entry
		p.eng.SetNext(m)
	}
	var reqs []cache.Request
	pins := []cache.Request{p.curReq}
	for _, s := range p.q.Upcoming(prefetchCount) {
		r, _ := p.requestFor(s, false)
		reqs = append(reqs, r)
		pins = append(pins, r)
	}
	p.tracks.Pin(pins...)
	p.tracks.Prefetch(reqs)
}

// ---------------- event loop ----------------

func (p *Player) loop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastSave := time.Now()
	for {
		select {
		case <-p.done:
			return
		case ev := <-p.eng.Events():
			p.handleEvent(ev)
			p.publish()
		case <-tick.C:
			p.checkScrobble()
			if p.dirty.Load() || (p.eng.State() == audio.Playing && time.Since(lastSave) > 15*time.Second) {
				p.SaveState()
				lastSave = time.Now()
			}
		}
	}
}

func (p *Player) handleEvent(ev audio.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.preview != nil && ev.ID == p.preview.token {
		if ev.Kind == audio.EvError {
			p.toastLocked(true, "Preview failed: %v", ev.Err)
		}
		return
	}
	switch ev.Kind {
	case audio.EvTrackStarted:
		switch {
		case ev.Auto && p.nextToken != "" && ev.ID == p.nextToken:
			p.q.Advance(true)
			p.curToken, p.curSong, p.curReq, p.curEntry = p.nextToken, p.nextSong, p.nextReq, p.nextEntry
			p.curTranscoded = p.curReq.Format != ""
			p.nextToken = ""
			p.scrobbled = false
			p.failStreak = 0
			p.updateNextLocked()
			p.fetchArtLocked()
			p.nowPlayingLocked()
			p.dirty.Store(true)
		case ev.ID == p.curToken:
			p.failStreak = 0
			p.nowPlayingLocked()
		}
	case audio.EvEnded:
		if ev.ID != p.curToken {
			return
		}
		if p.q.Advance(true) {
			p.loadCurrentLocked(0, false, false)
		}
	case audio.EvError:
		p.handleErrorLocked(ev)
	case audio.EvDeviceError:
		p.toastLocked(true, "Audio device error: %v (retrying)", ev.Err)
	}
}

func isNetworkErr(err error) bool {
	var he *subsonic.HTTPError
	var se *subsonic.Error
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &he) || errors.As(err, &se) || errors.As(err, &ne) || errors.As(err, &ue) ||
		errors.Is(err, subsonic.ErrTimeout) || strings.Contains(err.Error(), "not connected")
}

func (p *Player) handleErrorLocked(ev audio.Event) {
	if ev.ID == p.nextToken && ev.ID != "" {
		// The prepared successor failed; it is retried (with transcoding if
		// needed) when the current track ends.
		p.nextToken = ""
		return
	}
	if ev.ID != p.curToken {
		return
	}
	name := p.curSong.Title
	if !ev.Open {
		p.toastLocked(true, "Playback error in “%s”: %v", name, ev.Err)
		return
	}
	if !p.curTranscoded && !isNetworkErr(ev.Err) {
		// Unsupported or undecodable file: ask the server for an MP3.
		p.loadCurrentLocked(p.eng.Position(), p.eng.Paused(), true)
		return
	}
	p.toastLocked(true, "Can't play “%s”: %v", name, ev.Err)
	p.failStreak++
	if p.failStreak >= p.q.Len() || p.failStreak > 20 {
		p.failStreak = 0
		return
	}
	if p.q.Advance(false) {
		p.loadCurrentLocked(0, false, false)
	}
}

func (p *Player) nowPlayingLocked() {
	if !p.cfg.Scrobble || p.client == nil || p.curSong.ID == "" {
		return
	}
	c, id := p.client, p.curSong.ID
	go c.Scrobble(context.Background(), id, false, time.Now())
}

func (p *Player) checkScrobble() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scrobbled || !p.cfg.Scrobble || p.client == nil || p.curSong.ID == "" || p.preview != nil {
		return
	}
	dur := p.curSong.DurationTime()
	pos := p.eng.Position()
	if dur > 0 && (pos >= dur/2 || pos >= 4*time.Minute) {
		p.scrobbled = true
		c, id := p.client, p.curSong.ID
		go c.Scrobble(context.Background(), id, true, time.Now())
	}
}

// fetchArtLocked resolves the current cover's disk path for MPRIS.
func (p *Player) fetchArtLocked() {
	s := p.curSong
	if s.CoverArt == "" || p.artFor == s.CoverArt {
		return
	}
	p.artFor, p.artPath = s.CoverArt, ""
	id := s.CoverArt
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, path, err := p.covers.Load(ctx, id, CoverSize); err == nil {
			if _, err := os.Stat(path); err == nil {
				p.mu.Lock()
				if p.artFor == id {
					p.artPath = path
				}
				p.mu.Unlock()
			}
		}
		p.publish()
	}()
}

// publish pushes the state to MPRIS and the UI.
func (p *Player) publish() {
	if p.mpris != nil {
		m, st := p.mprisState()
		p.mpris.Update(m, st)
	}
	p.changed()
}

// ---------------- snapshot ----------------

// Snapshot returns the current state.
func (p *Player) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{
		Song:       p.curSong,
		HasSong:    p.curSong.ID != "",
		State:      p.eng.State(),
		Position:   p.eng.Position(),
		Duration:   p.curSong.DurationTime(),
		Shuffle:    p.q.Shuffle(),
		Repeat:     p.q.Repeat(),
		Volume:     p.eng.Volume(),
		Muted:      p.eng.Muted(),
		Previewing: p.preview != nil,
		Buffered:   1,
	}
	if p.preview != nil {
		s.Position = p.preview.pos
		s.State = audio.Paused
		if !p.preview.paused {
			s.State = audio.Playing
		}
	}
	if p.curEntry != nil {
		w, total, done, _ := p.curEntry.Progress()
		switch {
		case done:
			s.Buffered = 1
		case total > 0:
			s.Buffered = float64(w) / float64(total)
		case p.curSong.Size > 0:
			s.Buffered = math.Min(1, float64(w)/float64(p.curSong.Size))
		default:
			s.Buffered = 0
		}
	}
	if s.Duration > 0 && s.Position > s.Duration {
		s.Position = s.Duration
	}
	return s
}

// QueueView returns a copy of the queue tracks and the current index.
func (p *Player) QueueView() ([]subsonic.Song, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]subsonic.Song(nil), p.q.Tracks...), p.q.Index()
}

// ---------------- transport ----------------

// PlayTracks replaces the queue and starts playing tracks[start] (start < 0
// picks a random first track when shuffling).
func (p *Player) PlayTracks(tracks []subsonic.Song, start int, shuffle bool) {
	if len(tracks) == 0 {
		return
	}
	p.mu.Lock()
	p.endPreviewLocked(false)
	p.q.Set(tracks, start, shuffle)
	p.failStreak = 0
	p.loadCurrentLocked(0, false, false)
	p.mu.Unlock()
	p.publish()
}

// PlayPause toggles playback.
func (p *Player) PlayPause() {
	if p.eng.Paused() || p.eng.State() == audio.Stopped {
		p.Play()
	} else {
		p.Pause()
	}
}

// Play resumes (or restarts) playback.
func (p *Player) Play() {
	p.mu.Lock()
	if p.eng.State() == audio.Stopped && p.preview == nil {
		if _, ok := p.q.Current(); ok {
			p.loadCurrentLocked(0, false, false)
		}
		p.mu.Unlock()
		p.publish()
		return
	}
	p.mu.Unlock()
	p.eng.Resume()
	p.publish()
}

// Pause pauses playback.
func (p *Player) Pause() {
	p.eng.Pause()
	p.dirty.Store(true)
	p.publish()
}

// Stop stops playback.
func (p *Player) Stop() {
	p.eng.Stop()
	p.publish()
}

// Next skips to the next track.
func (p *Player) Next() {
	p.mu.Lock()
	if p.preview != nil {
		p.mu.Unlock()
		return
	}
	if p.q.Advance(false) {
		p.loadCurrentLocked(0, p.eng.Paused(), false)
	}
	p.mu.Unlock()
	p.publish()
}

// Previous restarts the track, or goes back if near its start.
func (p *Player) Previous() {
	p.mu.Lock()
	if p.preview != nil {
		p.mu.Unlock()
		return
	}
	if p.eng.Position() > 3*time.Second || p.q.Len() <= 1 {
		p.mu.Unlock()
		p.SeekTo(0)
		return
	}
	if p.q.Previous() {
		p.loadCurrentLocked(0, p.eng.Paused(), false)
	}
	p.mu.Unlock()
	p.publish()
}

// SeekTo seeks within the current track.
func (p *Player) SeekTo(pos time.Duration) {
	p.mu.Lock()
	if p.preview != nil || p.curSong.ID == "" {
		p.mu.Unlock()
		return
	}
	if d := p.curSong.DurationTime(); d > 0 && pos > d-500*time.Millisecond {
		pos = d - 500*time.Millisecond
	}
	if pos < 0 {
		pos = 0
	}
	if p.eng.State() == audio.Stopped {
		p.loadCurrentLocked(pos, true, p.curTranscoded)
	} else {
		p.eng.Seek(pos)
	}
	p.mu.Unlock()
	if p.mpris != nil {
		p.mpris.Seeked(pos)
	}
	p.dirty.Store(true)
	p.changed()
}

// SeekBy seeks relative to the current position.
func (p *Player) SeekBy(d time.Duration) { p.SeekTo(p.eng.Position() + d) }

// JumpTo plays queue index i.
func (p *Player) JumpTo(i int) {
	p.mu.Lock()
	p.endPreviewLocked(false)
	if p.q.Jump(i) {
		p.loadCurrentLocked(0, false, false)
	}
	p.mu.Unlock()
	p.publish()
}

// RemoveAt removes queue index i.
func (p *Player) RemoveAt(i int) {
	p.mu.Lock()
	if p.q.Remove(i) {
		p.loadCurrentLocked(0, p.eng.Paused(), false)
	} else {
		p.updateNextLocked()
	}
	p.dirty.Store(true)
	p.mu.Unlock()
	p.publish()
}

// ClearQueue empties the queue and stops.
func (p *Player) ClearQueue() {
	p.mu.Lock()
	p.endPreviewLocked(false)
	p.q.Clear()
	p.loadCurrentLocked(0, true, false)
	p.tracks.Prefetch(nil)
	p.mu.Unlock()
	p.publish()
}

// SetShuffle turns shuffle on/off.
func (p *Player) SetShuffle(on bool) {
	p.mu.Lock()
	p.q.SetShuffle(on)
	p.updateNextLocked()
	p.dirty.Store(true)
	p.mu.Unlock()
	p.publish()
}

// SetRepeat sets the repeat mode.
func (p *Player) SetRepeat(r queue.Repeat) {
	p.mu.Lock()
	p.q.SetRepeat(r)
	// Repeat-one needs a fresh successor token even for the same song.
	p.clearNextLocked()
	p.updateNextLocked()
	p.dirty.Store(true)
	p.mu.Unlock()
	p.publish()
}

// SetVolume sets the volume (0..1).
func (p *Player) SetVolume(v float64) {
	p.eng.SetVolume(v)
	p.mu.Lock()
	p.cfg.Volume = p.eng.Volume()
	p.mu.Unlock()
	p.dirty.Store(true)
	p.publish()
}

// SetMuted mutes/unmutes.
func (p *Player) SetMuted(m bool) {
	p.eng.SetMuted(m)
	p.mu.Lock()
	p.cfg.Muted = m
	p.mu.Unlock()
	p.dirty.Store(true)
	p.publish()
}

// SetEQ applies EQ settings live (without saving the config).
func (p *Player) SetEQ(s eq.Settings) { p.eng.EQ().SetSettings(s) }

// SaveState persists the queue and the config (volume, mode, window...).
func (p *Player) SaveState() {
	p.dirty.Store(false)
	p.mu.Lock()
	var st queue.State
	if p.preview != nil {
		st = p.q.Snapshot(p.preview.pos)
	} else {
		st = p.q.Snapshot(p.eng.Position())
	}
	cfg := p.cfg.Clone()
	p.mu.Unlock()
	queue.SaveState(st)
	cfg.Save()
}

// ---------------- EQ Assistant preview ----------------

// StartPreview loops a ~25 s section of song (the current track if nil)
// through the live EQ. The previous playback is restored by EndPreview.
func (p *Player) StartPreview(ctx context.Context, song *subsonic.Song) error {
	p.mu.Lock()
	if p.client == nil {
		p.mu.Unlock()
		return errors.New("not connected to a server")
	}
	var s subsonic.Song
	if song != nil {
		s = *song
	} else if cur, ok := p.q.Current(); ok {
		s = cur
	} else {
		p.mu.Unlock()
		return errors.New("play a song first, or choose a different song")
	}
	req, tc := p.requestFor(s, false)
	m, _, _ := p.mediaFor(s, req, tc, cache.High, false)
	p.mu.Unlock()

	dur := s.DurationTime()
	length := 25 * time.Second
	start := time.Duration(0)
	if dur > 0 {
		start = dur * 3 / 10
		if start+length > dur {
			start = max(0, dur-length)
		}
		length = min(length, dur)
	}
	pcm, err := audio.DecodeSection(ctx, m, start, length)
	if err != nil && !tc && ctx.Err() == nil {
		req, tc = p.requestFor(s, true)
		p.mu.Lock()
		m, _, _ = p.mediaFor(s, req, tc, cache.High, false)
		p.mu.Unlock()
		pcm, err = audio.DecodeSection(ctx, m, start, length)
	}
	if err != nil {
		return err
	}
	if len(pcm) < audio.SampleRate*2 {
		return errors.New("song too short for a preview")
	}
	p.mu.Lock()
	if p.preview == nil {
		p.preview = &previewState{pos: p.eng.Position(), paused: p.eng.Paused() || p.eng.State() == audio.Stopped}
	}
	p.tokenCtr++
	p.preview.token = fmt.Sprintf("preview:%d", p.tokenCtr)
	p.clearNextLocked()
	p.eng.Load(audio.LoopMedia(p.preview.token, pcm, 60*time.Millisecond), 0, false)
	p.mu.Unlock()
	p.publish()
	return nil
}

// EndPreview stops the preview loop and restores previous playback (paused
// if it was paused).
func (p *Player) EndPreview() {
	p.mu.Lock()
	p.endPreviewLocked(true)
	p.mu.Unlock()
	p.publish()
}

func (p *Player) endPreviewLocked(restore bool) {
	if p.preview == nil {
		return
	}
	pv := p.preview
	p.preview = nil
	if restore {
		if _, ok := p.q.Current(); ok {
			p.loadCurrentLocked(pv.pos, pv.paused, p.curTranscoded)
		} else {
			p.eng.Stop()
		}
	}
}

// ---------------- MPRIS ----------------

func (p *Player) mprisState() (mpris.Metadata, mpris.Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var m mpris.Metadata
	if p.curSong.ID != "" {
		m = mpris.Metadata{
			TrackID:     p.curSong.ID,
			Title:       p.curSong.Title,
			Artist:      p.curSong.Artist,
			Album:       p.curSong.Album,
			AlbumArtist: p.curSong.AlbumArtist,
			Length:      p.curSong.DurationTime(),
		}
		if p.artPath != "" {
			m.ArtURL = (&url.URL{Scheme: "file", Path: p.artPath}).String()
		}
	}
	status := "Stopped"
	switch p.eng.State() {
	case audio.Playing, audio.Buffering, audio.Loading:
		status = "Playing"
		if p.eng.Paused() {
			status = "Paused"
		}
	case audio.Paused:
		status = "Paused"
	}
	loop := map[queue.Repeat]string{queue.RepeatOff: "None", queue.RepeatAll: "Playlist", queue.RepeatOne: "Track"}[p.q.Repeat()]
	has := p.q.Len() > 0
	return m, mpris.Status{
		PlaybackStatus: status,
		LoopStatus:     loop,
		Shuffle:        p.q.Shuffle(),
		Volume:         p.eng.Volume(),
		CanGoNext:      has,
		CanGoPrevious:  has,
		CanPlay:        has,
		CanPause:       has,
		CanSeek:        p.curSong.ID != "",
	}
}

type mprisAdapter struct{ p *Player }

func (a mprisAdapter) Play()                   { a.p.Play() }
func (a mprisAdapter) Pause()                  { a.p.Pause() }
func (a mprisAdapter) PlayPause()              { a.p.PlayPause() }
func (a mprisAdapter) Stop()                   { a.p.Stop() }
func (a mprisAdapter) Next()                   { a.p.Next() }
func (a mprisAdapter) Previous()               { a.p.Previous() }
func (a mprisAdapter) Seek(d time.Duration)    { a.p.SeekBy(d) }
func (a mprisAdapter) SetVolume(v float64)     { a.p.SetVolume(v) }
func (a mprisAdapter) SetShuffle(on bool)      { a.p.SetShuffle(on) }
func (a mprisAdapter) Position() time.Duration { return a.p.eng.Position() }

func (a mprisAdapter) SetPosition(id string, d time.Duration) {
	a.p.mu.Lock()
	cur := a.p.curSong.ID
	a.p.mu.Unlock()
	if id == cur {
		a.p.SeekTo(d)
	}
}

func (a mprisAdapter) SetLoopStatus(s string) {
	switch s {
	case "Track":
		a.p.SetRepeat(queue.RepeatOne)
	case "Playlist":
		a.p.SetRepeat(queue.RepeatAll)
	default:
		a.p.SetRepeat(queue.RepeatOff)
	}
}

func (a mprisAdapter) Raise() {
	if a.p.raise != nil {
		a.p.raise()
	}
}

func (a mprisAdapter) Quit() {
	if a.p.quit != nil {
		a.p.quit()
	}
}
