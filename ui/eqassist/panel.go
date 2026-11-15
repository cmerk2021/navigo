// Package eqassist is the EQ Assistant panel ("Which sounds best?"): the
// user picks the best-sounding of four tunings in three rounds while a short
// section of a song loops through the real EQ, and the choices are combined
// into a custom EQ.
package eqassist

import (
	"context"
	"image"
	"image/color"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/internal/config"
	eqcore "github.com/cmerk2021/navigo/internal/eq"
	assist "github.com/cmerk2021/navigo/internal/eqassist"
	"github.com/cmerk2021/navigo/internal/subsonic"
	"github.com/cmerk2021/navigo/ui/env"
	uieq "github.com/cmerk2021/navigo/ui/eq"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

type stage int

const (
	stageStart stage = iota
	stagePreparing
	stageRounds
	stageResult
)

const (
	searchDebounce = 300 * time.Millisecond
	searchResults  = 30
)

// Panel is the EQ Assistant panel.
type Panel struct {
	e      *env.Env
	rounds [assist.Rounds]assist.Round

	stage stage

	// Original EQ settings, restored unless the result is applied.
	orig     eqcore.Settings
	haveOrig bool
	applied  bool

	// Song used for the preview (nil = current track).
	song *subsonic.Song

	// Rounds.
	round   int
	choices [assist.Rounds]int
	sel     int

	// Result.
	bands  [eqcore.NumBands]float64
	preamp float64
	after  int // 0 = Before, 1 = After

	// Song search.
	searchOpen  bool
	focusSearch bool
	search      widgets.TextField
	query       string // text typed, pending debounce
	due         time.Time
	searched    string // query of the last search started
	resultsList widget.List
	resultClick []widget.Clickable

	// Inline preset name field on the result screen.
	naming    bool
	focusName bool
	name      widgets.TextField

	// Data written by background goroutines.
	mu         sync.Mutex
	gen        int // invalidates preview goroutines
	prepDone   bool
	prepErr    error
	cancelPrep context.CancelFunc
	searchGen  int
	searching  bool
	results    []subsonic.Song
	searchErr  string
	cancelFind context.CancelFunc

	closeBtn     widgets.RoundButton
	songBtn      widgets.PillButton
	currentBtn   widgets.PillButton
	startBtn     widgets.PillButton
	optBtns      [assist.OptionsPerRound]widget.Clickable
	pickBtn      widgets.PillButton
	beforeAfter  widgets.Segmented
	bank         uieq.SliderBank
	saveBtn      widgets.PillButton
	applyBtn     widgets.PillButton
	redoBtn      widgets.PillButton
	cancelBtn    widgets.PillButton
	nameSaveBtn  widgets.PillButton
	nameCancelBt widgets.PillButton
}

// New creates the EQ Assistant panel.
func New(e *env.Env) *Panel {
	p := &Panel{e: e, rounds: assist.AllRounds()}
	p.resultsList.Axis = layout.Vertical
	for _, f := range []*widgets.TextField{&p.search, &p.name} {
		f.SingleLine = true
		f.Submit = true
	}
	return p
}

// Opened implements env.Opener: it remembers the user's EQ settings and
// resets the assistant to the start screen.
func (p *Panel) Opened() {
	p.orig = p.e.Player.Config().EQ.Settings
	p.haveOrig = true
	p.applied = false
	p.stage = stageStart
	p.song = nil
	p.searchOpen = false
	p.naming = false
	p.resetChoices()
}

// Closed implements env.Closer: unless the result was applied, the original
// EQ is restored; any preview is ended.
func (p *Panel) Closed() {
	p.mu.Lock()
	p.gen++
	if p.cancelPrep != nil {
		p.cancelPrep()
		p.cancelPrep = nil
	}
	if p.cancelFind != nil {
		p.cancelFind()
		p.cancelFind = nil
	}
	p.mu.Unlock()
	if !p.applied && p.haveOrig {
		p.e.Player.SetEQ(p.orig)
	}
	p.e.Player.EndPreview()
	p.haveOrig = false
	p.stage = stageStart
	p.searchOpen = false
	p.naming = false
}

// CapturesKeys implements env.KeyCapturer.
func (p *Panel) CapturesKeys() bool { return p.searchOpen || p.naming }

// HandleEsc implements env.EscHandler.
func (p *Panel) HandleEsc() bool {
	if p.searchOpen {
		p.closeSearch()
		return true
	}
	if p.naming {
		p.naming = false
		return true
	}
	return false
}

func (p *Panel) resetChoices() {
	p.round = 0
	for i := range p.choices {
		p.choices[i] = assist.NeutralOption
	}
	p.sel = assist.NeutralOption
}

func (p *Panel) applyPreview() {
	b, pre := assist.Preview(p.choices, p.round, p.sel)
	p.e.Player.SetEQ(eqcore.Settings{Enabled: true, Bands: b, Preamp: pre})
}

func (p *Panel) applyBeforeAfter() {
	if p.after == 1 {
		p.e.Player.SetEQ(p.resultSettings())
	} else {
		p.e.Player.SetEQ(eqcore.Settings{Enabled: true})
	}
}

func (p *Panel) resultSettings() eqcore.Settings {
	return eqcore.Settings{Enabled: true, Bands: p.bands, Preamp: p.preamp}
}

// start begins preparing the preview loop on a background goroutine.
func (p *Panel) start() {
	var song *subsonic.Song
	if p.song != nil {
		s := *p.song
		song = &s
	}
	p.closeSearch()
	p.resetChoices()
	p.applyPreview()
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.gen++
	gen := p.gen
	p.prepDone, p.prepErr = false, nil
	if p.cancelPrep != nil {
		p.cancelPrep()
	}
	p.cancelPrep = cancel
	p.mu.Unlock()
	p.stage = stagePreparing
	go func() {
		err := p.e.Player.StartPreview(ctx, song)
		p.mu.Lock()
		stale := gen != p.gen
		if !stale {
			p.prepDone, p.prepErr = true, err
			p.cancelPrep = nil
		}
		p.mu.Unlock()
		cancel()
		if stale && err == nil {
			// The panel was closed (or restarted) meanwhile.
			p.e.Player.EndPreview()
		}
		p.e.Invalidate()
	}()
}

// checkPrepared moves from the preparing stage to the first round (or back
// to the start screen on error).
func (p *Panel) checkPrepared() {
	p.mu.Lock()
	done, err := p.prepDone, p.prepErr
	p.prepDone = false
	p.mu.Unlock()
	if !done {
		return
	}
	if err != nil {
		p.e.Player.SetEQ(p.orig)
		p.e.Toast("Could not start the preview: "+err.Error(), true)
		p.stage = stageStart
		return
	}
	p.stage = stageRounds
	p.applyPreview()
}

func (p *Panel) selectOption(i int) {
	if i < 0 || i >= assist.OptionsPerRound || i == p.sel {
		return
	}
	p.sel = i
	p.applyPreview()
}

func (p *Panel) pick() {
	p.choices[p.round] = p.sel
	p.round++
	if p.round >= assist.Rounds {
		p.bands, p.preamp = assist.Result(p.choices)
		p.after = 1
		p.stage = stageResult
		p.naming = false
		p.applyBeforeAfter()
		return
	}
	p.sel = assist.NeutralOption
	p.applyPreview()
}

func (p *Panel) finish() {
	p.applied = true
	p.e.Player.EndPreview()
	p.e.Close()
}

func (p *Panel) saveAs() bool {
	name := strings.Join(strings.Fields(p.name.Text()), " ")
	if name == "" {
		p.e.Toast("Enter a name for the preset", true)
		return false
	}
	if _, ok := eqcore.FindBuiltin(name); ok {
		p.e.Toast("“"+name+"” is a built-in preset name", true)
		return false
	}
	uieq.SaveCustomPreset(p.e, name, p.resultSettings())
	p.e.Toast("Saved “"+name+"”", false)
	p.finish()
	return true
}

// ---------------- Song search ----------------

func (p *Panel) openSearch() {
	p.searchOpen = true
	p.focusSearch = true
	p.search.SetText("")
	p.query, p.searched = "", ""
	p.mu.Lock()
	p.results, p.searchErr, p.searching = nil, "", false
	p.mu.Unlock()
}

func (p *Panel) closeSearch() {
	p.searchOpen = false
	p.mu.Lock()
	p.searchGen++
	if p.cancelFind != nil {
		p.cancelFind()
		p.cancelFind = nil
	}
	p.mu.Unlock()
}

func (p *Panel) runSearch(q string) {
	p.searched = q
	p.mu.Lock()
	p.searchGen++
	gen := p.searchGen
	if p.cancelFind != nil {
		p.cancelFind()
		p.cancelFind = nil
	}
	if q == "" {
		p.results, p.searchErr, p.searching = nil, "", false
		p.mu.Unlock()
		return
	}
	c := p.e.Player.Client()
	if c == nil {
		p.results, p.searchErr, p.searching = nil, "Not connected to a server", false
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	p.cancelFind = cancel
	p.searching = true
	p.mu.Unlock()
	go func() {
		defer cancel()
		res, err := c.Search3(ctx, q, 0, 0, searchResults)
		p.mu.Lock()
		if gen == p.searchGen {
			p.searching = false
			p.results, p.searchErr = nil, ""
			switch {
			case err != nil:
				if ctx.Err() != context.Canceled {
					p.searchErr = "Search failed: " + err.Error()
				}
			case res != nil:
				p.results = res.Songs
			}
		}
		p.mu.Unlock()
		p.e.Invalidate()
	}()
}

// ---------------- Layout ----------------

// Layout implements env.Panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	if !p.haveOrig {
		p.Opened()
	}
	sz := gtx.Constraints.Max
	widgets.PanelBackground(gtx, p.e.Drag)
	if p.stage == stagePreparing {
		p.checkPrepared()
	}
	title := "Which sounds best?"
	if p.stage == stageResult {
		title = "Your EQ"
	}
	if widgets.PanelHeader(gtx, p.e.Theme, title, &p.closeBtn) {
		p.e.Close()
		return layout.Dimensions{Size: sz}
	}
	switch p.stage {
	case stageStart, stagePreparing:
		p.layoutStart(gtx)
	case stageRounds:
		p.layoutRounds(gtx)
	case stageResult:
		p.layoutResult(gtx)
	}
	return layout.Dimensions{Size: sz}
}

