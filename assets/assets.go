// Package assets embeds fonts and icons into the binary.
package assets

import "embed"

// Fonts contains the embedded Inter font files.
//
//go:embed fonts/*.ttf
var Fonts embed.FS

// Icons contains the application icon (SVG and PNG renditions).
//
//go:embed icon/*
var Icons embed.FS
