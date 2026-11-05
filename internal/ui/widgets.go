//go:build windows

package ui

import (
	"unsafe"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
)

// hit is a clickable region registered while painting.
type hit struct {
	r  rect
	id string
}

// canvasView is a double-buffered, fully owner-drawn widget. Because one paint
// draws the whole surface into a back buffer, updating it never flickers.
type canvasView struct {
	cw      *walk.CustomWidget
	paintFn func(cv *walk.Canvas, w, h int)
	hits    []hit
	hover   string
	down    string
	bgSide  bool // paint on the sidebar shade rather than the page shade

	onClick func(id string, btn walk.MouseButton)
	onWheel func(delta int)
	onMove  func(x, y int)
	w, h    int

	mx, my    int  // last mouse position (pixels)
	left      bool // left button currently held
	focusHook bool
	tracking  bool // Windows will post WM_MOUSELEAVE when the pointer exits
	keyboard  bool
	keyHit    string
	keyName   string // what the keyboard does here (the accessible help text)

	// accessibility: acc exposes the items registered while painting to
	// assistive technology (see uia.go); accName/accCtl describe the canvas.
	acc       bool
	accName   string
	accCtl    int32
	items     []accItem
	uia       *uiaRoot
	announced string
}

func newCanvasView(paint func(cv *walk.Canvas, w, h int)) *canvasView {
	return &canvasView{paintFn: paint}
}

// background is the surface colour: the sidebar/footer shade or the page shade.
func (v *canvasView) background() walk.Color {
	if v.bgSide {
		return T.Side
	}
	return T.Bg
}

func (v *canvasView) paint(cv *walk.Canvas, _ walk.Rectangle) error {
	if v.cw == nil {
		return nil
	}
	if !v.focusHook {
		v.focusHook = true
		v.cw.FocusedChanged().Attach(func() {
			if !v.cw.Focused() {
				v.down = ""
			}
			v.invalidate()
		})
		v.cw.Disposing().Attach(func() {
			v.stopTracking()
			v.focusHook = false
		})
	}
	r := v.cw.ClientBoundsPixels()
	if r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	if d := cv.DPI(); d > 0 {
		gs = float64(d) / 96
	}
	v.w, v.h = int(float64(r.Width)/gs+0.999), int(float64(r.Height)/gs+0.999) // design units
	v.hits = v.hits[:0]
	v.items = v.items[:0]
	fillRect(cv, rect{0, 0, v.w + 2, v.h + 2}, v.background())
	v.paintFn(cv, v.w, v.h)
	if v.keyboard && v.cw.Focused() && len(v.hits) > 0 {
		focused := v.hits[0]
		for _, item := range v.hits {
			if item.id == v.keyHit {
				focused = item
				break
			}
		}
		v.keyHit = focused.id
		r := focused.r
		focusRect := rect{int(float64(r.X) / gs), int(float64(r.Y) / gs), int(float64(r.W) / gs), int(float64(r.H) / gs)}
		strokeRound(cv, focusRect.inset(1), 5, T.Accent)
		strokeRound(cv, focusRect.inset(2), 4, v.background())
	}
	if v.acc {
		v.ensureUIA()
		v.announce()
	}
	return nil
}

// canvasKey is the keyboard handling shared by accessible canvases without a
// page handler of their own (the sidebar, the footer, the drive strip):
// arrows move between items, Home/End jump, Space or Enter activates.
func (v *canvasView) canvasKey(key walk.Key, mods walk.Modifiers) bool {
	if mods != 0 || !v.keyboard {
		return false
	}
	switch key {
	case walk.KeyUp, walk.KeyLeft:
		v.moveKeyboard(-1)
	case walk.KeyDown, walk.KeyRight:
		v.moveKeyboard(1)
	case walk.KeyHome:
		v.keyHit = ""
		v.moveKeyboard(1)
	case walk.KeyEnd:
		v.keyHit = ""
		v.moveKeyboard(-1)
	case walk.KeySpace, walk.KeyReturn:
		if v.keyHit == "" && len(v.hits) > 0 {
			v.keyHit = v.hits[0].id
		}
		v.click(v.keyHit)
	default:
		return false
	}
	return true
}