func (p *Panel) contentTop(gtx layout.Context) int {
	return gtx.Dp(theme.PanelPadding) + gtx.Dp(theme.PanelHeaderH) + gtx.Dp(8)
}

func (p *Panel) layoutStart(gtx layout.Context) {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	pad := gtx.Dp(theme.PanelPadding)
	cw := sz.X - 2*pad
	bh := gtx.Dp(theme.PillButtonHeight)
	preparing := p.stage == stagePreparing

	if p.startBtn.Clicked(gtx) && !preparing {
		p.start()
		preparing = true
	}
	y := p.contentTop(gtx)

	if p.searchOpen {
		p.layoutSearch(gtx, image.Rect(pad, y, sz.X-pad, sz.Y-pad-bh-gtx.Dp(12)))
	} else {
		if p.songBtn.Clicked(gtx) && !preparing {
			p.openSearch()
		}
		if p.currentBtn.Clicked(gtx) && !preparing {
			p.song = nil
		}
		y += paragraph(gtx, th, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(120)),
			"You'll hear the same short section of a song with different tunings. "+
				"In each of 3 rounds, pick the one that sounds best to you — switch between them as often as you like.",
			theme.BodySize, font.Normal, theme.TextSecondary, 0)
		y += gtx.Dp(10)
		y += paragraph(gtx, th, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(60)),
			"Tip: use the headphones or speakers you usually listen with.",
			theme.CaptionSize, font.Normal, theme.TextTertiary, 0)
		y += gtx.Dp(18)

		// Song card.
		cardH := gtx.Dp(64)
		card := theme.Rect(pad, y, cw, cardH)
		theme.FillRRect(gtx.Ops, card, gtx.Dp(14), theme.PanelCard)
		caption := "Test song · current track"
		var line string
		if p.song != nil {
			caption = "Test song"
			line = songLine(*p.song)
		} else if snap := p.e.Player.Snapshot(); snap.HasSong {
			line = songLine(snap.Song)
		} else {
			line = "No song playing"
		}
		theme.BoxLoose(gtx, image.Rect(card.Min.X+gtx.Dp(14), card.Min.Y+gtx.Dp(12), card.Max.X-gtx.Dp(14), card.Min.Y+gtx.Dp(30)), func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, caption, theme.CaptionSize, font.Medium, theme.TextTertiary)
		})
		theme.BoxLoose(gtx, image.Rect(card.Min.X+gtx.Dp(14), card.Min.Y+gtx.Dp(32), card.Max.X-gtx.Dp(14), card.Max.Y-gtx.Dp(8)), func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, line, theme.TitleSize, font.SemiBold, theme.White)
		})
		y = card.Max.Y + gtx.Dp(10)
		if p.song != nil {
			half := (cw - gtx.Dp(8)) / 2
			theme.Box(gtx, theme.Rect(pad, y, half, bh), func(gtx layout.Context) layout.Dimensions {
				return p.songBtn.LayoutColor(gtx, th, "Different song", false, !preparing)
			})
			theme.Box(gtx, theme.Rect(sz.X-pad-half, y, half, bh), func(gtx layout.Context) layout.Dimensions {
				return p.currentBtn.LayoutColor(gtx, th, "Current song", false, !preparing)
			})
		} else {
			theme.Box(gtx, theme.Rect(pad, y, cw, bh), func(gtx layout.Context) layout.Dimensions {
				return p.songBtn.LayoutColor(gtx, th, "Use a different song", false, !preparing)
			})
		}
	}

	label := "Start"
	if preparing {
		label = "Preparing…"
	}
	theme.Box(gtx, theme.Rect(pad, sz.Y-pad-bh, cw, bh), func(gtx layout.Context) layout.Dimensions {
		return p.startBtn.LayoutColor(gtx, th, label, true, !preparing)
	})
}

