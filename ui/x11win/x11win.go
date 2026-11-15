//go:build linux && !nox11

// Package x11win provides the few X11 window operations Gio does not expose:
// removing decorations (Gio always keeps them on X11), reading and setting the
// window position, and manual window dragging (Gio ignores system.ActionMove
// on X11). It uses its own Xlib connection so it never races with Gio's.
package x11win

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xatom.h>
#include <X11/Xutil.h>
#include <stdlib.h>

static int nv_ignore(Display *d, XErrorEvent *e) { return 0; }

static Display *nv_open(void) {
	XSetErrorHandler(nv_ignore);
	return XOpenDisplay(NULL);
}

static void nv_undecorate(Display *d, Window w) {
	Atom a = XInternAtom(d, "_MOTIF_WM_HINTS", False);
	long hints[5] = {2, 0, 0, 0, 0};
	XChangeProperty(d, w, a, a, 32, PropModeReplace, (unsigned char *)hints, 5);
	XFlush(d);
}

static void nv_set_class(Display *d, Window w, char *name) {
	XClassHint *h = XAllocClassHint();
	if (!h) return;
	h->res_name = name;
	h->res_class = name;
	XSetClassHint(d, w, h);
	XFree(h);
	XFlush(d);
}

static int nv_position(Display *d, Window w, int *x, int *y) {
	Window child;
	return XTranslateCoordinates(d, w, DefaultRootWindow(d), 0, 0, x, y, &child);
}

static void nv_move(Display *d, Window w, int x, int y) {
	XMoveWindow(d, w, x, y);
	XFlush(d);
}

static int nv_pointer(Display *d, int *x, int *y) {
	Window root, child;
	int wx, wy;
	unsigned int mask;
	return XQueryPointer(d, DefaultRootWindow(d), &root, &child, x, y, &wx, &wy, &mask);
}

static void nv_close(Display *d) { XCloseDisplay(d); }
*/
import "C"

import (
	"sync"
	"unsafe"
)

// Window wraps an X11 window ID with a private display connection.
type Window struct {
	mu       sync.Mutex
	d        *C.Display
	w        C.Window
	dragging bool
	px, py   int
	wx, wy   int
}

// Attach opens a connection for the window id. It returns nil if X11 is not
// reachable.
func Attach(id uintptr) *Window {
	d := C.nv_open()
	if d == nil {
		return nil
	}
	return &Window{d: d, w: C.Window(id)}
}

// Undecorate removes window-manager decorations via _MOTIF_WM_HINTS and sets
// the WM_CLASS to class.
func (w *Window) Undecorate(class string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	C.nv_undecorate(w.d, w.w)
	cs := C.CString(class)
	defer C.free(unsafe.Pointer(cs))
	C.nv_set_class(w.d, w.w, cs)
}

// Position returns the window's top-left corner in root coordinates.
func (w *Window) Position() (int, int, bool) {
	if w == nil {
		return 0, 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var x, y C.int
	if C.nv_position(w.d, w.w, &x, &y) == 0 {
		return 0, 0, false
	}
	return int(x), int(y), true
}

// Move moves the window's top-left corner to (x, y).
func (w *Window) Move(x, y int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	C.nv_move(w.d, w.w, C.int(x), C.int(y))
}

func (w *Window) pointer() (int, int, bool) {
	var x, y C.int
	if C.nv_pointer(w.d, &x, &y) == 0 {
		return 0, 0, false
	}
	return int(x), int(y), true
}

// BeginDrag records the pointer and window positions for a manual move.
func (w *Window) BeginDrag() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	px, py, ok := w.pointer()
	if !ok {
		return
	}
	var x, y C.int
	if C.nv_position(w.d, w.w, &x, &y) == 0 {
		return
	}
	w.dragging = true
	w.px, w.py, w.wx, w.wy = px, py, int(x), int(y)
}

// Drag moves the window following the pointer.
func (w *Window) Drag() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dragging {
		return
	}
	px, py, ok := w.pointer()
	if !ok {
		return
	}
	C.nv_move(w.d, w.w, C.int(w.wx+px-w.px), C.int(w.wy+py-w.py))
}

// EndDrag finishes a manual move.
func (w *Window) EndDrag() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.dragging = false
	w.mu.Unlock()
}

// Close releases the display connection.
func (w *Window) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.d != nil {
		C.nv_close(w.d)
		w.d = nil
	}
}
