// Command navigo is a minimal Navidrome/Subsonic desktop music player.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	gioapp "gioui.org/app"

	"github.com/cmerk2021/navigo/internal/config"
	"github.com/cmerk2021/navigo/internal/install"
	"github.com/cmerk2021/navigo/ui/app"
)

// Set via -ldflags by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `Navigo — minimal Navidrome/Subsonic music player

Usage:
  navigo                 Launch the player
  navigo install         Install desktop entry, icons and config directories
      --autostart          Also start Navigo on login
      --no-icons           Do not install icons
  navigo uninstall       Remove desktop entry, icons and autostart entry
      --purge              Also delete configuration, cache and state
  navigo version         Print version information
  navigo help            Show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "run" {
		runGUI(stderr)
		return 0
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		autostart := fs.Bool("autostart", false, "also install an autostart entry")
		noIcons := fs.Bool("no-icons", false, "do not install icons")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := install.Install(install.Options{Autostart: *autostart, NoIcons: *noIcons, Out: stdout}); err != nil {
			fmt.Fprintln(stderr, "navigo install:", err)
			return 1
		}
		return 0
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
		fs.SetOutput(stderr)
		purge := fs.Bool("purge", false, "also remove configuration, cache and state")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := install.Uninstall(*purge, stdout); err != nil {
			fmt.Fprintln(stderr, "navigo uninstall:", err)
			return 1
		}
		return 0
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "navigo %s (commit %s, built %s)\n", version, commit, date)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "navigo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runGUI(stderr io.Writer) {
	if err := config.EnsureDirs(); err != nil {
		fmt.Fprintln(stderr, "navigo: warning:", err)
	}
	cfg, err := config.Load()
	if errors.Is(err, fs.ErrNotExist) {
		cfg = config.Default()
	} else if err != nil {
		fmt.Fprintln(stderr, "navigo: could not load config, using defaults:", err)
		cfg = config.Default()
	}
	gioapp.ID = app.AppID
	go app.Run(cfg)
	gioapp.Main()
}
