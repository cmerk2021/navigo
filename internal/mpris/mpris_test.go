package mpris

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestTrackObjectPath(t *testing.T) {
	tests := map[string]dbus.ObjectPath{
		"":           noTrackPath,
		"abc123":     "/org/mpris/MediaPlayer2/navigo/track/abc123",
		"al-3f.x/y":  "/org/mpris/MediaPlayer2/navigo/track/al_3f_x_y",
		"ünï_cødé":   "/org/mpris/MediaPlayer2/navigo/track/_n__c_d_",
		"A_b-C 9":    "/org/mpris/MediaPlayer2/navigo/track/A_b_C_9",
		"0123456789": "/org/mpris/MediaPlayer2/navigo/track/0123456789",
	}
	for id, want := range tests {
		got := trackObjectPath(id)
		if got != want {
			t.Errorf("trackObjectPath(%q) = %s, want %s", id, got, want)
		}
		if !got.IsValid() {
			t.Errorf("trackObjectPath(%q) = %s is not a valid object path", id, got)
		}
	}

	if id, ok := resolveTrackID(trackObjectPath("al-3f"), "al-3f"); !ok || id != "al-3f" {
		t.Errorf("resolveTrackID = %q,%v", id, ok)
	}
	if _, ok := resolveTrackID(trackObjectPath("other"), "al-3f"); ok {
		t.Error("resolveTrackID matched a different track")
	}
	if _, ok := resolveTrackID(noTrackPath, ""); ok {
		t.Error("resolveTrackID matched NoTrack")
	}
}

func TestMetadataMap(t *testing.T) {
	m := metadataMap(Metadata{
		TrackID: "t-1", Title: "Song", Artist: "Artist", Album: "Album",
		AlbumArtist: "AA", ArtURL: "file:///tmp/a.jpg", Length: 3*time.Minute + 500*time.Millisecond,
	})
	want := map[string]any{
		"mpris:trackid":     dbus.ObjectPath("/org/mpris/MediaPlayer2/navigo/track/t_1"),
		"mpris:length":      int64(180_500_000),
		"xesam:title":       "Song",
		"xesam:artist":      []string{"Artist"},
		"xesam:album":       "Album",
		"xesam:albumArtist": []string{"AA"},
		"mpris:artUrl":      "file:///tmp/a.jpg",
	}
	if len(m) != len(want) {
		t.Fatalf("got %d keys: %v", len(m), m)
	}
	for k, w := range want {
		if got := m[k].Value(); dbus.MakeVariant(got).String() != dbus.MakeVariant(w).String() {
			t.Errorf("%s = %#v, want %#v", k, got, w)
		}
	}

	m = metadataMap(Metadata{TrackID: "x", Title: "T"})
	for _, k := range []string{"mpris:artUrl", "xesam:albumArtist", "xesam:artist", "mpris:length"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected key %s", k)
		}
	}

	m = metadataMap(Metadata{})
	if len(m) != 1 || m["mpris:trackid"].Value() != noTrackPath {
		t.Errorf("empty metadata = %v", m)
	}
}

func TestDiffProps(t *testing.T) {
	st := normalizeStatus(Status{PlaybackStatus: "Playing", Volume: 0.5, CanPlay: true})
	meta := Metadata{TrackID: "a", Title: "A"}
	prev := playerProps(meta, st)

	if d := diffProps(prev, playerProps(meta, st)); len(d) != 0 {
		t.Fatalf("identical state produced changes: %v", d)
	}

	st2 := st
	st2.PlaybackStatus = "Paused"
	st2.Volume = 0.7
	d := diffProps(prev, playerProps(meta, st2))
	if len(d) != 2 || d["PlaybackStatus"].Value() != "Paused" || d["Volume"].Value() != 0.7 {
		t.Fatalf("diff = %v", d)
	}

	meta2 := meta
	meta2.Title = "B"
	d = diffProps(prev, playerProps(meta2, st))
	if _, ok := d["Metadata"]; !ok || len(d) != 1 {
		t.Fatalf("diff = %v", d)
	}
	if _, ok := d["Position"]; ok {
		t.Fatal("Position must never be in PropertiesChanged")
	}
}

