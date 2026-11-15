# Navigo

A minimal, distraction-free desktop music player for **Navidrome** and other
**Subsonic-API** servers, for Linux (Wayland and X11).

* **Normal mode**: a fixed 370×370 square with the album art as background, a
  title/artist pill, and playback controls that appear when you hover.
* **Compact mode**: a fixed 370×64 pill with a round thumbnail, scrolling
  title/artist and previous/play/next. It's quiet enough to keep on screen
  all the time with KDE's *Keep Above Others*.
* Gapless playback, a 10-band EQ with presets and a live response curve, an
  "EQ Assistant" ("Which sounds best?"), a disk cache with prefetch, MPRIS
  (media keys, KDE media widget, `playerctl`), and scrobbling.

It's a single Go binary. The UI is drawn with [Gio](https://gioui.org) and
audio is decoded in pure Go and played through
[oto](https://github.com/ebitengine/oto).

---

## Installing

### From a release (package manager)

Each GitHub release contains:

| Asset | Contents |
|-------|----------|
| `navigo_<version>_linux_amd64.tar.gz` | `navigo` binary, `README.md`, `LICENSE`, `THIRD_PARTY_LICENSES.md` |
| `navigo_<version>_linux_arm64.tar.gz` | same, for arm64 (only when the optional arm64 job is enabled) |
| `checksums.txt` | SHA-256 sums of the archives |

`<version>` has no leading `v` (tag `v1.2.3` → `navigo_1.2.3_linux_amd64.tar.gz`).
The binary sits at the root of the archive, so a package manager only needs
to extract `navigo` to `~/.local/bin/navigo`.

Then run once:

```sh
navigo install
```

After that, launch Navigo from your application menu. You never need the
terminal again.

### `navigo install`

It's idempotent and safe to re-run, and it prints each step:

1. Resolves its own absolute path (symlinks resolved).
2. Creates `~/.config/navigo/` with a default `config.json` if missing (it
   never overwrites an existing config), plus the cache and state directories.
3. Writes `~/.local/share/applications/navigo.desktop` (`StartupWMClass=navigo`).
4. Installs the icon to `~/.local/share/icons/hicolor/{48x48,128x128,256x256}/apps/navigo.png`
   and `scalable/apps/navigo.svg`.
5. Refreshes `update-desktop-database`, `gtk-update-icon-cache` and
   `kbuildsycoca6`/`kbuildsycoca5` if they exist.
6. Warns if `~/.local/bin` isn't on your `$PATH`.

Flags:

* `--autostart`: also writes `~/.config/autostart/navigo.desktop`.
* `--no-icons`: skips the icons.

The GUI also works without `navigo install`; the desktop file is only a
convenience.

### `navigo uninstall`

Removes the desktop entry, icons and autostart entry. Your configuration is
kept unless you pass `--purge`, which also deletes the config, cache and
state directories. The binary itself is left for your package manager to
remove.

### Other commands

```
navigo            launch the player
navigo version    print version, commit and build date
navigo help       usage
```

---

## First run

If no server is configured, Navigo opens the **login** panel. Enter the server
URL (for example `https://music.example.com`), your username and password, and
press **Connect**. The connection is checked with `ping`, and any error is
shown inline. Once connected, the music picker opens: choose an album and
playback starts.

### Password storage

The password is stored in the desktop keyring (Secret Service via D-Bus: KDE
Wallet, GNOME Keyring, KeePassXC…) when one is available. If not, it falls
back to `~/.config/navigo/config.json`, which is always written with mode
`0600`. Authentication uses Subsonic token auth (`t = md5(password + salt)`
with a fresh salt per request), so the password itself is never sent to the
server.

---

## Using Navigo

* **Drag** the window from any empty area.
* **Right-click** anywhere (or use the ☰ button in Normal mode) to open the menu:
  Choose music…, Queue…, Equalizer…, EQ Assistant…, Shuffle, Repeat
  (Off/All/One), Switch to Compact/Normal mode, Settings…, Quit.
* **Oval button** (Normal) / **square button** (Compact) switch modes. You can
  hide them with *Settings → Show mode-switch buttons*. Mode switching then
  still works from the menu, with `C`, or by double-clicking the Compact
  thumbnail.
* Panels (picker, queue, EQ, EQ Assistant, settings) open as a 370×600 view.

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `Space` | Play / pause |
| `←` / `→` | Seek −5 s / +5 s |
| `Shift+←` / `Shift+→` | Previous / next track |
| `↑` / `↓` | Volume up / down |
| `M` | Mute |
| `C` | Toggle Normal / Compact mode |
| `/` or `Ctrl+L` | Choose music (picker) |
| `E` | Equalizer |
| `Esc` | Close menu / panel |
| `1`–`4`, `Enter` | Select / confirm an option in the EQ Assistant |

Single-key shortcuts are disabled while a text field (search, settings, login)
is focused.

### Keeping Compact mode on top (KDE Plasma)

Navigo never forces itself on top. To keep it above other windows:

* **One-off:** focus Navigo and press `Alt+F3` (or right-click its taskbar
  entry) → **More Actions → Keep Above Others**.
* **Permanently:** *System Settings → Window Management → Window Rules → Add
  New…*. Set **Window class (application)** to *Exact match* `navigo`, then
  add the property **Keep above other windows** → *Force* → *Yes*. You can also
  add *Skip taskbar* or a fixed position here.

### EQ Assistant

*Menu → EQ Assistant…* loops a 25-second section of the current track (or
another song) through the real EQ. It runs three rounds (low end, mids/vocals,
highs/air), each with four loudness-matched options. Switch freely with the
mouse or `1`–`4` and confirm with **Pick this one** / `Enter`. The final
screen shows the combined curve with a Before/After toggle, and lets you save
it as a preset or apply it.

---

## Formats and transcoding

Decoded natively in Go (original file, no transcoding):

* FLAC (`github.com/mewkiz/flac`)
* MP3 (`github.com/hajimehoshi/go-mp3`)
* Ogg Vorbis (`github.com/jfreymuth/oggvorbis`)
* WAV (PCM 8/16/24/32-bit and float)

Every other format (Opus, AAC/M4A/ALAC, WMA, APE, …) is requested from the
server as MP3 (`stream?format=mp3&maxBitRate=<Settings → Transcode bitrate>`,
320 kbps by default). If a native file turns out to be undecodable, Navigo
retries once with a transcode before skipping to the next track.

Audio is resampled to 44.1 kHz stereo float. One continuous output stream
carries the mixer chain (decoder → ReplayGain → EQ → preamp → soft limiter →
volume), so track changes are gapless and the EQ state is never reset.

## Cache locations

| What | Where |
|------|-------|
| Config | `$XDG_CONFIG_HOME/navigo/config.json` (default `~/.config/navigo/`) |
| Tracks | `$XDG_CACHE_HOME/navigo/tracks/` (default `~/.cache/navigo/tracks/`), LRU, 2 GB by default |
| Cover art | `$XDG_CACHE_HOME/navigo/covers/` |
| Queue / position | `$XDG_STATE_HOME/navigo/` (default `~/.local/state/navigo/`) |

Tracks are downloaded to `.part` files (resumed with HTTP Range requests) and
renamed when complete. Playback starts as soon as enough of the file has
arrived to decode (typically 1-2 s of audio). The next 3 tracks are prefetched in the background. The current and
prefetched tracks are never evicted. You can change the cache size and clear
the cache in Settings.

---

## Building from source

Requires Go 1.22+ and cgo (Gio links against Wayland/X11/EGL/Vulkan, and oto
against ALSA).

**Debian / Ubuntu**

```sh
sudo apt install build-essential pkg-config \
  libwayland-dev libx11-dev libx11-xcb-dev libxkbcommon-dev libxkbcommon-x11-dev \
  libxcursor-dev libxfixes-dev libgles2-mesa-dev libegl1-mesa-dev \
  libvulkan-dev libasound2-dev
```

**Fedora**

```sh
sudo dnf install gcc pkgconf-pkg-config wayland-devel libX11-devel \
  libxkbcommon-devel libxkbcommon-x11-devel libXcursor-devel libXfixes-devel \
  mesa-libGLES-devel mesa-libEGL-devel vulkan-headers alsa-lib-devel
```

**Arch**

```sh
sudo pacman -S --needed base-devel wayland libx11 libxkbcommon libxkbcommon-x11 \
  libxcursor libxfixes mesa vulkan-headers vulkan-icd-loader alsa-lib
```

Then:

```sh
make build      # ./navigo
make run
make test       # go test -race ./...
make lint       # go vet + gofmt check
make install    # copies to ~/.local/bin and runs `navigo install`
make snapshot   # goreleaser release --snapshot --clean → dist/
```

The icon PNGs in `assets/icon/` are generated from the same geometry as
`navigo.svg` by `make icons` (`go run ./tools/genicon`).

## Releasing (GoReleaser)

`.goreleaser.yaml` (version 2) builds `./cmd/navigo` for `linux/amd64` with
`CGO_ENABLED=1`, `-trimpath` and
`-ldflags "-s -w -X main.version=… -X main.commit=… -X main.date=…"`.
It produces `navigo_<version>_linux_<arch>.tar.gz`, `checksums.txt`, and a
changelog grouped by conventional commits.

Because Gio needs cgo and the system development headers, releases **must be
built on Linux with the packages above installed**. The
`.github/workflows/release.yml` workflow does this on `ubuntu-latest` whenever
a `v*` tag is pushed:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/ci.yml` runs vet, gofmt, tests and a build on every push
and pull request.

**arm64** is optional. Cross-compiling cgo from amd64 requires an aarch64
toolchain and arm64 system libraries, so either:

* enable the commented-out `goreleaser-arm64` job in `release.yml`. It runs on
  a native `ubuntu-24.04-arm` runner with a second config that has
  `goarch: [arm64]` and `release.mode: append`, or
* build inside [`goreleaser-cross`](https://github.com/goreleaser/goreleaser-cross)
  with the arm64 dev libraries installed in the image.

---

## Known limitations and fallbacks

* **Transparency / rounded corners:** Gio does not expose transparent window
  surfaces, so the window is painted with a solid near-black background
  (`#0E0E10`). The rounded card is drawn on top of it, which means the
  corners are dark rather than see-through.
* **Window decorations on X11:** Gio's X11 backend always requests server-side
  decorations. Navigo removes them itself through `_MOTIF_WM_HINTS`. KWin,
  Mutter, Openbox, xfwm4 and most other window managers honor this. Window
  dragging on X11 is done by Navigo (`XMoveWindow`), because Gio ignores the
  move action there. On Wayland, the compositor's interactive move is used.
* **Panel height changes:** opening a panel changes the window height to 600
  (and back). If the compositor doesn't apply the change within ~500 ms, Navigo
  switches automatically to opening panels (and the Compact-mode menu) in a
  separate undecorated 370×600 window. You can also force that behavior with
  *Settings → Open panels in separate window*. On X11 the separate window is
  placed exactly over the main window. On Wayland the compositor chooses its
  position.
* **Window position:** saved and restored on X11. Wayland does not let
  clients read or set their position, so the compositor decides (KWin can pin
  it with a Window Rule).
* **Window size:** the window is fixed at min = max size. Some tiling
  compositors may still force a size; the content is then drawn at its fixed
  size in the top-left corner.
* **Seeking beyond the downloaded part:** Navigo waits (Buffering) until the
  download reaches the target. It doesn't open a second Range request from
  that offset. Tracks are usually fully cached within seconds.
* **MP3 gapless:** encoder delay/padding (LAME/Xing info) is not trimmed, so
  gapless MP3 albums may have a few milliseconds of silence between tracks.
  FLAC, Vorbis and WAV are sample-accurate.
* **CPU:** Navigo only redraws while something animates (marquee text,
  fades, progress at ≤ 30 fps). Scrolling titles in software-rendered
  sessions (no GPU, e.g. VMs/llvmpipe) cost noticeably more CPU.
* **Audio device:** at startup Navigo waits up to 15 s for the
  PulseAudio/PipeWire socket (useful with autostart). Stream errors are
  retried automatically with a toast. If the device context itself can't be
  opened, restart Navigo: oto allows only one context per process.
* **No tray icon:** use MPRIS (KDE media widget, media keys, `playerctl`).

## License

Navigo is licensed under the **GNU General Public License v3.0**, see
[`LICENSE`](LICENSE). The repository already carried a GPL-3.0 license, so it
was kept instead of the MIT placeholder suggested in the original brief.
Bundled third-party assets (the Inter font, SIL OFL 1.1) are listed in
[`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md).
