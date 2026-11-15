// Package app is the application shell: it owns the windows, switches
// between Normal/Compact mode and panels, hosts the menu and handles global
// keyboard shortcuts.
package app

import (
	"context"
	"image"
	"image/color"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	gioapp "gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"

	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/core"
	"github.com/cmerk2021/navigo/internal/kde"
	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/eq"
	"github.com/cmerk2021/navigo/ui/eqassist"
	"github.com/cmerk2021/navigo/ui/login"
	"github.com/cmerk2021/navigo/ui/menu"
	"github.com/cmerk2021/navigo/ui/picker"
	"github.com/cmerk2021/navigo/ui/player"
	"github.com/cmerk2021/navigo/ui/queue"
	"github.com/cmerk2021/navigo/ui/settings"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
	"github.com/cmerk2021/navigo/ui/x11win"
)

// AppID is the application/desktop ID, also used as the X11 WM_CLASS.
const AppID = "navigo"

const (
	// verifyDelay is how long the shell waits for the compositor to honor a
	// size change before falling back to a separate window.
	verifyDelay = 500 * time.Millisecond
	volumeStep  = 0.05
	seekStep    = 5 * time.Second
)

type auxKind int

const (
	auxNone auxKind = iota
	auxPanel
	auxMenu
)

// winState is one native window.
type winState struct {
	w    *gioapp.Window
	x11  *x11win.Window
	ops  op.Ops
	drag widgets.DragArea
	jobs chan func()
	done chan struct{}
	kind auxKind // auxNone for the main window
	size image.Point
	// focused tracks keyboard focus of the window (for the menu window).
	focused, wasFocused bool
	// moveTo is the X11 position to apply once the window exists.
	moveTo  image.Point
	hasMove bool
}

func newWin(kind auxKind) *winState {
	ws := &winState{w: new(gioapp.Window), jobs: make(chan func(), 16), done: make(chan struct{}), kind: kind}
	go func() {
		for {
			select {
			case f := <-ws.jobs:
				f()
			case <-ws.done:
				return
			}
		}
	}()
	return ws
}

// async runs f (typically Window.Option/Perform) off the event goroutine:
// those calls block until the driver thread processes them.
func (ws *winState) async(f func()) {
	select {
	case ws.jobs <- f:
	default:
		go func() {
			select {
			case ws.jobs <- f:
			case <-ws.done:
			}
		}()
	}
}

func sizeOpts(h unit.Dp) []gioapp.Option {
	w := unit.Dp(theme.WindowWidth)
	return []gioapp.Option{gioapp.MaxSize(w, h), gioapp.Size(w, h), gioapp.MinSize(w, h)}
}

// App is the application shell.
type App struct {
	// mu serializes the frames of all windows and guards the UI state.
	mu sync.Mutex

	p      *core.Player
	th     *theme.Theme
	env    *env.Env
	view   *player.View
	menu   *menu.Menu
	toasts widgets.Toasts
	cfg    *config.Config

	mode      string
	panelKind env.Kind
	panels    map[env.Kind]env.Panel

	menuOpen   bool
	menuAnchor image.Point
	menuTag    int
	keyTag     int

	// fallback is set once the compositor ignored a size change.
	fallback bool
	wantH    unit.Dp
	sizeReq  time.Time
	checked  bool

	// Windows; wmu guards main/aux pointers for Invalidate from any goroutine.
	wmu     sync.Mutex
	main    *winState
	aux     *winState
	auxKind auxKind

	// pending holds shell requests (Open/Close) from panels and goroutines.
	pmu     sync.Mutex
	pending []func()

	amu       sync.Mutex
	kdeAccent color.NRGBA
	kdeOK     bool

	quitOnce sync.Once
}

