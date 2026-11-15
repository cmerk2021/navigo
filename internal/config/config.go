// Package config loads and saves Navigo's configuration and resolves the XDG
// directories used for configuration, cache and state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cmerk2021/navigo/internal/eq"
)

// AppName is used for directory names, the app ID and the D-Bus name.
const AppName = "navigo"

// Mode values.
const (
	ModeNormal  = "normal"
	ModeCompact = "compact"
)

// EQConfig is the persisted equalizer state.
type EQConfig struct {
	eq.Settings
	// Preset is the name of the currently selected preset ("" = custom/unsaved).
	Preset string `json:"preset"`
	// Custom holds user presets.
	Custom []eq.Preset `json:"custom"`
}

// Config is the persisted user configuration.
type Config struct {
	ServerURL string `json:"serverUrl"`
	Username  string `json:"username"`
	// Password is only stored here when the Secret Service keyring is not
	// available. The file is written with mode 0600.
	Password          string `json:"password,omitempty"`
	PasswordInKeyring bool   `json:"passwordInKeyring"`
	InsecureTLS       bool   `json:"insecureTls"`

	ShowModeButtons      bool   `json:"showModeButtons"`
	FollowKDEAccent      bool   `json:"followKdeAccent"`
	AccentOverride       string `json:"accentOverride"`
	CacheSizeMB          int    `json:"cacheSizeMb"`
	TranscodeBitrate     int    `json:"transcodeBitrate"`
	ReplayGain           bool   `json:"replayGain"`
	Scrobble             bool   `json:"scrobble"`
	PanelsSeparateWindow bool   `json:"panelsSeparateWindow"`

	Mode         string  `json:"mode"`
	HasWindowPos bool    `json:"hasWindowPos"`
	WindowX      int     `json:"windowX"`
	WindowY      int     `json:"windowY"`
	Volume       float64 `json:"volume"`
	Muted        bool    `json:"muted"`

	EQ EQConfig `json:"eq"`
}

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		ShowModeButtons:  true,
		CacheSizeMB:      2048,
		TranscodeBitrate: 320,
		Scrobble:         true,
		Mode:             ModeNormal,
		Volume:           1.0,
		EQ: EQConfig{
			Settings: eq.Settings{Enabled: false},
			Preset:   "Flat",
		},
	}
}

// HasServer reports whether server credentials have been configured.
func (c *Config) HasServer() bool {
	return c.ServerURL != "" && c.Username != ""
}

func xdgDir(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, AppName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, fallback, AppName)
}

// Dir returns $XDG_CONFIG_HOME/navigo.
func Dir() string { return xdgDir("XDG_CONFIG_HOME", ".config") }

// CacheDir returns $XDG_CACHE_HOME/navigo.
func CacheDir() string { return xdgDir("XDG_CACHE_HOME", ".cache") }

// StateDir returns $XDG_STATE_HOME/navigo.
func StateDir() string { return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")) }

// Path returns the config file path.
func Path() string { return filepath.Join(Dir(), "config.json") }

// EnsureDirs creates the config, cache and state directories.
func EnsureDirs() error {
	for _, d := range []string{Dir(), CacheDir(), StateDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Load reads the config file. If it doesn't exist, the default config is
// returned together with os.ErrNotExist wrapped in the error.
func Load() (*Config, error) {
	c := Default()
	b, err := os.ReadFile(Path())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return Default(), fmt.Errorf("parse %s: %w", Path(), err)
	}
	c.sanitize()
	return c, nil
}

func (c *Config) sanitize() {
	if c.CacheSizeMB <= 0 {
		c.CacheSizeMB = 2048
	}
	if c.TranscodeBitrate <= 0 {
		c.TranscodeBitrate = 320
	}
	if c.Mode != ModeCompact {
		c.Mode = ModeNormal
	}
	if c.Volume < 0 || c.Volume > 1 {
		c.Volume = 1
	}
	for i := range c.EQ.Bands {
		c.EQ.Bands[i] = clamp(c.EQ.Bands[i], -eq.MaxGain, eq.MaxGain)
	}
	c.EQ.Preamp = clamp(c.EQ.Preamp, -eq.MaxGain, eq.MaxGain)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

var saveMu sync.Mutex

// Save writes the config atomically with mode 0600.
func (c *Config) Save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(Path(), b, 0o600)
}

// WriteDefaultIfMissing writes a default config file unless one already
// exists. It reports whether a file was written.
func WriteDefaultIfMissing() (bool, error) {
	if _, err := os.Stat(Path()); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, Default().Save()
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	n := *c
	n.EQ.Custom = append([]eq.Preset(nil), c.EQ.Custom...)
	return &n
}

// WriteFileAtomic writes data to a temp file and renames it into place.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
