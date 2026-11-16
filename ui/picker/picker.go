// Package picker implements the music picker panel: a search field, tabs for
// albums, playlists, artists, random and recently played albums, and a
// virtualized grid of covers.
package picker

import (
	"context"
	"image"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

type tab int

const (
	tabAlbums tab = iota
	tabPlaylists
	tabArtists
	tabRandom
	tabRecent
	numTabs
)

var tabNames = []string{"Albums", "Playlists", "Artists", "Random", "Recent"}

const searchDebounce = 300 * time.Millisecond

// viewKey identifies what the grid currently shows.
type viewKey struct {
	tab    tab
	artist string
	search bool
	query  string
}

func (v viewKey) isTab() bool { return v.artist == "" && !v.search }

type rowsKey struct {
	view    viewKey
	version int
	pending bool
}

// Panel is the music picker.
type Panel struct {
	e *env.Env

	mu sync.Mutex
	d  data

	tab      int
	field    widgets.TextField
	clearBtn widgets.RoundButton
	closeBtn widgets.RoundButton
	back     widget.Clickable
	seg      widgets.Segmented
	connect  widgets.PillButton

	shuffleAll    widgets.PillButton
	artistPlay    widgets.RoundButton
	artistShuffle widgets.RoundButton
	ctx           ctxMenu

	list      layout.List
	positions [numTabs]layout.Position
	view      viewKey

	cells  map[string]*cell
	tracks map[string]*trackRow

	focusNext     bool
	lastText      string
	lastQuery     string
	searchDue     time.Time
	searchPending bool
	searchCancel  context.CancelFunc

	rows    []row
	rowsKey rowsKey
	actions []func()
}

// New creates the picker panel.
func New(e *env.Env) *Panel {
	p := &Panel{
		e:      e,
		cells:  map[string]*cell{},
		tracks: map[string]*trackRow{},
	}
	p.list.Axis = layout.Vertical
	p.field.SingleLine = true
	p.field.Submit = true
	p.rowsKey.version = -1
	p.mu.Lock()
	p.resetLocked(e.Player.Client())
	p.mu.Unlock()
	return p
}

// Opened focuses the search field and refreshes stale data.
func (p *Panel) Opened() {
	p.focusNext = true
	p.closeCtx()
	c := p.e.Player.Client()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.d.closeReq = false
	if c != p.d.client {
		p.resetLocked(c)
		p.positions = [numTabs]layout.Position{}
		p.list.Position = layout.Position{}
		p.searchPending = p.lastQuery != ""
		return
	}
	for i := range p.d.albums {
		l := &p.d.albums[i]
		if l.err != "" || tab(i) == tabRecent && !l.loading {
			p.resetAlbumListLocked(tab(i))
			p.positions[i] = layout.Position{}
			if p.view.isTab() && p.view.tab == tab(i) {
				p.list.Position = layout.Position{}
			}
		}
	}
	if !p.d.plState.loading {
		p.d.plState = loadState{seq: p.d.plState.seq + 1}
	}
	if p.d.arState.err != "" {
		p.d.arState = loadState{seq: p.d.arState.seq + 1}
	}
	p.d.version++
}

// CapturesKeys reports that the picker has text input.
func (p *Panel) CapturesKeys() bool { return true }

// HandleEsc leaves the artist sub-view or clears the search; it returns false
// when there is nothing to undo so the shell closes the panel.
func (p *Panel) HandleEsc() bool {
	if p.closeCtx() {
		return true
	}
	if p.closeArtist() {
		return true
	}
	if p.field.Text() != "" {
		p.clearSearch()
		return true
	}
	return false
}

func (p *Panel) clearSearch() {
	p.field.SetText("")
	p.lastText, p.lastQuery = "", ""
	p.clearSearchData()
}

func (p *Panel) textChanged(gtx layout.Context, t string) {
	p.lastText = t
	q := strings.TrimSpace(t)
	if q == p.lastQuery {
		return
	}
	p.lastQuery = q
	p.closeArtist()
	if q == "" {
		p.clearSearchData()
		return
	}
	p.searchPending = true
	p.searchDue = gtx.Now.Add(searchDebounce)
}

// Layout draws the panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	widgets.PanelBackground(gtx, p.e.Drag)

	if len(p.cells) > 3000 {
		p.cells = map[string]*cell{}
	}
	if len(p.tracks) > 3000 {
		p.tracks = map[string]*trackRow{}
	}

	c := p.e.Player.Client()
	p.mu.Lock()
	if c != p.d.client {
		p.resetLocked(c)
		p.positions = [numTabs]layout.Position{}
		p.list.Position = layout.Position{}
		p.searchPending = p.lastQuery != ""
	}
	closeReq := p.d.closeReq
	p.d.closeReq = false
	p.mu.Unlock()
	if p.closeBtn.Clicked(gtx) {
		closeReq = true
	}

	pad := gtx.Dp(theme.PanelPadding)
	gap := gtx.Dp(10)
	cs := gtx.Dp(theme.CloseButtonSize)
	sh := gtx.Dp(theme.SearchPillHeight)
	theme.Box(gtx, theme.Rect(sz.X-pad-cs, pad+(sh-cs)/2, cs, cs), func(gtx layout.Context) layout.Dimensions {
		return p.closeBtn.Layout(gtx, th, theme.IconClose, widgets.StyleCompact, 0.5)
	})

	if c == nil {
		p.layoutDisconnected(gtx, sz)
	} else {
		searchR := image.Rect(pad, pad, sz.X-pad-cs-gtx.Dp(8), pad+sh)
		p.layoutSearch(gtx, searchR)

		segR := theme.Rect(pad, searchR.Max.Y+gap, sz.X-2*pad, gtx.Dp(theme.SegmentHeight))
		changed := false
		theme.Box(gtx, segR, func(gtx layout.Context) layout.Dimensions {
			changed = p.seg.Layout(gtx, th, tabNames, &p.tab)
			return layout.Dimensions{Size: segR.Size()}
		})
		if changed {
			p.closeCtx()
			p.tabChanged()
		}

		p.layoutContent(gtx, image.Rect(pad, segR.Max.Y+gap, sz.X-pad, sz.Y-pad))
	}
	p.layoutCtx(gtx, sz)

	actions := p.actions
	p.actions = nil
	for _, a := range actions {
		a()
	}
	if closeReq {
		p.e.Close()
	}
	return layout.Dimensions{Size: sz}
}

