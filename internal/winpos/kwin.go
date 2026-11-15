package winpos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

// D-Bus names of the helper object the KWin scripts report back to.
const (
	HelperPath  = "/org/navigo/Helper"
	HelperIface = "org.navigo.Helper"
)

const (
	kwinService    = "org.kde.KWin"
	scriptingPath  = "/Scripting"
	scriptingIface = "org.kde.kwin.Scripting"
	scriptIface    = "org.kde.kwin.Script"
	scriptTimeout  = 3 * time.Second
)

// KWin places the window through KWin's scripting D-Bus interface: each
// operation loads a short script that finds the window (by resource class,
// PID and caption), optionally sets its frameGeometry, and reports the
// geometry and work area back by calling Result on the helper object this
// process exports. Scripts are always unloaded afterwards.
type KWin struct {
	conn  *dbus.Conn
	class string
	title string
	pid   int
	dir   string
	// timeout bounds one script round trip.
	timeout time.Duration

	mu      sync.Mutex // one script at a time
	waiters sync.Map   // token → chan kwinResult
	seq     atomic.Int64
	last    atomic.Pointer[kwinResult]
}

type kwinResult struct {
	Error          string `json:"error"`
	X, Y, W, H     int
	AX, AY, AW, AH int
}

func (r *kwinResult) rect() image.Rectangle { return image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H) }
func (r *kwinResult) area() image.Rectangle {
	return image.Rect(r.AX, r.AY, r.AX+r.AW, r.AY+r.AH)
}

type helper struct{ k *KWin }

// Result is called by the KWin scripts.
func (h helper) Result(token, data string) *dbus.Error {
	if ch, ok := h.k.waiters.LoadAndDelete(token); ok {
		var r kwinResult
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			r.Error = "bad result: " + err.Error()
		}
		ch.(chan kwinResult) <- r
	}
	return nil
}

// NewKWin connects to the session bus and returns a KWin placer if KWin's
// scripting interface is available. class is the window's resource class
// (Wayland app_id / X11 WM_CLASS) and title its caption.
func NewKWin(class, title string) (*KWin, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	k, err := newKWin(conn, class, title)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return k, nil
}

func newKWin(conn *dbus.Conn, class, title string) (*KWin, error) {
	var has bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, kwinService).Store(&has); err != nil {
		return nil, err
	}
	if !has {
		return nil, errors.New("KWin is not running")
	}
	var xml string
	if err := conn.Object(kwinService, scriptingPath).Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil {
		return nil, fmt.Errorf("KWin scripting: %w", err)
	}
	if !strings.Contains(xml, "loadScript") {
		return nil, errors.New("KWin scripting interface not available")
	}
	k := &KWin{conn: conn, class: class, title: title, pid: os.Getpid(), timeout: scriptTimeout}
	if err := conn.Export(helper{k}, HelperPath, HelperIface); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *KWin) Name() string    { return "KWin" }
func (k *KWin) Available() bool { return true }

// Close releases the D-Bus connection.
func (k *KWin) Close() {
	k.conn.Close()
	if k.dir != "" {
		os.RemoveAll(k.dir)
	}
}

func (k *KWin) Geometry(ctx context.Context) (image.Rectangle, error) {
	r, err := k.run(ctx, nil)
	if err != nil {
		return image.Rectangle{}, err
	}
	return r.rect(), nil
}

// WorkArea returns KWin's MaximizeArea for the window (its monitor minus
// panels). The result of a recent query is reused.
func (k *KWin) WorkArea(ctx context.Context) (image.Rectangle, error) {
	if r := k.last.Load(); r != nil {
		return r.area(), nil
	}
	r, err := k.run(ctx, nil)
	if err != nil {
		return image.Rectangle{}, err
	}
	return r.area(), nil
}

func (k *KWin) MoveResize(ctx context.Context, rect image.Rectangle) error {
	_, err := k.run(ctx, &rect)
	return err
}

// script builds the KWin script for one operation.
func (k *KWin) script(token, service string, set *image.Rectangle) string {
	p := map[string]any{"token": token, "svc": service, "cls": k.class, "title": k.title, "pid": k.pid}
	if set != nil {
		p["set"] = map[string]int{"x": set.Min.X, "y": set.Min.Y, "w": set.Dx(), "h": set.Dy()}
	}
	js, _ := json.Marshal(p)
	return fmt.Sprintf(kwinScript, js, HelperPath, HelperIface)
}