// moveKeyboard keeps focus tied to an action ID across repaints.
func (v *canvasView) moveKeyboard(direction int) (rect, bool) {
	if len(v.hits) == 0 {
		return rect{}, false
	}
	i := -1
	for j, h := range v.hits {
		if h.id == v.keyHit {
			i = j
			break
		}
	}
	if i < 0 {
		if direction < 0 {
			i = len(v.hits) - 1
		} else {
			i = 0
		}
	} else {
		i = (i + direction + len(v.hits)) % len(v.hits)
	}
	v.keyHit = v.hits[i].id
	v.invalidate()
	return v.hits[i].r, true
}

// addHit registers a clickable area for the current paint.
func (v *canvasView) addHit(r rect, id string) { v.hits = append(v.hits, hit{r.px(), id}) }

func (v *canvasView) hitAt(x, y int) string {
	for i := len(v.hits) - 1; i >= 0; i-- {
		if v.hits[i].r.contains(x, y) {
			return v.hits[i].id
		}
	}
	return ""
}

func (v *canvasView) invalidate() {
	if v.cw != nil {
		_ = v.cw.Invalidate()
	}
}

// declare builds the declarative widget. min/max of 0 mean "unconstrained".
func (v *canvasView) declare(minW, minH, maxW, maxH, stretch int) decl.CustomWidget {
	style := uint32(0)
	if v.keyboard {
		style = win.WS_TABSTOP
	}
	name := v.accName
	if name == "" {
		name = v.keyName
	}
	return decl.CustomWidget{
		Style:               style,
		Accessibility:       decl.Accessibility{Name: name, Description: v.keyName},
		AssignTo:            &v.cw,
		InvalidatesOnResize: true,
		PaintMode:           decl.PaintBuffered,
		MinSize:             decl.Size{Width: minW, Height: minH},
		MaxSize:             decl.Size{Width: maxW, Height: maxH},
		StretchFactor:       stretch,
		Paint:               v.paint,
		OnMouseMove: func(x, y int, b walk.MouseButton) {
			v.mx, v.my, v.left = x, y, b&walk.LeftButton != 0
			id := v.hitAt(x, y)
			if v.focusHook && !v.tracking && (id != "" || v.onMove != nil) {
				event := win.TRACKMOUSEEVENT{DwFlags: win.TME_LEAVE, HwndTrack: v.cw.Handle()}
				event.CbSize = uint32(unsafe.Sizeof(event))
				if win.TrackMouseEvent(&event) {
					v.tracking = true
					walk.App().AddPreTranslateHandlerForHWND(event.HwndTrack, v)
				}
			}
			if id != v.hover {
				v.hover = id
				if id != "" {
					v.cw.SetCursor(walk.CursorHand())
				} else {
					v.cw.SetCursor(walk.CursorArrow())
				}
				v.invalidate()
			}
			if v.onMove != nil {
				v.onMove(x, y)
			}
		},
		OnMouseDown: func(x, y int, b walk.MouseButton) {
			v.mx, v.my, v.left = x, y, b == walk.LeftButton
			if !v.left {
				v.down = ""
				return
			}
			v.down = v.hitAt(x, y)
			v.hover = v.down
			if v.keyboard && v.cw != nil {
				_ = v.cw.SetFocus()
				if v.down != "" {
					v.keyHit = v.down
				}
			}
			v.invalidate()
		},
		OnMouseUp: func(x, y int, b walk.MouseButton) {
			v.mx, v.my, v.left = x, y, false
			id := v.hitAt(x, y)
			was := v.down
			v.down = ""
			v.invalidate()
			if id != "" && id == was && v.onClick != nil {
				v.onClick(id, b)
			}
		},
	}
}

// Walk has no mouse-leave event for a CustomWidget. Listen for the native
// notification only while the pointer is over this canvas.
func (v *canvasView) OnPreTranslate(msg *win.MSG) bool {
	if msg.Message == win.WM_MOUSELEAVE && v.cw != nil && msg.HWnd == v.cw.Handle() {
		v.stopTracking()
		v.mx, v.my, v.hover = -1, -1, ""
		if v.onMove != nil && !v.left {
			v.onMove(-1, -1)
		}
		v.invalidate()
	}
	return false
}

