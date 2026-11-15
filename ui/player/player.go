// Package player draws the Normal (370×370) and Compact (370×64) player
// views.
package player

import (
	"image"
	"image/color"
	"time"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/core"
	"github.com/cmerk2021/navigo/internal/queue"
	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// Shell is the part of the application shell the views need.
type Shell interface {
	ToggleMode()
	// OpenMenu opens the context menu anchored at a window-local point.
	OpenMenu(anchor image.Point)
	ShowModeButtons() bool
	// OverlayOpen reports whether a menu or panel covers the view.
	OverlayOpen() bool
}

const (
	hideDelay    = 2500 * time.Millisecond
	fadeDuration = 200 * time.Millisecond
	artFade      = 300 * time.Millisecond
	artTimeout   = 3 * time.Second
	doubleClick  = 400 * time.Millisecond
)

// View holds the state of both player modes.
type View struct {
	e      *env.Env
	sh     Shell
	Drag   *widgets.DragArea
	Toasts *widgets.Toasts

	modeBtn, menuBtn                            widgets.RoundButton
	repeatBtn, prevBtn, playBtn, nextBtn, shBtn widgets.RoundButton
	seek                                        widgets.Seekbar
	title, artist                               widgets.Marquee

	cPrev, cPlay, cNext, cMode widgets.RoundButton
	cTitle, cArtist            widgets.Marquee
	thumbTag                   int
	lastThumbClick             time.Time

	// Album art crossfade.
	artKey, wantKey string
	art, prevArt    *image.RGBA
	prevKey         string
	fadeStart       time.Time
	wantSince       time.Time

	// Hover controls.
	hoverTag  int
	hovering  bool
	lastMove  time.Time
	ctrlAlpha float32
	lastFrame time.Time
}

// New creates the views.
func New(e *env.Env, sh Shell, drag *widgets.DragArea, toasts *widgets.Toasts) *View {
	return &View{e: e, sh: sh, Drag: drag, Toasts: toasts}
}

// ---------------------------------------------------------------- art

func (v *View) updateArt(gtx layout.Context, s core.Snapshot) {
	key := ""
	if s.HasSong {
		key = s.Song.CoverArt
	}
	if key != v.wantKey {
		v.wantKey = key
		v.wantSince = gtx.Now
	}
	if v.wantKey == v.artKey && (v.art != nil || v.wantKey == "") {
		return
	}
	var img *image.RGBA
	if v.wantKey != "" {
		img = v.e.Player.Covers().Request(v.wantKey, core.CoverSize, v.e.Invalidate)
		if img == nil && gtx.Now.Sub(v.wantSince) < artTimeout {
			theme.RedrawAt(gtx, v.wantSince.Add(artTimeout))
			return
		}
	}
	if v.artKey == v.wantKey && img == nil {
		return
	}
	v.prevArt, v.prevKey = v.art, v.artKey
	v.art, v.artKey = img, v.wantKey
	v.fadeStart = gtx.Now
}

// drawArt paints the current art (or a gradient) with a crossfade.
func (v *View) drawArt(gtx layout.Context, r image.Rectangle, radius int, s core.Snapshot) {
	seed := "navigo"
	if s.HasSong {
		seed = s.Song.Album + s.Song.Artist
	}
	t := float32(1)
	if el := gtx.Now.Sub(v.fadeStart); el < artFade {
		t = float32(el) / float32(artFade)
		theme.Animate(gtx)
	}
	draw := func(img *image.RGBA, key string, alpha float32) {
		if img != nil {
			v.e.Theme.DrawImage(gtx.Ops, img, r, radius, alpha)
			return
		}
		st := paint.PushOpacity(gtx.Ops, alpha)
		theme.Placeholder(gtx.Ops, r, radius, seed+key)
		st.Pop()
	}
	if t < 1 {
		draw(v.prevArt, v.prevKey, 1)
	}
	draw(v.art, v.artKey, t)
}

// ---------------------------------------------------------------- hover

func (v *View) trackHover(gtx layout.Context, r image.Rectangle) {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &v.hoverTag, Kinds: pointer.Enter | pointer.Leave | pointer.Move | pointer.Drag | pointer.Press})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Leave:
			v.hovering = false
		default:
			v.hovering = true
			v.lastMove = gtx.Now
		}
	}
	// Installed on top with PassOp so that it observes events without
	// stealing them from the widgets below.
	st := clip.Rect(r).Push(gtx.Ops)
	pt := pointer.PassOp{}.Push(gtx.Ops)
	event.Op(gtx.Ops, &v.hoverTag)
	pt.Pop()
	st.Pop()
}

