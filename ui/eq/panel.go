// Package eq is the Equalizer panel: a 10-band graphic EQ with preamp,
// presets, a live frequency-response curve and custom preset management.
// It also exports the response-curve card and slider bank reused by the EQ
// Assistant.
package eq

import (
	"image"
	"image/color"
	"slices"
	"strings"
	"unicode/utf8"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/internal/audio"
	"github.com/cmerk2021/navigo/internal/config"
	eqcore "github.com/cmerk2021/navigo/internal/eq"
	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// nameMode is the purpose of the inline name row.
type nameMode int

const (
	nameNone nameMode = iota
	nameSave
	nameRename
)

// DefaultPresetName is the suggested name for a new custom preset.
const DefaultPresetName = "My EQ"

// Panel is the Equalizer panel.
type Panel struct {
	e *env.Env

	// Current state, synced from the config whenever no slider is dragged.
	s      eqcore.Settings
	preset string
	custom []eqcore.Preset

	closeBtn   widgets.RoundButton
	presetBtn  widget.Clickable
	enabled    widgets.Toggle
	autoPreamp widgets.Toggle
	bank       SliderBank
	saveBtn    widgets.PillButton
	resetBtn   widgets.PillButton

	dropOpen  bool
	scrim     bool // event tag
	dropCard  bool // event tag
	list      widgets.SelectList
	renameBtn widgets.PillButton
	deleteBtn widgets.PillButton

	mode       nameMode
	renameFrom string
	name       widgets.TextField
	focusName  bool
	okBtn      widgets.PillButton
	cancelBtn  widgets.PillButton
}

// New creates the Equalizer panel.
func New(e *env.Env) *Panel {
	p := &Panel{e: e}
	p.name.SingleLine = true
	p.name.Submit = true
	return p
}

// Opened implements env.Opener.
func (p *Panel) Opened() {
	p.dropOpen = false
	p.mode = nameNone
	p.sync()
}

// Closed implements env.Closer. A drag still in progress is committed.
func (p *Panel) Closed() {
	if p.bank.Active() >= 0 {
		p.commit()
	}
	p.dropOpen = false
	p.mode = nameNone
}

// CapturesKeys implements env.KeyCapturer.
func (p *Panel) CapturesKeys() bool { return p.mode != nameNone }

// HandleEsc implements env.EscHandler.
func (p *Panel) HandleEsc() bool {
	if p.dropOpen {
		p.dropOpen = false
		return true
	}
	if p.mode != nameNone {
		p.mode = nameNone
		return true
	}
	return false
}

func (p *Panel) sync() {
	cfg := p.e.Player.Config()
	p.s = cfg.EQ.Settings
	p.preset = cfg.EQ.Preset
	p.custom = cfg.EQ.Custom
}

// findPreset looks up a built-in or custom preset by name.
func (p *Panel) findPreset(name string) (eqcore.Preset, bool) {
	if name == "" {
		return eqcore.Preset{}, false
	}
	if pr, ok := eqcore.FindBuiltin(name); ok {
		return pr, true
	}
	for _, pr := range p.custom {
		if strings.EqualFold(pr.Name, name) {
			return pr, true
		}
	}
	return eqcore.Preset{}, false
}

func (p *Panel) isCustom(name string) bool {
	if name == "" {
		return false
	}
	for _, pr := range p.custom {
		if strings.EqualFold(pr.Name, name) {
			return true
		}
	}
	return false
}

// commit persists the current settings; the preset stays selected only if
// the values still match it.
func (p *Panel) commit() {
	s := p.s
	preset := ""
	if pr, ok := p.findPreset(p.preset); ok && pr.Bands == s.Bands && pr.Preamp == s.Preamp {
		preset = pr.Name
	}
	p.preset = preset
	p.e.Player.UpdateConfig(func(c *config.Config) {
		c.EQ.Settings = s
		c.EQ.Preset = preset
	})
}

func (p *Panel) applyPreset(pr eqcore.Preset) {
	p.e.Player.UpdateConfig(func(c *config.Config) {
		c.EQ.Bands = pr.Bands
		c.EQ.Preamp = pr.Preamp
		c.EQ.Enabled = true
		c.EQ.Preset = pr.Name
	})
	p.sync()
}

func (p *Panel) openName(m nameMode, initial string) {
	p.mode = m
	p.dropOpen = false
	p.name.SetText(initial)
	n := utf8.RuneCountInString(initial)
	p.name.SetCaret(n, 0)
	p.focusName = true
}

// submitName saves or renames a preset. It returns false (and shows a
// toast) if the name is not acceptable.
func (p *Panel) submitName() bool {
	name := trimName(p.name.Text())
	if name == "" {
		p.e.Toast("Enter a name for the preset", true)
		return false
	}
	if _, ok := eqcore.FindBuiltin(name); ok {
		p.e.Toast("“"+name+"” is a built-in preset name", true)
		return false
	}
	switch p.mode {
	case nameSave:
		SaveCustomPreset(p.e, name, p.s)
		p.e.Toast("Saved “"+name+"”", false)
	case nameRename:
		old := p.renameFrom
		if !strings.EqualFold(old, name) && p.isCustom(name) {
			p.e.Toast("A preset named “"+name+"” already exists", true)
			return false
		}
		p.e.Player.UpdateConfig(func(c *config.Config) {
			for i := range c.EQ.Custom {
				if strings.EqualFold(c.EQ.Custom[i].Name, old) {
					c.EQ.Custom[i].Name = name
				}
			}
			if strings.EqualFold(c.EQ.Preset, old) {
				c.EQ.Preset = name
			}
		})
	}
	p.mode = nameNone
	p.sync()
	return true
}

// SaveCustomPreset stores s's bands and preamp as a custom preset (replacing
// a custom preset with the same name), applies s and selects the preset.
func SaveCustomPreset(e *env.Env, name string, s eqcore.Settings) {
	pr := eqcore.Preset{Name: name, Bands: s.Bands, Preamp: s.Preamp}
	e.Player.UpdateConfig(func(c *config.Config) {
		i := slices.IndexFunc(c.EQ.Custom, func(x eqcore.Preset) bool { return strings.EqualFold(x.Name, name) })
		if i >= 0 {
			c.EQ.Custom[i] = pr
		} else {
			c.EQ.Custom = append(c.EQ.Custom, pr)
		}
		c.EQ.Settings = s
		c.EQ.Preset = name
	})
}

func (p *Panel) deletePreset(name string) {
	p.e.Player.UpdateConfig(func(c *config.Config) {
		c.EQ.Custom = slices.DeleteFunc(c.EQ.Custom, func(x eqcore.Preset) bool { return strings.EqualFold(x.Name, name) })
		if strings.EqualFold(c.EQ.Preset, name) {
			c.EQ.Preset = ""
		}
	})
	p.e.Toast("Deleted “"+name+"”", false)
	p.sync()
}

// Layout implements env.Panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	widgets.PanelBackground(gtx, p.e.Drag)
	if p.bank.Active() < 0 {
		p.sync()
	}

	pad := gtx.Dp(theme.PanelPadding)
	w := sz.X
	cw := w - 2*pad

	// ---- Top row: preset pill, on/off toggle, close button.
	if p.closeBtn.Clicked(gtx) {
		p.e.Close()
	}
	for p.presetBtn.Clicked(gtx) {
		p.dropOpen = !p.dropOpen
		if p.dropOpen {
			p.mode = nameNone
		}
	}
	hh := gtx.Dp(theme.PanelHeaderH)
	cs := gtx.Dp(theme.CloseButtonSize)
	theme.Box(gtx, theme.Rect(w-pad-cs, pad+(hh-cs)/2, cs, cs), func(gtx layout.Context) layout.Dimensions {
		return p.closeBtn.Layout(gtx, th, theme.IconClose, widgets.StyleCompact, 0.5)
	})
	tw, tgh := gtx.Dp(theme.ToggleWidth), gtx.Dp(theme.ToggleHeight)
	tx := w - pad - cs - gtx.Dp(12) - tw
	on := p.s.Enabled
	theme.Offset(gtx, image.Pt(tx, pad+(hh-tgh)/2), func(gtx layout.Context) layout.Dimensions {
		if p.enabled.Layout(gtx, th, &on) {
			p.s.Enabled = on
			p.e.Player.UpdateConfig(func(c *config.Config) { c.EQ.Enabled = on })
		}
		return layout.Dimensions{}
	})
	onLabel := "Off"
	if on {
		onLabel = "On"
	}
	lw := gtx.Dp(28)
	theme.Box(gtx, image.Rect(tx-gtx.Dp(6)-lw, pad, tx-gtx.Dp(6), pad+hh), func(gtx layout.Context) layout.Dimensions {
		return theme.E(gtx, func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, onLabel, theme.CaptionSize, font.SemiBold, theme.TextSecondary)
		})
	})
	presetName := p.preset
	if presetName == "" {
		presetName = "Custom"
	}
	ph := gtx.Dp(36)
	is := gtx.Dp(18)
	pw := th.Measure(gtx, presetName, theme.BodySize, font.SemiBold) + gtx.Dp(14+6+14) + is
	pw = min(max(pw, gtx.Dp(120)), tx-gtx.Dp(6)-lw-gtx.Dp(8)-pad)
	pillR := theme.Rect(pad, pad+(hh-ph)/2, pw, ph)
	theme.Box(gtx, pillR, func(gtx layout.Context) layout.Dimensions {
		return p.presetBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			r := image.Rectangle{Max: gtx.Constraints.Min}
			theme.FillPill(gtx.Ops, r, theme.FieldBG)
			if p.presetBtn.Hovered() || p.dropOpen {
				theme.FillPill(gtx.Ops, r, theme.GlassHover)
			}
			theme.BoxLoose(gtx, image.Rect(gtx.Dp(14), 0, r.Dx()-gtx.Dp(14+6)-is, r.Dy()), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, presetName, theme.BodySize, font.SemiBold, theme.White)
				})
			})
			theme.DrawIcon(gtx.Ops, theme.IconChevronDown, theme.Rect(r.Dx()-gtx.Dp(12)-is, (r.Dy()-is)/2, is, is), theme.TextSecondary)
			defer clip.Rect(r).Push(gtx.Ops).Pop()
			pointer.CursorPointer.Add(gtx.Ops)
			return layout.Dimensions{Size: r.Max}
		})
	})

	// ---- Sliders (laid out before the curve so the curve reflects this
	// frame's values).
	y := pad + hh + gtx.Dp(12)
	curveR := theme.Rect(pad, y, cw, gtx.Dp(theme.EQCurveHeight))
	y = curveR.Max.Y + gtx.Dp(12)
	bankR := theme.Rect(pad, y, cw, gtx.Dp(theme.EQSliderHeight)+gtx.Dp(LabelHeight))
	effPre := eqcore.EffectivePreamp(p.s, audio.SampleRate)
	var res BankResult
	theme.Box(gtx, bankR, func(gtx layout.Context) layout.Dimensions {
		pre := &p.s.Preamp
		if p.s.AutoPreamp {
			pre = &effPre
		}
		res = p.bank.Layout(gtx, th, &p.s.Bands, pre, BankOptions{PreampLocked: p.s.AutoPreamp})
		return layout.Dimensions{Size: gtx.Constraints.Min}
	})
	if res.Changed {
		p.s.Enabled = true
		p.e.Player.SetEQ(p.s)
		effPre = eqcore.EffectivePreamp(p.s, audio.SampleRate)
	}
	if res.Committed {
		p.commit()
	}

	// ---- Curve card.
	caption := ActiveCaption(res.Active, p.s.Bands, p.s.Preamp)
	if caption == "" && !p.s.Enabled {
		caption = "EQ off"
	}
	theme.Box(gtx, curveR, func(gtx layout.Context) layout.Dimensions {
		return CurveCard(gtx, th, p.s.Bands, effPre, p.s.Enabled, caption)
	})

	// ---- Auto preamp row.
	y = bankR.Max.Y + gtx.Dp(14)
	rowH := gtx.Dp(32)
	rowR := theme.Rect(pad, y, cw, rowH)
	theme.FillRRect(gtx.Ops, rowR, gtx.Dp(12), theme.PanelCard)
	auto := p.s.AutoPreamp
	theme.Offset(gtx, image.Pt(rowR.Max.X-gtx.Dp(6)-tw, y+(rowH-tgh)/2), func(gtx layout.Context) layout.Dimensions {
		if p.autoPreamp.Layout(gtx, th, &auto) {
			p.s.AutoPreamp = auto
			p.e.Player.UpdateConfig(func(c *config.Config) { c.EQ.AutoPreamp = auto })
			effPre = eqcore.EffectivePreamp(p.s, audio.SampleRate)
		}
		return layout.Dimensions{}
	})
	theme.BoxLoose(gtx, image.Rect(rowR.Min.X+gtx.Dp(12), y, rowR.Max.X-tw-gtx.Dp(14), y+rowH), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
		return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, "Auto preamp", theme.BodySize, font.Medium, theme.White)
		})
	})
	if auto {
		theme.Box(gtx, image.Rect(rowR.Min.X+gtx.Dp(110), y, rowR.Max.X-tw-gtx.Dp(14), y+rowH), func(gtx layout.Context) layout.Dimensions {
			return theme.E(gtx, func(gtx layout.Context) layout.Dimensions {
				return th.Text(gtx, FormatDB(effPre), theme.CaptionSize, font.Medium, theme.TextSecondary)
			})
		})
	}

	// ---- Bottom row: Save as… / Reset, or the inline name row.
	bh := gtx.Dp(theme.PillButtonHeight)
	by := sz.Y - pad - bh
	gap := gtx.Dp(8)
	if p.mode == nameNone {
		if p.saveBtn.Clicked(gtx) {
			name := DefaultPresetName
			if p.isCustom(p.preset) {
				name = p.preset
			}
			p.openName(nameSave, name)
		}
		if p.resetBtn.Clicked(gtx) {
			if flat, ok := eqcore.FindBuiltin("Flat"); ok {
				p.e.Player.UpdateConfig(func(c *config.Config) {
					c.EQ.Bands = flat.Bands
					c.EQ.Preamp = flat.Preamp
					c.EQ.Preset = flat.Name
				})
				p.sync()
			}
		}
		half := (cw - gap) / 2
		theme.Box(gtx, theme.Rect(pad, by, half, bh), func(gtx layout.Context) layout.Dimensions {
			return p.saveBtn.Layout(gtx, th, "Save as…", false)
		})
		theme.Box(gtx, theme.Rect(pad+half+gap, by, cw-half-gap, bh), func(gtx layout.Context) layout.Dimensions {
			return p.resetBtn.Layout(gtx, th, "Reset", false)
		})
	} else {
		p.layoutNameRow(gtx, theme.Rect(pad, by, cw, bh))
	}

	// ---- Dropdown overlay.
	if p.dropOpen {
		p.layoutDropdown(gtx, image.Pt(pad, pillR.Max.Y+gtx.Dp(6)), sz)
	}
	return layout.Dimensions{Size: sz}
}

