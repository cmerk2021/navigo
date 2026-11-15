package winpos

import (
	"context"
	"image"
	"os/exec"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// TestX11Placer runs against Xvfb (no window manager) when available.
func TestX11Placer(t *testing.T) {
	xvfb, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("Xvfb not installed")
	}
	disp := ":97"
	cmd := exec.Command(xvfb, disp, "-screen", "0", "1920x1080x24", "-nolisten", "tcp")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	t.Setenv("DISPLAY", disp)
	var c *xgb.Conn
	for i := 0; i < 50; i++ {
		if c, err = xgb.NewConn(); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Skip("Xvfb did not start:", err)
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	wid, _ := xproto.NewWindowId(c)
	xproto.CreateWindow(c, scr.RootDepth, wid, scr.Root, 1550, 1016, 370, 64, 0,
		xproto.WindowClassInputOutput, scr.RootVisual, 0, nil)
	xproto.MapWindow(c, wid)
	c.Sync()

	x, err := NewX11(uint32(wid))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	ctx := context.Background()
	g, err := x.Geometry(ctx)
	if err != nil || g != image.Rect(1550, 1016, 1920, 1080) {
		t.Fatalf("Geometry = %v, %v", g, err)
	}
	wa, err := x.WorkArea(ctx)
	if err != nil || wa != image.Rect(0, 0, 1920, 1080) {
		t.Fatalf("WorkArea = %v, %v", wa, err)
	}
	target, dir := Place(g, 600, wa)
	if dir != Up || target != image.Rect(1550, 480, 1920, 1080) {
		t.Fatalf("Place = %v %v", target, dir)
	}
	if err := x.MoveResize(ctx, target); err != nil {
		t.Fatal(err)
	}
	if g, _ := x.Geometry(ctx); g != target {
		t.Fatalf("after MoveResize: %v, want %v", g, target)
	}
}
