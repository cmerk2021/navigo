package picker

import (
	"fmt"
	"image"
	"strconv"
	"strings"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/internal/subsonic"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

const (
	gridCoverSize  = 300
	trackCoverSize = 120
)

type itemKind int

const (
	kindAlbum itemKind = iota
	kindPlaylist
	kindArtist
)

// item is one cover of the grid.
type item struct {
	kind  itemKind
	id    string
	cover string
	title string
	sub   string
}

func (it item) key() string {
	return [...]string{"al:", "pl:", "ar:"}[it.kind] + it.id
}

type rowKind int

const (
	rowGrid rowKind = iota
	rowHeading
	rowTrack
	rowMessage
)

// row is one element of the virtualized list.
type row struct {
	kind  rowKind
	items []item
	text  string
	song  subsonic.Song
}

// cell holds the widget state of a grid item.
type cell struct {
	click   widget.Clickable
	play    widgets.RoundButton
	shuffle widgets.RoundButton
}

func (p *Panel) cell(k string) *cell {
	c, ok := p.cells[k]
	if !ok {
		c = new(cell)
		p.cells[k] = c
	}
	return c
}

func (p *Panel) trackClick(id string) *widget.Clickable {
	c, ok := p.tracks[id]
	if !ok {
		c = new(widget.Clickable)
		p.tracks[id] = c
	}
	return c
}

type rowBuilder struct{ rows []row }

func (b *rowBuilder) grid(items []item) {
	for i := 0; i < len(items); i += theme.GridColumns {
		b.rows = append(b.rows, row{kind: rowGrid, items: items[i:min(i+theme.GridColumns, len(items))]})
	}
}

func (b *rowBuilder) heading(s string) { b.rows = append(b.rows, row{kind: rowHeading, text: s}) }
func (b *rowBuilder) message(s string) { b.rows = append(b.rows, row{kind: rowMessage, text: s}) }

// status appends a loading/error/empty message for a collection of n items.
func (b *rowBuilder) status(s loadState, n int, empty string) {
	switch {
	case s.loading || !s.loaded && s.err == "":
		b.message("Loading…")
	case s.err != "":
		b.message(s.err)
	case n == 0:
		b.message(empty)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func albumItems(albums []subsonic.Album, withArtist bool) []item {
	items := make([]item, len(albums))
	for i, a := range albums {
		sub := a.Artist
		if !withArtist {
			if a.Year > 0 {
				sub = strconv.Itoa(a.Year)
			} else {
				sub = plural(a.SongCount, "track", "tracks")
			}
		}
		items[i] = item{kind: kindAlbum, id: a.ID, cover: a.CoverArt, title: a.Name, sub: sub}
	}
	return items
}

func playlistItems(pls []subsonic.Playlist) []item {
	items := make([]item, len(pls))
	for i, pl := range pls {
		items[i] = item{kind: kindPlaylist, id: pl.ID, cover: pl.CoverArt, title: pl.Name, sub: plural(pl.SongCount, "track", "tracks")}
	}
	return items
}

func artistItems(artists []subsonic.Artist) []item {
	items := make([]item, len(artists))
	for i, a := range artists {
		items[i] = item{kind: kindArtist, id: a.ID, cover: a.CoverArt, title: a.Name, sub: plural(a.AlbumCount, "album", "albums")}
	}
	return items
}

// buildRowsLocked turns the current view's data into list rows.
func (p *Panel) buildRowsLocked(vk viewKey) []row {
	var b rowBuilder
	switch {
	case p.d.artist != nil:
		av := p.d.artist
		b.grid(albumItems(av.albums, false))
		b.status(av.loadState, len(av.albums), "No albums")
	case vk.search:
		p.searchRowsLocked(&b, vk.query)
	case vk.tab == tabPlaylists:
		b.grid(playlistItems(p.d.playlists))
		b.status(p.d.plState, len(p.d.playlists), "No playlists")
	case vk.tab == tabArtists:
		b.grid(artistItems(p.d.artists))
		b.status(p.d.arState, len(p.d.artists), "No artists")
	default:
		l := &p.d.albums[vk.tab]
		b.grid(albumItems(l.albums, true))
		b.status(l.loadState, len(l.albums), "No albums")
	}
	return b.rows
}

func (p *Panel) searchRowsLocked(b *rowBuilder, q string) {
	s := p.d.search
	lq := strings.ToLower(q)
	var pls []subsonic.Playlist
	for _, pl := range p.d.playlists {
		if strings.Contains(strings.ToLower(pl.Name), lq) {
			pls = append(pls, pl)
		}
	}
	searching := p.searchPending || s.loading
	if res := s.res; res != nil {
		if len(res.Albums) > 0 {
			b.heading("Albums")
			b.grid(albumItems(res.Albums, true))
		}
		if len(res.Artists) > 0 {
			b.heading("Artists")
			b.grid(artistItems(res.Artists))
		}
	}
	if len(pls) > 0 {
		b.heading("Playlists")
		b.grid(playlistItems(pls))
	}
	if res := s.res; res != nil && len(res.Songs) > 0 {
		b.heading("Tracks")
		for _, song := range res.Songs {
			b.rows = append(b.rows, row{kind: rowTrack, song: song})
		}
	}
	switch {
	case s.res == nil && searching:
		b.rows = append([]row{{kind: rowMessage, text: "Searching…"}}, b.rows...)
	case s.res == nil && s.err != "":
		b.rows = append([]row{{kind: rowMessage, text: s.err}}, b.rows...)
	case len(b.rows) == 0 && !searching:
		b.message(fmt.Sprintf("No results for “%s”", q))
	}
}

// layoutRow draws one list row of width w; cw is the cover size and g the
// gap between columns and rows.
func (p *Panel) layoutRow(gtx layout.Context, r row, w, cw, g int) layout.Dimensions {
	th := p.e.Theme
	switch r.kind {
	case rowGrid:
		h := cw + gtx.Dp(theme.GridCaptionH)
		for j, it := range r.items {
			theme.Box(gtx, theme.Rect(j*(cw+g), 0, cw, h), func(gtx layout.Context) layout.Dimensions {
				return p.layoutCell(gtx, it)
			})
		}
		return layout.Dimensions{Size: image.Pt(w, h+g)}
	case rowHeading:
		h := gtx.Dp(34)
		theme.BoxLoose(gtx, image.Rect(0, gtx.Dp(6), w, h), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
			return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
				return th.Text(gtx, r.text, theme.BodySize, font.Bold, theme.White)
			})
		})
		return layout.Dimensions{Size: image.Pt(w, h)}
	case rowTrack:
		h := gtx.Dp(theme.RowHeight)
		theme.Box(gtx, image.Rect(0, 0, w, h), func(gtx layout.Context) layout.Dimensions {
			return p.layoutTrack(gtx, r.song)
		})
		return layout.Dimensions{Size: image.Pt(w, h)}
	default:
		h := gtx.Dp(64)
		inset := gtx.Dp(8)
		theme.Box(gtx, image.Rect(inset, 0, w-inset, h), func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = 0
				return th.Paragraph(gtx, r.text, theme.BodySize, font.Normal, theme.TextSecondary, 2, text.Middle)
			})
		})
		return layout.Dimensions{Size: image.Pt(w, h)}
	}
}