func TestNormalizeStatus(t *testing.T) {
	st := normalizeStatus(Status{Volume: 3})
	if st.PlaybackStatus != "Stopped" || st.LoopStatus != "None" || st.Volume != 1 {
		t.Fatalf("normalizeStatus = %+v", st)
	}
	if v := normalizeStatus(Status{Volume: -1}).Volume; v != 0 {
		t.Fatalf("volume = %v", v)
	}
}

// --- integration test ---

type fakePlayer struct {
	mu    sync.Mutex
	calls []string
	srv   *Server
	pos   time.Duration
	vol   float64
	loop  string
	shuf  bool
	setID string
}

func (f *fakePlayer) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakePlayer) has(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}

func (f *fakePlayer) Play()     { f.record("Play") }
func (f *fakePlayer) Pause()    { f.record("Pause") }
func (f *fakePlayer) Stop()     { f.record("Stop") }
func (f *fakePlayer) Next()     { f.record("Next") }
func (f *fakePlayer) Previous() { f.record("Previous") }
func (f *fakePlayer) Raise()    { f.record("Raise") }
func (f *fakePlayer) Quit()     { f.record("Quit") }

// PlayPause calls back into Update to verify that this does not deadlock.
func (f *fakePlayer) PlayPause() {
	f.record("PlayPause")
	f.srv.Update(Metadata{TrackID: "al-1", Title: "Song", Length: time.Minute}, Status{
		PlaybackStatus: "Paused", CanPlay: true, CanPause: true, CanSeek: true, Volume: 1,
	})
}
func (f *fakePlayer) Seek(d time.Duration) {
	f.mu.Lock()
	f.pos += d
	f.mu.Unlock()
	f.record("Seek")
}
func (f *fakePlayer) SetPosition(id string, p time.Duration) {
	f.mu.Lock()
	f.setID, f.pos = id, p
	f.mu.Unlock()
	f.record("SetPosition")
}
func (f *fakePlayer) SetVolume(v float64) {
	f.mu.Lock()
	f.vol = v
	f.mu.Unlock()
}
func (f *fakePlayer) SetLoopStatus(s string) {
	f.mu.Lock()
	f.loop = s
	f.mu.Unlock()
}
func (f *fakePlayer) SetShuffle(on bool) {
	f.mu.Lock()
	f.shuf = on
	f.mu.Unlock()
}
func (f *fakePlayer) Position() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos
}

// testBus starts a private dbus-daemon if available, otherwise uses an
// existing session bus, otherwise skips.
func testBus(t *testing.T) {
	t.Helper()
	if path, err := exec.LookPath("dbus-daemon"); err == nil {
		cmd := exec.Command(path, "--session", "--nofork", "--print-address=1")
		out, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
			line, err := bufio.NewReader(out).ReadString('\n')
			if err == nil && strings.TrimSpace(line) != "" {
				t.Setenv("DBUS_SESSION_BUS_ADDRESS", strings.TrimSpace(line))
				return
			}
		}
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus available")
	}
}

