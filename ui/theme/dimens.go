package theme

import "gioui.org/unit"

// All fixed dimensions used by the UI. Everything is fixed size; there is no
// scaling or resizing logic anywhere in the application.
const (
	// Window sizes.
	WindowWidth   unit.Dp = 370
	NormalHeight  unit.Dp = 370
	CompactHeight unit.Dp = 64
	PanelHeight   unit.Dp = 600

	// Normal mode.
	NormalRadius       unit.Dp = 30
	NormalMargin       unit.Dp = 14
	TopBarHeight       unit.Dp = 46
	TopButtonSize      unit.Dp = 36
	TopButtonGap       unit.Dp = 8
	TopPillPadding     unit.Dp = 16
	TopGradientHeight  unit.Dp = 120
	BottomGradient     unit.Dp = 170
	SeekBarY           unit.Dp = 266 // ~72% of 370
	SeekTrackHeight    unit.Dp = 4
	SeekKnobSize       unit.Dp = 12
	SeekHitHeight      unit.Dp = 24
	SeekTimeGap        unit.Dp = 6
	SmallControl       unit.Dp = 36
	SkipControl        unit.Dp = 52
	PlayControl        unit.Dp = 58
	ControlGap         unit.Dp = 14
	ControlBottomInset unit.Dp = 15
	ToastHeight        unit.Dp = 34

	// Compact mode.
	CompactRadius    unit.Dp = 32
	CompactPadding   unit.Dp = 12
	CompactInnerPad  unit.Dp = 4
	CompactThumb     unit.Dp = 40
	CompactButton    unit.Dp = 36
	CompactPlay      unit.Dp = 40
	CompactGap       unit.Dp = 6
	CompactTextInset unit.Dp = 10

	// Menu.
	MenuRadius     unit.Dp = 14
	MenuItemHeight unit.Dp = 36
	MenuWidth      unit.Dp = 250
	MenuPadding    unit.Dp = 6
	MenuSeparator  unit.Dp = 9
	MenuMargin     unit.Dp = 6

	// Panels.
	PanelRadius      unit.Dp = 24
	PanelPadding     unit.Dp = 16
	PanelHeaderH     unit.Dp = 40
	CloseButtonSize  unit.Dp = 32
	SearchPillHeight unit.Dp = 38
	SegmentHeight    unit.Dp = 32
	FieldHeight      unit.Dp = 40
	PillButtonHeight unit.Dp = 38
	RowHeight        unit.Dp = 48
	ToggleWidth      unit.Dp = 40
	ToggleHeight     unit.Dp = 24

	// Picker grid.
	GridColumns             = 3
	CoverSize       unit.Dp = 106
	CoverRadius     unit.Dp = 12
	GridGap         unit.Dp = 10
	GridCaptionH    unit.Dp = 38
	HoverButtonSize unit.Dp = 34
	TrackThumb      unit.Dp = 36

	// Equalizer panel.
	EQCurveHeight    unit.Dp = 150
	EQSliderWidth    unit.Dp = 30
	EQSliderHeight   unit.Dp = 220
	EQSliderTrack    unit.Dp = 4
	EQSliderKnob     unit.Dp = 16
	EQOptionSize     unit.Dp = 64
	DropdownItemH    unit.Dp = 34
	DropdownMaxItems         = 9

	// Text sizes.
	TitleSize   unit.Sp = 14
	ArtistSize  unit.Sp = 12.5
	SmallSize   unit.Sp = 11
	BodySize    unit.Sp = 13
	HeadingSize unit.Sp = 17
	CaptionSize unit.Sp = 11.5
)
