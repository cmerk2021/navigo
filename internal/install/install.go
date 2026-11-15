// Package install integrates Navigo with the desktop of the current user:
// it writes the .desktop entry, installs the icons, creates the default
// configuration and refreshes the relevant desktop caches. Everything is
// per-user (XDG directories) and idempotent.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cmerk2021/navigo/assets"
	"github.com/cmerk2021/navigo/internal/config"
)

// Options controls Install.
type Options struct {
	// Autostart additionally installs an XDG autostart entry.
	Autostart bool
	// NoIcons skips installing the icons into the hicolor theme.
	NoIcons bool
	// Out receives human-readable progress output. Nil discards it.
	Out io.Writer
	// ExecPath overrides the executable path written to the desktop entry.
	// By default it is resolved via os.Executable and filepath.EvalSymlinks.
	ExecPath string
}

const (
	desktopFile = config.AppName + ".desktop"
	toolTimeout = 30 * time.Second
)

// iconFiles maps embedded asset names to their hicolor theme sub-paths.
var iconFiles = []struct{ src, dst string }{
	{"icon/navigo-48.png", "48x48/apps/navigo.png"},
	{"icon/navigo-128.png", "128x128/apps/navigo.png"},
	{"icon/navigo-256.png", "256x256/apps/navigo.png"},
	{"icon/navigo.svg", "scalable/apps/navigo.svg"},
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func xdgHome(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(homeDir(), fallback)
}

func dataHome() string      { return xdgHome("XDG_DATA_HOME", filepath.Join(".local", "share")) }
func configHome() string    { return xdgHome("XDG_CONFIG_HOME", ".config") }
func appsDir() string       { return filepath.Join(dataHome(), "applications") }
func hicolorDir() string    { return filepath.Join(dataHome(), "icons", "hicolor") }
func desktopPath() string   { return filepath.Join(appsDir(), desktopFile) }
func autostartPath() string { return filepath.Join(configHome(), "autostart", desktopFile) }

// pretty shortens paths below the home directory to ~/...
func pretty(p string) string {
	h := homeDir()
	if h == "" || h == "/" {
		return p
	}
	if p == h {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, h+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return p
}

type printer struct{ w io.Writer }

func (p printer) ok(format string, a ...any)   { fmt.Fprintf(p.w, "  ✓ "+format+"\n", a...) }
func (p printer) info(format string, a ...any) { fmt.Fprintf(p.w, "  • "+format+"\n", a...) }
func (p printer) warn(format string, a ...any) { fmt.Fprintf(p.w, "  ! "+format+"\n", a...) }

func newPrinter(w io.Writer) printer {
	if w == nil {
		w = io.Discard
	}
	return printer{w}
}

// QuoteExec quotes path for use in the Exec key of a desktop entry. Paths
// containing reserved characters are enclosed in double quotes with `"`,
// "`", `$` and `\` backslash-escaped, as required by the Desktop Entry
// specification. Because Exec is itself of type string, the result is then
// escaped once more at the string level (so a quoted backslash ends up as
// `\\`). Literal percent signs are doubled so they aren't taken as field codes.
func QuoteExec(path string) string {
	s := strings.ReplaceAll(path, "%", "%%")
	if strings.ContainsAny(s, " \t\n\"'\\><~|&;$*?#()`") {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range s {
			switch r {
			case '"', '`', '$', '\\':
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
		s = b.String()
	}
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
}

// DesktopEntry renders the .desktop file content for execPath. If autostart
// is true the GNOME autostart key is appended.
func DesktopEntry(execPath string, autostart bool) string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Type=Application\n")
	b.WriteString("Name=Navigo\n")
	b.WriteString("Comment=Minimal Navidrome/Subsonic music player\n")
	b.WriteString("Exec=" + QuoteExec(execPath) + "\n")
	b.WriteString("Icon=" + config.AppName + "\n")
	b.WriteString("Terminal=false\n")
	b.WriteString("Categories=AudioVideo;Audio;Player;\n")
	b.WriteString("StartupWMClass=" + config.AppName + "\n")
	b.WriteString("StartupNotify=false\n")
	if autostart {
		b.WriteString("X-GNOME-Autostart-enabled=true\n")
	}
	return b.String()
}

func resolveExec(override string) (string, error) {
	p := override
	if p == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate executable: %w", err)
		}
		if p, err = filepath.EvalSymlinks(exe); err != nil {
			return "", fmt.Errorf("resolve executable: %w", err)
		}
	}
	return filepath.Abs(p)
}

// writeIfChanged writes data to path unless it already holds exactly data.
// It reports whether the file was written.
func writeIfChanged(path string, data []byte, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := config.WriteFileAtomic(path, data, perm); err != nil {
		return false, err
	}
	return true, nil
}

// Install integrates Navigo into the user's desktop. It is safe to run
// repeatedly; the existing configuration is never overwritten.
func Install(opts Options) error {
	p := newPrinter(opts.Out)

	exe, err := resolveExec(opts.ExecPath)
	if err != nil {
		return err
	}
	p.ok("Using executable %s", pretty(exe))

	_, statErr := os.Stat(config.Dir())
	if err := config.EnsureDirs(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		p.ok("Created config directory %s", pretty(config.Dir()))
	} else {
		p.info("Config directory exists (%s)", pretty(config.Dir()))
	}
	written, err := config.WriteDefaultIfMissing()
	if err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	if written {
		p.ok("Wrote default config %s", pretty(config.Path()))
	} else {
		p.info("Config already exists, leaving it untouched (%s)", pretty(config.Path()))
	}

	changed, err := writeIfChanged(desktopPath(), []byte(DesktopEntry(exe, false)), 0o644)
	if err != nil {
		return fmt.Errorf("write desktop entry: %w", err)
	}
	if changed {
		p.ok("Wrote desktop entry %s", pretty(desktopPath()))
	} else {
		p.info("Desktop entry unchanged (%s)", pretty(desktopPath()))
	}

	if opts.NoIcons {
		p.info("Skipping icons")
	} else if err := installIcons(p); err != nil {
		return err
	}

	refreshCaches(p)
	checkPath(p, exe)

	if opts.Autostart {
		changed, err := writeIfChanged(autostartPath(), []byte(DesktopEntry(exe, true)), 0o644)
		if err != nil {
			return fmt.Errorf("write autostart entry: %w", err)
		}
		if changed {
			p.ok("Enabled autostart %s", pretty(autostartPath()))
		} else {
			p.info("Autostart entry unchanged (%s)", pretty(autostartPath()))
		}
	}

	fmt.Fprintln(p.w, "Navigo is installed. Launch it from your application menu.")
	return nil
}

func installIcons(p printer) error {
	n := 0
	for _, f := range iconFiles {
		data, err := fs.ReadFile(assets.Icons, f.src)
		if err != nil {
			return fmt.Errorf("read embedded icon %s: %w", f.src, err)
		}
		changed, err := writeIfChanged(filepath.Join(hicolorDir(), f.dst), data, 0o644)
		if err != nil {
			return fmt.Errorf("install icon: %w", err)
		}
		if changed {
			n++
		}
	}
	if n > 0 {
		p.ok("Installed %d icons into %s", n, pretty(hicolorDir()))
	} else {
		p.info("Icons unchanged (%s)", pretty(hicolorDir()))
	}
	return nil
}

func runTool(p printer, name string, args ...string) bool {
	bin, err := exec.LookPath(name)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		p.info("Note: %s failed (%s); this is usually harmless", name, msg)
		return true
	}
	p.ok("Refreshed caches with %s", name)
	return true
}

