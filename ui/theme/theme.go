// Package theme holds Navigo's colors, fonts, dimensions and low-level drawing
// helpers (rounded shapes, images and vector icons).
package theme

import (
	"image"
	"image/color"
	"io/fs"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/assets"
)

// Typeface is the embedded UI font family.
const Typeface = "Inter"

// DefaultAccent is the default accent color (#F2490C).
var DefaultAccent = color.NRGBA{R: 0xF2, G: 0x49, B: 0x0C, A: 0xFF}

// Palette.
var (
	White         = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	TextSecondary = color.NRGBA{R: 255, G: 255, B: 255, A: 153}
	TextTertiary  = color.NRGBA{R: 255, G: 255, B: 255, A: 100}
	GlassPill     = color.NRGBA{A: 128}
	GlassButton   = color.NRGBA{R: 30, G: 30, B: 32, A: 150}
	GlassHover    = color.NRGBA{R: 255, G: 255, B: 255, A: 30}
	ButtonOverlay = color.NRGBA{R: 255, G: 255, B: 255, A: 31}
	CompactOuter  = color.NRGBA{R: 0x1C, G: 0x1C, B: 0x1E, A: 255}
	CompactInner  = color.NRGBA{R: 0x2A, G: 0x2A, B: 0x2C, A: 255}
	WindowBG      = color.NRGBA{R: 0x0E, G: 0x0E, B: 0x10, A: 255}
	PanelBG       = color.NRGBA{R: 0x16, G: 0x16, B: 0x18, A: 255}
	PanelCard     = color.NRGBA{R: 255, G: 255, B: 255, A: 16}
	FieldBG       = color.NRGBA{R: 255, G: 255, B: 255, A: 22}
	Divider       = color.NRGBA{R: 255, G: 255, B: 255, A: 22}
	SeekTrack     = color.NRGBA{R: 255, G: 255, B: 255, A: 64}
	SeekBuffered  = color.NRGBA{R: 255, G: 255, B: 255, A: 110}
	ErrorColor    = color.NRGBA{R: 0xFF, G: 0x6B, B: 0x5E, A: 255}
	MenuBG        = color.NRGBA{R: 0x22, G: 0x22, B: 0x24, A: 250}
)

// Theme carries the shared shaper and the current accent color.
type Theme struct {
	Shaper *text.Shaper

	mu     sync.Mutex
	accent color.NRGBA

	images imageCache
	widths map[widthKey]int
}

type widthKey struct {
	txt    string
	size   unit.Sp
	weight font.Weight
	ppdp   float32
}

var (
	collOnce sync.Once
	coll     []font.FontFace
)

// Fonts returns the embedded Inter faces.
func Fonts() []font.FontFace {
	collOnce.Do(func() {
		entries, _ := fs.ReadDir(assets.Fonts, "fonts")
		for _, e := range entries {
			data, err := assets.Fonts.ReadFile("fonts/" + e.Name())
			if err != nil {
				continue
			}
			faces, err := opentype.ParseCollection(data)
			if err != nil {
				continue
			}
			coll = append(coll, faces...)
		}
	})
	return coll
}

// New creates a theme. The shaper uses the embedded Inter faces first and
// falls back to system fonts (discovered with go-text fontscan) for glyphs
// Inter lacks, such as CJK.
func New() *Theme {
	return &Theme{
		Shaper: text.NewShaper(text.WithCollection(Fonts())),
		accent: DefaultAccent,
		images: imageCache{m: map[*image.RGBA]paint.ImageOp{}},
		widths: map[widthKey]int{},
	}
}

// Accent returns the current accent color.
func (t *Theme) Accent() color.NRGBA {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accent
}

// SetAccent changes the accent color.
func (t *Theme) SetAccent(c color.NRGBA) {
	c.A = 255
	t.mu.Lock()
	t.accent = c
	t.mu.Unlock()
}

// Font returns the UI font with the given weight.
func Font(w font.Weight) font.Font { return font.Font{Typeface: Typeface, Weight: w} }

// Text draws a single line of text, ellipsized to the available width.
func (t *Theme) Text(gtx layout.Context, txt string, size unit.Sp, w font.Weight, col color.NRGBA) layout.Dimensions {
	return t.TextAligned(gtx, txt, size, w, col, text.Start)
}

// TextAligned is Text with an explicit alignment.
func (t *Theme) TextAligned(gtx layout.Context, txt string, size unit.Sp, w font.Weight, col color.NRGBA, align text.Alignment) layout.Dimensions {
	l := widget.Label{MaxLines: 1, Truncator: "…", Alignment: align}
	return l.Layout(gtx, t.Shaper, Font(w), size, txt, Material(gtx.Ops, col))
}

// Paragraph draws wrapped text limited to maxLines (0 = unlimited).
func (t *Theme) Paragraph(gtx layout.Context, txt string, size unit.Sp, w font.Weight, col color.NRGBA, maxLines int, align text.Alignment) layout.Dimensions {
	l := widget.Label{MaxLines: maxLines, Truncator: "…", Alignment: align}
	return l.Layout(gtx, t.Shaper, Font(w), size, txt, Material(gtx.Ops, col))
}

// Measure returns the natural single-line width of txt in pixels.
func (t *Theme) Measure(gtx layout.Context, txt string, size unit.Sp, w font.Weight) int {
	k := widthKey{txt, size, w, gtx.Metric.PxPerDp}
	t.mu.Lock()
	v, ok := t.widths[k]
	t.mu.Unlock()
	if ok {
		return v
	}
	g := gtx
	g.Ops = new(op.Ops)
	g.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	l := widget.Label{MaxLines: 1}
	d := l.Layout(g, t.Shaper, Font(w), size, txt, Material(g.Ops, White))
	t.mu.Lock()
	if len(t.widths) > 2000 {
		t.widths = map[widthKey]int{}
	}
	t.widths[k] = d.Size.X
	t.mu.Unlock()
	return d.Size.X
}

// Material records a color paint op for text.
func Material(ops *op.Ops, col color.NRGBA) op.CallOp {
	m := op.Record(ops)
	paint.ColorOp{Color: col}.Add(ops)
	return m.Stop()
}

// WithAlpha scales a color's alpha by a (0..1).
func WithAlpha(c color.NRGBA, a float32) color.NRGBA {
	if a < 0 {
		a = 0
	} else if a > 1 {
		a = 1
	}
	c.A = uint8(float32(c.A)*a + 0.5)
	return c
}

// Mix linearly blends two colors.
func Mix(a, b color.NRGBA, t float32) color.NRGBA {
	l := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t + 0.5) }
	return color.NRGBA{R: l(a.R, b.R), G: l(a.G, b.G), B: l(a.B, b.B), A: l(a.A, b.A)}
}

// FrameInterval is the redraw interval used for animations (≤30 fps).
const FrameInterval = time.Second / 30

// Animate schedules a redraw for the next animation frame.
func Animate(gtx layout.Context) {
	gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(FrameInterval)})
}

// RedrawAt schedules a redraw at t.
func RedrawAt(gtx layout.Context, t time.Time) {
	gtx.Execute(op.InvalidateCmd{At: t})
}
