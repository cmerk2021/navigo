package eq

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"

	"github.com/cmerk2021/navigo/internal/audio"
	eqcore "github.com/cmerk2021/navigo/internal/eq"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// NumSliders is the number of sliders in a SliderBank: the bands plus the
// preamp.
const NumSliders = eqcore.NumBands + 1

// curveRange is the dB value mapped to the top (and, negated, the bottom) of
// the curve area.
const curveRange = 15.0

// curveFreqs are the frequencies at which the response curve is evaluated.
var curveFreqs = eqcore.LogFreqs(120, 20, 20000)

// doubleClick is the maximum interval between two presses of a double click.
const doubleClick = 400 * time.Millisecond

// FormatDB formats a gain as "+3.5 dB" / "−2.0 dB" / "0.0 dB".
func FormatDB(v float64) string {
	v = math.Round(v*10) / 10
	switch {
	case v > 0:
		return fmt.Sprintf("+%.1f dB", v)
	case v < 0:
		return fmt.Sprintf("−%.1f dB", -v)
	}
	return "0.0 dB"
}

// SliderLabel returns the label shown under slider i ("31" … "16k", "Pre").
func SliderLabel(i int) string {
	if i >= 0 && i < eqcore.NumBands {
		return eqcore.Labels[i]
	}
	return "Pre"
}

// freqX maps a frequency to an x offset within a width w (log scale 20 Hz–20 kHz).
func freqX(f float64, w int) float32 {
	return float32(math.Log(f/20) / math.Log(1000) * float64(w))
}

// CurveCard draws the frequency-response card filling gtx.Constraints.Min:
// a dark rounded card with a dB grid and the magnitude response of the band
// cascade (including preamp) computed from the actual filter coefficients.
// The curve is dimmed when active is false. caption, if not empty, is drawn
// in the top-right corner.
func CurveCard(gtx layout.Context, th *theme.Theme, bands [eqcore.NumBands]float64, preamp float64, active bool, caption string) layout.Dimensions {
	sz := gtx.Constraints.Min
	card := image.Rectangle{Max: sz}
	rad := gtx.Dp(14)
	theme.FillRRect(gtx.Ops, card, rad, theme.PanelCard)

	inset := gtx.Dp(10)
	plot := image.Rect(inset, inset, sz.X-inset, sz.Y-inset)
	h := plot.Dy()
	mid := float32(plot.Min.Y) + float32(h)/2
	yFor := func(db float64) float32 {
		return mid - float32(db/curveRange)*float32(h)/2
	}
	line := max(1, gtx.Dp(0.5))

	// Grid: vertical lines at the band frequencies, horizontal at ±6/±12 dB
	// and a stronger 0 dB line.
	faint := color.NRGBA{R: 255, G: 255, B: 255, A: 12}
	for _, f := range eqcore.Frequencies {
		x := plot.Min.X + int(freqX(f, plot.Dx()))
		theme.FillRect(gtx.Ops, image.Rect(x, plot.Min.Y, x+line, plot.Max.Y), faint)
	}
	for _, db := range []float64{-12, -6, 6, 12} {
		y := int(yFor(db))
		theme.FillRect(gtx.Ops, image.Rect(plot.Min.X, y, plot.Max.X, y+line), faint)
	}
	zy := int(mid)
	theme.FillRect(gtx.Ops, image.Rect(plot.Min.X, zy, plot.Max.X, zy+line), color.NRGBA{R: 255, G: 255, B: 255, A: 40})

	// Scale labels.
	lh := gtx.Sp(theme.SmallSize * 1.3)
	for _, l := range []struct {
		db  float64
		txt string
	}{{12, "+12"}, {0, "0"}, {-12, "−12"}} {
		y := int(yFor(l.db))
		theme.BoxLoose(gtx, image.Rect(plot.Min.X+gtx.Dp(2), y-lh-gtx.Dp(1), plot.Min.X+gtx.Dp(40), y-gtx.Dp(1)), func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, l.txt, theme.SmallSize, font.Normal, theme.TextTertiary)
		})
	}

	// Curve.
	resp := eqcore.Response(bands, preamp, audio.SampleRate, curveFreqs)
	pts := make([]f32.Point, len(resp))
	for i, db := range resp {
		pts[i] = f32.Pt(float32(plot.Min.X)+freqX(curveFreqs[i], plot.Dx()), yFor(db))
	}
	col := th.Accent()
	if !active {
		col = theme.Mix(col, color.NRGBA{R: 128, G: 128, B: 128, A: 255}, 0.7)
		col = theme.WithAlpha(col, 0.55)
	}
	cl := clip.UniformRRect(card, rad).Push(gtx.Ops)
	if len(pts) > 1 {
		var fill clip.Path
		fill.Begin(gtx.Ops)
		fill.MoveTo(f32.Pt(pts[0].X, mid))
		for _, p := range pts {
			fill.LineTo(p)
		}
		fill.LineTo(f32.Pt(pts[len(pts)-1].X, mid))
		fill.Close()
		paint.FillShape(gtx.Ops, theme.WithAlpha(col, 0.18), clip.Outline{Path: fill.End()}.Op())

		var path clip.Path
		path.Begin(gtx.Ops)
		path.MoveTo(pts[0])
		for _, p := range pts[1:] {
			path.LineTo(p)
		}
		paint.FillShape(gtx.Ops, col, clip.Stroke{Path: path.End(), Width: float32(gtx.Dp(2))}.Op())
	}
	cl.Pop()

	if caption != "" {
		ch := gtx.Sp(theme.CaptionSize * 1.4)
		theme.Box(gtx, image.Rect(sz.X/3, gtx.Dp(8), sz.X-gtx.Dp(12), gtx.Dp(8)+ch), func(gtx layout.Context) layout.Dimensions {
			return th.TextAligned(gtx, caption, theme.CaptionSize, font.SemiBold, theme.White, text.End)
		})
	}
	return layout.Dimensions{Size: sz}
}