// refreshCaches runs the desktop cache tools that are available. Missing
// tools and failures are not errors.
func refreshCaches(p printer) {
	if _, err := os.Stat(appsDir()); err == nil {
		runTool(p, "update-desktop-database", appsDir())
	}
	if _, err := os.Stat(hicolorDir()); err == nil {
		runTool(p, "gtk-update-icon-cache", "-f", "-t", hicolorDir())
	}
	if !runTool(p, "kbuildsycoca6") {
		runTool(p, "kbuildsycoca5")
	}
}

func checkPath(p printer, exe string) {
	h := homeDir()
	if h == "" {
		return
	}
	localBin := filepath.Clean(filepath.Join(h, ".local", "bin"))
	inPath := false
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" && filepath.Clean(d) == localBin {
			inPath = true
			break
		}
	}
	if !inPath {
		p.warn("%s is not in your $PATH; add it to run navigo from a terminal", pretty(localBin))
	}
	if filepath.Dir(exe) != localBin {
		p.info("The desktop entry points to %s", pretty(exe))
	}
}

func remove(p printer, path, what string) error {
	err := os.Remove(path)
	switch {
	case err == nil:
		p.ok("Removed %s %s", what, pretty(path))
	case errors.Is(err, fs.ErrNotExist):
		p.info("No %s at %s", what, pretty(path))
	default:
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Uninstall removes the desktop integration created by Install. The
// configuration is kept unless purge is set, in which case the config,
// cache and state directories are deleted as well.
func Uninstall(purge bool, out io.Writer) error {
	p := newPrinter(out)
	if err := remove(p, desktopPath(), "desktop entry"); err != nil {
		return err
	}
	for _, f := range iconFiles {
		if err := remove(p, filepath.Join(hicolorDir(), f.dst), "icon"); err != nil {
			return err
		}
	}
	if err := remove(p, autostartPath(), "autostart entry"); err != nil {
		return err
	}
	refreshCaches(p)

	if purge {
		for _, d := range []struct{ path, what string }{
			{config.Dir(), "config directory"},
			{config.CacheDir(), "cache directory"},
			{config.StateDir(), "state directory"},
		} {
			if err := os.RemoveAll(d.path); err != nil {
				return fmt.Errorf("remove %s: %w", d.path, err)
			}
			p.ok("Removed %s %s", d.what, pretty(d.path))
		}
	} else {
		p.info("Keeping configuration in %s", pretty(config.Dir()))
	}

	if exe, err := resolveExec(""); err == nil {
		p.info("To remove the program itself, delete %s", pretty(exe))
	}
	fmt.Fprintln(p.w, "Navigo has been uninstalled.")
	return nil
}
