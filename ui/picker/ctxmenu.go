package picker

import (
	"image"

	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"

	"github.com/cmerk2021/navigo/internal/subsonic"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// ctxTarget is what a context menu was opened for: a grid item or a track.
type ctxTarget struct {
	it     item
	song   subsonic.Song
	isSong bool
}

type ctxAction int

const (
	actNone ctxAction = iota
	actPlay
	actShuffle
	actOpen
	actNext
	actQueue
)

// ctxMenu is the right-click menu of covers and tracks.
type ctxMenu struct {
	pending *ctxTarget // right-clicked this frame, waiting for the position
	open    *ctxTarget
	pos     image.Point
	popup   widgets.Popup
	capture int // tags
	dismiss int
}

// ctxItems returns the menu entries for t and the action of each.
func ctxItems(t ctxTarget) ([]widgets.PopupItem, []ctxAction) {
	var items []widgets.PopupItem
	var acts []ctxAction
	add := func(label string, a ctxAction) {
		items = append(items, widgets.PopupItem{Label: label})
		acts = append(acts, a)
	}
	sep := func() {
		items = append(items, widgets.PopupItem{Separator: true})
		acts = append(acts, actNone)
	}
	switch {
	case t.isSong:
		add("Play", actPlay)
	case t.it.kind == kindArtist:
		add("Show albums", actOpen)
		add("Play all", actPlay)
		add("Shuffle all", actShuffle)
	default:
		add("Play", actPlay)
		add("Shuffle", actShuffle)
	}
	sep()
	add("Play next", actNext)
	add("Add to queue", actQueue)
	return items, acts
}

// contextEvents registers a right-click handler over the current clip
// rectangle r. It passes events through to the widgets below.
func (p *Panel) contextEvents(gtx layout.Context, tag event.Tag, r image.Rectangle, t ctxTarget) {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok && e.Buttons == pointer.ButtonSecondary {
			t := t
			p.ctx.pending = &t
		}
	}
	defer clip.Rect(r).Push(gtx.Ops).Pop()
	defer pointer.PassOp{}.Push(gtx.Ops).Pop()
	event.Op(gtx.Ops, tag)
}

// closeCtx closes the context menu; it reports whether one was open.
func (p *Panel) closeCtx() bool {
	open := p.ctx.open != nil
	p.ctx.open, p.ctx.pending = nil, nil
	return open
}

// layoutCtx draws the context menu over the panel of size sz, or installs
// the pass-through layer that learns where a right-click happened.
func (p *Panel) layoutCtx(gtx layout.Context, sz image.Point) {
	full := image.Rectangle{Max: sz}
	m := &p.ctx
	if m.open == nil {
		for {
			ev, ok := gtx.Event(pointer.Filter{Target: &m.capture, Kinds: pointer.Press})
			if !ok {
				break
			}
			if e, ok := ev.(pointer.Event); ok && e.Buttons == pointer.ButtonSecondary && m.pending != nil {
				m.open, m.pos = m.pending, e.Position.Round()
			}
		}
		m.pending = nil
		if m.open == nil {
			st := clip.Rect(full).Push(gtx.Ops)
			ps := pointer.PassOp{}.Push(gtx.Ops)
			event.Op(gtx.Ops, &m.capture)
			ps.Pop()
			st.Pop()
			return
		}
	}

	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &m.dismiss, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := ev.(pointer.Event); ok {
			p.closeCtx()
			return
		}
	}
	st := clip.Rect(full).Push(gtx.Ops)
	event.Op(gtx.Ops, &m.dismiss)
	st.Pop()

	t := *m.open
	items, acts := ctxItems(t)
	msz := widgets.PopupSize(gtx, items)
	margin := gtx.Dp(theme.MenuMargin)
	pos := m.pos
	if pos.X+msz.X > sz.X-margin {
		pos.X -= msz.X
	}
	if pos.Y+msz.Y > sz.Y-margin {
		pos.Y -= msz.Y
	}
	pos.X = max(margin, min(pos.X, sz.X-margin-msz.X))
	pos.Y = max(margin, min(pos.Y, sz.Y-margin-msz.Y))
	off := op.Offset(pos).Push(gtx.Ops)
	mg := gtx
	mg.Constraints = layout.Exact(msz)
	idx, _ := m.popup.Layout(mg, p.e.Theme, items)
	off.Pop()
	if idx < 0 || idx >= len(acts) || acts[idx] == actNone {
		return
	}
	p.closeCtx()
	a := acts[idx]
	p.actions = append(p.actions, func() { p.runCtx(t, a) })
}

func (p *Panel) runCtx(t ctxTarget, a ctxAction) {
	if t.isSong {
		switch a {
		case actPlay:
			p.playSong(t.song)
		case actNext, actQueue:
			p.enqueue(nil, t.song, a == actNext)
		}
		return
	}
	it := t.it
	switch a {
	case actOpen:
		p.openArtist(it.id, it.title)
	case actPlay, actShuffle:
		p.playItem(it, a == actShuffle)
	case actNext, actQueue:
		p.enqueue(&it, subsonic.Song{}, a == actNext)
	}
}