func (p *Panel) layoutNameRow(gtx layout.Context, r image.Rectangle) {
	th := p.e.Theme
	for {
		ev, ok := p.name.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			p.submitName()
		}
	}
	if p.mode == nameNone {
		return
	}
	if p.okBtn.Clicked(gtx) && p.submitName() {
		return
	}
	if p.cancelBtn.Clicked(gtx) {
		p.mode = nameNone
		return
	}
	if p.focusName {
		gtx.Execute(key.FocusCmd{Tag: &p.name.Editor})
		p.focusName = false
	}
	gap := gtx.Dp(8)
	okW, cancelW := gtx.Dp(70), gtx.Dp(76)
	fw := r.Dx() - okW - cancelW - 2*gap
	label := "Save"
	if p.mode == nameRename {
		label = "Rename"
		okW = gtx.Dp(84)
		fw = r.Dx() - okW - cancelW - 2*gap
	}
	theme.Box(gtx, theme.Rect(r.Min.X, r.Min.Y, fw, r.Dy()), func(gtx layout.Context) layout.Dimensions {
		return p.name.Layout(gtx, th, "Preset name", theme.IconNone)
	})
	theme.Box(gtx, theme.Rect(r.Min.X+fw+gap, r.Min.Y, okW, r.Dy()), func(gtx layout.Context) layout.Dimensions {
		return p.okBtn.Layout(gtx, th, label, true)
	})
	theme.Box(gtx, theme.Rect(r.Max.X-cancelW, r.Min.Y, cancelW, r.Dy()), func(gtx layout.Context) layout.Dimensions {
		return p.cancelBtn.Layout(gtx, th, "Cancel", false)
	})
}