func (p *Panel) tabChanged() {
	if p.field.Text() != "" {
		p.clearSearch()
	}
	p.closeArtist()
	p.mu.Lock()
	t := tab(p.tab)
	switch t {
	case tabPlaylists:
		if p.d.plState.err != "" {
			p.d.plState = loadState{seq: p.d.plState.seq + 1}
		}
	case tabArtists:
		if p.d.arState.err != "" {
			p.d.arState = loadState{seq: p.d.arState.seq + 1}
		}
	default:
		if p.d.albums[t].err != "" {
			p.resetAlbumListLocked(t)
		}
	}
	p.d.version++
	p.mu.Unlock()
}

func (p *Panel) layoutSearch(gtx layout.Context, r image.Rectangle) {
	th := p.e.Theme
	if p.focusNext {
		p.focusNext = false
		gtx.Execute(key.FocusCmd{Tag: &p.field.Editor})
	}
	submitted := false
	for {
		ev, ok := p.field.Editor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			submitted = true
		}
	}
	if p.clearBtn.Clicked(gtx) {
		p.clearSearch()
		gtx.Execute(key.FocusCmd{Tag: &p.field.Editor})
	}

	theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
		return p.field.Layout(gtx, th, "Search albums, artists, tracks", theme.IconSearch)
	})
	if t := p.field.Text(); t != p.lastText {
		p.textChanged(gtx, t)
	}
	if p.lastText != "" {
		bs := gtx.Dp(24)
		br := theme.Rect(r.Max.X-gtx.Dp(7)-bs, r.Min.Y+(r.Dy()-bs)/2, bs, bs)
		theme.Box(gtx, br, func(gtx layout.Context) layout.Dimensions {
			return p.clearBtn.Layout(gtx, th, theme.IconClose, widgets.StyleCompact, 0.5)
		})
	}

	if p.searchPending && p.lastQuery != "" {
		if submitted || !gtx.Now.Before(p.searchDue) {
			p.searchPending = false
			p.startSearch(p.lastQuery)
		} else {
			theme.RedrawAt(gtx, p.searchDue)
		}
	}
}

func (p *Panel) layoutDisconnected(gtx layout.Context, sz image.Point) {
	th := p.e.Theme
	if p.connect.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.e.Open(env.Settings) })
	}
	pad := gtx.Dp(theme.PanelPadding)
	cy := sz.Y / 2
	theme.Box(gtx, image.Rect(pad, cy-gtx.Dp(50), sz.X-pad, cy-gtx.Dp(6)), func(gtx layout.Context) layout.Dimensions {
		return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return th.Paragraph(gtx, "Not connected — open Settings to sign in.", theme.BodySize, font.Normal, theme.TextSecondary, 2, text.Middle)
		})
	})
	bw, bh := gtx.Dp(170), gtx.Dp(theme.PillButtonHeight)
	theme.Box(gtx, theme.Rect((sz.X-bw)/2, cy+gtx.Dp(8), bw, bh), func(gtx layout.Context) layout.Dimensions {
		return p.connect.Layout(gtx, th, "Open Settings", true)
	})
}

