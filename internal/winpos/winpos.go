package winpos

import (
	"context"
	"errors"
	"image"
)

// Placer reads and sets the geometry of the main window. Coordinates are
// those of the backend (logical pixels on Wayland, physical on X11); the
// caller derives sizes from the current geometry, so the unit doesn't matter.
// Calls may block (D-Bus / X11 round trips) and must not run on the UI
// goroutine.
type Placer interface {
	// Name is shown in Settings ("KWin", "X11", "unavailable").
	Name() string
	// Available reports whether the window can be positioned.
	Available() bool
	// Geometry returns the window's frame rectangle.
	Geometry(ctx context.Context) (image.Rectangle, error)
	// WorkArea returns the usable area (excluding panels) of the monitor
	// containing the window's center.
	WorkArea(ctx context.Context) (image.Rectangle, error)
	// MoveResize moves and resizes the window in a single operation.
	MoveResize(ctx context.Context, r image.Rectangle) error
	Close()
}

// ErrUnsupported is returned by the no-op placer.
var ErrUnsupported = errors.New("window placement unavailable")

// Noop is used when the window can't be positioned (e.g. Wayland outside
// KDE). The app then shows panels and menus in separate windows that the
// compositor places.
type Noop struct{}

func (Noop) Name() string    { return "unavailable" }
func (Noop) Available() bool { return false }
func (Noop) Geometry(context.Context) (image.Rectangle, error) {
	return image.Rectangle{}, ErrUnsupported
}
func (Noop) WorkArea(context.Context) (image.Rectangle, error) {
	return image.Rectangle{}, ErrUnsupported
}
func (Noop) MoveResize(context.Context, image.Rectangle) error { return ErrUnsupported }
func (Noop) Close()                                            {}