// layoutCell draws a cover with its caption in gtx.Constraints.Min. The
// hover buttons are laid out as siblings on top of the cover's clickable so
// their clicks never reach it.
func (p *Panel) layoutCell(gtx layout.Context, it item) layout.Dimensions {
	th := p.e.Theme
	c := p.cell(it.key())
	for {
		cl, ok := c.click.Update(gtx)
		if !ok {
			break
		}
		shuffle := cl.Modifiers.Contain(key.ModShift)
		p.actions = append(p.actions, func() { p.activate(it, shuffle) })
	}
	for c.play.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.playItem(it, false) })
	}
	for c.shuffle.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.playItem(it, true) })
	}

	sz := gtx.Constraints.Min
	cw := sz.X
	playable := it.kind != kindArtist
	hovered := c.click.Hovered() || playable && (c.play.Hovered() || c.shuffle.Hovered())
	radius := gtx.Dp(theme.CoverRadius)
	if it.kind == kindArtist {
		radius = -1
	}

	c.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		cr := image.Rect(0, 0, cw, cw)
		if img := p.e.Player.Covers().Request(it.cover, gridCoverSize, p.e.Invalidate); img != nil {
			th.DrawImage(gtx.Ops, img, cr, radius, 1)
		} else {
			theme.Placeholder(gtx.Ops, cr, radius, it.title)
		}
		if hovered || c.click.Pressed() {
			shade := theme.WithAlpha(theme.GlassButton, 0.5)
			if radius < 0 {
				theme.FillCircle(gtx.Ops, cr, shade)
			} else {
				theme.FillRRect(gtx.Ops, cr, radius, shade)
			}
		}
		align := text.Start
		if it.kind == kindArtist {
			align = text.Middle
		}
		ty := cw + gtx.Dp(6)
		theme.BoxLoose(gtx, theme.Rect(0, ty, cw, gtx.Dp(17)), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = cw
			return th.TextAligned(gtx, it.title, theme.CaptionSize, font.SemiBold, theme.White, align)
		})
		theme.BoxLoose(gtx, theme.Rect(0, ty+gtx.Dp(16), cw, gtx.Dp(16)), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = cw
			return th.TextAligned(gtx, it.sub, theme.SmallSize, font.Normal, theme.TextSecondary, align)
		})
		defer clip.Rect{Max: sz}.Push(gtx.Ops).Pop()
		pointer.CursorPointer.Add(gtx.Ops)
		return layout.Dimensions{Size: sz}
	})

	if hovered && playable {
		bs := gtx.Dp(theme.HoverButtonSize)
		g := gtx.Dp(8)
		x := (cw - 2*bs - g) / 2
		y := (cw - bs) / 2
		theme.Box(gtx, theme.Rect(x, y, bs, bs), func(gtx layout.Context) layout.Dimensions {
			return c.play.Layout(gtx, th, theme.IconPlay, widgets.StyleAccent, 0.5)
		})
		theme.Box(gtx, theme.Rect(x+bs+g, y, bs, bs), func(gtx layout.Context) layout.Dimensions {
			return c.shuffle.Layout(gtx, th, theme.IconShuffle, widgets.StyleGlass, 0.5)
		})
	}
	return layout.Dimensions{Size: sz}
}

