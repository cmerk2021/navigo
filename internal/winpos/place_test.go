package winpos

import (
	"image"
	"testing"
)

func r(x, y, w, h int) image.Rectangle { return image.Rect(x, y, x+w, y+h) }

func TestPlace(t *testing.T) {
	// 1920×1080 screen with a 44 px panel at the bottom.
	area := r(0, 0, 1920, 1036)
	cases := []struct {
		name string
		win  image.Rectangle
		h    int
		want image.Rectangle
		dir  Direction
	}{
		{"center grows down", r(800, 300, 370, 370), 600, r(800, 300, 370, 600), Down},
		{"top-left grows down", r(0, 0, 370, 64), 600, r(0, 0, 370, 600), Down},
		{"top-right grows down", r(1550, 0, 370, 370), 600, r(1550, 0, 370, 600), Down},
		{"bottom-right grows up", r(1550, 666, 370, 370), 600, r(1550, 436, 370, 600), Up},
		{"bottom-left compact grows up", r(0, 972, 370, 64), 600, r(0, 436, 370, 600), Up},
		{"bottom edge exact fit grows down", r(500, 436, 370, 370), 600, r(500, 436, 370, 600), Down},
		{"just too low grows up", r(500, 437, 370, 370), 600, r(500, 207, 370, 600), Up},
		{"partially off right edge shifts left", r(1700, 100, 370, 370), 600, r(1550, 100, 370, 600), Down},
		{"partially off left edge shifts right", r(-100, 100, 370, 370), 600, r(0, 100, 370, 600), Down},
		{"partially below work area grows up and clamps", r(100, 900, 370, 370), 600, r(100, 436, 370, 600), Up},
		{"above the top edge is pulled down", r(100, -50, 370, 370), 600, r(100, 0, 370, 600), Down},
	}
	for _, c := range cases {
		got, dir := Place(c.win, c.h, area)
		if got != c.want || dir != c.dir {
			t.Errorf("%s: Place(%v, %d) = %v %v, want %v %v", c.name, c.win, c.h, got, dir, c.want, c.dir)
		}
		if !got.In(area) {
			t.Errorf("%s: %v not inside the work area", c.name, got)
		}
	}
}

func TestPlaceNoRoomEitherWay(t *testing.T) {
	// A short screen: 600 doesn't fit below or above the window.
	area := r(0, 0, 1280, 700)
	// More room below: grow down, clamped so the bottom fits.
	got, dir := Place(r(0, 200, 370, 64), 600, area)
	if dir != Down || got != r(0, 100, 370, 600) {
		t.Errorf("more room below: %v %v", got, dir)
	}
	// More room above: grow up, clamped to the top.
	got, dir = Place(r(0, 500, 370, 64), 600, area)
	if dir != Up || got != r(0, 0, 370, 600) {
		t.Errorf("more room above: %v %v", got, dir)
	}
	// Taller than the work area: keep the top visible.
	got, _ = Place(r(0, 100, 370, 64), 900, area)
	if got.Min.Y != 0 {
		t.Errorf("taller than screen: %v", got)
	}
}

func TestMultiMonitor(t *testing.T) {
	// Left: 1920×1080 at the origin with a bottom panel. Right: 2560×1440
	// at x=1920, positioned 200 px lower, no panel.
	mons := []image.Rectangle{r(0, 0, 1920, 1080), r(1920, 200, 2560, 1440)}
	work := image.Rect(0, 0, 4480, 1640)
	// The global work area excludes the left monitor's bottom panel.
	leftWork := r(0, 0, 1920, 1036)

	// Window in the bottom-right corner of the right monitor.
	win := r(4110, 1270, 370, 370)
	wa := MonitorWorkArea(win, mons, work)
	if wa != mons[1] {
		t.Fatalf("work area = %v, want right monitor %v", wa, mons[1])
	}
	got, dir := Place(win, 600, wa)
	if dir != Up || got != r(4110, 1040, 370, 600) {
		t.Errorf("right monitor corner: %v %v", got, dir)
	}

	// Window near the top of the right monitor at a y that would be off the
	// left monitor: it must not be pulled to the left monitor's area.
	win = r(2000, 220, 370, 64)
	got, dir = Place(win, 600, MonitorWorkArea(win, mons, work))
	if dir != Down || got != r(2000, 220, 370, 600) {
		t.Errorf("right monitor top: %v %v", got, dir)
	}

	// Window straddling both monitors: the one containing its center wins.
	win = r(1800, 500, 370, 370) // center x = 1985 → right monitor
	if m, _ := MonitorFor(win, mons); m != mons[1] {
		t.Errorf("straddling: monitor %v", m)
	}

	// Left monitor uses the per-monitor work area (panel excluded).
	win = r(100, 700, 370, 336)
	wa = MonitorWorkArea(win, mons, leftWork)
	got, dir = Place(win, 600, wa)
	if dir != Up || got.Max.Y != 1036 {
		t.Errorf("left monitor above panel: %v %v", got, dir)
	}

	// Window entirely off-screen: nearest monitor.
	if m, _ := MonitorFor(r(5000, 300, 370, 64), mons); m != mons[1] {
		t.Errorf("off-screen: monitor %v", m)
	}
}

func TestExpansionRestore(t *testing.T) {
	area := r(0, 0, 1920, 1036)
	orig := r(1550, 972, 370, 64)
	exp, dir := Place(orig, 600, area)
	e := Expansion{Orig: orig, Expanded: exp, Dir: dir}
	if got := e.Restore(exp); got != orig {
		t.Fatalf("restore without move = %v, want %v", got, orig)
	}
	// Dragged 300 px left and 200 px up while expanded: the bottom edge
	// anchor moves with it.
	moved := exp.Add(image.Pt(-300, -200))
	if got, want := e.Restore(moved), orig.Add(image.Pt(-300, -200)); got != want {
		t.Fatalf("restore after move = %v, want %v", got, want)
	}
	// A clamped expansion (window partially off-screen before) restores the
	// original, off-screen rectangle exactly.
	orig = r(1700, 100, 370, 370)
	exp, dir = Place(orig, 600, area)
	e = Expansion{Orig: orig, Expanded: exp, Dir: dir}
	if got := e.Restore(exp); got != orig {
		t.Fatalf("restore clamped = %v, want %v", got, orig)
	}
}