func (p *Panel) layoutSearch(gtx layout.Context, r image.Rectangle) {
	th := p.e.Theme
	for {
		ev, ok := p.search.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.ChangeEvent:
			p.query = strings.TrimSpace(p.search.Text())
			p.due = gtx.Now.Add(searchDebounce)
		case widget.SubmitEvent:
			p.query = strings.TrimSpace(p.search.Text())
			p.due = gtx.Now
		}
	}
	if p.query != p.searched {
		if !gtx.Now.Before(p.due) {
			p.runSearch(p.query)
		} else {
			theme.RedrawAt(gtx, p.due)
		}
	}
	if p.focusSearch {
		gtx.Execute(key.FocusCmd{Tag: &p.search.Editor})
		p.focusSearch = false
	}
	fh := gtx.Dp(theme.FieldHeight)
	theme.Box(gtx, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+fh), func(gtx layout.Context) layout.Dimensions {
		return p.search.Layout(gtx, th, "Search songs", theme.IconSearch)
	})

	p.mu.Lock()
	results, searching, errText := p.results, p.searching, p.searchErr
	p.mu.Unlock()

	lr := image.Rect(r.Min.X, r.Min.Y+fh+gtx.Dp(8), r.Max.X, r.Max.Y)
	msg := ""
	switch {
	case errText != "":
		msg = errText
	case searching && len(results) == 0:
		msg = "Searching…"
	case p.searched == "":
		msg = "Type to search your library."
	case len(results) == 0:
		msg = "No songs found."
	}
	if msg != "" {
		paragraph(gtx, th, image.Rect(lr.Min.X, lr.Min.Y+gtx.Dp(12), lr.Max.X, lr.Max.Y), msg, theme.BodySize, font.Normal, theme.TextSecondary, text.Middle)
		return
	}
	if len(p.resultClick) < len(results) {
		p.resultClick = append(p.resultClick, make([]widget.Clickable, len(results)-len(p.resultClick))...)
	}
	for i := range results {
		if p.resultClick[i].Clicked(gtx) {
			s := results[i]
			p.song = &s
			p.closeSearch()
			return
		}
	}
	rowH := gtx.Dp(theme.RowHeight)
	theme.Box(gtx, lr, func(gtx layout.Context) layout.Dimensions {
		defer clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops).Pop()
		return p.resultsList.Layout(gtx, len(results), func(gtx layout.Context, i int) layout.Dimensions {
			s := results[i]
			rsz := image.Pt(gtx.Constraints.Max.X, rowH)
			gtx.Constraints = layout.Exact(rsz)
			return p.resultClick[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				rr := image.Rectangle{Max: rsz}
				if p.resultClick[i].Hovered() {
					theme.FillRRect(gtx.Ops, rr, gtx.Dp(10), color.NRGBA{R: 255, G: 255, B: 255, A: 20})
				}
				x := gtx.Dp(10)
				theme.BoxLoose(gtx, image.Rect(x, gtx.Dp(6), rsz.X-x, gtx.Dp(26)), func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, s.Title, theme.BodySize, font.Medium, theme.White)
				})
				sub := s.Artist
				if s.Album != "" {
					sub += " · " + s.Album
				}
				theme.BoxLoose(gtx, image.Rect(x, gtx.Dp(26), rsz.X-x, rsz.Y-gtx.Dp(4)), func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, sub, theme.CaptionSize, font.Normal, theme.TextSecondary)
				})
				defer clip.Rect(rr).Push(gtx.Ops).Pop()
				pointer.CursorPointer.Add(gtx.Ops)
				return layout.Dimensions{Size: rsz}
			})
		})
	})
}

