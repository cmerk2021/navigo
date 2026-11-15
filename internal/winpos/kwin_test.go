package winpos

import (
	"bufio"
	"context"
	"encoding/json"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// privateBus starts a dbus-daemon for the test and returns its address.
func privateBus(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skip("dbus-daemon:", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

// fakeKWin emulates KWin's scripting interface: it "runs" a script by
// parsing its parameters and calling back the helper like the real script.
type fakeKWin struct {
	conn *dbus.Conn
	mu   sync.Mutex
	win  image.Rectangle
	area image.Rectangle
	// silent makes scripts never report back.
	silent  bool
	nextID  int32
	scripts map[int32]string // id → path
	plugins map[string]int32
	loaded  map[string]bool
}

func (f *fakeKWin) LoadScript(path, plugin string) (int32, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	f.scripts[id] = path
	f.plugins[plugin] = id
	f.loaded[plugin] = true
	f.conn.ExportWithMap(&fakeScript{f, id}, map[string]string{"Run": "run"}, dbus.ObjectPath("/Scripting/Script"+itoa(id)), scriptIface)
	return id, nil
}

func (f *fakeKWin) UnloadScript(plugin string) (bool, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := f.loaded[plugin]
	delete(f.loaded, plugin)
	return ok, nil
}

func itoa(n int32) string { b, _ := json.Marshal(n); return string(b) }

type fakeScript struct {
	f  *fakeKWin
	id int32
}

func (s *fakeScript) Run() *dbus.Error {
	f := s.f
	f.mu.Lock()
	src, err := os.ReadFile(f.scripts[s.id])
	silent := f.silent
	f.mu.Unlock()
	if err != nil {
		return dbus.MakeFailedError(err)
	}
	if silent {
		return nil
	}
	// Extract "var P = {...};".
	text := string(src)
	i := strings.Index(text, "var P = ")
	j := strings.Index(text[i:], ";\n")
	var p struct {
		Token, Svc, Cls, Title string
		Set                    *struct{ X, Y, W, H int }
	}
	if err := json.Unmarshal([]byte(text[i+8:i+j]), &p); err != nil {
		return dbus.MakeFailedError(err)
	}
	f.mu.Lock()
	if p.Set != nil {
		f.win = image.Rect(p.Set.X, p.Set.Y, p.Set.X+p.Set.W, p.Set.Y+p.Set.H)
	}
	w, a := f.win, f.area
	f.mu.Unlock()
	res, _ := json.Marshal(map[string]int{"x": w.Min.X, "y": w.Min.Y, "w": w.Dx(), "h": w.Dy(),
		"ax": a.Min.X, "ay": a.Min.Y, "aw": a.Dx(), "ah": a.Dy()})
	go f.conn.Object(p.Svc, HelperPath).Call(HelperIface+".Result", 0, p.Token, string(res))
	return nil
}

func startFakeKWin(t *testing.T, addr string) *fakeKWin {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	f := &fakeKWin{conn: conn, scripts: map[int32]string{}, plugins: map[string]int32{}, loaded: map[string]bool{}}
	conn.ExportWithMap(f, map[string]string{"LoadScript": "loadScript", "UnloadScript": "unloadScript"}, scriptingPath, scriptingIface)
	node := &introspect.Node{Interfaces: []introspect.Interface{{
		Name:    scriptingIface,
		Methods: []introspect.Method{{Name: "loadScript"}, {Name: "unloadScript"}},
	}}}
	conn.Export(introspect.NewIntrospectable(node), scriptingPath, "org.freedesktop.DBus.Introspectable")
	if r, err := conn.RequestName(kwinService, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal("request name:", r, err)
	}
	return f
}

func TestKWinPlacer(t *testing.T) {
	addr := privateBus(t)
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := newKWin(conn, "navigo", "Navigo"); err == nil {
		t.Fatal("KWin detected without KWin on the bus")
	}

	f := startFakeKWin(t, addr)
	f.win = image.Rect(1550, 972, 1920, 1036)
	f.area = image.Rect(0, 0, 1920, 1036)
	k, err := newKWin(conn, "navigo", "Navigo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(k.dir)
	ctx := context.Background()

	win := func() image.Rectangle { f.mu.Lock(); defer f.mu.Unlock(); return f.win }
	g, err := k.Geometry(ctx)
	if err != nil || g != win() {
		t.Fatalf("Geometry = %v, %v", g, err)
	}
	wa, err := k.WorkArea(ctx)
	if err != nil || wa != f.area {
		t.Fatalf("WorkArea = %v, %v", wa, err)
	}
	target, dir := Place(g, 600, wa)
	if dir != Up {
		t.Fatalf("dir = %v", dir)
	}
	if err := k.MoveResize(ctx, target); err != nil {
		t.Fatal(err)
	}
	if win() != target {
		t.Fatalf("window = %v, want %v", win(), target)
	}

	// A script that never reports back times out, and is still unloaded.
	f.mu.Lock()
	f.silent = true
	f.mu.Unlock()
	k.timeout = 200 * time.Millisecond
	if _, err := k.Geometry(ctx); err == nil {
		t.Fatal("expected a timeout")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.loaded) != 0 {
		t.Fatalf("scripts leaked: %v", f.loaded)
	}
	// Geometry, MoveResize and the timed-out query (WorkArea reused the
	// result of Geometry).
	if len(f.plugins) != 3 {
		t.Fatalf("scripts run: %d", len(f.plugins))
	}
	if left, _ := filepath.Glob(filepath.Join(k.dir, "*.js")); len(left) != 0 {
		t.Fatalf("script files left: %v", left)
	}
}

// The generated script must be valid JavaScript (checked with node when
// available).
func TestKWinScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	k := &KWin{class: "navigo", title: "Navigo", pid: 42}
	r := image.Rect(1, 2, 3, 4)
	path := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(path, []byte(k.script("t", ":1.5", &r)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("invalid script: %v\n%s", err, out)
	}
}