// DrawSlider draws a static (non-interactive) vertical slider with the same
// look as widgets.VSlider into a rectangle of size sz at the origin.
func DrawSlider(gtx layout.Context, th *theme.Theme, sz image.Point, v, lo, hi float64, dim bool) {
	if dim {
		defer paint.PushOpacity(gtx.Ops, 0.45).Pop()
	}
	knob := gtx.Dp(theme.EQSliderKnob)
	pad := knob / 2
	th2 := sz.Y - 2*pad
	track := gtx.Dp(theme.EQSliderTrack)
	cx := sz.X / 2
	tr := image.Rect(cx-track/2, pad, cx-track/2+track, pad+th2)
	theme.FillPill(gtx.Ops, tr, theme.SeekTrack)
	f := (min(max(v, lo), hi) - lo) / (hi - lo)
	ky := pad + int(float64(th2)*(1-f))
	zy := pad + int(float64(th2)*(1-(0-lo)/(hi-lo)))
	fr := image.Rect(tr.Min.X, min(ky, zy), tr.Max.X, max(ky, zy))
	if fr.Dy() > 0 {
		theme.FillRect(gtx.Ops, fr, th.Accent())
	}
	theme.FillRect(gtx.Ops, image.Rect(cx-track, zy, cx+track, zy+1), theme.TextTertiary)
	theme.FillCircle(gtx.Ops, theme.Rect(cx-knob/2, ky-knob/2, knob, knob), theme.White)
}

// BankOptions configure a SliderBank layout.
type BankOptions struct {
	// ReadOnly draws all sliders without interaction.
	ReadOnly bool
	// PreampLocked draws the preamp slider dimmed and non-interactive (used
	// when the automatic preamp is on).
	PreampLocked bool
}

// BankResult reports what happened during a SliderBank layout.
type BankResult struct {
	// Changed is true when a value changed this frame.
	Changed bool
	// Committed is true when a drag ended or a slider was reset by a double
	// click; callers persist the values then.
	Committed bool
	// Active is the index of the slider being dragged (NumBands = preamp),
	// or -1.
	Active int
}

// SliderBank is the row of 10 band sliders plus the preamp slider, with
// frequency labels underneath. A double click resets a slider to 0 dB.
type SliderBank struct {
	sliders  [NumSliders]widgets.VSlider
	taps     [NumSliders]bool // distinct addresses used as event tags
	lastTap  [NumSliders]time.Duration
	dragging [NumSliders]bool
}