func TestServerIntegration(t *testing.T) {
	testBus(t)
	fp := &fakePlayer{pos: 42 * time.Second}
	srv, err := New(fp)
	if err != nil {
		t.Skipf("cannot create server: %v", err)
	}
	fp.srv = srv
	defer srv.Close()

	client, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sigs := make(chan *dbus.Signal, 16)
	client.Signal(sigs)
	if err := client.AddMatchSignal(dbus.WithMatchObjectPath(objectPath)); err != nil {
		t.Fatal(err)
	}

	obj := client.Object(srv.Name(), objectPath)
	get := func(iface, prop string) dbus.Variant {
		t.Helper()
		v, err := obj.GetProperty(iface + "." + prop)
		if err != nil {
			t.Fatalf("Get %s: %v", prop, err)
		}
		return v
	}

	srv.Update(Metadata{TrackID: "al-1", Title: "Song", Artist: "Me", Length: time.Minute}, Status{
		PlaybackStatus: "Playing", CanPlay: true, CanPause: true, CanSeek: true, CanGoNext: true, Volume: 1,
	})

	if v := get(ifacePlayer, "PlaybackStatus"); v.Value() != "Playing" {
		t.Errorf("PlaybackStatus = %v", v)
	}
	if v := get(ifaceRoot, "Identity"); v.Value() != "Navigo" {
		t.Errorf("Identity = %v", v)
	}
	md, ok := get(ifacePlayer, "Metadata").Value().(map[string]dbus.Variant)
	if !ok || md["xesam:title"].Value() != "Song" || md["mpris:trackid"].Value() != dbus.ObjectPath("/org/mpris/MediaPlayer2/navigo/track/al_1") {
		t.Errorf("Metadata = %v", md)
	}
	if v := get(ifacePlayer, "Position"); v.Value() != int64(42_000_000) {
		t.Errorf("Position = %v", v)
	}
	fp.Seek(time.Second)
	if v := get(ifacePlayer, "Position"); v.Value() != int64(43_000_000) {
		t.Errorf("Position not live: %v", v)
	}

	waitSig := func(name string, check func(*dbus.Signal) bool) {
		t.Helper()
		timeout := time.After(3 * time.Second)
		for {
			select {
			case s := <-sigs:
				if s.Name == name && check(s) {
					return
				}
			case <-timeout:
				t.Fatalf("signal %s not received", name)
			}
		}
	}

	if err := obj.Call(ifacePlayer+".PlayPause", 0).Err; err != nil {
		t.Fatal(err)
	}
	if !fp.has("PlayPause") {
		t.Error("PlayPause not called through")
	}
	waitSig(ifaceProps+".PropertiesChanged", func(s *dbus.Signal) bool {
		ch, _ := s.Body[1].(map[string]dbus.Variant)
		_, hasPos := ch["Position"]
		_, hasVol := ch["Volume"]
		return ch["PlaybackStatus"].Value() == "Paused" && !hasPos && !hasVol
	})

	if err := obj.Call(ifacePlayer+".SetPosition", 0, dbus.ObjectPath("/org/mpris/MediaPlayer2/navigo/track/al_1"), int64(5_000_000)).Err; err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	setID, pos := fp.setID, fp.pos
	fp.mu.Unlock()
	if setID != "al-1" || pos != 5*time.Second {
		t.Errorf("SetPosition -> id=%q pos=%v", setID, pos)
	}
	if err := obj.Call(ifacePlayer+".Seek", 0, int64(1_000_000)).Err; err != nil {
		t.Fatal(err)
	}
	if fp.Position() != 6*time.Second {
		t.Errorf("Seek -> pos=%v", fp.Position())
	}

	if err := obj.SetProperty(ifacePlayer+".Volume", dbus.MakeVariant(0.25)); err != nil {
		t.Fatal(err)
	}
	if err := obj.SetProperty(ifacePlayer+".LoopStatus", dbus.MakeVariant("Playlist")); err != nil {
		t.Fatal(err)
	}
	if err := obj.SetProperty(ifacePlayer+".Shuffle", dbus.MakeVariant(true)); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	if fp.vol != 0.25 || fp.loop != "Playlist" || !fp.shuf {
		t.Errorf("setters: vol=%v loop=%q shuf=%v", fp.vol, fp.loop, fp.shuf)
	}
	fp.mu.Unlock()

	if err := obj.SetProperty(ifacePlayer+".LoopStatus", dbus.MakeVariant("Bogus")); err == nil {
		t.Error("invalid LoopStatus accepted")
	}
	if err := obj.SetProperty(ifacePlayer+".Volume", dbus.MakeVariant("loud")); err == nil {
		t.Error("string Volume accepted")
	}
	if err := obj.SetProperty(ifacePlayer+".PlaybackStatus", dbus.MakeVariant("Playing")); err == nil {
		t.Error("read-only property set")
	}
	if _, err := obj.GetProperty(ifacePlayer + ".Nope"); err == nil {
		t.Error("unknown property returned")
	}
	if _, err := obj.GetProperty("org.example.Nope.X"); err == nil {
		t.Error("unknown interface accepted")
	}

	srv.Seeked(10 * time.Second)
	waitSig(ifacePlayer+".Seeked", func(s *dbus.Signal) bool {
		return len(s.Body) == 1 && s.Body[0] == int64(10_000_000)
	})

	var xml string
	if err := obj.Call(ifaceIntro+".Introspect", 0).Store(&xml); err != nil || !strings.Contains(xml, ifacePlayer) {
		t.Errorf("Introspect: %v", err)
	}
}
