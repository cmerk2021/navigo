// Package settings implements the settings panel.
package settings

import (
	"context"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/core"
	"github.com/cmerk2021/navigo/internal/kde"
	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

var (
	cacheSizes = []int{512, 1024, 2048, 4096, 8192, 16384}
	bitrates   = []int{128, 192, 256, 320}
	rgModes    = []string{config.ReplayGainOff, config.ReplayGainTrack, config.ReplayGainAlbum}
	rgLabels   = []string{"Off", "Track", "Album"}
	swatches   = []string{"", "#F2490C", "#3D8BFD", "#2EBD85", "#A970FF", "#FF4F8B", "#F5B400"}
)

// Panel is the settings panel.
type Panel struct {
	e   *env.Env
	cfg *config.Config

	closeBtn, backBtn         widgets.RoundButton
	url, user, pass, accent   widgets.TextField
	testBtn, saveBtn, clearBt widgets.PillButton
	backPill                  widgets.PillButton
	insecure, modeBtns, kdeAc widgets.Toggle
	scrobble, separate        widgets.Toggle
	rg                        widgets.Dropdown
	cacheMinus, cachePlus     widgets.RoundButton
	bitrate                   widgets.Segmented
	swatchBtns                []widget.Clickable
	list                      widget.List
	rowH                      []int // last laid-out height of each row
	curRow                    int

	mu        sync.Mutex
	status    string
	statusErr bool
	busy      bool
	cacheUsed int64
}

// New creates the settings panel.
func New(e *env.Env) *Panel {
	p := &Panel{e: e, swatchBtns: make([]widget.Clickable, len(swatches))}
	p.list.Axis = layout.Vertical
	p.pass.Mask = '•'
	return p
}

// Opened reloads the configuration.
func (p *Panel) Opened() {
	p.cfg = p.e.Player.Config()
	p.url.SetText(p.cfg.ServerURL)
	p.user.SetText(p.cfg.Username)
	p.pass.SetText(p.e.Player.StoredPassword())
	p.accent.SetText(p.cfg.AccentOverride)
	p.setStatus("", false)
	p.refreshCacheUsage()
}

func (p *Panel) refreshCacheUsage() {
	go func() {
		n := p.e.Player.CacheSize()
		p.mu.Lock()
		p.cacheUsed = n
		p.mu.Unlock()
		p.e.Invalidate()
	}()
}

// HandleEsc implements env.EscHandler by closing an open dropdown.
func (p *Panel) HandleEsc() bool { return p.rg.Close() }

// Closed implements env.Closer.
func (p *Panel) Closed() { p.rg.Close() }

// CapturesKeys reports that the panel has text input.
func (p *Panel) CapturesKeys() bool { return true }

func (p *Panel) setStatus(s string, isErr bool) {
	p.mu.Lock()
	p.status, p.statusErr = s, isErr
	p.mu.Unlock()
	p.e.Invalidate()
}

func (p *Panel) update(f func(c *config.Config)) {
	f(p.cfg)
	p.e.Player.UpdateConfig(f)
}

func (p *Panel) connect(save bool) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	p.busy = true
	p.mu.Unlock()
	url, user, pw := strings.TrimSpace(p.url.Text()), strings.TrimSpace(p.user.Text()), p.pass.Text()
	insecure := p.cfg.InsecureTLS
	if save {
		p.setStatus("Connecting…", false)
	} else {
		p.setStatus("Testing…", false)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var err error
		if save {
			err = p.e.Player.Connect(ctx, url, user, pw, insecure)
		} else {
			err = core.TestConnection(ctx, url, user, pw, insecure)
		}
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
		switch {
		case err != nil:
			p.setStatus(err.Error(), true)
		case save:
			p.setStatus("Connected and saved.", false)
		default:
			p.setStatus("Connection OK.", false)
		}
	}()
}