func (p *Panel) layoutRounds(gtx layout.Context) {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	pad := gtx.Dp(theme.PanelPadding)
	cw := sz.X - 2*pad

	// Keys 1–4 select, Enter/Return confirms.
	filters := []event.Filter{
		key.Filter{Name: key.NameReturn},
		key.Filter{Name: key.NameEnter},
	}
	for i := range assist.OptionsPerRound {
		filters = append(filters, key.Filter{Name: key.Name(rune('1' + i))})
	}
	confirm := false
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		e, ok := ev.(key.Event)
		if !ok || e.State != key.Press {
			continue
		}
		switch e.Name {
		case key.NameReturn, key.NameEnter:
			confirm = true
		default:
			if n := []rune(string(e.Name)); len(n) == 1 && n[0] >= '1' && n[0] <= '9' {
				p.selectOption(int(n[0] - '1'))
			}
		}
	}
	for i := range p.optBtns {
		if p.optBtns[i].Clicked(gtx) {
			p.selectOption(i)
		}
	}
	if p.pickBtn.Clicked(gtx) {
		confirm = true
	}
	if confirm {
		p.pick()
		if p.stage != stageRounds {
			p.layoutResult(gtx)
			return
		}
	}

	rd := p.rounds[p.round]
	y := p.contentTop(gtx)

	// Progress.
	lh := gtx.Sp(theme.CaptionSize * 1.4)
	theme.BoxLoose(gtx, image.Rect(pad, y, sz.X-pad, y+lh), func(gtx layout.Context) layout.Dimensions {
		return th.Text(gtx, "Round "+strconv.Itoa(p.round+1)+" of "+strconv.Itoa(assist.Rounds), theme.CaptionSize, font.SemiBold, theme.TextSecondary)
	})
	y += lh + gtx.Dp(8)
	sgap := gtx.Dp(6)
	segW := (cw - (assist.Rounds-1)*sgap) / assist.Rounds
	segH := gtx.Dp(4)
	for i := range assist.Rounds {
		col := theme.SeekTrack
		if i <= p.round {
			col = th.Accent()
		}
		theme.FillPill(gtx.Ops, theme.Rect(pad+i*(segW+sgap), y, segW, segH), col)
	}
	y += segH + gtx.Dp(36)

	// Round title and subtitle.
	y += paragraph(gtx, th, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(30)), rd.Title, theme.HeadingSize+3, font.Bold, theme.White, text.Middle)
	y += gtx.Dp(6)
	y += paragraph(gtx, th, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(44)), rd.Subtitle, theme.BodySize, font.Normal, theme.TextSecondary, text.Middle)
	y += gtx.Dp(48)

	// Option buttons.
	os := gtx.Dp(theme.EQOptionSize)
	slot := cw / assist.OptionsPerRound
	for i, opt := range rd.Options {
		cx := pad + i*slot + slot/2
		br := theme.Rect(cx-os/2, y, os, os)
		active := i == p.sel
		theme.Box(gtx, br, func(gtx layout.Context) layout.Dimensions {
			return p.optBtns[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				r := image.Rectangle{Max: gtx.Constraints.Min}
				if active {
					theme.FillCircle(gtx.Ops, r, th.Accent())
				} else {
					theme.FillCircle(gtx.Ops, r, theme.FieldBG)
				}
				if p.optBtns[i].Hovered() {
					theme.FillCircle(gtx.Ops, r, theme.GlassHover)
				}
				layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min = image.Point{}
					return th.TextAligned(gtx, opt.Label, theme.HeadingSize+5, font.Bold, theme.White, text.Middle)
				})
				defer clip.Ellipse(r).Push(gtx.Ops).Pop()
				pointer.CursorPointer.Add(gtx.Ops)
				return layout.Dimensions{Size: r.Max}
			})
		})
		col := theme.TextSecondary
		w := font.Normal
		if active {
			col, w = theme.White, font.SemiBold
		}
		paragraph(gtx, th, image.Rect(cx-slot/2+gtx.Dp(2), br.Max.Y+gtx.Dp(10), cx+slot/2-gtx.Dp(2), br.Max.Y+gtx.Dp(50)), opt.Name, theme.CaptionSize, w, col, text.Middle)
	}
	y += os + gtx.Dp(62)
	paragraph(gtx, th, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(40)),
		"Switch freely while the music plays (keys 1–4).", theme.CaptionSize, font.Normal, theme.TextTertiary, text.Middle)

	bh := gtx.Dp(theme.PillButtonHeight)
	theme.Box(gtx, theme.Rect(pad, sz.Y-pad-bh, cw, bh), func(gtx layout.Context) layout.Dimensions {
		return p.pickBtn.Layout(gtx, th, "Pick this one", true)
	})
}

