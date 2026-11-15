package theme

import (
	"image"
	"image/color"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// spinnerPeriod is the time for one revolution of the spinner arc.
const spinnerPeriod = 900 * time.Millisecond

// Spinner draws a rotating arc (a loading ring) just inside r and schedules
// the next frame. It must only be drawn while loading so the app stays idle
// otherwise.
func Spinner(gtx layout.Context, r image.Rectangle, width float32, col color.NRGBA) {
	d := float32(min(r.Dx(), r.Dy()))
	if d <= 2*width {
		return
	}
	rad := (d - width) / 2
	c := f32.Pt(float32(r.Min.X+r.Max.X)/2, float32(r.Min.Y+r.Max.Y)/2)
	ring := func(start, sweep float32, col color.NRGBA) {
		var p clip.Path
		p.Begin(gtx.Ops)
		from := f32.Pt(c.X+rad*float32(math.Cos(float64(start))), c.Y+rad*float32(math.Sin(float64(start))))
		p.MoveTo(from)
		p.Arc(c.Sub(from), c.Sub(from), sweep)
		paint.FillShape(gtx.Ops, col, clip.Stroke{Path: p.End(), Width: width}.Op())
	}
	faint := col
	faint.A /= 4
	ring(0, 2*math.Pi-0.001, faint)
	t := float64(gtx.Now.UnixNano()%int64(spinnerPeriod)) / float64(spinnerPeriod)
	ring(float32(2*math.Pi*t-math.Pi/2), 1.6, col)
	Animate(gtx)
}

// Shimmer paints a soft highlight band sweeping across r (an indeterminate
// progress indication) and schedules the next frame.
func Shimmer(gtx layout.Context, r image.Rectangle, col color.NRGBA) {
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return
	}
	const period = 1400 * time.Millisecond
	t := float32(gtx.Now.UnixNano()%int64(period)) / float32(period)
	band := float32(max(r.Dx()/3, 24))
	x := float32(r.Min.X) - band + t*(float32(r.Dx())+2*band)
	defer clip.UniformRRect(r, r.Dy()/2).Push(gtx.Ops).Pop()
	clear := col
	clear.A = 0
	half := func(x0, x1 float32, a, b color.NRGBA) {
		rc := image.Rect(int(x0), r.Min.Y, int(math.Ceil(float64(x1))), r.Max.Y).Intersect(r)
		if rc.Empty() {
			return
		}
		st := clip.Rect(rc).Push(gtx.Ops)
		paint.LinearGradientOp{Stop1: f32.Pt(x0, 0), Color1: a, Stop2: f32.Pt(x1, 0), Color2: b}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		st.Pop()
	}
	half(x-band/2, x, clear, col)
	half(x, x+band/2, col, clear)
	Animate(gtx)
}
