package widgets

import (
	"fmt"
	"image"
	"time"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"

	"github.com/cmerk2021/navigo/ui/theme"
)

// Seekbar is a thin white progress bar with a round knob, elapsed time on the
// left and negative remaining time on the right.
type Seekbar struct {
	dragging bool
	frac     float32
}

// Dragging reports whether the user is dragging the knob.
func (s *Seekbar) Dragging() bool { return s.dragging }

// Layout draws the bar in gtx.Constraints.Max (width) and returns a seek
// target when the user releases the knob.
func (s *Seekbar) Layout(gtx layout.Context, th *theme.Theme, pos, dur time.Duration, buffered float64) (time.Duration, bool) {
	w := gtx.Constraints.Max.X
	hit := gtx.Dp(theme.SeekHitHeight)
	knob := gtx.Dp(theme.SeekKnobSize)
	track := gtx.Dp(theme.SeekTrackHeight)
	pad := knob / 2
	tw := w - 2*pad
	var target time.Duration
	seek := false
	toFrac := func(x float32) float32 {
		f := (x - float32(pad)) / float32(max(tw, 1))
		return min(max(f, 0), 1)
	}
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: s, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			if e.Buttons == pointer.ButtonPrimary && dur > 0 {
				s.dragging = true
				s.frac = toFrac(e.Position.X)
			}
		case pointer.Drag:
			if s.dragging {
				s.frac = toFrac(e.Position.X)
			}
		case pointer.Release:
			if s.dragging {
				s.frac = toFrac(e.Position.X)
				target = time.Duration(float64(s.frac) * float64(dur))
				seek = true
			}
			s.dragging = false
		case pointer.Cancel:
			s.dragging = false
		}
	}
	frac := float32(0)
	if dur > 0 {
		frac = float32(float64(pos) / float64(dur))
	}
	if s.dragging {
		frac = s.frac
		pos = time.Duration(float64(frac) * float64(dur))
	}
	frac = min(max(frac, 0), 1)

	cy := hit / 2
	tr := image.Rect(pad, cy-track/2, pad+tw, cy-track/2+track)
	theme.FillPill(gtx.Ops, tr, theme.SeekTrack)
	if buffered > 0 && buffered < 1 {
		br := tr
		br.Max.X = pad + int(float64(tw)*buffered)
		theme.FillPill(gtx.Ops, br, theme.SeekBuffered)
	}
	fx := pad + int(float32(tw)*frac)
	fr := tr
	fr.Max.X = fx
	if fr.Dx() > 0 {
		theme.FillPill(gtx.Ops, fr, theme.White)
	}
	ks := knob
	if s.dragging {
		ks = knob + gtx.Dp(2)
	}
	theme.FillCircle(gtx.Ops, theme.Rect(fx-ks/2, cy-ks/2, ks, ks), theme.White)

	area := clip.Rect{Max: image.Pt(w, hit)}.Push(gtx.Ops)
	event.Op(gtx.Ops, s)
	pointer.CursorPointer.Add(gtx.Ops)
	area.Pop()

	// Time labels.
	ty := cy + knob/2 + gtx.Dp(theme.SeekTimeGap) - gtx.Dp(4)
	g := gtx
	g.Constraints = layout.Constraints{Max: image.Pt(w/2-pad, gtx.Dp(20))}
	Off(g, image.Pt(pad, ty), func(gtx layout.Context) layout.Dimensions {
		return th.Text(gtx, FormatDuration(pos), theme.SmallSize, font.Medium, theme.TextSecondary)
	})
	g.Constraints.Min.X = g.Constraints.Max.X
	Off(g, image.Pt(w/2, ty), func(gtx layout.Context) layout.Dimensions {
		rem := dur - pos
		if rem < 0 {
			rem = 0
		}
		return th.TextAligned(gtx, "-"+FormatDuration(rem), theme.SmallSize, font.Medium, theme.TextSecondary, text.End)
	})
	return target, seek
}

// FormatDuration formats d as m:ss (or h:mm:ss).
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// Off draws w translated by p.
func Off(gtx layout.Context, p image.Point, w layout.Widget) layout.Dimensions {
	return theme.Offset(gtx, p, w)
}

// VSlider is a vertical slider for EQ bands.
type VSlider struct {
	dragging bool
}

// Dragging reports whether the knob is being dragged.
func (s *VSlider) Dragging() bool { return s.dragging }

// Layout draws the slider in gtx.Constraints.Min and updates *v within
// [lo, hi]. The fill is drawn from the center (zero) to the knob.
func (s *VSlider) Layout(gtx layout.Context, th *theme.Theme, v *float64, lo, hi float64) bool {
	sz := gtx.Constraints.Min
	knob := gtx.Dp(theme.EQSliderKnob)
	pad := knob / 2
	th2 := sz.Y - 2*pad
	changed := false
	toVal := func(y float32) float64 {
		f := 1 - float64(y-float32(pad))/float64(max(th2, 1))
		f = min(max(f, 0), 1)
		val := lo + f*(hi-lo)
		return float64(int(val*2+sign(val)*0.5)) / 2 // 0.5 dB steps
	}
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: s, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			if e.Buttons == pointer.ButtonPrimary {
				s.dragging = true
				if nv := toVal(e.Position.Y); nv != *v {
					*v, changed = nv, true
				}
			}
		case pointer.Drag:
			if s.dragging {
				if nv := toVal(e.Position.Y); nv != *v {
					*v, changed = nv, true
				}
			}
		case pointer.Release, pointer.Cancel:
			s.dragging = false
		}
	}
	track := gtx.Dp(theme.EQSliderTrack)
	cx := sz.X / 2
	tr := image.Rect(cx-track/2, pad, cx-track/2+track, pad+th2)
	theme.FillPill(gtx.Ops, tr, theme.SeekTrack)
	f := (*v - lo) / (hi - lo)
	ky := pad + int(float64(th2)*(1-f))
	zy := pad + int(float64(th2)*(1-(0-lo)/(hi-lo)))
	fr := image.Rect(tr.Min.X, min(ky, zy), tr.Max.X, max(ky, zy))
	if fr.Dy() > 0 {
		theme.FillRect(gtx.Ops, fr, th.Accent())
	}
	theme.FillRect(gtx.Ops, image.Rect(cx-track, zy, cx+track, zy+1), theme.TextTertiary)
	ks := knob
	if s.dragging {
		ks += gtx.Dp(2)
	}
	theme.FillCircle(gtx.Ops, theme.Rect(cx-ks/2, ky-ks/2, ks, ks), theme.White)
	area := clip.Rect{Max: sz}.Push(gtx.Ops)
	event.Op(gtx.Ops, s)
	pointer.CursorPointer.Add(gtx.Ops)
	area.Pop()
	return changed
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