func (v *View) controlsAlpha(gtx layout.Context) float32 {
	dt := float32(0)
	if !v.lastFrame.IsZero() {
		dt = float32(gtx.Now.Sub(v.lastFrame)) / float32(fadeDuration)
	}
	v.lastFrame = gtx.Now
	visible := !v.sh.OverlayOpen() && ((v.hovering && gtx.Now.Sub(v.lastMove) < hideDelay) || v.seek.Dragging())
	target := float32(0)
	if visible {
		target = 1
		if !v.seek.Dragging() {
			theme.RedrawAt(gtx, v.lastMove.Add(hideDelay+10*time.Millisecond))
		}
	}
	switch {
	case v.ctrlAlpha < target:
		v.ctrlAlpha = min(target, v.ctrlAlpha+max(dt, 0.05))
	case v.ctrlAlpha > target:
		v.ctrlAlpha = max(target, v.ctrlAlpha-max(dt, 0.05))
	}
	if v.ctrlAlpha != target {
		theme.Animate(gtx)
	}
	return v.ctrlAlpha
}

// ---------------------------------------------------------------- normal

func songText(s core.Snapshot) (string, string) {
	if !s.HasSong {
		return "Nothing playing", "Right-click or press / to choose music"
	}
	artist := s.Song.Artist
	if artist == "" {
		artist = s.Song.AlbumArtist
	}
	if s.Previewing {
		artist = "EQ Assistant preview · " + artist
	}
	return s.Song.Title, artist
}

func playing(s core.Snapshot) bool {
	return s.State == audio.Playing || s.State == audio.Loading || s.State == audio.Buffering
}

// LayoutNormal draws the 370×370 view.
func (v *View) LayoutNormal(gtx layout.Context, s core.Snapshot) {
	p := v.e.Player
	th := v.e.Theme
	for v.modeBtn.Clicked(gtx) {
		v.sh.ToggleMode()
	}
	for v.menuBtn.Clicked(gtx) {
		v.sh.OpenMenu(image.Pt(gtx.Constraints.Max.X-gtx.Dp(theme.NormalMargin), gtx.Dp(theme.NormalMargin+theme.TopBarHeight+4)))
	}
	for v.repeatBtn.Clicked(gtx) {
		p.SetRepeat(s.Repeat.Next())
	}
	for v.prevBtn.Clicked(gtx) {
		p.Previous()
	}
	for v.playBtn.Clicked(gtx) {
		p.PlayPause()
	}
	for v.nextBtn.Clicked(gtx) {
		p.Next()
	}
	for v.shBtn.Clicked(gtx) {
		p.SetShuffle(!s.Shuffle)
	}

	sz := gtx.Constraints.Max
	full := image.Rectangle{Max: sz}
	rad := gtx.Dp(theme.NormalRadius)
	theme.FillRect(gtx.Ops, full, theme.WindowBG)
	v.updateArt(gtx, s)
	v.drawArt(gtx, full, rad, s)

	alpha := v.controlsAlpha(gtx)
	{
		cl := clip.UniformRRect(full, rad).Push(gtx.Ops)
		theme.VerticalGradient(gtx.Ops, image.Rect(0, 0, sz.X, gtx.Dp(theme.TopGradientHeight)), color.NRGBA{A: 150}, color.NRGBA{})
		if alpha > 0 {
			theme.VerticalGradient(gtx.Ops, image.Rect(0, sz.Y-gtx.Dp(theme.BottomGradient), sz.X, sz.Y), color.NRGBA{}, theme.WithAlpha(color.NRGBA{A: 190}, alpha))
		}
		cl.Pop()
	}
	v.Drag.Layout(gtx, full)

	// Top bar.
	m := gtx.Dp(theme.NormalMargin)
	bh := gtx.Dp(theme.TopBarHeight)
	bs := gtx.Dp(theme.TopButtonSize)
	gap := gtx.Dp(theme.TopButtonGap)
	by := m + (bh-bs)/2
	x := sz.X - m - bs
	theme.Box(gtx, theme.Rect(x, by, bs, bs), func(gtx layout.Context) layout.Dimensions {
		return v.menuBtn.Layout(gtx, th, theme.IconMenu, widgets.StyleGlass, 0.55)
	})
	if v.sh.ShowModeButtons() {
		x -= gap + bs
		theme.Box(gtx, theme.Rect(x, by, bs, bs), func(gtx layout.Context) layout.Dimensions {
			return v.modeBtn.Layout(gtx, th, theme.IconCompact, widgets.StyleGlass, 0.6)
		})
	}
	pill := image.Rect(m, m, x-gap, m+bh)
	theme.FillPill(gtx.Ops, pill, theme.GlassPill)
	title, artist := songText(s)
	pp := gtx.Dp(theme.TopPillPadding)
	tw := pill.Dx() - 2*pp
	theme.BoxLoose(gtx, image.Rect(pill.Min.X+pp, pill.Min.Y+gtx.Dp(5), pill.Max.X-pp, pill.Min.Y+gtx.Dp(25)), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.X = tw
		return v.title.Layout(gtx, th, title, theme.TitleSize, font.SemiBold, theme.White)
	})
	theme.BoxLoose(gtx, image.Rect(pill.Min.X+pp, pill.Min.Y+gtx.Dp(24), pill.Max.X-pp, pill.Max.Y), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.X = tw
		return v.artist.Layout(gtx, th, artist, theme.ArtistSize, font.Normal, theme.TextSecondary)
	})

	// Toast below the top bar.
	ty := m + bh + gtx.Dp(10)
	v.Toasts.Layout(gtx, th, image.Rect(m, ty, sz.X-m, ty+gtx.Dp(theme.ToastHeight)))

	if alpha > 0 {
		op := paint.PushOpacity(gtx.Ops, alpha)
		v.layoutControls(gtx, s)
		op.Pop()
	}
	v.trackHover(gtx, full)
}

