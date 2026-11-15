package widgets

import (
	"gioui.org/io/key"
	"image"
	"image/color"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/theme"
)

// TextField is a pill-shaped single-line text input.
type TextField struct {
	widget.Editor
	init bool
}

// Focus requests keyboard focus for the field.
func (f *TextField) Focus(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &f.Editor})
}

// Layout draws the field filling gtx.Constraints.Min. leftIcon may be
// theme.IconNone.
func (f *TextField) Layout(gtx layout.Context, th *theme.Theme, placeholder string, leftIcon theme.Icon) layout.Dimensions {
	if !f.init {
		f.SingleLine = true
		f.Submit = true
		f.init = true
	}
	sz := gtx.Constraints.Min
	r := image.Rectangle{Max: sz}
	theme.FillPill(gtx.Ops, r, theme.FieldBG)
	if gtx.Focused(&f.Editor) {
		theme.StrokeRRect(gtx.Ops, r, sz.Y/2, float32(gtx.Dp(1)), theme.WithAlpha(th.Accent(), 0.8))
	}
	x := gtx.Dp(14)
	if leftIcon != theme.IconNone {
		is := gtx.Dp(18)
		theme.DrawIcon(gtx.Ops, leftIcon, theme.Rect(x-gtx.Dp(2), (sz.Y-is)/2, is, is), theme.TextSecondary)
		x += is + gtx.Dp(6)
	}
	w := sz.X - x - gtx.Dp(14)
	lh := gtx.Sp(theme.BodySize * 1.4)
	g := gtx
	g.Constraints = layout.Constraints{Min: image.Pt(w, 0), Max: image.Pt(w, lh)}
	Off(g, image.Pt(x, (sz.Y-lh)/2), func(gtx layout.Context) layout.Dimensions {
		if f.Editor.Len() == 0 {
			th.Text(gtx, placeholder, theme.BodySize, font.Normal, theme.TextTertiary)
		}
		return f.Editor.Layout(gtx, th.Shaper, theme.Font(font.Normal), theme.BodySize,
			theme.Material(gtx.Ops, theme.White), theme.Material(gtx.Ops, theme.WithAlpha(th.Accent(), 0.4)))
	})
	return layout.Dimensions{Size: sz}
}

// Segmented is a pill-shaped segmented control.
type Segmented struct {
	clicks []widget.Clickable
}

// Layout draws the control in gtx.Constraints.Min and updates *sel.
func (s *Segmented) Layout(gtx layout.Context, th *theme.Theme, opts []string, sel *int) bool {
	if len(s.clicks) != len(opts) {
		s.clicks = make([]widget.Clickable, len(opts))
	}
	changed := false
	for i := range s.clicks {
		for s.clicks[i].Clicked(gtx) {
			if *sel != i {
				*sel = i
				changed = true
			}
		}
	}
	sz := gtx.Constraints.Min
	theme.FillPill(gtx.Ops, image.Rectangle{Max: sz}, theme.FieldBG)
	pad := gtx.Dp(3)
	n := len(opts)
	segW := (sz.X - 2*pad) / max(n, 1)
	for i, o := range opts {
		r := theme.Rect(pad+i*segW, pad, segW, sz.Y-2*pad)
		col := theme.TextSecondary
		if i == *sel {
			theme.FillPill(gtx.Ops, r, color.NRGBA{R: 255, G: 255, B: 255, A: 40})
			col = theme.White
		}
		theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
			return s.clicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if s.clicks[i].Hovered() && i != *sel {
					col = theme.White
				}
				layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min = image.Point{}
					return th.TextAligned(gtx, o, theme.CaptionSize, font.SemiBold, col, text.Middle)
				})
				defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
				pointer.CursorPointer.Add(gtx.Ops)
				return layout.Dimensions{Size: gtx.Constraints.Min}
			})
		})
	}
	return changed
}

// PopupItem is an entry in a Popup.
type PopupItem struct {
	Label    string
	Shortcut string
	// Check shows a check mark when Checked.
	Check     bool
	Checked   bool
	Separator bool
	// Segments renders an inline segmented choice (e.g. Repeat Off/All/One).
	Segments []string
	Selected int
}

// Popup is a custom-drawn dark-glass menu.
type Popup struct {
	clicks []widget.Clickable
	segs   [][]widget.Clickable
}

// PopupSize returns the size of a popup with items.
func PopupSize(gtx layout.Context, items []PopupItem) image.Point {
	h := 2 * gtx.Dp(theme.MenuPadding)
	for _, it := range items {
		if it.Separator {
			h += gtx.Dp(theme.MenuSeparator)
		} else {
			h += gtx.Dp(theme.MenuItemHeight)
		}
	}
	return image.Pt(gtx.Dp(theme.MenuWidth), h)
}

