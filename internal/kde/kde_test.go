package kde

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAccent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    color.NRGBA
		ok      bool
	}{
		{"rgb", "[General]\nAccentColor=61,174,233\n", color.NRGBA{61, 174, 233, 255}, true},
		{"hex", "[General]\nAccentColor=#3daee9\n", color.NRGBA{0x3d, 0xae, 0xe9, 255}, true},
		{"whitespace", "  [General]  \r\n  AccentColor = 1, 2 ,3 \r\n", color.NRGBA{1, 2, 3, 255}, true},
		{"missing", "[General]\nColorScheme=BreezeDark\n", color.NRGBA{}, false},
		{"other section ignored", "[Colors:Selection]\nAccentColor=1,2,3\n[General]\nFoo=bar\n", color.NRGBA{}, false},
		{"after other section", "[KDE]\nAccentColor=9,9,9\n[General]\nAccentColor=4,5,6\n", color.NRGBA{4, 5, 6, 255}, true},
		{"invalid", "[General]\nAccentColor=300,1,1\n", color.NRGBA{}, false},
		{"empty", "[General]\nAccentColor=\n", color.NRGBA{}, false},
		{"kconfig flag", "[General]\nAccentColor[$e]=10,20,30\n", color.NRGBA{10, 20, 30, 255}, true},
		{"no section", "AccentColor=1,2,3\n", color.NRGBA{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseAccent(tt.content)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ParseAccent = %v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseHex(t *testing.T) {
	tests := []struct {
		in   string
		want color.NRGBA
		ok   bool
	}{
		{"#3DAEE9", color.NRGBA{0x3d, 0xae, 0xe9, 255}, true},
		{"3daee9", color.NRGBA{0x3d, 0xae, 0xe9, 255}, true},
		{"#abc", color.NRGBA{0xaa, 0xbb, 0xcc, 255}, true},
		{"#12345", color.NRGBA{}, false},
		{"#GGGGGG", color.NRGBA{}, false},
		{"", color.NRGBA{}, false},
	}
	for _, tt := range tests {
		got, ok := ParseHex(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("ParseHex(%q) = %v,%v want %v,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
	if h := Hex(color.NRGBA{0x3d, 0xae, 0xe9, 255}); h != "#3DAEE9" {
		t.Errorf("Hex = %s", h)
	}
}

func TestWatchInitialCallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "kdeglobals"), []byte("[General]\nAccentColor=1,2,3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan color.NRGBA, 1)
	Watch(ctx, func(c color.NRGBA, ok bool) {
		if ok {
			select {
			case ch <- c:
			default:
			}
		}
	})
	select {
	case c := <-ch:
		if c != (color.NRGBA{1, 2, 3, 255}) {
			t.Fatalf("got %v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no initial callback")
	}
}
