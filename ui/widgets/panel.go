package widgets

import (
	"image"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op/clip"

	"github.com/cmerk2021/navigo/ui/theme"
)

// PanelBackground paints the rounded dark panel card over the whole
// constraint area (the corners stay transparent) and installs a window-move area behind the content.
func PanelBackground(gtx layout.Context, drag *DragArea) {
	sz := gtx.Constraints.Max
	r := image.Rectangle{Max: sz}
	theme.FillRRect(gtx.Ops, r, gtx.Dp(theme.PanelRadius), theme.PanelBG)
	if drag != nil {
		drag.Layout(gtx, r)
	}
}

// PanelHeader draws a title on the left and a round close button on the right
// at the top of a panel. It returns true when the close button was clicked.
func PanelHeader(gtx layout.Context, th *theme.Theme, title string, closeBtn *RoundButton) bool {
	clicked := closeBtn.Clicked(gtx)
	pad := gtx.Dp(theme.PanelPadding)
	hh := gtx.Dp(theme.PanelHeaderH)
	w := gtx.Constraints.Max.X
	cs := gtx.Dp(theme.CloseButtonSize)
	theme.BoxLoose(gtx, image.Rect(pad, pad, w-pad-cs-gtx.Dp(8), pad+hh), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
		return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, title, theme.HeadingSize, font.Bold, theme.White)
		})
	})
	theme.Box(gtx, theme.Rect(w-pad-cs, pad+(hh-cs)/2, cs, cs), func(gtx layout.Context) layout.Dimensions {
		return closeBtn.Layout(gtx, th, theme.IconClose, StyleCompact, 0.5)
	})
	return clicked
}

// DragArea is a window-move area. On Wayland (and other platforms where Gio
// supports it) it uses system.ActionMove; on X11 the shell installs a
// MoveHandler that moves the window manually. It also reports secondary
// (right) clicks so the shell can open the context menu.
type DragArea struct {
	// OnPress is called for every press: secondary reports right clicks,
	// pos is window-local.
	OnPress func(pos image.Point, secondary bool)
	// OnDrag is called for primary drags (X11 manual move).
	OnDrag func()
	// OnRelease ends a manual move.
	OnRelease func()
}

// Layout installs the area over r. It must be laid out before (below) the
// interactive widgets, never as their ancestor.
func (d *DragArea) Layout(gtx layout.Context, r image.Rectangle) {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: d, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			if d.OnPress != nil {
				d.OnPress(e.Position.Round(), e.Buttons == pointer.ButtonSecondary)
			}
		case pointer.Drag:
			if d.OnDrag != nil && e.Buttons == pointer.ButtonPrimary {
				d.OnDrag()
			}
		case pointer.Release, pointer.Cancel:
			if d.OnRelease != nil {
				d.OnRelease()
			}
		}
	}
	defer clip.Rect(r).Push(gtx.Ops).Pop()
	system.ActionInputOp(system.ActionMove).Add(gtx.Ops)
	event.Op(gtx.Ops, d)
}