// Layout draws the popup at the origin with size PopupSize. It returns the
// index of the activated item (-1 if none) and, for segmented items, the
// chosen segment.
func (p *Popup) Layout(gtx layout.Context, th *theme.Theme, items []PopupItem) (int, int) {
	if len(p.clicks) != len(items) {
		p.clicks = make([]widget.Clickable, len(items))
		p.segs = make([][]widget.Clickable, len(items))
	}
	item, seg := -1, -1
	for i := range items {
		for p.clicks[i].Clicked(gtx) {
			item = i
		}
		if len(p.segs[i]) != len(items[i].Segments) {
			p.segs[i] = make([]widget.Clickable, len(items[i].Segments))
		}
		for j := range p.segs[i] {
			for p.segs[i][j].Clicked(gtx) {
				item, seg = i, j
			}
		}
	}
	sz := PopupSize(gtx, items)
	rad := gtx.Dp(theme.MenuRadius)
	r := image.Rectangle{Max: sz}
	// Block clicks from reaching content below the popup.
	for {
		if _, ok := gtx.Event(pointer.Filter{Target: p, Kinds: pointer.Press | pointer.Release}); !ok {
			break
		}
	}
	area := clip.UniformRRect(r, rad).Push(gtx.Ops)
	event.Op(gtx.Ops, p)
	theme.FillRRect(gtx.Ops, r, rad, theme.MenuBG)
	theme.StrokeRRect(gtx.Ops, r, rad, float32(gtx.Dp(1)), color.NRGBA{R: 255, G: 255, B: 255, A: 24})
	area.Pop()
	pad := gtx.Dp(theme.MenuPadding)
	y := pad
	ih := gtx.Dp(theme.MenuItemHeight)
	for i, it := range items {
		if it.Separator {
			sh := gtx.Dp(theme.MenuSeparator)
			theme.FillRect(gtx.Ops, image.Rect(pad+gtx.Dp(8), y+sh/2, sz.X-pad-gtx.Dp(8), y+sh/2+max(1, gtx.Dp(0.5))), theme.Divider)
			y += sh
			continue
		}
		ir := image.Rect(pad, y, sz.X-pad, y+ih)
		i, it := i, it
		if len(it.Segments) > 0 {
			theme.BoxLoose(gtx, image.Rect(ir.Min.X+gtx.Dp(12), ir.Min.Y, ir.Max.X, ir.Max.Y), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, it.Label, theme.BodySize, font.Normal, theme.White)
				})
			})
			sw := gtx.Dp(40)
			for j, s := range it.Segments {
				j, s := j, s
				sr := image.Rect(ir.Max.X-gtx.Dp(4)-(len(it.Segments)-j)*sw, ir.Min.Y+gtx.Dp(5), ir.Max.X-gtx.Dp(4)-(len(it.Segments)-j-1)*sw, ir.Max.Y-gtx.Dp(5))
				theme.Box(gtx, sr, func(gtx layout.Context) layout.Dimensions {
					return p.segs[i][j].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lr := image.Rectangle{Max: gtx.Constraints.Min}
						col := theme.TextSecondary
						if j == it.Selected {
							theme.FillPill(gtx.Ops, lr, th.Accent())
							col = theme.White
						} else if p.segs[i][j].Hovered() {
							theme.FillPill(gtx.Ops, lr, theme.GlassHover)
						}
						layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min = image.Point{}
							return th.Text(gtx, s, theme.CaptionSize, font.SemiBold, col)
						})
						return layout.Dimensions{Size: lr.Max}
					})
				})
			}
		} else {
			theme.Box(gtx, ir, func(gtx layout.Context) layout.Dimensions {
				return p.clicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lr := image.Rectangle{Max: gtx.Constraints.Min}
					if p.clicks[i].Hovered() {
						theme.FillRRect(gtx.Ops, lr, gtx.Dp(9), color.NRGBA{R: 255, G: 255, B: 255, A: 26})
					}
					x := gtx.Dp(12)
					if it.Check {
						cs := gtx.Dp(16)
						cr := theme.Rect(lr.Max.X-gtx.Dp(12)-cs, (lr.Dy()-cs)/2, cs, cs)
						if it.Checked {
							theme.DrawIcon(gtx.Ops, theme.IconCheck, cr, th.Accent())
						}
					}
					rpad := gtx.Dp(12)
					if it.Shortcut != "" || it.Check {
						rpad = gtx.Dp(44)
					}
					g := gtx
					g.Constraints = layout.Constraints{Max: image.Pt(lr.Dx()-x-rpad, lr.Dy())}
					Off(g, image.Pt(x, 0), func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
						return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, it.Label, theme.BodySize, font.Normal, theme.White)
						})
					})
					if it.Shortcut != "" {
						g := gtx
						g.Constraints = layout.Exact(image.Pt(gtx.Dp(50), lr.Dy()))
						Off(g, image.Pt(lr.Max.X-gtx.Dp(62), 0), func(gtx layout.Context) layout.Dimensions {
							return theme.E(gtx, func(gtx layout.Context) layout.Dimensions {
								return th.Text(gtx, it.Shortcut, theme.CaptionSize, font.Normal, theme.TextTertiary)
							})
						})
					}
					defer clip.Rect(lr).Push(gtx.Ops).Pop()
					pointer.CursorPointer.Add(gtx.Ops)
					return layout.Dimensions{Size: lr.Max}
				})
			})
		}
		y += ih
	}
	return item, seg
}