// Run creates the main window and runs the application. It never returns
// (the process exits on quit). Call it from a goroutine and then
// gioapp.Main() from the main goroutine.
func Run(cfg *config.Config) {
	a := &App{
		p:      core.New(cfg),
		th:     theme.New(),
		panels: map[env.Kind]env.Panel{},
		mode:   cfg.Mode,
		cfg:    cfg.Clone(),
	}
	a.env = &env.Env{
		Player:     a.p,
		Theme:      a.th,
		Invalidate: a.invalidate,
		Open:       func(k env.Kind) { a.request(func() { a.setPanel(k) }) },
		Close:      func() { a.request(func() { a.setPanel(env.None) }) },
		Toast:      a.toast,
	}
	a.menu = menu.New(a.env, a)
	a.main = newWin(auxNone)
	a.setupDrag(a.main)
	a.view = player.New(a.env, a, &a.main.drag, &a.toasts)
	if cfg.HasWindowPos {
		a.main.moveTo, a.main.hasMove = image.Pt(cfg.WindowX, cfg.WindowY), true
	}

	a.wantH = a.modeHeight()
	opts := append([]gioapp.Option{gioapp.Title("Navigo"), gioapp.Decorated(false), gioapp.Transparent(true)}, sizeOpts(a.wantH)...)
	a.main.w.Option(opts...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c, ok := kde.AccentColor(); ok {
		a.kdeAccent, a.kdeOK = c, true
	}
	go kde.Watch(ctx, func(c color.NRGBA, ok bool) {
		a.amu.Lock()
		a.kdeAccent, a.kdeOK = c, ok
		a.amu.Unlock()
		a.invalidate()
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		<-sig
		a.Quit()
	}()

	a.p.Start(a.invalidate, a.Quit, func() {
		a.main.async(func() { a.main.w.Perform(system.ActionRaise) })
	})
	go func() {
		for t := range a.p.Toasts() {
			a.toast(t.Text, t.Error)
		}
	}()
	if !cfg.HasServer() {
		a.request(func() { a.setPanel(env.Login) })
	}
	a.loop(a.main)
}

func (a *App) toast(text string, isErr bool) {
	a.toasts.Push(text, isErr)
	a.invalidate()
}

func (a *App) invalidate() {
	a.wmu.Lock()
	m, x := a.main, a.aux
	a.wmu.Unlock()
	if m != nil {
		m.w.Invalidate()
	}
	if x != nil {
		x.w.Invalidate()
	}
}

func (a *App) request(f func()) {
	a.pmu.Lock()
	a.pending = append(a.pending, f)
	a.pmu.Unlock()
	a.invalidate()
}

func (a *App) runPending() {
	for i := 0; i < 4; i++ {
		a.pmu.Lock()
		fs := a.pending
		a.pending = nil
		a.pmu.Unlock()
		if len(fs) == 0 {
			return
		}
		for _, f := range fs {
			f()
		}
	}
}

// ------------------------------------------------------------ shell API

// ToggleMode switches between Normal and Compact mode.
func (a *App) ToggleMode() {
	if a.mode == config.ModeCompact {
		a.mode = config.ModeNormal
	} else {
		a.mode = config.ModeCompact
	}
	a.menuOpen = false
	m := a.mode
	go a.p.UpdateConfig(func(c *config.Config) { c.Mode = m })
	a.invalidate()
}

// Compact reports whether Compact mode is active.
func (a *App) Compact() bool { return a.mode == config.ModeCompact }

// OpenMenu opens the context menu.
func (a *App) OpenMenu(anchor image.Point) {
	if a.panelKind != env.None {
		return
	}
	a.menuOpen, a.menuAnchor = true, anchor
	a.invalidate()
}

// ShowModeButtons reports the "Show mode-switch buttons" setting.
func (a *App) ShowModeButtons() bool { return a.cfg.ShowModeButtons }

// OverlayOpen reports whether a menu or in-window panel is shown.
func (a *App) OverlayOpen() bool { return a.menuOpen || a.panelKind != env.None }

// Quit saves state and exits.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		go func() {
			a.savePosition()
			a.p.Close()
			os.Exit(0)
		}()
		// Safety net if shutdown hangs on a device or D-Bus call.
		time.AfterFunc(4*time.Second, func() { os.Exit(0) })
	})
}

