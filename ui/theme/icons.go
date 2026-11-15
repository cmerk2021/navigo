package theme

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// Icon identifies a vector icon. Icons are designed on a 24×24 grid and drawn
// as filled or round-capped stroked paths.
type Icon int

// Icons.
const (
	IconNone Icon = iota
	IconPlay
	IconPause
	IconPrev
	IconNext
	IconShuffle
	IconRepeat
	IconRepeatOne
	IconMenu
	IconCompact
	IconNormal
	IconSearch
	IconClose
	IconCheck
	IconChevronDown
	IconChevronLeft
	IconPlus
	IconMinus
)

type pen struct {
	ops *op.Ops
	o   f32.Point
	s   float32
	col color.NRGBA
}

func (p pen) pt(x, y float32) f32.Point { return f32.Pt(p.o.X+x*p.s, p.o.Y+y*p.s) }

// poly fills a polygon, softened with a thin stroke so corners look rounded.
func (p pen) poly(pts ...float32) {
	var path clip.Path
	path.Begin(p.ops)
	path.MoveTo(p.pt(pts[0], pts[1]))
	for i := 2; i < len(pts); i += 2 {
		path.LineTo(p.pt(pts[i], pts[i+1]))
	}
	path.Close()
	spec := path.End()
	paint.FillShape(p.ops, p.col, clip.Outline{Path: spec}.Op())
	p.lines(1.6, true, pts...)
}

// lines strokes a polyline with round caps and joins.
func (p pen) lines(width float32, closed bool, pts ...float32) {
	var path clip.Path
	path.Begin(p.ops)
	path.MoveTo(p.pt(pts[0], pts[1]))
	for i := 2; i < len(pts); i += 2 {
		path.LineTo(p.pt(pts[i], pts[i+1]))
	}
	if closed {
		path.Close()
	}
	paint.FillShape(p.ops, p.col, clip.Stroke{Path: path.End(), Width: width * p.s}.Op())
	for i := 0; i < len(pts); i += 2 {
		p.dot(pts[i], pts[i+1], width/2)
	}
}

func (p pen) dot(x, y, r float32) {
	c := p.pt(x, y)
	paint.FillShape(p.ops, p.col, clip.Outline{Path: circlePath(p.ops, c, r*p.s)}.Op())
}

func (p pen) rrect(x, y, w, h, r float32) {
	rect := image.Rect(int(p.o.X+x*p.s+.5), int(p.o.Y+y*p.s+.5), int(p.o.X+(x+w)*p.s+.5), int(p.o.Y+(y+h)*p.s+.5))
	FillRRect(p.ops, rect, int(r*p.s+.5), p.col)
}

func (p pen) strokeRRect(x, y, w, h, r, width float32) {
	rect := image.Rect(int(p.o.X+x*p.s+.5), int(p.o.Y+y*p.s+.5), int(p.o.X+(x+w)*p.s+.5), int(p.o.Y+(y+h)*p.s+.5))
	StrokeRRect(p.ops, rect, int(r*p.s+.5), width*p.s, p.col)
}

// curve strokes a path made of line and quadratic segments. Each segment is
// either {x, y} (line) or {cx, cy, x, y} (quad).
func (p pen) curve(width float32, start [2]float32, segs ...[]float32) {
	var path clip.Path
	path.Begin(p.ops)
	path.MoveTo(p.pt(start[0], start[1]))
	last := start
	for _, s := range segs {
		if len(s) == 2 {
			path.LineTo(p.pt(s[0], s[1]))
			last = [2]float32{s[0], s[1]}
		} else {
			path.QuadTo(p.pt(s[0], s[1]), p.pt(s[2], s[3]))
			last = [2]float32{s[2], s[3]}
		}
	}
	paint.FillShape(p.ops, p.col, clip.Stroke{Path: path.End(), Width: width * p.s}.Op())
	p.dot(start[0], start[1], width/2)
	p.dot(last[0], last[1], width/2)
}