// SelectList is a scrolling list of selectable strings (used by dropdowns).
type SelectList struct {
	list   widget.List
	clicks []widget.Clickable
}

// Layout draws the list in gtx.Constraints.Max and returns the clicked index.
func (s *SelectList) Layout(gtx layout.Context, th *theme.Theme, items []string, selected int) int {
	s.list.Axis = layout.Vertical
	if len(s.clicks) != len(items) {
		s.clicks = make([]widget.Clickable, len(items))
	}
	clicked := -1
	for i := range s.clicks {
		for s.clicks[i].Clicked(gtx) {
			clicked = i
		}
	}
	ih := gtx.Dp(theme.DropdownItemH)
	s.list.Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
		sz := image.Pt(gtx.Constraints.Max.X, ih)
		gtx.Constraints = layout.Exact(sz)
		return s.clicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			r := image.Rectangle{Max: sz}
			if s.clicks[i].Hovered() {
				theme.FillRRect(gtx.Ops, r, gtx.Dp(9), color.NRGBA{R: 255, G: 255, B: 255, A: 26})
			}
			col := theme.White
			if i == selected {
				col = th.Accent()
			}
			g := gtx
			g.Constraints = layout.Exact(image.Pt(sz.X-gtx.Dp(24), sz.Y))
			Off(g, image.Pt(gtx.Dp(12), 0), func(gtx layout.Context) layout.Dimensions {
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, items[i], theme.BodySize, font.Normal, col)
				})
			})
			defer clip.Rect(r).Push(gtx.Ops).Pop()
			pointer.CursorPointer.Add(gtx.Ops)
			return layout.Dimensions{Size: sz}
		})
	})
	return clicked
}

// Toasts shows short transient messages.
type Toasts struct {
	mu    sync.Mutex
	text  string
	err   bool
	until time.Time
}

// Push shows a message for a few seconds.
func (t *Toasts) Push(text string, isErr bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text, t.err = text, isErr
	d := 3 * time.Second
	if isErr {
		d = 5 * time.Second
	}
	t.until = time.Now().Add(d)
}

// Current returns the active message, if any.
func (t *Toasts) Current(now time.Time) (string, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.text == "" || now.After(t.until) {
		return "", false, false
	}
	return t.text, t.err, true
}

// Schedule requests a redraw when the active toast expires.
func (t *Toasts) Schedule(gtx layout.Context) {
	t.mu.Lock()
	until := t.until
	t.mu.Unlock()
	if gtx.Now.Before(until) {
		theme.RedrawAt(gtx, until.Add(10*time.Millisecond))
	}
}

// Layout draws the active toast centered horizontally in r.
func (t *Toasts) Layout(gtx layout.Context, th *theme.Theme, r image.Rectangle) {
	txt, isErr, ok := t.Current(gtx.Now)
	if !ok {
		return
	}
	t.mu.Lock()
	until := t.until
	t.mu.Unlock()
	theme.RedrawAt(gtx, until.Add(10*time.Millisecond))
	pad := gtx.Dp(14)
	w := min(th.Measure(gtx, txt, theme.CaptionSize, font.Medium)+2*pad, r.Dx())
	tr := image.Rect(r.Min.X+(r.Dx()-w)/2, r.Min.Y, r.Min.X+(r.Dx()-w)/2+w, r.Max.Y)
	bg := color.NRGBA{R: 0x26, G: 0x26, B: 0x28, A: 245}
	if isErr {
		bg = color.NRGBA{R: 0x5A, G: 0x1E, B: 0x18, A: 245}
	}
	theme.FillPill(gtx.Ops, tr, bg)
	theme.Box(gtx, image.Rect(tr.Min.X+pad, tr.Min.Y, tr.Max.X-pad, tr.Max.Y), func(gtx layout.Context) layout.Dimensions {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = image.Point{}
			return th.Text(gtx, txt, theme.CaptionSize, font.Medium, theme.White)
		})
	})
}