func cacheLabel(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%g GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

func (p *Panel) handle(gtx layout.Context) {
	if p.closeBtn.Clicked(gtx) || p.backPill.Clicked(gtx) {
		p.e.Close()
	}
	if p.testBtn.Clicked(gtx) {
		p.connect(false)
	}
	if p.saveBtn.Clicked(gtx) {
		p.connect(true)
	}
	for _, f := range []*widgets.TextField{&p.url, &p.user, &p.pass} {
		for {
			ev, ok := f.Editor.Update(gtx)
			if !ok {
				break
			}
			if _, ok := ev.(widget.SubmitEvent); ok {
				p.connect(true)
			}
		}
	}
	for {
		ev, ok := p.accent.Editor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			p.applyAccent(p.accent.Text())
		}
	}
	for i := range p.swatchBtns {
		if p.swatchBtns[i].Clicked(gtx) {
			p.accent.SetText(swatches[i])
			p.applyAccent(swatches[i])
		}
	}
	if p.clearBt.Clicked(gtx) {
		go func() {
			if err := p.e.Player.ClearCache(); err != nil {
				p.e.Toast("Could not clear cache: "+err.Error(), true)
			} else {
				p.e.Toast("Cache cleared", false)
			}
			p.refreshCacheUsage()
		}()
	}
	idx := 0
	for i, s := range cacheSizes {
		if s <= p.cfg.CacheSizeMB {
			idx = i
		}
	}
	if p.cacheMinus.Clicked(gtx) && idx > 0 {
		v := cacheSizes[idx-1]
		p.update(func(c *config.Config) { c.CacheSizeMB = v })
	}
	if p.cachePlus.Clicked(gtx) && idx < len(cacheSizes)-1 {
		v := cacheSizes[idx+1]
		p.update(func(c *config.Config) { c.CacheSizeMB = v })
	}
}

func (p *Panel) applyAccent(s string) {
	s = strings.TrimSpace(s)
	if s != "" {
		c, ok := kde.ParseHex(s)
		if !ok {
			p.e.Toast("Invalid color — use #RRGGBB", true)
			return
		}
		s = kde.Hex(c)
		p.accent.SetText(s)
	}
	p.update(func(c *config.Config) { c.AccentOverride = s })
}

