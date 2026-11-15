package winpos

import (
	"context"
	"errors"
	"image"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// X11 places a window on X11 (or XWayland) with a pure-Go X connection:
// geometry from the window, work area from _NET_WORKAREA intersected with
// the RandR monitor containing the window, and moves through
// _NET_MOVERESIZE_WINDOW (EWMH window managers) or ConfigureWindow.
type X11 struct {
	mu    sync.Mutex
	c     *xgb.Conn
	root  xproto.Window
	win   xproto.Window
	randr bool
}

// NewX11 connects to the X server for window id.
func NewX11(id uint32) (*X11, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	x := &X11{c: c, root: xproto.Setup(c).DefaultScreen(c).Root, win: xproto.Window(id)}
	if _, err := xproto.GetGeometry(c, xproto.Drawable(x.win)).Reply(); err != nil {
		c.Close()
		return nil, err
	}
	x.randr = randr.Init(c) == nil
	return x, nil
}

func (x *X11) Name() string    { return "X11" }
func (x *X11) Available() bool { return true }

func (x *X11) Close() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.c.Close()
}

func (x *X11) Geometry(context.Context) (image.Rectangle, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.geometry()
}

func (x *X11) geometry() (image.Rectangle, error) {
	g, err := xproto.GetGeometry(x.c, xproto.Drawable(x.win)).Reply()
	if err != nil {
		return image.Rectangle{}, err
	}
	t, err := xproto.TranslateCoordinates(x.c, x.win, x.root, 0, 0).Reply()
	if err != nil {
		return image.Rectangle{}, err
	}
	px, py := int(t.DstX), int(t.DstY)
	return image.Rect(px, py, px+int(g.Width), py+int(g.Height)), nil
}

func (x *X11) atom(name string) xproto.Atom {
	r, err := xproto.InternAtom(x.c, true, uint16(len(name)), name).Reply()
	if err != nil {
		return xproto.AtomNone
	}
	return r.Atom
}

func (x *X11) cardinals(name string) []uint32 {
	a := x.atom(name)
	if a == xproto.AtomNone {
		return nil
	}
	r, err := xproto.GetProperty(x.c, false, x.root, a, xproto.GetPropertyTypeAny, 0, 1024).Reply()
	if err != nil || r.Format != 32 {
		return nil
	}
	v := make([]uint32, r.ValueLen)
	for i := range v {
		v[i] = xgb.Get32(r.Value[4*i:])
	}
	return v
}

// monitors returns the RandR monitors (or CRTCs), else the root window.
func (x *X11) monitors() []image.Rectangle {
	var out []image.Rectangle
	if x.randr {
		if r, err := randr.GetMonitors(x.c, x.root, true).Reply(); err == nil {
			for _, m := range r.Monitors {
				out = append(out, image.Rect(int(m.X), int(m.Y), int(m.X)+int(m.Width), int(m.Y)+int(m.Height)))
			}
		} else if res, err := randr.GetScreenResourcesCurrent(x.c, x.root).Reply(); err == nil {
			for _, crtc := range res.Crtcs {
				ci, err := randr.GetCrtcInfo(x.c, crtc, res.ConfigTimestamp).Reply()
				if err == nil && ci.Width > 0 && ci.Height > 0 {
					out = append(out, image.Rect(int(ci.X), int(ci.Y), int(ci.X)+int(ci.Width), int(ci.Y)+int(ci.Height)))
				}
			}
		}
	}
	if len(out) == 0 {
		if g, err := xproto.GetGeometry(x.c, xproto.Drawable(x.root)).Reply(); err == nil {
			out = append(out, image.Rect(0, 0, int(g.Width), int(g.Height)))
		}
	}
	return out
}

func (x *X11) WorkArea(context.Context) (image.Rectangle, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	win, err := x.geometry()
	if err != nil {
		return image.Rectangle{}, err
	}
	mons := x.monitors()
	var work image.Rectangle
	if wa := x.cardinals("_NET_WORKAREA"); len(wa) >= 4 {
		i := 0
		if d := x.cardinals("_NET_CURRENT_DESKTOP"); len(d) == 1 && int(d[0])*4+4 <= len(wa) {
			i = int(d[0]) * 4
		}
		work = image.Rect(int(int32(wa[i])), int(int32(wa[i+1])), int(int32(wa[i]))+int(wa[i+2]), int(int32(wa[i+1]))+int(wa[i+3]))
	} else {
		for _, m := range mons {
			work = work.Union(m)
		}
	}
	if len(mons) == 0 {
		if work.Empty() {
			return image.Rectangle{}, errors.New("no screen geometry")
		}
		return work, nil
	}
	return MonitorWorkArea(win, mons, work), nil
}

func (x *X11) supported(a xproto.Atom) bool {
	for _, v := range x.cardinals("_NET_SUPPORTED") {
		if xproto.Atom(v) == a {
			return true
		}
	}
	return false
}

func (x *X11) MoveResize(_ context.Context, r image.Rectangle) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if a := x.atom("_NET_MOVERESIZE_WINDOW"); a != xproto.AtomNone && x.supported(a) {
		// Gravity static (10), x/y/width/height present, source = pager (2).
		flags := uint32(10) | 0xf<<8 | 2<<12
		ev := xproto.ClientMessageEvent{
			Format: 32,
			Window: x.win,
			Type:   a,
			Data:   xproto.ClientMessageDataUnionData32New([]uint32{flags, uint32(int32(r.Min.X)), uint32(int32(r.Min.Y)), uint32(r.Dx()), uint32(r.Dy())}),
		}
		mask := uint32(xproto.EventMaskSubstructureRedirect | xproto.EventMaskSubstructureNotify)
		return xproto.SendEventChecked(x.c, false, x.root, mask, string(ev.Bytes())).Check()
	}
	return xproto.ConfigureWindowChecked(x.c, x.win,
		xproto.ConfigWindowX|xproto.ConfigWindowY|xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
		[]uint32{uint32(int32(r.Min.X)), uint32(int32(r.Min.Y)), uint32(r.Dx()), uint32(r.Dy())}).Check()
}
