// Package env defines the contract between the application shell and its
// panels (picker, EQ, queue, settings, ...).
package env

import (
	"gioui.org/layout"

	"github.com/cmerk2021/navigo/internal/core"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// Kind identifies a panel.
type Kind int

// Panels.
const (
	None Kind = iota
	Picker
	Queue
	EQ
	EQAssist
	Settings
	Login
)

// Panel is a 370×600 view. Layout is called with exact constraints of the
// panel size, on the UI goroutine of whichever window hosts the panel. The
// shell serializes frames of all windows, so panels need no locking.
type Panel interface {
	Layout(gtx layout.Context) layout.Dimensions
}

// Opener is implemented by panels that want to know when they are shown.
type Opener interface{ Opened() }

// Closer is implemented by panels that need cleanup when they are hidden.
type Closer interface{ Closed() }

// KeyCapturer is implemented by panels with text input. While it returns
// true, single-key global shortcuts (Space, C, E, M, /, arrows) are disabled.
type KeyCapturer interface{ CapturesKeys() bool }

// EscHandler lets a panel consume Esc (e.g. to close a dropdown) before the
// shell closes the panel.
type EscHandler interface{ HandleEsc() bool }

// Env gives panels access to the player and the shell.
type Env struct {
	Player *core.Player
	Theme  *theme.Theme
	// Drag is the window-move area panels install behind their content via
	// widgets.PanelBackground(gtx, env.Drag).
	Drag *widgets.DragArea
	// Invalidate requests a redraw of all windows (safe from any goroutine).
	Invalidate func()
	// Open shows a panel (replacing the current one).
	Open func(Kind)
	// Close hides the current panel and restores the previous window size.
	Close func()
	// Toast shows a transient message (safe from any goroutine).
	Toast func(text string, isErr bool)
	// Placement names the active window placement method ("KWin", "X11",
	// "unavailable").
	Placement func() string
}