// layoutContent draws the back row (artist view), the virtualized grid and
// the loading indicator inside r.
func (p *Panel) layoutContent(gtx layout.Context, r image.Rectangle) {
	th := p.e.Theme
	q := p.lastQuery

	p.mu.Lock()
	vk := viewKey{tab: tab(p.tab)}
	artistName := ""
	artistPlayable := false
	if p.d.artist != nil {
		vk.artist = p.d.artist.id
		artistName = p.d.artist.name
		artistPlayable = len(p.d.artist.albums) > 0
	} else if q != "" {
		vk.search, vk.query = true, q
	}
	rk := rowsKey{view: vk, version: p.d.version, pending: p.searchPending}
	if rk != p.rowsKey {
		p.rows = p.buildRowsLocked(vk)
		p.rowsKey = rk
	}
	busy := p.d.busy > 0 || vk.search && p.d.search.loading && p.d.search.res != nil
	p.mu.Unlock()

	if vk != p.view {
		if p.view.isTab() {
			p.positions[p.view.tab] = p.list.Position
		}
		p.view = vk
		if vk.isTab() {
			p.list.Position = p.positions[vk.tab]
		} else {
			p.list.Position = layout.Position{}
		}
	}

	switch {
	case vk.search:
		p.loadPlaylists()
	case !vk.isTab():
	case vk.tab == tabPlaylists:
		p.loadPlaylists()
	case vk.tab == tabArtists:
		p.loadArtists()
	default:
		p.mu.Lock()
		loaded := p.d.albums[vk.tab].loaded
		p.mu.Unlock()
		if !loaded {
			p.loadAlbumPage(vk.tab)
		}
	}

	if vk.artist != "" {
		bh := gtx.Dp(30)
		p.layoutBack(gtx, theme.Rect(r.Min.X, r.Min.Y, r.Dx(), bh), item{kind: kindArtist, id: vk.artist, title: artistName}, artistPlayable)
		r.Min.Y += bh + gtx.Dp(4)
	}

	rows := p.rows
	theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
		w := gtx.Constraints.Max.X
		g := gtx.Dp(theme.GridGap)
		cw := (w - (theme.GridColumns-1)*g) / theme.GridColumns
		return p.list.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			return p.layoutRow(gtx, rows[i], w, cw, g)
		})
	})

	if vk.isTab() && vk.tab != tabPlaylists && vk.tab != tabArtists {
		pos := p.list.Position
		if pos.First+pos.Count >= len(rows)-2 {
			p.loadAlbumPage(vk.tab)
		}
	}

	if busy {
		txt := "Loading…"
		pw := th.Measure(gtx, txt, theme.CaptionSize, font.Medium) + gtx.Dp(28)
		ph := gtx.Dp(26)
		pr := theme.Rect(r.Min.X+(r.Dx()-pw)/2, r.Max.Y-ph-gtx.Dp(4), pw, ph)
		theme.FillPill(gtx.Ops, pr, theme.MenuBG)
		theme.Box(gtx, pr, func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = image.Point{}
				return th.Text(gtx, txt, theme.CaptionSize, font.Medium, theme.TextSecondary)
			})
		})
	}
}

// layoutBack draws the "‹ Artist name" row of the artist sub-view, with
// buttons that play or shuffle all of the artist's songs.
func (p *Panel) layoutBack(gtx layout.Context, r image.Rectangle, artist item, playable bool) {
	th := p.e.Theme
	name := artist.title
	if p.back.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.closeArtist() })
	}
	if p.artistPlay.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.playItem(artist, false) })
	}
	if p.artistShuffle.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.playItem(artist, true) })
	}
	bs := r.Dy()
	btns := 0
	if playable {
		btns = 2*bs + gtx.Dp(6)
		x := r.Max.X - btns
		theme.Box(gtx, theme.Rect(x, r.Min.Y, bs, bs), func(gtx layout.Context) layout.Dimensions {
			return p.artistPlay.Layout(gtx, th, theme.IconPlay, widgets.StyleAccent, 0.5)
		})
		theme.Box(gtx, theme.Rect(x+bs+gtx.Dp(6), r.Min.Y, bs, bs), func(gtx layout.Context) layout.Dimensions {
			return p.artistShuffle.Layout(gtx, th, theme.IconShuffle, widgets.StyleGlass, 0.5)
		})
		btns += gtx.Dp(8)
	}
	is := gtx.Dp(18)
	tw := min(th.Measure(gtx, name, theme.BodySize, font.SemiBold), r.Dx()-is-gtx.Dp(16)-btns)
	w := is + gtx.Dp(12) + tw
	theme.Box(gtx, theme.Rect(r.Min.X, r.Min.Y, w, r.Dy()), func(gtx layout.Context) layout.Dimensions {
		return p.back.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			sz := gtx.Constraints.Min
			br := image.Rectangle{Max: sz}
			col := theme.TextSecondary
			if p.back.Hovered() {
				col = theme.White
			}
			theme.DrawIcon(gtx.Ops, theme.IconChevronLeft, theme.Rect(0, (sz.Y-is)/2, is, is), col)
			theme.BoxLoose(gtx, image.Rect(is+gtx.Dp(6), 0, sz.X, sz.Y), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, name, theme.BodySize, font.SemiBold, col)
				})
			})
			defer clip.Rect(br).Push(gtx.Ops).Pop()
			pointer.CursorPointer.Add(gtx.Ops)
			return layout.Dimensions{Size: sz}
		})
	})
}