func (a *App) savePosition() {
	a.wmu.Lock()
	m := a.main
	a.wmu.Unlock()
	if m == nil || m.x11 == nil {
		return
	}
	if x, y, ok := m.x11.Position(); ok {
		a.p.UpdateConfig(func(c *config.Config) {
			c.HasWindowPos, c.WindowX, c.WindowY = true, x, y
		})
	}
}

// separate reports whether panels/menus use their own window.
func (a *App) separate() bool { return a.fallback || a.cfg.PanelsSeparateWindow }

func (a *App) modeHeight() unit.Dp {
	if a.mode == config.ModeCompact {
		return theme.CompactHeight
	}
	return theme.NormalHeight
}

func (a *App) desiredHeight() unit.Dp {
	if a.separate() {
		return a.modeHeight()
	}
	if a.panelKind != env.None {
		return theme.PanelHeight
	}
	if a.menuOpen && a.mode == config.ModeCompact {
		return theme.CompactHeight + 2*theme.MenuMargin + menu.HeightDp()
	}
	return a.modeHeight()
}

func (a *App) panel(k env.Kind) env.Panel {
	if p, ok := a.panels[k]; ok {
		return p
	}
	var p env.Panel
	switch k {
	case env.Picker:
		p = picker.New(a.env)
	case env.Queue:
		p = queue.New(a.env)
	case env.EQ:
		p = eq.New(a.env)
	case env.EQAssist:
		p = eqassist.New(a.env)
	case env.Settings:
		p = settings.New(a.env)
	case env.Login:
		p = login.New(a.env)
	default:
		return nil
	}
	a.panels[k] = p
	return p
}

// setPanel switches the current panel (env.None closes it).
func (a *App) setPanel(k env.Kind) {
	if k == a.panelKind {
		return
	}
	if old := a.panels[a.panelKind]; old != nil {
		if c, ok := old.(env.Closer); ok {
			c.Closed()
		}
	}
	a.panelKind = k
	a.menuOpen = false
	if p := a.panel(k); p != nil {
		if o, ok := p.(env.Opener); ok {
			o.Opened()
		}
	}
	a.invalidate()
}

// ------------------------------------------------------------ windows

func (a *App) setupDrag(ws *winState) {
	ws.drag.OnPress = func(pos image.Point, secondary bool) {
		if secondary {
			if ws.kind == auxNone {
				a.OpenMenu(pos)
			}
			return
		}
		if ws.x11 != nil {
			ws.x11.BeginDrag()
		}
	}
	ws.drag.OnDrag = func() {
		if ws.x11 != nil {
			ws.x11.Drag()
		}
	}
	ws.drag.OnRelease = func() {
		if ws.x11 != nil {
			ws.x11.EndDrag()
			if ws.kind == auxNone {
				go a.savePosition()
			}
		}
	}
}

// syncAux opens/closes the separate panel or menu window. Called with a.mu
// held at the end of a main-window frame.
func (a *App) syncAux() {
	want := auxNone
	if a.separate() {
		switch {
		case a.panelKind != env.None:
			want = auxPanel
		case a.menuOpen && a.mode == config.ModeCompact:
			want = auxMenu
		}
	}
	if want == a.auxKind {
		return
	}
	a.wmu.Lock()
	old := a.aux
	a.aux = nil
	a.wmu.Unlock()
	if old != nil {
		old.async(func() { old.w.Perform(system.ActionClose) })
	}
	a.auxKind = want
	if want == auxNone {
		return
	}
	ws := newWin(want)
	a.setupDrag(ws)
	w, h := unit.Dp(theme.WindowWidth), unit.Dp(theme.PanelHeight)
	if want == auxMenu {
		w = theme.MenuWidth + 2*theme.MenuMargin
		h = menu.HeightDp() + 2*theme.MenuMargin
	}
	// Place the window over (panel) or below (menu) the main window on X11.
	if a.main.x11 != nil {
		if x, y, ok := a.main.x11.Position(); ok {
			if want == auxMenu {
				y += int(float32(theme.CompactHeight) * a.scale())
				x += int(float32(theme.WindowWidth-w) * a.scale())
			}
			ws.moveTo, ws.hasMove = image.Pt(x, y), true
		}
	}
	title := "Navigo"
	if want == auxMenu {
		title = "Navigo menu"
	}
	ws.w.Option(gioapp.Title(title), gioapp.Decorated(false), gioapp.Transparent(true), gioapp.MaxSize(w, h), gioapp.Size(w, h), gioapp.MinSize(w, h))
	a.wmu.Lock()
	a.aux = ws
	a.wmu.Unlock()
	go a.loop(ws)
}