func (v *canvasView) stopTracking() {
	if v.tracking {
		walk.App().DeletePreTranslateHandlerForHWND(v.cw.Handle())
		v.tracking = false
	}
}

// attachWheel wires the wheel after creation (the declarative struct has no field).
func (v *canvasView) attachWheel() {
	if v.cw == nil {
		return
	}
	v.cw.MouseWheel().Attach(func(x, y int, b walk.MouseButton) {
		if v.onWheel != nil {
			v.onWheel(int(int16(uint32(b) >> 16)))
		}
	})
}

// ---- flat button ------------------------------------------------------------

// button is a small owner-drawn push/toggle button.
type button struct {
	v       *canvasView
	text    string
	glyph   string
	enabled bool
	active  bool // toggled on
	danger  bool
	accent  bool
	toggle  bool
	onClick func()
	width   int
	tip     string
	// glyphRight puts the glyph after the text (a drop-down chevron).
	glyphRight bool
}

func newButton(text, glyph string, width int, onClick func()) *button {
	b := &button{text: text, glyph: glyph, enabled: true, onClick: onClick, width: width}
	b.v = newCanvasView(b.draw)
	b.v.onClick = func(id string, _ walk.MouseButton) {
		if b.enabled && b.onClick != nil {
			b.onClick()
		}
	}
	return b
}

func (b *button) widget() decl.CustomWidget {
	minWidth := b.width
	if b.glyph != "" && !b.glyphRight {
		minWidth = min(b.width, 42)
	} else if b.glyphRight {
		minWidth = min(b.width, 74)
	}
	w := b.v.declare(minWidth, 34, b.width, 34, 0)
	w.Style = win.WS_TABSTOP
	w.Enabled = b.enabled
	w.Accessibility = decl.Accessibility{Name: b.accessibleName(), Description: b.tip, Role: decl.AccRolePushbutton, DefaultAction: "Press"}
	w.ToolTipText = b.tip
	if b.tip == "" {
		w.ToolTipText = b.text
	}
	mouseDown := w.OnMouseDown
	w.OnMouseDown = func(x, y int, btn walk.MouseButton) {
		if btn == walk.LeftButton {
			_ = b.v.cw.SetFocus()
		}
		mouseDown(x, y, btn)
	}
	w.OnKeyDown = func(key walk.Key) {
		if b.enabled && (key == walk.KeySpace || key == walk.KeyReturn) {
			b.v.down = "b"
			b.v.invalidate()
		}
	}
	w.OnKeyUp = func(key walk.Key) {
		pressed := b.v.down == "b"
		b.v.down = ""
		b.v.invalidate()
		if pressed && b.enabled && (key == walk.KeySpace || key == walk.KeyReturn) && b.onClick != nil {
			b.onClick()
		}
	}
	return w
}

func (b *button) set(enabled, active bool) {
	if b.enabled != enabled || b.active != active {
		b.enabled, b.active = enabled, active
		if !enabled {
			b.v.down, b.v.hover = "", ""
		}
		if b.v.cw != nil {
			b.v.cw.SetEnabled(enabled)
			if b.toggle {
				_ = b.v.cw.Accessibility().SetName(b.accessibleName())
			}
		}
		b.v.invalidate()
	}
}

func (b *button) setText(s string) {
	if b.text != s {
		b.text = s
		if b.v.cw != nil {
			_ = b.v.cw.Accessibility().SetName(b.accessibleName())
		}
		b.v.invalidate()
	}
}

func (b *button) accessibleName() string {
	name := b.text
	if name == "" {
		name = b.tip
	}
	if b.toggle {
		if b.active {
			return name + ", on"
		}
		return name + ", off"
	}
	return name
}

