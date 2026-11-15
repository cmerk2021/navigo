package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cmerk2021/navigo/internal/config"
)

type env struct {
	home, data, conf string
	exe              string
}

func setup(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{
		home: filepath.Join(root, "home"),
		data: filepath.Join(root, "data"),
		conf: filepath.Join(root, "config"),
		exe:  filepath.Join(root, "bin", "navigo"),
	}
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_DATA_HOME", e.data)
	t.Setenv("XDG_CONFIG_HOME", e.conf)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("PATH", t.TempDir())
	return e
}

func (e env) installedFiles() []string {
	return []string{
		filepath.Join(e.data, "applications", "navigo.desktop"),
		filepath.Join(e.data, "icons", "hicolor", "48x48", "apps", "navigo.png"),
		filepath.Join(e.data, "icons", "hicolor", "128x128", "apps", "navigo.png"),
		filepath.Join(e.data, "icons", "hicolor", "256x256", "apps", "navigo.png"),
		filepath.Join(e.data, "icons", "hicolor", "scalable", "apps", "navigo.svg"),
		filepath.Join(e.conf, "autostart", "navigo.desktop"),
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallIdempotent(t *testing.T) {
	e := setup(t)
	opts := Options{Autostart: true, ExecPath: e.exe}

	var out1 bytes.Buffer
	opts.Out = &out1
	if err := Install(opts); err != nil {
		t.Fatal(err)
	}
	first := map[string]string{}
	for _, f := range e.installedFiles() {
		first[f] = readFile(t, f)
	}
	if _, err := os.Stat(config.Path()); err != nil {
		t.Fatalf("default config not written: %v", err)
	}

	var out2 bytes.Buffer
	opts.Out = &out2
	if err := Install(opts); err != nil {
		t.Fatal(err)
	}
	for f, want := range first {
		if got := readFile(t, f); got != want {
			t.Errorf("%s changed on second install", f)
		}
	}
	if !strings.Contains(out2.String(), "Desktop entry unchanged") {
		t.Errorf("second run should report unchanged desktop entry:\n%s", out2.String())
	}

	desktop := first[filepath.Join(e.data, "applications", "navigo.desktop")]
	if !strings.Contains(desktop, "Exec="+e.exe+"\n") || strings.Contains(desktop, "Autostart") {
		t.Errorf("unexpected desktop entry:\n%s", desktop)
	}
	auto := first[filepath.Join(e.conf, "autostart", "navigo.desktop")]
	if !strings.HasSuffix(auto, "X-GNOME-Autostart-enabled=true\n") {
		t.Errorf("unexpected autostart entry:\n%s", auto)
	}
}

func TestInstallKeepsExistingConfig(t *testing.T) {
	e := setup(t)
	custom := `{"serverUrl":"https://music.example.com","username":"me"}`
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(Options{ExecPath: e.exe, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, config.Path()); got != custom {
		t.Errorf("config overwritten: %s", got)
	}
	if !strings.Contains(out.String(), "Config already exists") {
		t.Errorf("missing config notice:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(e.conf, "autostart", "navigo.desktop")); !os.IsNotExist(err) {
		t.Errorf("autostart entry created without Autostart option")
	}
}

func TestInstallNoIcons(t *testing.T) {
	e := setup(t)
	if err := Install(Options{ExecPath: e.exe, NoIcons: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.data, "icons")); !os.IsNotExist(err) {
		t.Errorf("icons installed despite NoIcons")
	}
}

func TestQuoteExec(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/me/.local/bin/navigo", "/home/me/.local/bin/navigo"},
		{"/opt/My Apps/navigo", `"/opt/My Apps/navigo"`},
		{`/opt/a$b"c/navigo`, `"/opt/a\\$b\\"c/navigo"`},
		{`/opt/back\slash/navigo`, `"/opt/back\\\\slash/navigo"`},
		{"/opt/100%/navigo", "/opt/100%%/navigo"},
	}
	for _, c := range cases {
		if got := QuoteExec(c.in); got != c.want {
			t.Errorf("QuoteExec(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUninstall(t *testing.T) {
	e := setup(t)
	if err := Install(Options{ExecPath: e.exe, Autostart: true}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(false, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range e.installedFiles() {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still present", f)
		}
	}
	if _, err := os.Stat(config.Path()); err != nil {
		t.Errorf("config removed without purge: %v", err)
	}
	// Running again with nothing installed must succeed.
	if err := Uninstall(false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallPurge(t *testing.T) {
	e := setup(t)
	if err := Install(Options{ExecPath: e.exe}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(true, nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{config.Dir(), config.CacheDir(), config.StateDir()} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s still present after purge", d)
		}
	}
}