func (a *App) scale() float32 {
	if a.main.size.X > 0 {
		return float32(a.main.size.X) / float32(theme.WindowWidth)
	}
	return 1
}

// loop runs the event loop of one window.
func (a *App) loop(ws *winState) {
	for {
		switch e := ws.w.Event().(type) {
		case gioapp.X11ViewEvent:
			if e.Valid() && ws.x11 == nil {
				ws.x11 = x11win.Attach(uintptr(e.Window))
				if ws.x11 != nil {
					ws.x11.Undecorate(AppID)
					if ws.hasMove {
						ws.x11.Move(ws.moveTo.X, ws.moveTo.Y)
					}
				}
			}
		case gioapp.ConfigEvent:
			ws.focused = e.Config.Focused
		case gioapp.FrameEvent:
			a.mu.Lock()
			gtx := gioapp.NewContext(&ws.ops, e)
			ws.size = e.Size
			if ws.kind == auxNone {
				a.frameMain(gtx)
			} else {
				a.frameAux(gtx, ws)
			}
			a.mu.Unlock()
			e.Frame(gtx.Ops)
		case gioapp.DestroyEvent:
			close(ws.done)
			if ws.x11 != nil {
				ws.x11.Close()
			}
			if ws.kind == auxNone {
				a.Quit()
				select {}
			}
			a.mu.Lock()
			a.wmu.Lock()
			if a.aux == ws {
				a.aux = nil
				a.auxKind = auxNone
				switch ws.kind {
				case auxPanel:
					a.setPanel(env.None)
				case auxMenu:
					a.menuOpen = false
				}
			}
			a.wmu.Unlock()
			a.mu.Unlock()
			a.invalidate()
			return
		}
	}
}

func (a *App) updateAccent() {
	a.amu.Lock()
	kc, kok := a.kdeAccent, a.kdeOK
	a.amu.Unlock()
	c := theme.DefaultAccent
	if a.cfg.FollowKDEAccent && kok {
		c = kc
	} else if oc, ok := kde.ParseHex(a.cfg.AccentOverride); ok {
		c = oc
	}
	a.th.SetAccent(c)
}

func (a *App) beginFrame(gtx layout.Context, ws *winState) {
	a.env.Drag = &ws.drag
	a.runPending()
	a.cfg = a.p.Config()
	a.updateAccent()
	a.handleKeys(gtx)
}

