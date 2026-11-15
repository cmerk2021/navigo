package widgets

import (
	"image"
	"image/color"

	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/theme"
)

// ButtonStyle selects the fill of a round button.
type ButtonStyle int

// Button styles.
const (
	// StyleGlass is the translucent dark circle used over album art.
	StyleGlass ButtonStyle = iota
	// StyleCompact is the lighter circle used inside the compact pill.
	StyleCompact
	// StyleAccent is a solid accent-colored circle.
	StyleAccent
	// StyleFlat draws only the icon with a subtle hover background.
	StyleFlat
)

// RoundButton is a circular icon button.
type RoundButton struct {
	widget.Clickable
}

// Layout draws the button filling gtx.Constraints.Min (a square).
func (b *RoundButton) Layout(gtx layout.Context, th *theme.Theme, ic theme.Icon, style ButtonStyle, iconScale float32) layout.Dimensions {
	return b.Clickable.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Constraints.Min
		r := image.Rectangle{Max: sz}
		hov := b.Hovered()
		switch style {
		case StyleGlass:
			theme.FillCircle(gtx.Ops, r, theme.GlassButton)
			theme.FillCircle(gtx.Ops, r, theme.ButtonOverlay)
		case StyleCompact:
			theme.FillCircle(gtx.Ops, r, color.NRGBA{R: 255, G: 255, B: 255, A: 20})
		case StyleAccent:
			theme.FillCircle(gtx.Ops, r, th.Accent())
		}
		if hov {
			theme.FillCircle(gtx.Ops, r, theme.GlassHover)
		}
		if b.Pressed() {
			theme.FillCircle(gtx.Ops, r, color.NRGBA{A: 40})
		}
		isz := int(float32(min(sz.X, sz.Y)) * iconScale)
		theme.DrawIcon(gtx.Ops, ic, theme.Centered(r, isz), theme.White)
		defer clip.Ellipse(r).Push(gtx.Ops).Pop()
		pointer.CursorPointer.Add(gtx.Ops)
		return layout.Dimensions{Size: sz}
	})
}

// PillButton is a rounded text button.
type PillButton struct {
	widget.Clickable
}

// Layout draws the button with the size gtx.Constraints.Min. primary uses the
// accent color as the fill.
func (b *PillButton) Layout(gtx layout.Context, th *theme.Theme, label string, primary bool) layout.Dimensions {
	return b.LayoutColor(gtx, th, label, primary, true)
}

// LayoutColor is Layout with an enabled flag.
func (b *PillButton) LayoutColor(gtx layout.Context, th *theme.Theme, label string, primary, enabled bool) layout.Dimensions {
	return b.Clickable.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Constraints.Min
		r := image.Rectangle{Max: sz}
		fill := theme.FieldBG
		if primary {
			fill = th.Accent()
		}
		if !enabled {
			fill = theme.WithAlpha(fill, 0.4)
		}
		theme.FillPill(gtx.Ops, r, fill)
		if enabled && b.Hovered() {
			theme.FillPill(gtx.Ops, r, theme.GlassHover)
		}
		col := theme.White
		if !enabled {
			col = theme.TextTertiary
		}
		g := gtx
		g.Constraints = layout.Exact(sz)
		layout.Center.Layout(g, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = image.Point{}
			return th.TextAligned(gtx, label, theme.BodySize, font.SemiBold, col, text.Middle)
		})
		if enabled {
			defer clip.Rect(r).Push(gtx.Ops).Pop()
			pointer.CursorPointer.Add(gtx.Ops)
		}
		return layout.Dimensions{Size: sz}
	})
}

// Toggle is an on/off switch.
type Toggle struct {
	widget.Clickable
}

// Layout draws the switch and returns the new value.
func (t *Toggle) Layout(gtx layout.Context, th *theme.Theme, on *bool) bool {
	changed := false
	for t.Clicked(gtx) {
		*on = !*on
		changed = true
	}
	w, h := gtx.Dp(theme.ToggleWidth), gtx.Dp(theme.ToggleHeight)
	gtx.Constraints = layout.Exact(image.Pt(w, h))
	t.Clickable.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		r := image.Rect(0, 0, w, h)
		bg := color.NRGBA{R: 255, G: 255, B: 255, A: 50}
		if *on {
			bg = th.Accent()
		}
		theme.FillPill(gtx.Ops, r, bg)
		pad := gtx.Dp(3)
		k := h - 2*pad
		x := pad
		if *on {
			x = w - pad - k
		}
		theme.FillCircle(gtx.Ops, theme.Rect(x, pad, k, k), theme.White)
		defer clip.Rect(r).Push(gtx.Ops).Pop()
		pointer.CursorPointer.Add(gtx.Ops)
		return layout.Dimensions{Size: r.Max}
	})
	return changed
}
