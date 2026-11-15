// Package login implements the first-run login panel.
package login

import (
	"context"
	"image"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/text"
	"gioui.org/widget"

	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// Panel is the login form.
type Panel struct {
	e *env.Env

	closeBtn        widgets.RoundButton
	url, user, pass widgets.TextField
	insecure        widgets.Toggle
	insecureOn      bool
	connectBtn      widgets.PillButton
	focused         bool

	mu      sync.Mutex
	busy    bool
	errText string
}

// New creates the login panel.
func New(e *env.Env) *Panel {
	p := &Panel{e: e}
	p.pass.Mask = '•'
	return p
}

// Opened pre-fills the form from the configuration.
func (p *Panel) Opened() {
	cfg := p.e.Player.Config()
	if p.url.Text() == "" {
		p.url.SetText(cfg.ServerURL)
	}
	if p.user.Text() == "" {
		p.user.SetText(cfg.Username)
	}
	p.insecureOn = cfg.InsecureTLS
	p.focused = false
}

// CapturesKeys reports that the panel has text input.
func (p *Panel) CapturesKeys() bool { return true }

func (p *Panel) connect() {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	url := strings.TrimSpace(p.url.Text())
	user := strings.TrimSpace(p.user.Text())
	pw := p.pass.Text()
	if url == "" || user == "" {
		p.errText = "Enter a server URL and username."
		p.mu.Unlock()
		return
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
		p.url.SetText(url)
	}
	p.busy, p.errText = true, ""
	insecure := p.insecureOn
	p.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := p.e.Player.Connect(ctx, url, user, pw, insecure)
		p.mu.Lock()
		p.busy = false
		if err != nil {
			p.errText = err.Error()
		}
		p.mu.Unlock()
		if err == nil {
			p.e.Toast("Connected", false)
			p.e.Open(env.Picker)
		}
		p.e.Invalidate()
	}()
}

// Layout draws the panel.
func (p *Panel) Layout(gtx layout.Context) layout.Dimensions {
	th := p.e.Theme
	sz := gtx.Constraints.Max
	if p.closeBtn.Clicked(gtx) {
		p.e.Close()
	}
	if p.connectBtn.Clicked(gtx) {
		p.connect()
	}
	for _, f := range []*widgets.TextField{&p.url, &p.user, &p.pass} {
		for {
			ev, ok := f.Editor.Update(gtx)
			if !ok {
				break
			}
			if _, ok := ev.(widget.SubmitEvent); ok {
				p.connect()
			}
		}
	}
	if !p.focused {
		p.focused = true
		if p.url.Text() == "" {
			p.url.Focus(gtx)
		} else {
			p.pass.Focus(gtx)
		}
	}

	widgets.PanelBackground(gtx, p.e.Drag)
	hasServer := p.e.Player.Config().HasServer()
	if hasServer {
		widgets.PanelHeader(gtx, th, "Connect", &p.closeBtn)
	}
	pad := gtx.Dp(theme.PanelPadding) + gtx.Dp(6)
	w := sz.X - 2*pad
	y := gtx.Dp(70)

	// Logo mark.
	ls := gtx.Dp(64)
	lr := theme.Rect((sz.X-ls)/2, y, ls, ls)
	theme.FillRRect(gtx.Ops, lr, gtx.Dp(16), th.Accent())
	theme.DrawIcon(gtx.Ops, theme.IconPlay, theme.Centered(lr, ls/2), theme.White)
	y += ls + gtx.Dp(16)
	theme.Box(gtx, image.Rect(0, y, sz.X, y+gtx.Dp(26)), func(gtx layout.Context) layout.Dimensions {
		return th.TextAligned(gtx, "Welcome to Navigo", theme.HeadingSize+3, font.Bold, theme.White, text.Middle)
	})
	y += gtx.Dp(28)
	theme.Box(gtx, image.Rect(pad, y, sz.X-pad, y+gtx.Dp(36)), func(gtx layout.Context) layout.Dimensions {
		return th.Paragraph(gtx, "Sign in to your Navidrome or Subsonic server.", theme.BodySize, font.Normal, theme.TextSecondary, 2, text.Middle)
	})
	y += gtx.Dp(44)

	fh := gtx.Dp(theme.FieldHeight)
	gap := gtx.Dp(10)
	fields := []struct {
		f    *widgets.TextField
		ph   string
		icon theme.Icon
	}{
		{&p.url, "Server URL (https://…)", theme.IconNone},
		{&p.user, "Username", theme.IconNone},
		{&p.pass, "Password", theme.IconNone},
	}
	for _, f := range fields {
		f := f
		theme.Box(gtx, theme.Rect(pad, y, w, fh), func(gtx layout.Context) layout.Dimensions {
			return f.f.Layout(gtx, th, f.ph, f.icon)
		})
		y += fh + gap
	}

	// Insecure TLS toggle.
	rh := gtx.Dp(40)
	theme.BoxLoose(gtx, image.Rect(pad+gtx.Dp(4), y, sz.X-pad-gtx.Dp(theme.ToggleWidth)-gtx.Dp(8), y+rh), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
		return theme.W(gtx, func(gtx layout.Context) layout.Dimensions {
			return th.Text(gtx, "Allow insecure TLS", theme.BodySize, font.Normal, theme.White)
		})
	})
	widgets.Off(gtx, image.Pt(sz.X-pad-gtx.Dp(theme.ToggleWidth), y+(rh-gtx.Dp(theme.ToggleHeight))/2), func(gtx layout.Context) layout.Dimensions {
		p.insecure.Layout(gtx, th, &p.insecureOn)
		return layout.Dimensions{}
	})
	y += rh + gtx.Dp(12)

	p.mu.Lock()
	busy, errText := p.busy, p.errText
	p.mu.Unlock()
	bh := gtx.Dp(theme.PillButtonHeight) + gtx.Dp(4)
	label := "Connect"
	if busy {
		label = "Connecting…"
	}
	theme.Box(gtx, theme.Rect(pad, y, w, bh), func(gtx layout.Context) layout.Dimensions {
		return p.connectBtn.LayoutColor(gtx, th, label, true, !busy)
	})
	y += bh + gtx.Dp(14)
	if errText != "" {
		theme.BoxLoose(gtx, image.Rect(pad, y, sz.X-pad, sz.Y-pad), func(gtx layout.Context) layout.Dimensions {
			return th.Paragraph(gtx, errText, theme.CaptionSize, font.Normal, theme.ErrorColor, 4, text.Middle)
		})
	}
	return layout.Dimensions{Size: sz}
}
