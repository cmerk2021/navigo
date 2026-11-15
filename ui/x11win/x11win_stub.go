//go:build !linux || nox11

// Package x11win is a no-op on platforms without X11.
package x11win

// Window is a no-op placeholder.
type Window struct{}

// Attach returns nil without X11.
func Attach(id uintptr) *Window { return nil }

// Undecorate is a no-op.
func (w *Window) Undecorate(class string) {}

// Position always fails.
func (w *Window) Position() (int, int, bool) { return 0, 0, false }

// Move is a no-op.
func (w *Window) Move(x, y int) {}

// BeginDrag is a no-op.
func (w *Window) BeginDrag() {}

// Drag is a no-op.
func (w *Window) Drag() {}

// EndDrag is a no-op.
func (w *Window) EndDrag() {}

// Close is a no-op.
func (w *Window) Close() {}