// frameMain draws the main window.
func (a *App) frameMain(gtx layout.Context) {
	a.beginFrame(gtx, a.main)
	ws := a.main

	// Size management and compositor verification.
	h := a.desiredHeight()
	if h != a.wantH {
		a.wantH, a.sizeReq, a.checked = h, gtx.Now, false
		opts := sizeOpts(h)
		ws.async(func() { ws.w.Option(opts...) })
		theme.RedrawAt(gtx, gtx.Now.Add(verifyDelay+50*time.Millisecond))
	}
	if !a.checked && gtx.Now.Sub(a.sizeReq) >= verifyDelay {
		a.checked = true
		if d := gtx.Constraints.Max.Y - gtx.Dp(a.wantH); (d > 2 || d < -2) && a.wantH > a.modeHeight() {
			a.fallback = true
		}
		if a.fallback {
			if h := a.desiredHeight(); h != a.wantH {
				a.wantH, a.sizeReq, a.checked = h, gtx.Now, true
				opts := sizeOpts(h)
				ws.async(func() { ws.w.Option(opts...) })
			}
		}
	}

	paintBase(gtx, ws)
	s := a.p.Snapshot()
	sep := a.separate()

	if a.panelKind != env.None && !sep {
		a.layoutPanel(gtx, image.Pt(gtx.Dp(theme.WindowWidth), gtx.Dp(theme.PanelHeight)))
	} else if a.mode == config.ModeCompact {
		a.withSize(gtx, image.Pt(gtx.Dp(theme.WindowWidth), gtx.Dp(theme.CompactHeight)), func(gtx layout.Context) {
			a.view.LayoutCompact(gtx, s)
		})
	} else {
		a.withSize(gtx, image.Pt(gtx.Dp(theme.WindowWidth), gtx.Dp(theme.NormalHeight)), func(gtx layout.Context) {
			a.view.LayoutNormal(gtx, s)
		})
	}

	if a.menuOpen && a.panelKind == env.None && !(sep && a.mode == config.ModeCompact) {
		a.layoutMenuInWindow(gtx)
	}
	a.syncAux()
}

// paintBase fills the window background on X11, where Gio has no
// transparent window surface. Elsewhere (Wayland) the area outside the
// rounded shapes stays transparent.
func paintBase(gtx layout.Context, ws *winState) {
	if ws.x11 != nil {
		theme.FillRect(gtx.Ops, image.Rectangle{Max: gtx.Constraints.Max}, theme.WindowBG)
	}
}

func (a *App) withSize(gtx layout.Context, sz image.Point, f func(gtx layout.Context)) {
	gtx.Constraints = layout.Exact(sz)
	defer clip.Rect(image.Rectangle{Max: sz}).Push(gtx.Ops).Pop()
	f(gtx)
}

func (a *App) layoutPanel(gtx layout.Context, sz image.Point) {
	p := a.panel(a.panelKind)
	if p == nil {
		return
	}
	a.withSize(gtx, sz, func(gtx layout.Context) {
		p.Layout(gtx)
		m := gtx.Dp(theme.PanelPadding)
		th := gtx.Dp(theme.ToastHeight)
		a.toasts.Layout(gtx, a.th, image.Rect(m, sz.Y-m-th, sz.X-m, sz.Y-m))
		a.toasts.Schedule(gtx)
	})
}

