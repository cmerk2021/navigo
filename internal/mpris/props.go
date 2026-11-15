package mpris

import (
	"reflect"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	trackPathPrefix = "/org/mpris/MediaPlayer2/navigo/track/"
	noTrackPath     = dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack")
)

// trackObjectPath maps an application track id to an MPRIS track object path.
func trackObjectPath(id string) dbus.ObjectPath {
	if id == "" {
		return noTrackPath
	}
	var b strings.Builder
	b.Grow(len(trackPathPrefix) + len(id))
	b.WriteString(trackPathPrefix)
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return dbus.ObjectPath(b.String())
}

// resolveTrackID maps an object path back to the raw id of the current track.
func resolveTrackID(path dbus.ObjectPath, currentID string) (string, bool) {
	if currentID == "" || path != trackObjectPath(currentID) {
		return "", false
	}
	return currentID, true
}

func usec(d time.Duration) int64 { return d.Microseconds() }

func metadataMap(m Metadata) map[string]dbus.Variant {
	out := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(trackObjectPath(m.TrackID)),
	}
	if m.TrackID == "" && m.Title == "" {
		return out
	}
	if m.Length > 0 {
		out["mpris:length"] = dbus.MakeVariant(usec(m.Length))
	}
	out["xesam:title"] = dbus.MakeVariant(m.Title)
	if m.Artist != "" {
		out["xesam:artist"] = dbus.MakeVariant([]string{m.Artist})
	}
	if m.Album != "" {
		out["xesam:album"] = dbus.MakeVariant(m.Album)
	}
	if m.AlbumArtist != "" {
		out["xesam:albumArtist"] = dbus.MakeVariant([]string{m.AlbumArtist})
	}
	if m.ArtURL != "" {
		out["mpris:artUrl"] = dbus.MakeVariant(m.ArtURL)
	}
	return out
}

func normalizeStatus(st Status) Status {
	switch st.PlaybackStatus {
	case "Playing", "Paused", "Stopped":
	default:
		st.PlaybackStatus = "Stopped"
	}
	if !validLoopStatus(st.LoopStatus) {
		st.LoopStatus = "None"
	}
	st.Volume = clampVolume(st.Volume)
	return st
}

func validLoopStatus(s string) bool {
	return s == "None" || s == "Track" || s == "Playlist"
}

func clampVolume(v float64) float64 {
	if v < 0 || v != v {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// playerProps returns all Player interface properties except Position.
func playerProps(m Metadata, st Status) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"PlaybackStatus": dbus.MakeVariant(st.PlaybackStatus),
		"LoopStatus":     dbus.MakeVariant(st.LoopStatus),
		"Rate":           dbus.MakeVariant(1.0),
		"Shuffle":        dbus.MakeVariant(st.Shuffle),
		"Metadata":       dbus.MakeVariant(metadataMap(m)),
		"Volume":         dbus.MakeVariant(st.Volume),
		"MinimumRate":    dbus.MakeVariant(1.0),
		"MaximumRate":    dbus.MakeVariant(1.0),
		"CanGoNext":      dbus.MakeVariant(st.CanGoNext),
		"CanGoPrevious":  dbus.MakeVariant(st.CanGoPrevious),
		"CanPlay":        dbus.MakeVariant(st.CanPlay),
		"CanPause":       dbus.MakeVariant(st.CanPause),
		"CanSeek":        dbus.MakeVariant(st.CanSeek),
		"CanControl":     dbus.MakeVariant(true),
	}
}

func rootProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"CanQuit":             dbus.MakeVariant(true),
		"CanRaise":            dbus.MakeVariant(true),
		"Fullscreen":          dbus.MakeVariant(false),
		"CanSetFullscreen":    dbus.MakeVariant(false),
		"HasTrackList":        dbus.MakeVariant(false),
		"Identity":            dbus.MakeVariant("Navigo"),
		"DesktopEntry":        dbus.MakeVariant("navigo"),
		"SupportedUriSchemes": dbus.MakeVariant([]string{}),
		"SupportedMimeTypes":  dbus.MakeVariant([]string{}),
	}
}

// diffProps returns the entries of next whose value differs from (or is missing in) prev.
func diffProps(prev, next map[string]dbus.Variant) map[string]dbus.Variant {
	changed := map[string]dbus.Variant{}
	for k, v := range next {
		if old, ok := prev[k]; !ok || old.Signature() != v.Signature() || !reflect.DeepEqual(old.Value(), v.Value()) {
			changed[k] = v
		}
	}
	return changed
}