func (b *button) draw(cv *walk.Canvas, w, h int) {
	r := rect{1, 2, w - 2, h - 4}
	fill, stroke, ink := T.Card, T.Stroke, T.Text
	pressed, hovered := b.v.down == "b" && (!b.v.left || b.v.hover == "b"), b.v.hover == "b"
	switch {
	case !b.enabled:
		ink, fill = T.Faint, T.Bg
	case highContrast:
		if b.active || b.accent || pressed || hovered {
			fill, ink = T.Accent, T.AccentText
		}
		stroke = T.Text
	case b.danger:
		ink, stroke, fill = T.Bad, blend(T.Bad, T.Card, 0.4), blend(T.Bad, T.Card, 0.08)
		if pressed {
			fill = blend(T.Bad, T.Card, 0.34)
		} else if hovered {
			fill = blend(T.Bad, T.Card, 0.20)
		}
	case b.accent || b.active:
		fill, stroke = T.RowSel, T.Accent
		if pressed {
			fill = blend(T.Accent, T.RowSel, 0.28)
		} else if hovered {
			fill = blend(T.Accent, T.RowSel, 0.14)
		}
	case pressed:
		fill, stroke = T.CardSel, T.Accent
	case hovered:
		fill, stroke = T.CardHi, T.Faint
	}
	fillRound(cv, r, 5, fill)
	strokeRound(cv, r, 5, stroke)
	if b.enabled && b.toggle && b.active {
		fillRound(cv, rect{w/2 - 7, h - 6, 14, 2}, 1, ink)
	}
	if b.enabled && b.v.cw != nil && b.v.cw.Focused() {
		strokeRound(cv, r.inset(2), 4, ink)
	}
	if b.enabled {
		b.v.addHit(rect{0, 0, w, h}, "b")
	}
	if pressed {
		r.Y++ // a tactile cue also when Windows controls both colours
	}
	if b.glyphRight {
		drawText(cv, b.text, uiFont(10), ink, rect{8, r.Y, w - 30, r.H}, taLeft)
		drawText(cv, b.glyph, iconFont(8), ink, rect{w - 23, r.Y, 16, r.H}, taCenter)
		return
	}
	if b.glyph == "" {
		drawText(cv, b.text, uiFont(10), ink, r.inset(2), taCenter)
		return
	}
	if b.text == "" {
		drawText(cv, b.glyph, iconFont(11), ink, r, taCenter)
		return
	}
	labelWidth := textWidth(cv, b.text, uiFont(10))
	switch {
	case w < labelWidth+12:
		drawText(cv, b.glyph, iconFont(11), ink, r, taCenter)
	case w < labelWidth+36:
		drawText(cv, b.text, uiFont(10), ink, r, taCenter)
	default:
		x := (w - labelWidth - 22) / 2
		drawText(cv, b.glyph, iconFont(10), ink, rect{x, r.Y, 17, r.H}, taCenter)
		drawText(cv, b.text, uiFont(10), ink, rect{x + 22, r.Y, w - x - 25, r.H}, taLeft)
	}
}

// menuItem is one entry of a popup menu built at click time.
type menuItem struct {
	text    string
	checked bool
	do      func()
}

// popupMenu shows items as a menu just below widget w. Walk only exposes menus
// as context menus, so the menu is attached to w and opened with a synthetic
// WM_CONTEXTMENU at the widget's bottom-left corner.
func popupMenu(w walk.Widget, items []menuItem) {
	if w == nil {
		return
	}
	m, err := walk.NewMenu()
	if err != nil {
		return
	}
	for _, it := range items {
		a := walk.NewAction()
		_ = a.SetText(it.text)
		_ = a.SetChecked(it.checked)
		do := it.do
		a.Triggered().Attach(func() { do() })
		_ = m.Actions().Add(a)
	}
	w.SetContextMenu(m)
	var rc win.RECT
	win.GetWindowRect(win.HWND(w.Handle()), &rc)
	lp := uintptr(uint32(uint16(int16(rc.Left))) | uint32(uint16(int16(rc.Bottom)))<<16)
	win.SendMessage(win.HWND(w.Handle()), win.WM_CONTEXTMENU, uintptr(w.Handle()), lp)
	// TrackPopupMenu posts the chosen item's WM_COMMAND rather than sending it.
	// Disposing now would unregister the actions before that message arrives
	// (the click would do nothing), so dispose from a later posted message.
	// The app-level queue needs no form; the per-form one is equivalent when
	// the widget has one.
	if f := w.Form(); f != nil {
		f.Synchronize(func() {
			w.SetContextMenu(nil)
			m.Dispose()
		})
	} else {
		walk.App().Synchronize(func() {
			w.SetContextMenu(nil)
			m.Dispose()
		})
	}
}
