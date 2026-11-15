package app

import (
	"image"
	"testing"

	"github.com/cmerk2021/navigo/internal/winpos"
)

func TestPlanExpandRestore(t *testing.T) {
	wa := image.Rect(0, 0, 1920, 1040) // taskbar below
	compact := image.Rect(1550, 976, 1920, 1040)

	// Compact window in the bottom-right corner opens the menu: grows up.
	g, exp := plan(compact, wa, nil, 300, 64)
	if exp == nil || exp.Dir != winpos.Up || g != image.Rect(1550, 740, 1920, 1040) {
		t.Fatalf("expand = %v %+v", g, exp)
	}
	// Closing restores the exact original rect.
	r, exp2 := plan(g, wa, exp, 64, 64)
	if exp2 != nil || r != compact {
		t.Fatalf("restore = %v %+v", r, exp2)
	}

	// Dragged while expanded: restore follows the move, bottom anchored.
	moved := g.Add(image.Pt(-500, -200))
	r, _ = plan(moved, wa, exp, 64, 64)
	if want := compact.Add(image.Pt(-500, -200)); r != want {
		t.Fatalf("restore after drag = %v, want %v", r, want)
	}

	// Top-left: grows down, restore exact.
	tl := image.Rect(0, 0, 370, 370)
	g, exp = plan(tl, wa, nil, 600, 370)
	if exp.Dir != winpos.Down || g != image.Rect(0, 0, 370, 600) {
		t.Fatalf("expand down = %v %+v", g, exp)
	}
	if r, _ := plan(g, wa, exp, 370, 370); r != tl {
		t.Fatalf("restore down = %v", r)
	}
}

func TestPlanModeChangeAtBottomEdge(t *testing.T) {
	wa := image.Rect(0, 0, 1920, 1080)
	compact := image.Rect(100, 1016, 470, 1080)
	// Compact -> Normal at the bottom edge: the new rest rect grows upward
	// and stays on screen.
	r, exp := plan(compact, wa, nil, 370, 370)
	if exp != nil || r != image.Rect(100, 710, 470, 1080) {
		t.Fatalf("mode change = %v %+v", r, exp)
	}
}

func TestPlanScaled(t *testing.T) {
	// Physical pixels at 2x on a second monitor to the right.
	wa := image.Rect(3840, 0, 7680, 2160)
	win := image.Rect(7680-740, 2160-740, 7680, 2160)
	g, exp := plan(win, wa, nil, 1200, 740)
	if exp.Dir != winpos.Up || g != image.Rect(7680-740, 960, 7680, 2160) {
		t.Fatalf("got %v %+v", g, exp)
	}
}
