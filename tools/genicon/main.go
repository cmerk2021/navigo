// Command genicon renders the Navigo application icon. It writes the SVG
// master and anti-aliased PNG renditions (via supersampling) into
// assets/icon. Run it from the repository root: go run ./tools/genicon
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

// All geometry is expressed in a 256×256 design space shared with the SVG.
const design = 256.0

type rrect struct{ x, y, w, h, r float64 }

func (q rrect) contains(px, py float64) bool {
	hw, hh := q.w/2, q.h/2
	dx := math.Max(math.Abs(px-(q.x+hw))-(hw-q.r), 0)
	dy := math.Max(math.Abs(py-(q.y+hh))-(hh-q.r), 0)
	return dx*dx+dy*dy <= q.r*q.r
}

var (
	bgTop    = color.NRGBA{0x2A, 0x2A, 0x2C, 0xFF}
	bgBottom = color.NRGBA{0x1C, 0x1C, 0x1E, 0xFF}
	accent   = color.NRGBA{0xF2, 0x49, 0x0C, 0xFF}

	// Rounded square with an 8px transparent margin; corner radius ≈ 22%.
	tile = rrect{16, 16, 224, 224, 49}

	bars = func() []rrect {
		const w, gap = 20.0, 14.0
		heights := []float64{56, 112, 156, 96, 64}
		x := design/2 - (float64(len(heights))*w+float64(len(heights)-1)*gap)/2
		out := make([]rrect, len(heights))
		for i, h := range heights {
			out[i] = rrect{x, design/2 - h/2, w, h, w / 2}
			x += w + gap
		}
		return out
	}()
)

func lerp(a, b uint8, t float64) float64 { return float64(a) + (float64(b)-float64(a))*t }

// sample returns the straight-alpha color at a design-space point.
func sample(px, py float64) (r, g, b, a float64) {
	if !tile.contains(px, py) {
		return 0, 0, 0, 0
	}
	for _, bar := range bars {
		if bar.contains(px, py) {
			return float64(accent.R), float64(accent.G), float64(accent.B), 255
		}
	}
	t := math.Min(math.Max((py-tile.y)/tile.h, 0), 1)
	return lerp(bgTop.R, bgBottom.R, t), lerp(bgTop.G, bgBottom.G, t), lerp(bgTop.B, bgBottom.B, t), 255
}

func render(size, ss int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := design / float64(size)
	n := float64(ss * ss)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var pr, pg, pb, pa float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					dx := (float64(x) + (float64(sx)+0.5)/float64(ss)) * scale
					dy := (float64(y) + (float64(sy)+0.5)/float64(ss)) * scale
					r, g, b, a := sample(dx, dy)
					pr += r * a
					pg += g * a
					pb += b * a
					pa += a
				}
			}
			if pa == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round(pr / pa)),
				G: uint8(math.Round(pg / pa)),
				B: uint8(math.Round(pb / pa)),
				A: uint8(math.Round(pa / n)),
			})
		}
	}
	return img
}

func hex(c color.NRGBA) string { return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B) }

func svg() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" viewBox="0 0 256 256">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="%s"/>
      <stop offset="1" stop-color="%s"/>
    </linearGradient>
  </defs>
  <rect x="%g" y="%g" width="%g" height="%g" rx="%g" fill="url(#bg)"/>
  <g fill="%s">
`, hex(bgTop), hex(bgBottom), tile.x, tile.y, tile.w, tile.h, tile.r, hex(accent))
	for _, q := range bars {
		fmt.Fprintf(&b, "    <rect x=\"%g\" y=\"%g\" width=\"%g\" height=\"%g\" rx=\"%g\"/>\n", q.x, q.y, q.w, q.h, q.r)
	}
	b.WriteString("  </g>\n</svg>\n")
	return b.Bytes()
}

func main() {
	out := flag.String("out", filepath.Join("assets", "icon"), "output directory")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "navigo.svg"), svg(), 0o644); err != nil {
		log.Fatal(err)
	}
	for _, size := range []int{48, 128, 256} {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(size, 8)); err != nil {
			log.Fatal(err)
		}
		name := filepath.Join(*out, fmt.Sprintf("navigo-%d.png", size))
		if err := os.WriteFile(name, buf.Bytes(), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
