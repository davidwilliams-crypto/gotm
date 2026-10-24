//go:build windows

package ui

import (
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

// Optional page capabilities, discovered with type assertions.
type (
	searcher   interface{ searchBox() *walk.LineEdit }
	focuser    interface{ focusTarget() walk.Window }
	reloader   interface{ reload() }
	canvasKeys interface {
		focuser
		pageKey(walk.Key, walk.Modifiers) bool
	}
)

// shortcutHelp is shown on the Settings page; keep it in step with setupShortcuts.
var shortcutHelp = [][2]string{
	{"Ctrl+1 … Ctrl+9, Ctrl+0", "Go to a page, in sidebar order"},
	{"Ctrl+,", "Settings"},
	{"Ctrl+F", "Search the current page"},
	{"Esc", "Clear the search"},
	{"F5", "Reload the current page"},
	{"Delete", "End the selected task (Processes)"},
	{"← / →", "Collapse / expand a branch (process tree)"},
	{"Tab / Shift+Tab", "Move between controls; Space or Enter activates"},
	{"↑ / ↓, Enter (sidebar, status bar)", "Choose a page or a status-bar button"},
	{"↑ / ↓, Home / End", "Select a resource (Performance)"},
	{"← / →, Home / End", "Scrub recording; End returns to live"},
	{"PgUp / PgDn", "Scroll Settings; Tab selects a setting"},
}

type shortcut struct {
	mods walk.Modifiers
	key  walk.Key
	fn   func()
}

// keyRouter runs gotm's shortcuts before a message reaches the focused
// control. Walk's own ShortcutActions only fire when the message's window is
// one Walk wraps, which is not true for the native list views inside a table.
type keyRouter struct {
	hwnd win.HWND
	keys []shortcut
	app  *App
}

func (r *keyRouter) OnPreTranslate(msg *win.MSG) bool {
	if msg.Message != win.WM_KEYDOWN && msg.Message != win.WM_SYSKEYDOWN {
		return false
	}
	if msg.HWnd != r.hwnd && !win.IsChild(r.hwnd, msg.HWnd) {
		return false
	}
	key, mods := walk.Key(msg.WParam), walk.ModifiersDown()
	for _, k := range r.keys {
		if k.key == key && k.mods == mods {
			k.fn()
			return true
		}
	}
	if r.app != nil {
		focus := win.GetFocus()
		if p, ok := r.app.pages[r.app.cur].impl.(canvasKeys); ok && p.focusTarget() != nil && win.HWND(p.focusTarget().Handle()) == focus {
			if (key == walk.KeySpace || key == walk.KeyReturn) && msg.LParam&(1<<30) != 0 {
				return true
			}
			if p.pageKey(key, mods) {
				return true
			}
		}
		// the sidebar, the status bar and other accessible canvases without a page handler
		if v := canvasAt(focus); v != nil && v.canvasKey(key, mods) {
			return true
		}
	}
	// This Walk version pre-translates each HWND separately. Native list views
	// and custom controls need the main form to perform ordinary tab navigation.
	if key == walk.KeyTab && (mods == 0 || mods == walk.ModShift) {
		return win.IsDialogMessage(r.hwnd, msg)
	}
	return false
}

func (a *App) setupShortcuts() {
	r := &keyRouter{hwnd: win.HWND(a.mw.Handle()), app: a}
	add := func(mods walk.Modifiers, key walk.Key, fn func()) {
		r.keys = append(r.keys, shortcut{mods, key, fn})
	}
	digits := []walk.Key{walk.Key1, walk.Key2, walk.Key3, walk.Key4, walk.Key5,
		walk.Key6, walk.Key7, walk.Key8, walk.Key9, walk.Key0}
	for i, k := range digits {
		page := i // sidebar order: Performance … Disk Space
		if page >= pgSettings {
			break
		}
		add(walk.ModControl, k, func() { a.navigate(page) })
		add(walk.ModControl, walk.KeyNumpad0+walk.Key((i+1)%10), func() { a.navigate(page) })
	}
	add(walk.ModControl, walk.KeyOEMComma, func() { a.navigate(pgSettings) })
	add(walk.ModControl, walk.KeyF, a.focusSearch)
	add(0, walk.KeyF5, func() {
		if rl, ok := a.pages[a.cur].impl.(reloader); ok {
			rl.reload()
		}
	})
	walk.App().AddPreTranslateHandlerForHWND(r.hwnd, r)
}

// focusSearch puts the caret in the current page's search box, text selected.
func (a *App) focusSearch() {
	s, ok := a.pages[a.cur].impl.(searcher)
	if !ok || s.searchBox() == nil {
		return
	}
	le := s.searchBox()
	_ = le.SetFocus()
	le.SetTextSelection(0, -1)
}

// focusPage gives keyboard focus to the page's main control (its table), so
// arrow keys work at once and no search box sits there with a blinking caret.
func (a *App) focusPage() {
	if f, ok := a.pages[a.cur].impl.(focuser); ok && f.focusTarget() != nil {
		_ = f.focusTarget().SetFocus()
		return
	}
	_ = a.mw.SetFocus()
}

const emSetCueBanner = 0x1501

// searchCue shows placeholder text in a search box even while it has focus,
// and wires Esc (clear, back to the list) and Down (into the list).
func searchCue(le *walk.LineEdit, text string, list walk.Window) {
	if le == nil {
		return
	}
	p, err := syscallUTF16(text)
	if err == nil {
		win.SendMessage(win.HWND(le.Handle()), emSetCueBanner, 1, uintptr(unsafe.Pointer(p)))
	}
	le.KeyDown().Attach(func(key walk.Key) {
		switch key {
		case walk.KeyEscape:
			_ = le.SetText("")
			if list != nil {
				_ = list.SetFocus()
			}
		case walk.KeyDown, walk.KeyReturn:
			if list != nil {
				_ = list.SetFocus()
			}
		}
	})
}

const cbSetCueBanner = 0x1703

// comboCue shows grey placeholder text in a drop-down list with no selection.
func comboCue(cb *walk.ComboBox, text string) {
	if cb == nil {
		return
	}
	if p, err := syscallUTF16(text); err == nil {
		win.SendMessage(win.HWND(cb.Handle()), cbSetCueBanner, 0, uintptr(unsafe.Pointer(p)))
	}
}