func (p *Panel) layoutDropdown(gtx layout.Context, at image.Point, sz image.Point) {
	th := p.e.Theme
	// Scrim: a press outside the list closes it.
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &p.scrim, Kinds: pointer.Press})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok && e.Kind == pointer.Press {
			p.dropOpen = false
		}
	}
	for {
		if _, ok := gtx.Event(pointer.Filter{Target: &p.dropCard, Kinds: pointer.Press}); !ok {
			break
		}
	}
	if p.isCustom(p.preset) {
		if p.renameBtn.Clicked(gtx) {
			p.renameFrom = p.preset
			p.openName(nameRename, p.preset)
			return
		}
		if p.deleteBtn.Clicked(gtx) {
			p.deletePreset(p.preset)
		}
	}
	if !p.dropOpen {
		return
	}
	builtins := eqcore.BuiltinPresets()
	items := make([]string, 0, len(builtins)+len(p.custom))
	for _, b := range builtins {
		items = append(items, b.Name)
	}
	for _, c := range p.custom {
		items = append(items, c.Name)
	}
	selected := slices.IndexFunc(items, func(s string) bool { return p.preset != "" && strings.EqualFold(s, p.preset) })
	customSel := p.isCustom(p.preset)

	sc := clip.Rect{Max: sz}.Push(gtx.Ops)
	event.Op(gtx.Ops, &p.scrim)
	sc.Pop()

	ih := gtx.Dp(theme.DropdownItemH)
	vpad := gtx.Dp(6)
	listH := min(len(items), theme.DropdownMaxItems) * ih
	actH := 0
	if customSel {
		actH = gtx.Dp(44)
	}
	cardW := gtx.Dp(240)
	card := image.Rectangle{Min: at, Max: at.Add(image.Pt(cardW, listH+2*vpad+actH))}
	rad := gtx.Dp(theme.MenuRadius)
	area := clip.UniformRRect(card, rad).Push(gtx.Ops)
	event.Op(gtx.Ops, &p.dropCard)
	area.Pop()
	theme.FillRRect(gtx.Ops, card, rad, theme.MenuBG)
	theme.StrokeRRect(gtx.Ops, card, rad, float32(gtx.Dp(1)), color.NRGBA{R: 255, G: 255, B: 255, A: 24})

	listR := image.Rect(card.Min.X+vpad, card.Min.Y+vpad, card.Max.X-vpad, card.Min.Y+vpad+listH)
	var clicked int
	theme.Box(gtx, listR, func(gtx layout.Context) layout.Dimensions {
		defer clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops).Pop()
		clicked = p.list.Layout(gtx, th, items, selected)
		return layout.Dimensions{Size: gtx.Constraints.Max}
	})
	if customSel {
		ay := listR.Max.Y + vpad
		theme.FillRect(gtx.Ops, image.Rect(listR.Min.X+gtx.Dp(8), ay, listR.Max.X-gtx.Dp(8), ay+1), theme.Divider)
		bh := gtx.Dp(30)
		by := ay + (actH-vpad-bh)/2
		half := (listR.Dx() - gtx.Dp(8)) / 2
		theme.Box(gtx, theme.Rect(listR.Min.X, by, half, bh), func(gtx layout.Context) layout.Dimensions {
			return p.renameBtn.Layout(gtx, th, "Rename", false)
		})
		theme.Box(gtx, theme.Rect(listR.Max.X-half, by, half, bh), func(gtx layout.Context) layout.Dimensions {
			return p.deleteBtn.Layout(gtx, th, "Delete", false)
		})
	}
	if clicked >= 0 {
		var pr eqcore.Preset
		if clicked < len(builtins) {
			pr = builtins[clicked]
		} else {
			pr = p.custom[clicked-len(builtins)]
		}
		p.applyPreset(pr)
		p.dropOpen = false
	}
}

// Ensure Panel satisfies the shell interfaces.
var (
	_ env.Panel       = (*Panel)(nil)
	_ env.Opener      = (*Panel)(nil)
	_ env.Closer      = (*Panel)(nil)
	_ env.KeyCapturer = (*Panel)(nil)
	_ env.EscHandler  = (*Panel)(nil)
)
