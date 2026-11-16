// Package queue implements the queue panel. Tracks are listed in play
// order; upcoming tracks added with "Add to queue" are marked.
package queue

import (
	"image"
	"image/color"

	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// Panel shows the play queue.
type Panel struct {
	e        *env.Env
	closeBtn widgets.RoundButton
	clearBtn widgets.PillButton
	list     widget.List
	rows     []widget.Clickable
	removes  []widgets.RoundButton
	scrolled bool
}

// New creates the queue panel.
func New(e *env.Env) *Panel {
	p := &Panel{e: e}
	p.list.Axis = layout.Vertical
	return p
}

// Opened scrolls to the current track.
func (p *Panel) Opened() { p.scrolled = false }

// Layout draws the panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	th := p.e.Theme
	pl := p.e.Player
	sz := gtx.Constraints.Max
	widgets.PanelBackground(gtx, p.e.Drag)
	if widgets.PanelHeader(gtx, th, "Queue", &p.closeBtn) {
		p.e.Close()
	}
	entries, cur := pl.QueueEntries()
	if len(p.rows) != len(entries) {
		p.rows = make([]widget.Clickable, len(entries))
		p.removes = make([]widgets.RoundButton, len(entries))
	}
	jump, remove := -1, -1
	for i := range p.rows {
		if p.rows[i].Clicked(gtx) {
			jump = entries[i].Index
		}
		if p.removes[i].Clicked(gtx) {
			remove = entries[i].Index
		}
	}
	if remove >= 0 {
		pl.RemoveAt(remove)
	} else if jump >= 0 {
		pl.JumpTo(jump)
	}
	if p.clearBtn.Clicked(gtx) {
		pl.ClearQueue()
	}

	pad := gtx.Dp(theme.PanelPadding)
	top := pad + gtx.Dp(theme.PanelHeaderH) + gtx.Dp(8)
	bh := gtx.Dp(theme.PillButtonHeight)
	bottom := sz.Y - pad - bh - gtx.Dp(12)
	if len(entries) == 0 {
		theme.Box(gtx, image.Rect(pad, top, sz.X-pad, bottom), func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = image.Point{}
				return th.TextAligned(gtx, "The queue is empty", theme.BodySize, font.Normal, theme.TextSecondary, text.Middle)
			})
		})
	} else {
		if !p.scrolled && cur >= 0 {
			p.list.Position.First = max(0, cur-2)
			p.list.Position.Offset = 0
			p.scrolled = true
		}
		rh := gtx.Dp(theme.RowHeight)
		theme.Box(gtx, image.Rect(pad/2, top, sz.X-pad/2, bottom), func(gtx layout.Context) layout.Dimensions {
			return p.list.Layout(gtx, len(entries), func(gtx layout.Context, i int) layout.Dimensions {
				s := entries[i].Song
				sub := s.Artist + " · " + widgets.FormatDuration(s.DurationTime())
				if entries[i].Queued {
					sub = "In queue · " + sub
				}
				w := gtx.Constraints.Max.X
				gtx.Constraints = layout.Exact(image.Pt(w, rh))
				p.rows[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					r := image.Rect(0, 0, w, rh)
					if i == cur {
						theme.FillRRect(gtx.Ops, r, gtx.Dp(12), theme.WithAlpha(th.Accent(), 0.22))
					} else if p.rows[i].Hovered() {
						theme.FillRRect(gtx.Ops, r, gtx.Dp(12), color.NRGBA{R: 255, G: 255, B: 255, A: 18})
					}
					ts := gtx.Dp(theme.TrackThumb)
					tr := theme.Rect(gtx.Dp(8), (rh-ts)/2, ts, ts)
					if img := p.e.Player.Covers().Request(s.CoverArt, 120, p.e.Invalidate); img != nil {
						th.DrawImage(gtx.Ops, img, tr, gtx.Dp(8), 1)
					} else {
						theme.Placeholder(gtx.Ops, tr, gtx.Dp(8), s.Album+s.Artist)
					}
					tx := tr.Max.X + gtx.Dp(10)
					tw := w - tx - gtx.Dp(44)
					col := theme.White
					if i == cur {
						col = th.Accent()
					}
					theme.BoxLoose(gtx, image.Rect(tx, gtx.Dp(7), tx+tw, gtx.Dp(26)), func(gtx layout.Context) layout.Dimensions {
						return th.Text(gtx, s.Title, theme.BodySize, font.SemiBold, col)
					})
					theme.BoxLoose(gtx, image.Rect(tx, gtx.Dp(26), tx+tw, rh), func(gtx layout.Context) layout.Dimensions {
						return th.Text(gtx, sub, theme.CaptionSize, font.Normal, theme.TextSecondary)
					})
					defer clip.Rect(r).Push(gtx.Ops).Pop()
					pointer.CursorPointer.Add(gtx.Ops)
					return layout.Dimensions{Size: r.Max}
				})
				bs := gtx.Dp(28)
				theme.Box(gtx, theme.Rect(w-bs-gtx.Dp(8), (rh-bs)/2, bs, bs), func(gtx layout.Context) layout.Dimensions {
					return p.removes[i].Layout(gtx, th, theme.IconClose, widgets.StyleFlat, 0.5)
				})
				return layout.Dimensions{Size: image.Pt(w, rh)}
			})
		})
	}
	bw := gtx.Dp(120)
	theme.Box(gtx, theme.Rect((sz.X-bw)/2, sz.Y-pad-bh, bw, bh), func(gtx layout.Context) layout.Dimensions {
		return p.clearBtn.LayoutColor(gtx, th, "Clear", false, len(entries) > 0)
	})
	return layout.Dimensions{Size: sz}
}