// DrawIcon draws ic into the square r.
func DrawIcon(ops *op.Ops, ic Icon, r image.Rectangle, col color.NRGBA) {
	sz := min(r.Dx(), r.Dy())
	p := pen{ops: ops, o: f32.Pt(float32(r.Min.X+(r.Dx()-sz)/2), float32(r.Min.Y+(r.Dy()-sz)/2)), s: float32(sz) / 24, col: col}
	const w = 2.2
	switch ic {
	case IconPlay:
		p.poly(8, 5.2, 19, 12, 8, 18.8)
	case IconPause:
		p.rrect(6, 5, 4.2, 14, 1.4)
		p.rrect(13.8, 5, 4.2, 14, 1.4)
	case IconNext:
		p.poly(5.5, 5.8, 15, 12, 5.5, 18.2)
		p.rrect(16, 5.5, 2.8, 13, 1.4)
	case IconPrev:
		p.poly(18.5, 5.8, 9, 12, 18.5, 18.2)
		p.rrect(5.2, 5.5, 2.8, 13, 1.4)
	case IconShuffle:
		p.curve(w, [2]float32{3.5, 7.5}, []float32{6.5, 7.5}, []float32{9.5, 7.5, 12, 12}, []float32{14.5, 16.5, 17.5, 16.5}, []float32{19.5, 16.5})
		p.curve(w, [2]float32{3.5, 16.5}, []float32{6.5, 16.5}, []float32{9.5, 16.5, 12, 12}, []float32{14.5, 7.5, 17.5, 7.5}, []float32{19.5, 7.5})
		p.lines(w, false, 17, 5, 19.8, 7.5, 17, 10)
		p.lines(w, false, 17, 14, 19.8, 16.5, 17, 19)
	case IconRepeat, IconRepeatOne:
		p.curve(w, [2]float32{4.5, 12}, []float32{4.5, 9.5}, []float32{4.5, 7, 7, 7}, []float32{18.5, 7})
		p.lines(w, false, 16, 4.5, 18.7, 7, 16, 9.5)
		p.curve(w, [2]float32{19.5, 12}, []float32{19.5, 14.5}, []float32{19.5, 17, 17, 17}, []float32{5.5, 17})
		p.lines(w, false, 8, 14.5, 5.3, 17, 8, 19.5)
	case IconMenu:
		p.lines(w, false, 5, 7, 19, 7)
		p.lines(w, false, 5, 12, 19, 12)
		p.lines(w, false, 5, 17, 19, 17)
	case IconCompact:
		p.strokeRRect(3, 8, 18, 8, 4, 2)
	case IconNormal:
		p.strokeRRect(5, 5, 14, 14, 3.5, 2)
	case IconSearch:
		c := p.pt(10.5, 10.5)
		rr := 6 * p.s
		paint.FillShape(ops, col, clip.Stroke{Path: circlePath(ops, c, rr), Width: w * p.s}.Op())
		p.lines(w, false, 15.2, 15.2, 19.5, 19.5)
	case IconClose:
		p.lines(w, false, 6.5, 6.5, 17.5, 17.5)
		p.lines(w, false, 17.5, 6.5, 6.5, 17.5)
	case IconCheck:
		p.lines(w, false, 5, 12.5, 10, 17.5, 19, 7)
	case IconChevronDown:
		p.lines(w, false, 7, 10, 12, 15, 17, 10)
	case IconPlus:
		p.lines(w, false, 12, 6, 12, 18)
		p.lines(w, false, 6, 12, 18, 12)
	case IconMinus:
		p.lines(w, false, 6, 12, 18, 12)
	case IconChevronLeft:
		p.lines(w, false, 14.5, 6, 8.5, 12, 14.5, 18)
	}
}

func circlePath(ops *op.Ops, c f32.Point, r float32) clip.PathSpec {
	var path clip.Path
	path.Begin(ops)
	path.MoveTo(f32.Pt(c.X+r, c.Y))
	path.ArcTo(c, c, 2*math.Pi)
	path.Close()
	return path.End()
}
