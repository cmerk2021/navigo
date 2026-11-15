// Package mpris exposes the player on the session bus via the MPRIS2 D-Bus
// interface so desktop widgets, media keys and playerctl can control it.
package mpris

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

const (
	busName     = "org.mpris.MediaPlayer2.navigo"
	objectPath  = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	ifaceRoot   = "org.mpris.MediaPlayer2"
	ifacePlayer = "org.mpris.MediaPlayer2.Player"
	ifaceProps  = "org.freedesktop.DBus.Properties"
	ifaceIntro  = "org.freedesktop.DBus.Introspectable"
)

// Player is implemented by the application to receive MPRIS commands.
type Player interface {
	Play()
	Pause()
	PlayPause()
	Stop()
	Next()
	Previous()
	Seek(offset time.Duration)                     // relative
	SetPosition(trackID string, pos time.Duration) // trackID is the app's raw track id
	SetVolume(v float64)                           // 0..1
	SetLoopStatus(status string)                   // "None" | "Track" | "Playlist"
	SetShuffle(on bool)
	Position() time.Duration
	Raise()
	Quit()
}

// Metadata describes the current track. An empty TrackID means no track.
type Metadata struct {
	TrackID, Title, Artist, Album, AlbumArtist, ArtURL string
	Length                                             time.Duration
}

// Status describes the player state.
type Status struct {
	PlaybackStatus                                       string // "Playing" | "Paused" | "Stopped"
	LoopStatus                                           string // "None" | "Track" | "Playlist"
	Shuffle                                              bool
	Volume                                               float64
	CanGoNext, CanGoPrevious, CanPlay, CanPause, CanSeek bool
}

// Server is an MPRIS2 server on the session bus.
type Server struct {
	conn *dbus.Conn
	name string
	p    Player

	mu    sync.Mutex
	meta  Metadata
	st    Status
	props map[string]dbus.Variant // current Player props, excluding Position
}

// New connects to the session bus, exports the MPRIS objects and requests the
// bus name, falling back to a per-process instance name if it is taken.
func New(p Player) (*Server, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("mpris: connect session bus: %w", err)
	}
	s := &Server{conn: conn, p: p}
	s.st = normalizeStatus(Status{})
	s.props = playerProps(s.meta, s.st)

	exports := []struct {
		v       any
		mapping map[string]string
		iface   string
	}{
		{rootIface{s}, nil, ifaceRoot},
		// Seek is renamed in Go to avoid clashing with io.Seeker's signature.
		{playerIface{s}, map[string]string{"SeekBy": "Seek"}, ifacePlayer},
		{propsIface{s}, nil, ifaceProps},
		{introspect.Introspectable(introspectXML), nil, ifaceIntro},
	}
	for _, e := range exports {
		if err := conn.ExportWithMap(e.v, e.mapping, objectPath, e.iface); err != nil {
			conn.Close()
			return nil, fmt.Errorf("mpris: export %s: %w", e.iface, err)
		}
	}

	for _, name := range []string{busName, fmt.Sprintf("%s.instance%d", busName, os.Getpid())} {
		reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("mpris: request name: %w", err)
		}
		if reply == dbus.RequestNameReplyPrimaryOwner || reply == dbus.RequestNameReplyAlreadyOwner {
			s.name = name
			return s, nil
		}
	}
	conn.Close()
	return nil, errors.New("mpris: could not acquire a bus name")
}

// Name returns the acquired bus name.
func (s *Server) Name() string { return s.name }

// Update sets metadata and status and emits a single PropertiesChanged signal
// listing only the properties that changed. It is safe for concurrent use.
func (s *Server) Update(m Metadata, st Status) {
	st = normalizeStatus(st)
	next := playerProps(m, st)

	s.mu.Lock()
	defer s.mu.Unlock()
	changed := diffProps(s.props, next)
	s.meta, s.st, s.props = m, st, next
	if len(changed) == 0 {
		return
	}
	// Emitting under the lock keeps signal order consistent with state order.
	s.conn.Emit(objectPath, ifaceProps+".PropertiesChanged", ifacePlayer, changed, []string{})
}

// Seeked emits the Seeked signal with the new absolute position.
func (s *Server) Seeked(pos time.Duration) {
	s.conn.Emit(objectPath, ifacePlayer+".Seeked", usec(pos))
}

// Close releases the bus name and closes the connection.
func (s *Server) Close() error {
	if s.name != "" {
		s.conn.ReleaseName(s.name)
	}
	return s.conn.Close()
}

func (s *Server) state() (Metadata, Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta, s.st
}

// rootIface implements org.mpris.MediaPlayer2.
type rootIface struct{ s *Server }

func (r rootIface) Raise() *dbus.Error { r.s.p.Raise(); return nil }
func (r rootIface) Quit() *dbus.Error  { r.s.p.Quit(); return nil }

// playerIface implements org.mpris.MediaPlayer2.Player. Player callbacks are
// invoked without holding the server lock so they may call Update.
type playerIface struct{ s *Server }

func (pi playerIface) Next() *dbus.Error {
	if _, st := pi.s.state(); st.CanGoNext {
		pi.s.p.Next()
	}
	return nil
}

func (pi playerIface) Previous() *dbus.Error {
	if _, st := pi.s.state(); st.CanGoPrevious {
		pi.s.p.Previous()
	}
	return nil
}

