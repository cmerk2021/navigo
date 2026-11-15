package theme

import (
	"hash/fnv"
	"image"
	"image/color"
	"math"
	"sync"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// FillRect fills r with col.
func FillRect(ops *op.Ops, r image.Rectangle, col color.NRGBA) {
	paint.FillShape(ops, col, clip.Rect(r).Op())
}

// FillRRect fills a rounded rectangle.
func FillRRect(ops *op.Ops, r image.Rectangle, radius int, col color.NRGBA) {
	if col.A == 0 {
		return
	}
	paint.FillShape(ops, col, clip.UniformRRect(r, radius).Op(ops))
}

// FillPill fills a fully rounded rectangle.
func FillPill(ops *op.Ops, r image.Rectangle, col color.NRGBA) {
	FillRRect(ops, r, min(r.Dx(), r.Dy())/2, col)
}

// FillCircle fills a circle of diameter d with its top-left at p.
func FillCircle(ops *op.Ops, r image.Rectangle, col color.NRGBA) {
	if col.A == 0 {
		return
	}
	paint.FillShape(ops, col, clip.Ellipse(r).Op(ops))
}

// StrokeRRect strokes the outline of a rounded rectangle.
func StrokeRRect(ops *op.Ops, r image.Rectangle, radius int, width float32, col color.NRGBA) {
	rr := clip.UniformRRect(r, radius)
	paint.FillShape(ops, col, clip.Stroke{Path: rr.Path(ops), Width: width}.Op())
}

// Glass draws the dark-glass fill used for pills and round buttons over art.
func Glass(ops *op.Ops, r image.Rectangle, radius int, hovered bool) {
	FillRRect(ops, r, radius, GlassButton)
	FillRRect(ops, r, radius, ButtonOverlay)
	if hovered {
		FillRRect(ops, r, radius, GlassHover)
	}
}

// VerticalGradient paints a vertical gradient into r.
func VerticalGradient(ops *op.Ops, r image.Rectangle, top, bottom color.NRGBA) {
	defer clip.Rect(r).Push(ops).Pop()
	paint.LinearGradientOp{
		Stop1:  f32.Pt(0, float32(r.Min.Y)),
		Color1: top,
		Stop2:  f32.Pt(0, float32(r.Max.Y)),
		Color2: bottom,
	}.Add(ops)
	paint.PaintOp{}.Add(ops)
}

// imageCache keeps one ImageOp per decoded image so textures are uploaded once.
type imageCache struct {
	mu sync.Mutex
	m  map[*image.RGBA]paint.ImageOp
}

// ImageOp returns a cached ImageOp for img.
func (t *Theme) ImageOp(img *image.RGBA) paint.ImageOp {
	t.images.mu.Lock()
	defer t.images.mu.Unlock()
	if op, ok := t.images.m[img]; ok {
		return op
	}
	if len(t.images.m) > 400 {
		t.images.m = map[*image.RGBA]paint.ImageOp{}
	}
	o := paint.NewImageOp(img)
	o.Filter = paint.FilterLinear
	t.images.m[img] = o
	return o
}

// DrawImage draws img cover-cropped into r, clipped by a rounded rectangle of
// the given radius (radius < 0 means a circle), with the given opacity.
func (t *Theme) DrawImage(ops *op.Ops, img *image.RGBA, r image.Rectangle, radius int, alpha float32) {
	if img == nil || r.Empty() || alpha <= 0 {
		return
	}
	var cl clip.Stack
	if radius < 0 {
		cl = clip.Ellipse(r).Push(ops)
	} else {
		cl = clip.UniformRRect(r, radius).Push(ops)
	}
	defer cl.Pop()
	if alpha < 1 {
		defer paint.PushOpacity(ops, alpha).Pop()
	}
	b := img.Bounds()
	sx := float32(r.Dx()) / float32(b.Dx())
	sy := float32(r.Dy()) / float32(b.Dy())
	s := max(sx, sy)
	w, h := float32(b.Dx())*s, float32(b.Dy())*s
	off := f32.Pt(float32(r.Min.X)+(float32(r.Dx())-w)/2, float32(r.Min.Y)+(float32(r.Dy())-h)/2)
	tr := f32.Affine2D{}.Scale(f32.Point{}, f32.Pt(s, s)).Offset(off)
	defer op.Affine(tr).Push(ops).Pop()
	t.ImageOp(img).Add(ops)
	paint.PaintOp{}.Add(ops)
}

// Placeholder draws a deterministic gradient for items without artwork.
func Placeholder(ops *op.Ops, r image.Rectangle, radius int, seed string) {
	var cl clip.Stack
	if radius < 0 {
		cl = clip.Ellipse(r).Push(ops)
	} else {
		cl = clip.UniformRRect(r, radius).Push(ops)
	}
	defer cl.Pop()
	a, b := SeedColors(seed)
	paint.LinearGradientOp{
		Stop1: f32.Pt(float32(r.Min.X), float32(r.Min.Y)), Color1: a,
		Stop2: f32.Pt(float32(r.Max.X), float32(r.Max.Y)), Color2: b,
	}.Add(ops)
	paint.PaintOp{}.Add(ops)
}

// SeedColors derives two pleasant dark colors from a string.
func SeedColors(seed string) (color.NRGBA, color.NRGBA) {
	h := fnv.New32a()
	h.Write([]byte(seed))
	v := h.Sum32()
	hue := float64(v%360) / 360
	hue2 := math.Mod(hue+0.12+float64(v>>9%20)/100, 1)
	return hsl(hue, 0.45, 0.32), hsl(hue2, 0.55, 0.14)
}

func hsl(h, s, l float64) color.NRGBA {
	f := func(n float64) uint8 {
		k := math.Mod(n+h*12, 12)
		a := s * math.Min(l, 1-l)
		v := l - a*math.Max(-1, math.Min(math.Min(k-3, 9-k), 1))
		return uint8(v*255 + 0.5)
	}
	return color.NRGBA{R: f(0), G: f(8), B: f(4), A: 255}
}

// Rect returns a rectangle at (x, y) with size (w, h).
func Rect(x, y, w, h int) image.Rectangle { return image.Rect(x, y, x+w, y+h) }

// Centered returns a size×size square centered in r.
func Centered(r image.Rectangle, size int) image.Rectangle {
	c := r.Min.Add(r.Size().Div(2))
	return image.Rect(c.X-size/2, c.Y-size/2, c.X-size/2+size, c.Y-size/2+size)
}

// Offset runs f with the origin translated to p, and returns f's result.
func Offset(gtx layout.Context, p image.Point, f layout.Widget) layout.Dimensions {
	defer op.Offset(p).Push(gtx.Ops).Pop()
	return f(gtx)
}

// Box runs f constrained exactly to r (translated to r.Min).
func Box(gtx layout.Context, r image.Rectangle, f layout.Widget) layout.Dimensions {
	defer op.Offset(r.Min).Push(gtx.Ops).Pop()
	g := gtx
	g.Constraints = layout.Exact(r.Size())
	return f(g)
}

// BoxLoose runs f with max constraints r.Size() and min zero.
func BoxLoose(gtx layout.Context, r image.Rectangle, f layout.Widget) layout.Dimensions {
	defer op.Offset(r.Min).Push(gtx.Ops).Pop()
	g := gtx
	g.Constraints = layout.Constraints{Max: r.Size()}
	return f(g)
}

// W lays out w left-aligned and vertically centered within the minimum
// constraints (layout.W only resets Min.X, which stretches labels).
func W(gtx layout.Context, w layout.Widget) layout.Dimensions { return align(gtx, w, false) }

// E is W aligned to the right.
func E(gtx layout.Context, w layout.Widget) layout.Dimensions { return align(gtx, w, true) }

func align(gtx layout.Context, w layout.Widget, right bool) layout.Dimensions {
	min := gtx.Constraints.Min
	gtx.Constraints.Min = image.Point{}
	m := op.Record(gtx.Ops)
	d := w(gtx)
	call := m.Stop()
	sz := image.Pt(max(min.X, d.Size.X), max(min.Y, d.Size.Y))
	off := image.Pt(0, (sz.Y-d.Size.Y)/2)
	if right {
		off.X = sz.X - d.Size.X
	}
	t := op.Offset(off).Push(gtx.Ops)
	call.Add(gtx.Ops)
	t.Pop()
	return layout.Dimensions{Size: sz, Baseline: d.Baseline + sz.Y - d.Size.Y - off.Y}
}
