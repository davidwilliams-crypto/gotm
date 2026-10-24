//go:build windows

package ui

import (
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
		v.cw.FocusedChanged().Attach(v.invalidate)
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
	if v.keyboard && v.cw.Focused() && v.keyHit != "" {
		for _, h := range v.hits {
			if h.id == v.keyHit {
				r := h.r
				strokeRound(cv, rect{int(float64(r.X) / gs), int(float64(r.Y) / gs), int(float64(r.W) / gs), int(float64(r.H) / gs)}, 6, T.Accent)
				break
			}
		}
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
	w := b.v.declare(b.width, 34, b.width, 34, 0)
	w.Style = win.WS_TABSTOP
	w.Enabled = b.enabled
	name := b.text
	if name == "" {
		name = b.tip
	}
	w.Accessibility = decl.Accessibility{Name: name, Description: b.tip, Role: decl.AccRolePushbutton, DefaultAction: "Press"}
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
		if b.v.cw != nil {
			b.v.cw.SetEnabled(enabled)
		}
		b.v.invalidate()
	}
}

func (b *button) setText(s string) {
	if b.text != s {
		b.text = s
		if b.v.cw != nil {
			_ = b.v.cw.Accessibility().SetName(s)
		}
		b.v.invalidate()
	}
}

func (b *button) draw(cv *walk.Canvas, w, h int) {
	r := rect{0, 2, w, h - 4}
	fill, stroke, ink := T.Card, T.Stroke, T.Text
	switch {
	case !b.enabled:
		ink, fill = T.Faint, blend(T.Card, T.Bg, 0.55)
	case b.accent || b.active:
		fill, stroke = blend(T.Accent, T.Bg, 0.30), blend(T.Accent, T.Bg, 0.70)
		ink = T.Text
	case b.v.down == "b":
		fill = T.CardSel
	case b.v.hover == "b":
		fill = T.CardHi
	}
	if b.danger && b.enabled {
		ink = T.Bad
		if b.v.hover == "b" {
			fill = blend(T.Bad, T.Bg, 0.18)
		}
	}
	fillRound(cv, r, 6, fill)
	if b.enabled && b.v.cw != nil && b.v.cw.Focused() {
		stroke = T.Accent
	}
	strokeRound(cv, r, 6, stroke)
	x := 12
	if b.glyphRight {
		drawText(cv, b.text, uiFont(10), ink, rect{x, 0, w - x - 30, h}, taLeft)
		drawText(cv, b.glyph, iconFont(9), ink, rect{w - 28, 0, 18, h}, taCenter)
		if b.enabled {
			b.v.addHit(rect{0, 0, w, h}, "b")
		}
		return
	}
	if b.glyph != "" && b.text == "" {
		drawText(cv, b.glyph, iconFont(12), ink, rect{0, 0, w, h}, taCenter) // icon-only button
	} else if b.glyph != "" {
		drawText(cv, b.glyph, iconFont(11), ink, rect{x, 0, 20, h}, taLeft)
		x += 24
	}
	drawText(cv, b.text, uiFont(10), ink, rect{x, 0, w - x - 8, h}, taLeft)
	if b.enabled {
		b.v.addHit(rect{0, 0, w, h}, "b")
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
	if f := w.Form(); f != nil {
		f.Synchronize(func() {
			w.SetContextMenu(nil)
			m.Dispose()
		})
	}
}
