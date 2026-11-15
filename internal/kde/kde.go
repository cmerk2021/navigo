// Package kde reads the KDE Plasma accent color from kdeglobals.
package kde

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const pollInterval = 2 * time.Second

// KDEGlobalsPath returns $XDG_CONFIG_HOME/kdeglobals (default ~/.config/kdeglobals).
func KDEGlobalsPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kdeglobals")
}

// ParseAccent extracts [General] AccentColor from kdeglobals content. The value
// may be "R,G,B" (decimal, optionally with a fourth alpha component) or "#RRGGBB".
func ParseAccent(content string) (color.NRGBA, bool) {
	section := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if end := strings.IndexByte(line, ']'); end > 0 {
				section = strings.TrimSpace(line[1:end])
			}
			continue
		}
		if section != "General" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		// Ignore KConfig flags such as "AccentColor[$e]".
		if i := strings.IndexByte(key, '['); i >= 0 {
			key = strings.TrimSpace(key[:i])
		}
		if key != "AccentColor" {
			continue
		}
		return parseColorValue(strings.TrimSpace(val))
	}
	return color.NRGBA{}, false
}

func parseColorValue(v string) (color.NRGBA, bool) {
	if v == "" {
		return color.NRGBA{}, false
	}
	if strings.HasPrefix(v, "#") {
		return ParseHex(v)
	}
	parts := strings.Split(v, ",")
	if len(parts) != 3 && len(parts) != 4 {
		return color.NRGBA{}, false
	}
	var comps [4]uint8
	comps[3] = 255
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 255 {
			return color.NRGBA{}, false
		}
		comps[i] = uint8(n)
	}
	return color.NRGBA{R: comps[0], G: comps[1], B: comps[2], A: comps[3]}, true
}

// AccentColor reads and parses the kdeglobals file.
func AccentColor() (color.NRGBA, bool) {
	data, err := os.ReadFile(KDEGlobalsPath())
	if err != nil {
		return color.NRGBA{}, false
	}
	return ParseAccent(string(data))
}

// Watch polls kdeglobals every 2 s until ctx is done, invoking cb from its own
// goroutine immediately with the current value and then whenever it changes.
func Watch(ctx context.Context, cb func(c color.NRGBA, ok bool)) {
	go func() {
		path := KDEGlobalsPath()
		var lastMod time.Time
		var lastSize int64 = -1
		var lastC color.NRGBA
		var lastOK bool
		first := true

		check := func() {
			fi, err := os.Stat(path)
			var mod time.Time
			var size int64 = -1
			if err == nil {
				mod, size = fi.ModTime(), fi.Size()
			}
			if !first && mod.Equal(lastMod) && size == lastSize {
				return
			}
			lastMod, lastSize = mod, size
			c, ok := AccentColor()
			if first || c != lastC || ok != lastOK {
				first = false
				lastC, lastOK = c, ok
				cb(c, ok)
			}
		}

		check()
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}

// ParseHex parses "#RRGGBB", "RRGGBB", "#RGB" or "RGB" into an opaque color.
func ParseHex(s string) (color.NRGBA, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	switch len(s) {
	case 3:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 6:
	default:
		return color.NRGBA{}, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, true
}

// Hex formats c as "#RRGGBB".
func Hex(c color.NRGBA) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}
