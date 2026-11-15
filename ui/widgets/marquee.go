// Package widgets contains Navigo's custom-drawn widgets.
package widgets

import (
	"image"
	"image/color"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/theme"
)

// Marquee draws a single line of text that scrolls horizontally only when it
// overflows: it pauses at the start, scrolls by one text width plus a gap and
// loops seamlessly.
type Marquee struct {
	txt   string
	start time.Time
}

const (
	marqueePause = 1500 * time.Millisecond
	marqueeSpeed = 28 // dp per second
	marqueeGap   = 36 // dp
)

// Layout draws txt within the width of gtx.Constraints.Max.X.
func (m *Marquee) Layout(gtx layout.Context, th *theme.Theme, txt string, size unit.Sp, w font.Weight, col color.NRGBA) layout.Dimensions {
	if txt != m.txt || m.start.IsZero() {
		m.txt = txt
		m.start = gtx.Now
	}
	avail := gtx.Constraints.Max.X
	tw := th.Measure(gtx, txt, size, w)
	lh := gtx.Sp(size * 1.32)
	if tw <= avail {
		g := gtx
		g.Constraints.Min = image.Point{}
		d := th.Text(g, txt, size, w, col)
		return layout.Dimensions{Size: image.Pt(avail, max(d.Size.Y, lh)), Baseline: d.Baseline}
	}
	gap := gtx.Dp(marqueeGap)
	dist := float64(tw + gap)
	speed := float64(gtx.Dp(marqueeSpeed))
	scroll := time.Duration(dist / speed * float64(time.Second))
	cycle := marqueePause + scroll
	phase := gtx.Now.Sub(m.start) % cycle
	off := 0
	if phase >= marqueePause {
		off = int(float64(phase-marqueePause) / float64(time.Second) * speed)
		theme.Animate(gtx)
	} else {
		theme.RedrawAt(gtx, gtx.Now.Add(marqueePause-phase))
	}
	defer clip.Rect{Max: image.Pt(avail, lh+gtx.Dp(2))}.Push(gtx.Ops).Pop()
	draw := func(x int) {
		st := op.Offset(image.Pt(x, 0)).Push(gtx.Ops)
		g := gtx
		g.Constraints = layout.Constraints{Max: image.Pt(tw+2, lh*2)}
		l := widget.Label{MaxLines: 1}
		l.Layout(g, th.Shaper, theme.Font(w), size, txt, theme.Material(gtx.Ops, col))
		st.Pop()
	}
	draw(-off)
	if off > 0 {
		draw(-off + int(dist))
	}
	return layout.Dimensions{Size: image.Pt(avail, lh)}
}