const kwinScript = `(function () {
var P = %s;
function send(o) {
    callDBus(P.svc, %q, %q, "Result", P.token, JSON.stringify(o));
}
try {
    var list = workspace.windowList ? workspace.windowList() : workspace.clientList();
    var win = null;
    for (var i = 0; i < list.length; i++) {
        var w = list[i];
        var cls = String(w.resourceClass || ""), dfn = String(w.desktopFileName || "");
        if (cls != P.cls && dfn != P.cls) continue;
        if (P.pid > 0 && w.pid > 0 && w.pid != P.pid) continue;
        var cap = String(w.caption || "");
        if (cap != P.title && cap.indexOf(P.title + " <") != 0) continue;
        win = w;
        break;
    }
    if (!win) { send({error: "window not found"}); return; }
    if (P.set) {
        win.frameGeometry = {x: P.set.x, y: P.set.y, width: P.set.w, height: P.set.h};
    }
    var g = win.frameGeometry;
    var a = workspace.clientArea(KWin.MaximizeArea, win);
    send({x: Math.round(g.x), y: Math.round(g.y), w: Math.round(g.width), h: Math.round(g.height),
          ax: Math.round(a.x), ay: Math.round(a.y), aw: Math.round(a.width), ah: Math.round(a.height)});
} catch (e) {
    send({error: String(e)});
}
})();
`

// run loads, runs and unloads one script and waits for its result.
func (k *KWin) run(ctx context.Context, set *image.Rectangle) (*kwinResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()

	if k.dir == "" {
		d, err := os.MkdirTemp("", "navigo-kwin-")
		if err != nil {
			return nil, err
		}
		k.dir = d
	}
	n := k.seq.Add(1)
	token := fmt.Sprintf("%d-%d", k.pid, n)
	plugin := fmt.Sprintf("navigo-winpos-%d-%d", k.pid, n)
	svc := k.conn.Names()[0]
	f, err := os.CreateTemp(k.dir, "winpos-*.js")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	_, err = f.WriteString(k.script(token, svc, set))
	f.Close()
	defer os.Remove(path)
	if err != nil {
		return nil, err
	}

	ch := make(chan kwinResult, 1)
	k.waiters.Store(token, ch)
	defer k.waiters.Delete(token)

	sc := k.conn.Object(kwinService, scriptingPath)
	var id int32
	if err := sc.CallWithContext(ctx, scriptingIface+".loadScript", 0, path, plugin).Store(&id); err != nil {
		return nil, fmt.Errorf("loadScript: %w", err)
	}
	// Always unload, also on errors and timeouts (with a fresh context so a
	// cancelled caller can't leak the script).
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), time.Second)
		defer ucancel()
		var ok bool
		if err := sc.CallWithContext(uctx, scriptingIface+".unloadScript", 0, plugin).Store(&ok); err != nil {
			slog.Debug("winpos: KWin unloadScript failed", "plugin", plugin, "err", err)
		}
	}()
	if id < 0 {
		return nil, errors.New("KWin refused to load the script")
	}
	if err := k.start(ctx, id); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return nil, errors.New("KWin script: " + r.Error)
		}
		k.last.Store(&r)
		return &r, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("KWin script: %w", ctx.Err())
	}
}

// start runs a loaded script. The object path differs between KWin
// versions (Plasma 6: /Scripting/ScriptN, Plasma 5: /N).
func (k *KWin) start(ctx context.Context, id int32) error {
	var errs []error
	for _, p := range []string{fmt.Sprintf("/Scripting/Script%d", id), fmt.Sprintf("/%d", id)} {
		err := k.conn.Object(kwinService, dbus.ObjectPath(p)).CallWithContext(ctx, scriptIface+".run", 0).Err
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	// Last resort: start all loaded scripts.
	if err := k.conn.Object(kwinService, scriptingPath).CallWithContext(ctx, scriptingIface+".start", 0).Err; err == nil {
		return nil
	} else {
		errs = append(errs, err)
	}
	return fmt.Errorf("running KWin script: %w", errors.Join(errs...))
}