func (v *View) layoutControls(gtx layout.Context, s core.Snapshot) {
	th := v.e.Theme
	sz := gtx.Constraints.Max
	// Seek bar.
	hit := gtx.Dp(theme.SeekHitHeight)
	sx := gtx.Dp(22)
	sy := gtx.Dp(theme.SeekBarY) - hit/2
	g := gtx
	g.Constraints = layout.Constraints{Max: image.Pt(sz.X-2*sx, hit)}
	widgets.Off(g, image.Pt(sx, sy), func(gtx layout.Context) layout.Dimensions {
		if t, ok := v.seek.Layout(gtx, th, s.Position, s.Duration, s.Buffered); ok {
			v.e.Player.SeekTo(t)
		}
		return layout.Dimensions{}
	})
	if s.State == audio.Playing && v.ctrlAlpha > 0 {
		theme.RedrawAt(gtx, gtx.Now.Add(250*time.Millisecond))
	}

	// Control row.
	small, skip, play := gtx.Dp(theme.SmallControl), gtx.Dp(theme.SkipControl), gtx.Dp(theme.PlayControl)
	gap := gtx.Dp(theme.ControlGap)
	total := 2*small + 2*skip + play + 4*gap
	x := (sz.X - total) / 2
	cy := sz.Y - gtx.Dp(theme.ControlBottomInset) - play/2
	btn := func(b *widgets.RoundButton, size int, ic theme.Icon, style widgets.ButtonStyle, scale float32) {
		r := theme.Rect(x, cy-size/2, size, size)
		theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
			return b.Layout(gtx, th, ic, style, scale)
		})
		x += size + gap
	}
	rstyle := widgets.StyleGlass
	ric := theme.IconRepeat
	if s.Repeat != queue.RepeatOff {
		rstyle = widgets.StyleAccent
	}
	if s.Repeat == queue.RepeatOne {
		ric = theme.IconRepeatOne
	}
	rx := x
	btn(&v.repeatBtn, small, ric, rstyle, 0.5)
	if s.Repeat == queue.RepeatOne {
		badge := gtx.Dp(14)
		br := theme.Rect(rx+small-badge+gtx.Dp(2), cy-small/2-gtx.Dp(2), badge, badge)
		theme.FillCircle(gtx.Ops, br, theme.White)
		theme.Box(gtx, br, func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = image.Point{}
				return th.Text(gtx, "1", 8.5, font.Bold, th.Accent())
			})
		})
	}
	btn(&v.prevBtn, skip, theme.IconPrev, widgets.StyleGlass, 0.46)
	pic := theme.IconPlay
	if playing(s) {
		pic = theme.IconPause
	}
	btn(&v.playBtn, play, pic, widgets.StyleGlass, 0.48)
	btn(&v.nextBtn, skip, theme.IconNext, widgets.StyleGlass, 0.46)
	sstyle := widgets.StyleGlass
	if s.Shuffle {
		sstyle = widgets.StyleAccent
	}
	btn(&v.shBtn, small, theme.IconShuffle, sstyle, 0.5)
}

// ---------------------------------------------------------------- compact