// Active returns the index of the slider being dragged, or -1.
func (b *SliderBank) Active() int {
	for i := range b.sliders {
		if b.sliders[i].Dragging() {
			return i
		}
	}
	return -1
}

// LabelHeight is the height reserved under the sliders for their labels.
const LabelHeight unit.Dp = 18

// Layout draws the bank filling gtx.Constraints.Min. The sliders are
// EQSliderWidth wide, evenly spaced, and as tall as the area minus
// LabelHeight. bands and preamp are updated in place (±MaxGain).
func (b *SliderBank) Layout(gtx layout.Context, th *theme.Theme, bands *[eqcore.NumBands]float64, preamp *float64, o BankOptions) BankResult {
	res := BankResult{Active: -1}
	sz := gtx.Constraints.Min
	sw := gtx.Dp(theme.EQSliderWidth)
	lh := gtx.Dp(LabelHeight)
	sh := sz.Y - lh
	gap := 0
	if NumSliders > 1 {
		gap = max(0, (sz.X-NumSliders*sw)/(NumSliders-1))
	}
	left := (sz.X - NumSliders*sw - (NumSliders-1)*gap) / 2
	val := func(i int) *float64 {
		if i < eqcore.NumBands {
			return &bands[i]
		}
		return preamp
	}
	for i := range NumSliders {
		x := left + i*(sw+gap)
		r := theme.Rect(x, 0, sw, sh)
		v := val(i)
		static := o.ReadOnly || (i == eqcore.NumBands && o.PreampLocked)
		if static {
			b.dragging[i] = false
			theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
				DrawSlider(gtx, th, gtx.Constraints.Min, *v, -eqcore.MaxGain, eqcore.MaxGain, !o.ReadOnly)
				return layout.Dimensions{Size: gtx.Constraints.Min}
			})
		} else {
			theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
				area := clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops)
				event.Op(gtx.Ops, &b.taps[i])
				if b.sliders[i].Layout(gtx, th, v, -eqcore.MaxGain, eqcore.MaxGain) {
					res.Changed = true
				}
				area.Pop()
				for {
					ev, ok := gtx.Event(pointer.Filter{Target: &b.taps[i], Kinds: pointer.Press})
					if !ok {
						break
					}
					e, ok := ev.(pointer.Event)
					if !ok || e.Kind != pointer.Press || e.Buttons != pointer.ButtonPrimary {
						continue
					}
					if b.lastTap[i] != 0 && e.Time-b.lastTap[i] < doubleClick {
						b.lastTap[i] = 0
						if *v != 0 {
							*v = 0
							res.Changed = true
						}
						if !b.sliders[i].Dragging() {
							res.Committed = true
						}
					} else {
						b.lastTap[i] = e.Time
					}
				}
				return layout.Dimensions{Size: gtx.Constraints.Min}
			})
			d := b.sliders[i].Dragging()
			if b.dragging[i] && !d {
				res.Committed = true
			}
			b.dragging[i] = d
			if d {
				res.Active = i
			}
		}
		col := theme.TextSecondary
		if res.Active == i {
			col = theme.White
		}
		theme.Box(gtx, image.Rect(x-gap/2-gtx.Dp(4), sh, x+sw+gap/2+gtx.Dp(4), sz.Y), func(gtx layout.Context) layout.Dimensions {
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = image.Point{}
				return th.TextAligned(gtx, SliderLabel(i), theme.SmallSize, font.Medium, col, text.Middle)
			})
		})
	}
	return res
}

// ActiveCaption returns the readout for the slider being dragged, e.g.
// "+3.5 dB @ 1k" or "Preamp −2.0 dB".
func ActiveCaption(i int, bands [eqcore.NumBands]float64, preamp float64) string {
	switch {
	case i < 0:
		return ""
	case i < eqcore.NumBands:
		return FormatDB(bands[i]) + " @ " + eqcore.Labels[i]
	}
	return "Preamp " + FormatDB(preamp)
}

// trimName normalizes a preset name.
func trimName(s string) string { return strings.Join(strings.Fields(s), " ") }
