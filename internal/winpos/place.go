// Package winpos places the main window on screen. Gio exposes no window
// position, and Wayland forbids clients from positioning toplevels, so
// placement goes through the best available backend: KWin scripting over
// D-Bus (KDE Plasma, Wayland or X11), plain X11, or none.
//
// The geometry (which way to grow so the window stays on screen) is pure and
// shared by all backends.
package winpos

import "image"

// Direction is the way the window grows when it expands.
type Direction int

// Growth directions.
const (
	// Down keeps the top edge fixed.
	Down Direction = iota
	// Up keeps the bottom edge fixed.
	Up
)

func (d Direction) String() string {
	if d == Up {
		return "up"
	}
	return "down"
}

// Place returns the rectangle for win resized to height h inside the work
// area. The width never changes. The window grows downward if there is room
// below its top edge, otherwise upward if there is room above its bottom
// edge, otherwise towards the side with more room; the result is then
// clamped into the work area (horizontally as well, so a partially
// off-screen window is pulled back in).
func Place(win image.Rectangle, h int, area image.Rectangle) (image.Rectangle, Direction) {
	w := win.Dx()
	dir := Down
	y := win.Min.Y
	below := area.Max.Y - win.Min.Y
	above := win.Max.Y - area.Min.Y
	switch {
	case h <= below:
	case h <= above:
		dir, y = Up, win.Max.Y-h
	case above > below:
		dir, y = Up, win.Max.Y-h
	}
	if !area.Empty() {
		y = clamp(y, area.Min.Y, area.Max.Y-h)
	}
	x := win.Min.X
	if !area.Empty() {
		x = clamp(x, area.Min.X, area.Max.X-w)
	}
	return image.Rect(x, y, x+w, y+h), dir
}

// clamp limits v to [lo, hi]; if the range is empty lo wins (keep the top /
// left edge visible).
func clamp(v, lo, hi int) int {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}

// MonitorFor returns the monitor containing the center of win; if none
// does, the one overlapping it most, else the nearest, else the first.
func MonitorFor(win image.Rectangle, monitors []image.Rectangle) (image.Rectangle, bool) {
	if len(monitors) == 0 {
		return image.Rectangle{}, false
	}
	c := image.Pt((win.Min.X+win.Max.X)/2, (win.Min.Y+win.Max.Y)/2)
	for _, m := range monitors {
		if c.In(m) {
			return m, true
		}
	}
	best, bestArea := -1, 0
	for i, m := range monitors {
		in := m.Intersect(win)
		if a := in.Dx() * in.Dy(); !in.Empty() && a > bestArea {
			best, bestArea = i, a
		}
	}
	if best >= 0 {
		return monitors[best], true
	}
	best, bestD := 0, -1
	for i, m := range monitors {
		dx := max(m.Min.X-c.X, 0, c.X-m.Max.X)
		dy := max(m.Min.Y-c.Y, 0, c.Y-m.Max.Y)
		if d := dx*dx + dy*dy; bestD < 0 || d < bestD {
			best, bestD = i, d
		}
	}
	return monitors[best], true
}

// MonitorWorkArea returns the usable area of the monitor containing win:
// the monitor rectangle intersected with the desktop work area (which
// excludes panels). An empty work area or intersection yields the monitor.
func MonitorWorkArea(win image.Rectangle, monitors []image.Rectangle, workArea image.Rectangle) image.Rectangle {
	m, ok := MonitorFor(win, monitors)
	if !ok {
		return workArea
	}
	if wa := m.Intersect(workArea); !wa.Empty() {
		return wa
	}
	return m
}

// Expansion remembers how the window was expanded so it can be restored.
type Expansion struct {
	// Orig is the rectangle before expanding, Expanded the one requested.
	Orig, Expanded image.Rectangle
	Dir            Direction
}

// Restore returns the rectangle to go back to given the window's current
// rectangle. Without user moves this is exactly Orig; if the window was
// dragged while expanded, the restored rectangle moves with it (keeping
// the same edge anchored as the expansion did).
func (e Expansion) Restore(cur image.Rectangle) image.Rectangle {
	var d image.Point
	if e.Dir == Up {
		// Anchor the bottom edge.
		d = image.Pt(cur.Min.X-e.Expanded.Min.X, cur.Min.Y+cur.Dy()-e.Expanded.Max.Y)
	} else {
		d = cur.Min.Sub(e.Expanded.Min)
	}
	return e.Orig.Add(d)
}