func (pi playerIface) Pause() *dbus.Error {
	if _, st := pi.s.state(); st.CanPause {
		pi.s.p.Pause()
	}
	return nil
}

func (pi playerIface) PlayPause() *dbus.Error {
	if _, st := pi.s.state(); st.CanPause || st.CanPlay {
		pi.s.p.PlayPause()
	}
	return nil
}

func (pi playerIface) Stop() *dbus.Error { pi.s.p.Stop(); return nil }

func (pi playerIface) Play() *dbus.Error {
	if _, st := pi.s.state(); st.CanPlay {
		pi.s.p.Play()
	}
	return nil
}

func (pi playerIface) SeekBy(offset int64) *dbus.Error {
	if _, st := pi.s.state(); st.CanSeek {
		pi.s.p.Seek(time.Duration(offset) * time.Microsecond)
	}
	return nil
}

func (pi playerIface) SetPosition(track dbus.ObjectPath, pos int64) *dbus.Error {
	m, st := pi.s.state()
	if !st.CanSeek || pos < 0 {
		return nil
	}
	d := time.Duration(pos) * time.Microsecond
	if m.Length > 0 && d > m.Length {
		return nil
	}
	id, ok := resolveTrackID(track, m.TrackID)
	if !ok {
		return nil
	}
	pi.s.p.SetPosition(id, d)
	return nil
}

func (pi playerIface) OpenUri(uri string) *dbus.Error {
	return dbus.NewError("org.mpris.MediaPlayer2.Player.Error.NotSupported", []any{"OpenUri is not supported"})
}

// propsIface implements org.freedesktop.DBus.Properties.
type propsIface struct{ s *Server }

func errUnknownInterface(iface string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", []any{"unknown interface " + iface})
}

func errUnknownProperty(prop string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{"unknown property " + prop})
}

func errInvalidArgs(msg string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{msg})
}

func (pr propsIface) all(iface string) (map[string]dbus.Variant, *dbus.Error) {
	switch iface {
	case ifaceRoot:
		return rootProps(), nil
	case ifacePlayer:
		pr.s.mu.Lock()
		out := make(map[string]dbus.Variant, len(pr.s.props)+1)
		for k, v := range pr.s.props {
			out[k] = v
		}
		pr.s.mu.Unlock()
		out["Position"] = dbus.MakeVariant(usec(pr.s.p.Position()))
		return out, nil
	}
	return nil, errUnknownInterface(iface)
}

// resolveIface picks the interface for an empty interface name, as the
// Properties spec permits.
func resolveIface(iface, prop string) string {
	if iface != "" {
		return iface
	}
	if _, ok := rootProps()[prop]; ok {
		return ifaceRoot
	}
	return ifacePlayer
}

func (pr propsIface) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	iface = resolveIface(iface, prop)
	switch iface {
	case ifaceRoot:
		if v, ok := rootProps()[prop]; ok {
			return v, nil
		}
		return dbus.Variant{}, errUnknownProperty(prop)
	case ifacePlayer:
		if prop == "Position" {
			return dbus.MakeVariant(usec(pr.s.p.Position())), nil
		}
		pr.s.mu.Lock()
		v, ok := pr.s.props[prop]
		pr.s.mu.Unlock()
		if ok {
			return v, nil
		}
		return dbus.Variant{}, errUnknownProperty(prop)
	}
	return dbus.Variant{}, errUnknownInterface(iface)
}

func (pr propsIface) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	return pr.all(iface)
}

func (pr propsIface) Set(iface, prop string, value dbus.Variant) *dbus.Error {
	iface = resolveIface(iface, prop)
	var known bool
	switch iface {
	case ifaceRoot:
		_, known = rootProps()[prop]
	case ifacePlayer:
		_, known = playerProps(Metadata{}, Status{})[prop]
		known = known || prop == "Position"
	default:
		return errUnknownInterface(iface)
	}
	if !known {
		return errUnknownProperty(prop)
	}
	switch iface + "." + prop {
	case ifaceRoot + ".Fullscreen":
		// CanSetFullscreen is false; ignore.
		return nil
	case ifacePlayer + ".Volume":
		v, ok := toFloat(value.Value())
		if !ok {
			return errInvalidArgs("Volume must be a double")
		}
		pr.s.p.SetVolume(clampVolume(v))
		return nil
	case ifacePlayer + ".Rate":
		v, ok := toFloat(value.Value())
		if !ok {
			return errInvalidArgs("Rate must be a double")
		}
		if v != 1.0 {
			return errInvalidArgs("only Rate 1.0 is supported")
		}
		return nil
	case ifacePlayer + ".LoopStatus":
		v, ok := value.Value().(string)
		if !ok || !validLoopStatus(v) {
			return errInvalidArgs("LoopStatus must be one of None, Track, Playlist")
		}
		pr.s.p.SetLoopStatus(v)
		return nil
	case ifacePlayer + ".Shuffle":
		v, ok := value.Value().(bool)
		if !ok {
			return errInvalidArgs("Shuffle must be a boolean")
		}
		pr.s.p.SetShuffle(v)
		return nil
	}
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []any{"property " + prop + " is read-only"})
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case int16:
		return float64(n), true
	case uint16:
		return float64(n), true
	case byte:
		return float64(n), true
	}
	return 0, false
}