func (p *Panel) layoutResult(gtx layout.Context) {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	pad := gtx.Dp(theme.PanelPadding)
	cw := sz.X - 2*pad
	bh := gtx.Dp(theme.PillButtonHeight)
	gap := gtx.Dp(8)

	if p.redoBtn.Clicked(gtx) {
		p.resetChoices()
		p.naming = false
		p.stage = stageRounds
		p.applyPreview()
		return
	}
	if p.cancelBtn.Clicked(gtx) {
		p.e.Player.SetEQ(p.orig)
		p.e.Close()
		return
	}
	if !p.naming {
		if p.saveBtn.Clicked(gtx) {
			p.naming = true
			p.focusName = true
			p.name.SetText(uieq.DefaultPresetName)
			p.name.SetCaret(utf8.RuneCountInString(uieq.DefaultPresetName), 0)
		}
		if p.applyBtn.Clicked(gtx) {
			s := p.resultSettings()
			p.e.Player.UpdateConfig(func(c *config.Config) {
				c.EQ.Settings = s
				c.EQ.Preset = ""
			})
			p.finish()
			return
		}
	}

	y := p.contentTop(gtx)
	curveH := gtx.Dp(130)
	curveR := theme.Rect(pad, y, cw, curveH)
	caption := "After"
	if p.after == 0 {
		caption = "Before (flat)"
	}
	theme.Box(gtx, curveR, func(gtx layout.Context) layout.Dimensions {
		return uieq.CurveCard(gtx, th, p.bands, p.preamp, p.after == 1, caption)
	})
	y = curveR.Max.Y + gtx.Dp(10)

	segH := gtx.Dp(theme.SegmentHeight)
	rows := 2*bh + gap
	bankBottom := sz.Y - pad - rows - gtx.Dp(12) - segH - gtx.Dp(12)
	bankR := image.Rect(pad, y, sz.X-pad, bankBottom)
	theme.Box(gtx, bankR, func(gtx layout.Context) layout.Dimensions {
		b, pre := p.bands, p.preamp
		p.bank.Layout(gtx, th, &b, &pre, uieq.BankOptions{ReadOnly: true})
		return layout.Dimensions{Size: gtx.Constraints.Min}
	})
	y = bankR.Max.Y + gtx.Dp(12)

	theme.Box(gtx, theme.Rect(pad, y, cw, segH), func(gtx layout.Context) layout.Dimensions {
		if p.beforeAfter.Layout(gtx, th, []string{"Before", "After"}, &p.after) {
			p.applyBeforeAfter()
		}
		return layout.Dimensions{Size: gtx.Constraints.Min}
	})

	y1 := sz.Y - pad - rows
	y2 := y1 + bh + gap
	half := (cw - gap) / 2
	if p.naming {
		p.layoutName(gtx, theme.Rect(pad, y1, cw, bh), theme.Rect(pad, y2, half, bh), theme.Rect(pad+half+gap, y2, cw-half-gap, bh))
		return
	}
	theme.Box(gtx, theme.Rect(pad, y1, half, bh), func(gtx layout.Context) layout.Dimensions {
		return p.saveBtn.Layout(gtx, th, "Save as preset", true)
	})
	theme.Box(gtx, theme.Rect(pad+half+gap, y1, cw-half-gap, bh), func(gtx layout.Context) layout.Dimensions {
		return p.applyBtn.Layout(gtx, th, "Apply without saving", false)
	})
	theme.Box(gtx, theme.Rect(pad, y2, half, bh), func(gtx layout.Context) layout.Dimensions {
		return p.redoBtn.Layout(gtx, th, "Redo", false)
	})
	theme.Box(gtx, theme.Rect(pad+half+gap, y2, cw-half-gap, bh), func(gtx layout.Context) layout.Dimensions {
		return p.cancelBtn.Layout(gtx, th, "Cancel", false)
	})
}