// activate handles a click on a cover: artists open their albums, albums and
// playlists start playing.
func (p *Panel) activate(it item, shuffle bool) {
	if it.kind == kindArtist {
		p.openArtist(it.id, it.title)
		return
	}
	p.playItem(it, shuffle)
}

func (p *Panel) layoutTrack(gtx layout.Context, s subsonic.Song) layout.Dimensions {
	th := p.e.Theme
	c := p.trackClick(s.ID)
	for c.Clicked(gtx) {
		p.actions = append(p.actions, func() { p.playSong(s) })
	}
	sz := gtx.Constraints.Min
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		r := image.Rectangle{Max: sz}
		if c.Hovered() {
			theme.FillRRect(gtx.Ops, r, gtx.Dp(10), theme.PanelCard)
		}
		inset := gtx.Dp(6)
		ts := gtx.Dp(theme.TrackThumb)
		tr := theme.Rect(inset, (sz.Y-ts)/2, ts, ts)
		cover := s.CoverArt
		if cover == "" {
			cover = s.AlbumID
		}
		if img := p.e.Player.Covers().Request(cover, trackCoverSize, p.e.Invalidate); img != nil {
			th.DrawImage(gtx.Ops, img, tr, gtx.Dp(6), 1)
		} else {
			theme.Placeholder(gtx.Ops, tr, gtx.Dp(6), s.Album+s.Title)
		}

		x := tr.Max.X + gtx.Dp(10)
		right := sz.X - gtx.Dp(10)
		if s.Duration > 0 {
			dw := gtx.Dp(40)
			theme.BoxLoose(gtx, image.Rect(right-dw, 0, right, sz.Y), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = gtx.Constraints.Max
				return theme.E(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, formatDuration(s.Duration), theme.SmallSize, font.Normal, theme.TextTertiary)
				})
			})
			right -= dw + gtx.Dp(6)
		}
		cy := sz.Y / 2
		theme.BoxLoose(gtx, image.Rect(x, cy-gtx.Dp(18), right, cy), func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, s.Title, theme.BodySize, font.SemiBold, theme.White)
		})
		sub := s.Artist
		if s.Album != "" {
			if sub != "" {
				sub += " · "
			}
			sub += s.Album
		}
		theme.BoxLoose(gtx, image.Rect(x, cy+gtx.Dp(1), right, cy+gtx.Dp(18)), func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, sub, theme.CaptionSize, font.Normal, theme.TextSecondary)
		})
		defer clip.Rect(r).Push(gtx.Ops).Pop()
		pointer.CursorPointer.Add(gtx.Ops)
		return layout.Dimensions{Size: sz}
	})
}

func formatDuration(sec int) string {
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}