// Layout draws the panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	if p.cfg == nil {
		p.Opened()
	}
	p.handle(gtx)
	th := p.e.Theme
	sz := gtx.Constraints.Max
	widgets.PanelBackground(gtx, p.e.Drag)
	widgets.PanelHeader(gtx, th, "Settings", &p.closeBtn)
	pad := gtx.Dp(theme.PanelPadding)
	top := pad + gtx.Dp(theme.PanelHeaderH) + gtx.Dp(6)

	p.mu.Lock()
	status, statusErr, busy, used := p.status, p.statusErr, p.busy, p.cacheUsed
	p.mu.Unlock()

	fh := gtx.Dp(theme.FieldHeight)
	field := func(f *widgets.TextField, ph string) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(gtx.Constraints.Max.X, fh))
			return f.Layout(gtx, th, ph, theme.IconNone)
		}
	}
	toggleRow := func(label, sub string, t *widgets.Toggle, val bool, set func(c *config.Config, v bool)) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			h := gtx.Dp(44)
			if sub != "" {
				h = gtx.Dp(54)
			}
			tw := gtx.Dp(theme.ToggleWidth)
			theme.BoxLoose(gtx, image.Rect(0, 0, w-tw-gtx.Dp(10), h), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, label, theme.BodySize, font.Normal, theme.White)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if sub == "" {
								return layout.Dimensions{}
							}
							return th.Text(gtx, sub, theme.CaptionSize, font.Normal, theme.TextTertiary)
						}),
					)
				})
			})
			v := val
			widgets.Off(gtx, image.Pt(w-tw, (h-gtx.Dp(theme.ToggleHeight))/2), func(gtx layout.Context) layout.Dimensions {
				if t.Layout(gtx, th, &v) {
					p.update(func(c *config.Config) { set(c, v) })
				}
				return layout.Dimensions{}
			})
			return layout.Dimensions{Size: image.Pt(w, h)}
		}
	}
	heading := func(s string) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			h := gtx.Dp(34)
			theme.BoxLoose(gtx, image.Rect(0, gtx.Dp(12), gtx.Constraints.Max.X, h), func(gtx layout.Context) layout.Dimensions {
				return th.Text(gtx, strings.ToUpper(s), theme.SmallSize, font.SemiBold, theme.TextTertiary)
			})
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, h)}
		}
	}
	space := func(dp int) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{Size: image.Pt(0, gtx.Dp(theme.FieldHeight)*dp/40)}
		}
	}
	c := p.cfg
	rows := []layout.Widget{
		heading("Server"),
		field(&p.url, "https://music.example.com"),
		space(8),
		field(&p.user, "Username"),
		space(8),
		field(&p.pass, "Password"),
		space(10),
		func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			bh := gtx.Dp(theme.PillButtonHeight)
			bw := (w - gtx.Dp(10)) / 2
			theme.Box(gtx, theme.Rect(0, 0, bw, bh), func(gtx layout.Context) layout.Dimensions {
				return p.testBtn.LayoutColor(gtx, th, "Test connection", false, !busy)
			})
			theme.Box(gtx, theme.Rect(w-bw, 0, bw, bh), func(gtx layout.Context) layout.Dimensions {
				return p.saveBtn.LayoutColor(gtx, th, "Save & connect", true, !busy)
			})
			return layout.Dimensions{Size: image.Pt(w, bh)}
		},
		func(gtx layout.Context) layout.Dimensions {
			if status == "" {
				return layout.Dimensions{Size: image.Pt(0, gtx.Dp(4))}
			}
			col := theme.TextSecondary
			if statusErr {
				col = theme.ErrorColor
			}
			return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return th.Paragraph(gtx, status, theme.CaptionSize, font.Normal, col, 3, text.Start)
			})
		},
		toggleRow("Allow insecure TLS", "Accept self-signed certificates", &p.insecure, c.InsecureTLS, func(c *config.Config, v bool) { c.InsecureTLS = v }),

		heading("Appearance"),
		toggleRow("Show mode-switch buttons", "Oval (Normal) and square (Compact) buttons", &p.modeBtns, c.ShowModeButtons, func(c *config.Config, v bool) { c.ShowModeButtons = v }),
		toggleRow("Follow KDE accent color", "Reads AccentColor from kdeglobals", &p.kdeAc, c.FollowKDEAccent, func(c *config.Config, v bool) { c.FollowKDEAccent = v }),
		func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, "Accent color override", theme.BodySize, font.Normal, theme.White)
		},
		space(8),
		func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			fw := gtx.Dp(108)
			theme.Box(gtx, theme.Rect(0, 0, fw, fh), func(gtx layout.Context) layout.Dimensions {
				return p.accent.Layout(gtx, th, "#RRGGBB", theme.IconNone)
			})
			ss := gtx.Dp(26)
			x := fw + gtx.Dp(10)
			step := (w - x - ss) / max(len(swatches)-1, 1)
			for i, s := range swatches {
				r := theme.Rect(x+i*step, (fh-ss)/2, ss, ss)
				i, s := i, s
				theme.Box(gtx, r, func(gtx layout.Context) layout.Dimensions {
					return p.swatchBtns[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						cr := image.Rectangle{Max: gtx.Constraints.Min}
						if s == "" {
							theme.FillCircle(gtx.Ops, cr, theme.FieldBG)
							theme.DrawIcon(gtx.Ops, theme.IconClose, theme.Centered(cr, cr.Dx()*6/10), theme.TextSecondary)
						} else {
							col, _ := kde.ParseHex(s)
							theme.FillCircle(gtx.Ops, cr, col)
						}
						if strings.EqualFold(s, c.AccentOverride) {
							theme.DrawIcon(gtx.Ops, theme.IconCheck, theme.Centered(cr, cr.Dx()*6/10), theme.White)
						}
						defer clip.Ellipse(cr).Push(gtx.Ops).Pop()
						pointer.CursorPointer.Add(gtx.Ops)
						return layout.Dimensions{Size: cr.Max}
					})
				})
			}
			return layout.Dimensions{Size: image.Pt(w, fh)}
		},
		space(6),

		heading("Playback"),
		func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			h := gtx.Dp(54)
			theme.BoxLoose(gtx, image.Rect(0, 0, w/2, h), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Text(gtx, "Transcode bitrate", theme.BodySize, font.Normal, theme.White)
				})
			})
			sel := 0
			for i, b := range bitrates {
				if b == c.TranscodeBitrate {
					sel = i
				}
			}
			sw := w / 2
			theme.Box(gtx, theme.Rect(w-sw, (h-gtx.Dp(theme.SegmentHeight))/2, sw, gtx.Dp(theme.SegmentHeight)), func(gtx layout.Context) layout.Dimensions {
				if p.bitrate.Layout(gtx, th, []string{"128", "192", "256", "320"}, &sel) {
					v := bitrates[sel]
					p.update(func(c *config.Config) { c.TranscodeBitrate = v })
				}
				return layout.Dimensions{Size: gtx.Constraints.Min}
			})
			return layout.Dimensions{Size: image.Pt(w, h)}
		},
		func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			h := gtx.Dp(54)
			dw := gtx.Dp(120)
			theme.BoxLoose(gtx, image.Rect(0, 0, w-dw-gtx.Dp(10), h), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, "ReplayGain", theme.BodySize, font.Normal, theme.White)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, "Normalize volume using gain tags", theme.CaptionSize, font.Normal, theme.TextTertiary)
						}),
					)
				})
			})
			sel := 0
			for i, m := range rgModes {
				if m == c.ReplayGainMode {
					sel = i
				}
			}
			dh := gtx.Dp(theme.SegmentHeight)
			// Open upwards if the list would run past the panel bottom.
			y := top - p.list.Position.Offset
			for j := p.list.Position.First; j < p.curRow && j < len(p.rowH); j++ {
				y += p.rowH[j]
			}
			p.rg.Up = y+(h+dh)/2+widgets.DropdownListHeight(gtx, len(rgLabels)) > sz.Y-gtx.Dp(8)
			theme.Box(gtx, theme.Rect(w-dw, (h-dh)/2, dw, dh), func(gtx layout.Context) layout.Dimensions {
				if p.rg.Layout(gtx, th, rgLabels, &sel) {
					v := rgModes[sel]
					p.update(func(c *config.Config) { c.ReplayGainMode = v })
				}
				return layout.Dimensions{Size: gtx.Constraints.Min}
			})
			return layout.Dimensions{Size: image.Pt(w, h)}
		},
		toggleRow("Scrobbling", "Report plays to the server", &p.scrobble, c.Scrobble, func(c *config.Config, v bool) { c.Scrobble = v }),

		heading("Cache"),
		func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			h := gtx.Dp(54)
			theme.BoxLoose(gtx, image.Rect(0, 0, w-gtx.Dp(140), h), func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
				return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, "Cache size", theme.BodySize, font.Normal, theme.White)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return th.Text(gtx, fmt.Sprintf("Using %.0f MB", float64(used)/(1<<20)), theme.CaptionSize, font.Normal, theme.TextTertiary)
						}),
					)
				})
			})
			bs := gtx.Dp(30)
			y := (h - bs) / 2
			theme.Box(gtx, theme.Rect(w-bs, y, bs, bs), func(gtx layout.Context) layout.Dimensions {
				return p.cachePlus.Layout(gtx, th, theme.IconPlus, widgets.StyleCompact, 0.55)
			})
			theme.Box(gtx, theme.Rect(w-bs-gtx.Dp(100), y, bs, bs), func(gtx layout.Context) layout.Dimensions {
				return p.cacheMinus.Layout(gtx, th, theme.IconMinus, widgets.StyleCompact, 0.55)
			})
			theme.Box(gtx, image.Rect(w-gtx.Dp(100), y, w-bs, y+bs), func(gtx layout.Context) layout.Dimensions {
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min = image.Point{}
					return th.Text(gtx, cacheLabel(c.CacheSizeMB), theme.BodySize, font.SemiBold, theme.White)
				})
			})
			return layout.Dimensions{Size: image.Pt(w, h)}
		},
		func(gtx layout.Context) layout.Dimensions {
			bh := gtx.Dp(theme.PillButtonHeight)
			gtx.Constraints = layout.Exact(image.Pt(gtx.Dp(130), bh))
			return p.clearBt.Layout(gtx, th, "Clear cache", false)
		},
		space(6),

		heading("Windows"),
		toggleRow("Open panels in separate window", "Use if your compositor ignores size changes", &p.separate, c.PanelsSeparateWindow, func(c *config.Config, v bool) { c.PanelsSeparateWindow = v }),
		space(14),
		func(gtx layout.Context) layout.Dimensions {
			bh := gtx.Dp(theme.PillButtonHeight)
			gtx.Constraints = layout.Exact(image.Pt(gtx.Constraints.Max.X, bh))
			return p.backPill.Layout(gtx, th, "Back", false)
		},
		space(8),
		func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, "Config: "+config.Path(), theme.SmallSize, font.Normal, theme.TextTertiary)
		},
		space(12),
	}
	theme.Box(gtx, image.Rect(pad, top, sz.X-pad, sz.Y-gtx.Dp(8)), func(gtx layout.Context) layout.Dimensions {
		if len(p.rowH) != len(rows) {
			p.rowH = make([]int, len(rows))
		}
		return p.list.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
			p.curRow = i
			d := rows[i](gtx)
			p.rowH[i] = d.Size.Y
			return d
		})
	})
	return layout.Dimensions{Size: sz}
}