func (p *Panel) layoutName(gtx layout.Context, field, save, cancel image.Rectangle) {
	th := p.e.Theme
	for {
		ev, ok := p.name.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok && p.saveAs() {
			return
		}
	}
	if p.nameSaveBtn.Clicked(gtx) && p.saveAs() {
		return
	}
	if p.nameCancelBt.Clicked(gtx) {
		p.naming = false
		return
	}
	if p.focusName {
		gtx.Execute(key.FocusCmd{Tag: &p.name.Editor})
		p.focusName = false
	}
	theme.Box(gtx, field, func(gtx layout.Context) layout.Dimensions {
		return p.name.Layout(gtx, th, "Preset name", theme.IconNone)
	})
	theme.Box(gtx, save, func(gtx layout.Context) layout.Dimensions {
		return p.nameSaveBtn.Layout(gtx, th, "Save", true)
	})
	theme.Box(gtx, cancel, func(gtx layout.Context) layout.Dimensions {
		return p.nameCancelBt.Layout(gtx, th, "Cancel", false)
	})
}

// ---------------- helpers ----------------

// paragraph draws wrapped text into r and returns its height.
func paragraph(gtx layout.Context, th *theme.Theme, r image.Rectangle, txt string, size unit.Sp, w font.Weight, col color.NRGBA, align text.Alignment) int {
	d := theme.BoxLoose(gtx, r, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return th.Paragraph(gtx, txt, size, w, col, 0, align)
	})
	return d.Size.Y
}

func songLine(s subsonic.Song) string {
	if s.Artist == "" {
		return s.Title
	}
	return s.Title + " – " + s.Artist
}

var (
	_ env.Panel       = (*Panel)(nil)
	_ env.Opener      = (*Panel)(nil)
	_ env.Closer      = (*Panel)(nil)
	_ env.KeyCapturer = (*Panel)(nil)
	_ env.EscHandler  = (*Panel)(nil)
)
