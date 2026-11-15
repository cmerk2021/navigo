// Package menu implements the context menu shared by the hamburger button and
// right-click.
package menu

import (
	"image"

	"gioui.org/layout"
	"gioui.org/unit"

	"github.com/cmerk2021/navigo/internal/core"
	"github.com/cmerk2021/navigo/internal/queue"
	"github.com/cmerk2021/navigo/ui/env"
	"github.com/cmerk2021/navigo/ui/theme"
	"github.com/cmerk2021/navigo/ui/widgets"
)

// Actions are the shell operations the menu triggers.
type Actions interface {
	ToggleMode()
	Compact() bool
	Quit()
}

// Item indices.
const (
	itemPicker = iota
	itemQueue
	itemEQ
	itemAssist
	sep1
	itemShuffle
	itemRepeat
	sep2
	itemMode
	itemSettings
	itemQuit
	numItems
)

// Menu is the popup menu.
type Menu struct {
	e     *env.Env
	act   Actions
	popup widgets.Popup
}

// New creates the menu.
func New(e *env.Env, act Actions) *Menu { return &Menu{e: e, act: act} }

// HeightDp is the fixed menu height in dp.
func HeightDp() unit.Dp {
	return 2*theme.MenuPadding + 2*theme.MenuSeparator + (numItems-2)*theme.MenuItemHeight
}

func (m *Menu) items(s core.Snapshot) []widgets.PopupItem {
	mode := "Switch to Compact mode"
	if m.act.Compact() {
		mode = "Switch to Normal mode"
	}
	it := make([]widgets.PopupItem, numItems)
	it[itemPicker] = widgets.PopupItem{Label: "Choose music…", Shortcut: "/"}
	it[itemQueue] = widgets.PopupItem{Label: "Queue…"}
	it[itemEQ] = widgets.PopupItem{Label: "Equalizer…", Shortcut: "E"}
	it[itemAssist] = widgets.PopupItem{Label: "EQ Assistant…"}
	it[sep1] = widgets.PopupItem{Separator: true}
	it[itemShuffle] = widgets.PopupItem{Label: "Shuffle", Check: true, Checked: s.Shuffle}
	it[itemRepeat] = widgets.PopupItem{Label: "Repeat", Segments: []string{"Off", "All", "One"}, Selected: int(s.Repeat)}
	it[sep2] = widgets.PopupItem{Separator: true}
	it[itemMode] = widgets.PopupItem{Label: mode, Shortcut: "C"}
	it[itemSettings] = widgets.PopupItem{Label: "Settings…"}
	it[itemQuit] = widgets.PopupItem{Label: "Quit"}
	return it
}

// Size returns the menu size in pixels.
func (m *Menu) Size(gtx layout.Context) image.Point {
	return widgets.PopupSize(gtx, m.items(core.Snapshot{}))
}

// Layout draws the menu at the origin and runs the chosen action. It returns
// true when the menu should close.
func (m *Menu) Layout(gtx layout.Context) bool {
	p := m.e.Player
	s := p.Snapshot()
	item, seg := m.popup.Layout(gtx, m.e.Theme, m.items(s))
	switch item {
	case -1:
		return false
	case itemPicker:
		m.e.Open(env.Picker)
	case itemQueue:
		m.e.Open(env.Queue)
	case itemEQ:
		m.e.Open(env.EQ)
	case itemAssist:
		m.e.Open(env.EQAssist)
	case itemShuffle:
		p.SetShuffle(!s.Shuffle)
		return false
	case itemRepeat:
		if seg >= 0 {
			p.SetRepeat(queue.Repeat(seg))
		}
		return false
	case itemMode:
		m.act.ToggleMode()
	case itemSettings:
		m.e.Open(env.Settings)
	case itemQuit:
		m.act.Quit()
	}
	return true
}