// layoutMenuInWindow draws the menu over the main window with a dismiss
// layer underneath.
func (a *App) layoutMenuInWindow(gtx layout.Context) {
	full := image.Rectangle{Max: gtx.Constraints.Max}
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &a.menuTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := ev.(pointer.Event); ok {
			a.menuOpen = false
			a.invalidate()
			return
		}
	}
	area := clip.Rect(full).Push(gtx.Ops)
	event.Op(gtx.Ops, &a.menuTag)
	area.Pop()

	msz := a.menu.Size(gtx)
	margin := gtx.Dp(theme.MenuMargin)
	var pos image.Point
	if a.mode == config.ModeCompact {
		pos = image.Pt(gtx.Dp(theme.WindowWidth)-margin-msz.X, gtx.Dp(theme.CompactHeight)+margin)
		if a.menuAnchor.X < gtx.Dp(theme.WindowWidth)/2 {
			pos.X = margin
		}
	} else {
		pos = a.menuAnchor
		if pos.X > full.Dx()/2 {
			pos.X -= msz.X
		}
		h := gtx.Dp(theme.NormalHeight)
		pos.X = clampInt(pos.X, margin, full.Dx()-margin-msz.X)
		pos.Y = clampInt(pos.Y, margin, h-margin-msz.Y)
	}
	defer op.Offset(pos).Push(gtx.Ops).Pop()
	mg := gtx
	mg.Constraints = layout.Exact(msz)
	if a.menu.Layout(mg) {
		a.menuOpen = false
		a.invalidate()
	}
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// frameAux draws the separate panel or menu window.
func (a *App) frameAux(gtx layout.Context, ws *winState) {
	a.beginFrame(gtx, ws)
	full := image.Rectangle{Max: gtx.Constraints.Max}
	paintBase(gtx, ws)
	if a.aux != ws {
		return
	}
	switch ws.kind {
	case auxPanel:
		a.layoutPanel(gtx, gtx.Constraints.Max)
	case auxMenu:
		// Close when the menu window loses focus (click elsewhere).
		if ws.focused {
			ws.wasFocused = true
		} else if ws.wasFocused {
			a.menuOpen = false
			a.invalidate()
			return
		}
		ws.drag.Layout(gtx, full)
		m := gtx.Dp(theme.MenuMargin)
		defer op.Offset(image.Pt(m, m)).Push(gtx.Ops).Pop()
		mg := gtx
		mg.Constraints = layout.Exact(a.menu.Size(gtx))
		if a.menu.Layout(mg) {
			a.menuOpen = false
			a.invalidate()
		}
	}
	if a.wantH != a.desiredHeight() {
		// Let the main window apply state changes (e.g. panel closed).
		a.invalidate()
	}
}

// ------------------------------------------------------------ keys

func (a *App) capturesKeys() bool {
	if a.panelKind == env.None {
		return false
	}
	if kc, ok := a.panel(a.panelKind).(env.KeyCapturer); ok {
		return kc.CapturesKeys()
	}
	return false
}

func (a *App) handleKeys(gtx layout.Context) {
	filters := []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: "L", Required: key.ModShortcut},
		key.Filter{Name: key.NameSpace},
		key.Filter{Name: key.NameLeftArrow, Optional: key.ModShift},
		key.Filter{Name: key.NameRightArrow, Optional: key.ModShift},
		key.Filter{Name: key.NameUpArrow},
		key.Filter{Name: key.NameDownArrow},
		key.Filter{Name: "M"},
		key.Filter{Name: "C"},
		key.Filter{Name: "E"},
		key.Filter{Name: "/"},
	}
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}
		a.onKey(ke)
	}
}

func (a *App) onKey(ke key.Event) {
	if ke.Name == key.NameEscape {
		switch {
		case a.menuOpen:
			a.menuOpen = false
		case a.panelKind != env.None:
			if h, ok := a.panel(a.panelKind).(env.EscHandler); ok && h.HandleEsc() {
				break
			}
			a.setPanel(env.None)
		}
		a.invalidate()
		return
	}
	if ke.Name == "L" && ke.Modifiers.Contain(key.ModShortcut) {
		a.setPanel(env.Picker)
		return
	}
	if a.capturesKeys() || ke.Modifiers.Contain(key.ModCtrl) || ke.Modifiers.Contain(key.ModAlt) {
		return
	}
	p := a.p
	switch ke.Name {
	case key.NameSpace:
		p.PlayPause()
	case key.NameLeftArrow:
		if ke.Modifiers.Contain(key.ModShift) {
			p.Previous()
		} else {
			p.SeekBy(-seekStep)
		}
	case key.NameRightArrow:
		if ke.Modifiers.Contain(key.ModShift) {
			p.Next()
		} else {
			p.SeekBy(seekStep)
		}
	case key.NameUpArrow:
		p.SetVolume(p.Snapshot().Volume + volumeStep)
	case key.NameDownArrow:
		p.SetVolume(p.Snapshot().Volume - volumeStep)
	case "M":
		p.SetMuted(!p.Snapshot().Muted)
	case "C":
		a.ToggleMode()
	case "E":
		a.setPanel(env.EQ)
	case "/":
		a.setPanel(env.Picker)
	}
	a.invalidate()
}