// LayoutCompact draws the 370×64 pill in the top 64 dp of the window.
func (v *View) LayoutCompact(gtx layout.Context, s core.Snapshot) {
	p := v.e.Player
	th := v.e.Theme
	for v.cPrev.Clicked(gtx) {
		p.Previous()
	}
	for v.cPlay.Clicked(gtx) {
		p.PlayPause()
	}
	for v.cNext.Clicked(gtx) {
		p.Next()
	}
	for v.cMode.Clicked(gtx) {
		v.sh.ToggleMode()
	}
	w := gtx.Constraints.Max.X
	h := gtx.Dp(theme.CompactHeight)
	full := image.Rect(0, 0, w, h)
	theme.FillRect(gtx.Ops, image.Rectangle{Max: gtx.Constraints.Max}, theme.WindowBG)
	theme.FillRRect(gtx.Ops, full, gtx.Dp(theme.CompactRadius), theme.CompactOuter)
	v.Drag.Layout(gtx, image.Rectangle{Max: gtx.Constraints.Max})

	pad := gtx.Dp(theme.CompactPadding)
	gap := gtx.Dp(theme.CompactGap)
	bs, ps := gtx.Dp(theme.CompactButton), gtx.Dp(theme.CompactPlay)
	x := w - pad
	cy := h / 2
	btn := func(b *widgets.RoundButton, size int, ic theme.Icon, scale float32) {
		x -= size
		r := theme.Rect(x, cy-size/2, size, size)
		theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
			return b.Layout(gtx, th, ic, widgets.StyleCompact, scale)
		})
		x -= gap
	}
	if v.sh.ShowModeButtons() {
		btn(&v.cMode, bs, theme.IconNormal, 0.5)
	}
	btn(&v.cNext, bs, theme.IconNext, 0.46)
	pic := theme.IconPlay
	if playing(s) {
		pic = theme.IconPause
	}
	btn(&v.cPlay, ps, pic, 0.5)
	btn(&v.cPrev, bs, theme.IconPrev, 0.46)

	inset := (h - gtx.Dp(theme.CompactThumb) - 2*gtx.Dp(theme.CompactInnerPad)) / 2
	inner := image.Rect(inset, inset, x, h-inset)
	theme.FillPill(gtx.Ops, inner, theme.CompactInner)
	ip := gtx.Dp(theme.CompactInnerPad)
	ts := gtx.Dp(theme.CompactThumb)
	thumb := theme.Rect(inner.Min.X+ip, inner.Min.Y+ip, ts, ts)
	v.updateArt(gtx, s)
	v.drawArt(gtx, thumb, -1, s)
	v.thumbEvents(gtx, thumb)

	tx := thumb.Max.X + gtx.Dp(theme.CompactTextInset)
	tw := inner.Max.X - gtx.Dp(14) - tx
	title, artist := songText(s)
	if txt, isErr, ok := v.Toasts.Current(gtx.Now); ok {
		title, artist = txt, ""
		col := theme.White
		if isErr {
			col = theme.ErrorColor
		}
		v.Toasts.Schedule(gtx)
		theme.BoxLoose(gtx, image.Rect(tx, inner.Min.Y, tx+tw, inner.Max.Y), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
				return th.Paragraph(gtx, title, theme.CaptionSize, font.Medium, col, 2, 0)
			})
		})
		return
	}
	ty := inner.Min.Y + (inner.Dy()-gtx.Dp(34))/2
	theme.BoxLoose(gtx, image.Rect(tx, ty, tx+tw, ty+gtx.Dp(19)), func(gtx layout.Context) layout.Dimensions {
		return v.cTitle.Layout(gtx, th, title, theme.TitleSize, font.SemiBold, theme.White)
	})
	theme.BoxLoose(gtx, image.Rect(tx, ty+gtx.Dp(18), tx+tw, ty+gtx.Dp(35)), func(gtx layout.Context) layout.Dimensions {
		return v.cArtist.Layout(gtx, th, artist, theme.ArtistSize, font.Normal, theme.TextSecondary)
	})
}

// thumbEvents handles double-click (toggle mode) and right-click (menu) on
// the compact thumbnail.
func (v *View) thumbEvents(gtx layout.Context, r image.Rectangle) {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &v.thumbTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		if e.Buttons == pointer.ButtonSecondary {
			v.sh.OpenMenu(e.Position.Round())
			continue
		}
		if e.Buttons != pointer.ButtonPrimary {
			continue
		}
		if gtx.Now.Sub(v.lastThumbClick) < doubleClick {
			v.lastThumbClick = time.Time{}
			v.sh.ToggleMode()
		} else {
			v.lastThumbClick = gtx.Now
		}
	}
	st := clip.Ellipse(r).Push(gtx.Ops)
	event.Op(gtx.Ops, &v.thumbTag)
	st.Pop()
}
