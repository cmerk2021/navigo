package app

import (
	"context"
	"errors"
	"image"
	"log/slog"
	"math"
	"time"

	gioapp "gioui.org/app"
	"gioui.org/unit"

	"github.com/cmerk2021/navigo/internal/winpos"
	"github.com/cmerk2021/navigo/ui/theme"
)

// Screen-edge aware window growth. Gio can neither read nor set the
// window's global position (and Wayland forbids it), so a winpos.Placer
// (KWin scripting, X11, or none) does it. All placer calls block and run on
// the placement worker goroutine; the UI goroutine only posts requests.

// placeReq asks the worker to size the main window to h, where rest is the
// height of the current mode (the size to restore to).
type placeReq struct {
	h, rest unit.Dp
	gen     int
}

// placerState is the selected backend: nil while still probing.
func (a *App) getPlacer() winpos.Placer {
	a.plmu.Lock()
	defer a.plmu.Unlock()
	return a.placer
}

func (a *App) setPlacer(p winpos.Placer) {
	a.plmu.Lock()
	if a.placer != nil {
		a.plmu.Unlock()
		if p != nil {
			p.Close()
		}
		return
	}
	a.placer = p
	a.plmu.Unlock()
	slog.Info("window placement", "method", p.Name())
	a.invalidate()
}

// PlacementName is shown in Settings.
func (a *App) PlacementName() string {
	if p := a.getPlacer(); p != nil {
		return p.Name()
	}
	return "detecting…"
}

// probeKWin tries the KWin backend; it works on KDE Plasma both on Wayland
// and X11.
func (a *App) probeKWin() {
	defer close(a.kwinProbed)
	if k, err := winpos.NewKWin(AppID, "Navigo"); err == nil {
		a.setPlacer(k)
	} else {
		slog.Debug("KWin window placement unavailable", "err", err)
	}
}

// selectX11 picks the X11 backend unless KWin is available.
func (a *App) selectX11(id uint32) {
	<-a.kwinProbed
	if a.getPlacer() != nil {
		return
	}
	x, err := winpos.NewX11(id)
	if err != nil {
		slog.Debug("X11 window placement unavailable", "err", err)
		a.setPlacer(winpos.Noop{})
		return
	}
	a.setPlacer(x)
}

// selectWayland falls back to no positioning outside KWin.
func (a *App) selectWayland() {
	<-a.kwinProbed
	a.setPlacer(winpos.Noop{})
}

// usePlacer reports whether size changes go through the placement worker.
// Called with a.mu held.
func (a *App) usePlacer() bool {
	p := a.getPlacer()
	return p != nil && p.Available() && !a.fallback
}

// postPlace queues a placement request; only the latest one matters.
// Called with a.mu held.
func (a *App) postPlace(h, rest unit.Dp) {
	a.placeGen++
	a.placeBusy = true
	r := placeReq{h: h, rest: rest, gen: a.placeGen}
	for {
		select {
		case a.placeCh <- r:
			return
		default:
			select {
			case <-a.placeCh:
			default:
			}
		}
	}
}

// signalFrame tells the worker a main-window frame was produced (which
// commits pending Wayland size constraints).
func (a *App) signalFrame() {
	select {
	case a.frameCh <- struct{}{}:
	default:
	}
}

// waitFrame waits for the next main-window frame (bounded).
func (a *App) waitFrame() {
	select {
	case <-a.frameCh:
	default:
	}
	a.invalidate()
	select {
	case <-a.frameCh:
	case <-time.After(150 * time.Millisecond):
	}
}

// placeWorker serializes window placement.
func (a *App) placeWorker() {
	pw := &placeWorker{a: a}
	for r := range a.placeCh {
		err := pw.apply(r)
		a.mu.Lock()
		if r.gen == a.placeGen {
			a.placeBusy = false
			a.sizeReq, a.checked = time.Now(), false
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("window placement failed; using separate windows", "err", err)
			a.fallback = true
		}
		a.mu.Unlock()
		a.invalidate()
	}
}

type placeWorker struct {
	a *App
	// exp is the active expansion (nil when the window is at rest).
	exp *winpos.Expansion
}

// lock pins the window size in Gio.
func (pw *placeWorker) lock(h unit.Dp) {
	pw.a.main.w.Option(sizeOpts(h)...)
	pw.a.mu.Lock()
	pw.a.lockedH = h
	pw.a.mu.Unlock()
}

func (pw *placeWorker) apply(r placeReq) error {
	a := pw.a
	pl := a.getPlacer()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	g, err := pl.Geometry(ctx)
	if err != nil {
		pw.lock(r.h)
		return err
	}
	wa, err := pl.WorkArea(ctx)
	if err != nil {
		pw.lock(r.h)
		return err
	}
	// Placer units (logical or physical pixels) per dp.
	k := float64(g.Dx()) / float64(theme.WindowWidth)
	if k <= 0 {
		k = 1
	}
	target, exp := plan(g, wa, pw.exp, int(math.Round(float64(r.h)*k)), int(math.Round(float64(r.rest)*k)))
	slog.Debug("window placement", "from", g, "to", target, "area", wa, "expanded", exp != nil)

	up := exp != nil && exp.Dir == winpos.Up
	if up {
		// Lay out bottom-anchored before the window grows, so content
		// already sits where it will stay.
		a.setGrowUp(true)
	}
	if target != g {
		// Relax the constraints so the window manager accepts the new
		// size, let Gio commit them, then move and resize in one step.
		w := unit.Dp(theme.WindowWidth)
		a.mu.Lock()
		locked := a.lockedH
		a.mu.Unlock()
		a.main.w.Option(gioapp.MinSize(w, min(locked, r.h)), gioapp.MaxSize(w, max(locked, r.h)))
		a.waitFrame()
		if err := pl.MoveResize(ctx, target); err != nil {
			pw.lock(r.h)
			pw.exp = nil
			a.setGrowUp(false)
			return err
		}
	}
	pw.lock(r.h)
	pw.exp = exp
	if !up {
		a.setGrowUp(false)
	}
	return nil
}

func (a *App) setGrowUp(v bool) {
	a.mu.Lock()
	changed := a.growUp != v
	a.growUp = v
	a.mu.Unlock()
	if changed {
		a.invalidate()
	}
}

// plan computes the target rectangle for window g (in placer units) with
// work area wa: rest is the height of the current mode, h the wanted
// height. It returns the new expansion (nil when h == rest). Pure.
func plan(g, wa image.Rectangle, cur *winpos.Expansion, h, rest int) (image.Rectangle, *winpos.Expansion) {
	base := g
	if cur != nil {
		// Back to the pre-expansion rect, following any user drag.
		base = cur.Restore(g)
	}
	if d := base.Dy() - rest; d > 1 || d < -1 {
		// The mode changed (Normal <-> Compact): a new rest rect, placed
		// with the same edge rules.
		base, _ = winpos.Place(base, rest, wa)
	}
	if d := h - rest; d <= 1 && d >= -1 {
		return base, nil
	}
	t, dir := winpos.Place(base, h, wa)
	return t, &winpos.Expansion{Orig: base, Expanded: t, Dir: dir}
}
