//go:build windows

package ui

// UI Automation for the owner-drawn surfaces.
//
// Screen readers see a canvasView as one opaque window: Windows knows nothing
// about the cards, chips and rows painted inside it. This file gives every
// accessible canvas a UI Automation provider (the modern accessibility API
// Narrator, NVDA and JAWS use) that exposes the items registered during the
// last paint as children with a name, a control type, a bounding rectangle,
// keyboard focus, and the Invoke / SelectionItem / Toggle patterns, so a
// screen reader can read "CPU: 12%, 4.30 GHz, selected" and activate items.
//
// The COM objects are plain Go structs whose first fields hold vtable
// pointers; Go never moves heap objects, and a registry keeps them alive while
// Windows holds references. ProviderOptions_UseComThreading makes UI
// Automation call back on the UI thread, so provider code touches view state
// without locks.

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

const (
	uiaRootObjectID = -25

	provServerSide   = 0x2
	provUseComThread = 0x20

	propControlType         = 30003
	propName                = 30005
	propHasKeyboardFocus    = 30008
	propIsKeyboardFocusable = 30009
	propIsEnabled           = 30010
	propAutomationID        = 30011
	propClassName           = 30012
	propHelpText            = 30013
	propIsControlElement    = 30016
	propIsContentElement    = 30017
	propIsOffscreen         = 30022
	propFrameworkID         = 30024
	propIsSelected          = 30079
	propToggleState         = 30086

	ctlButton      int32 = 50000
	ctlCheckBox    int32 = 50002
	ctlListItem    int32 = 50007
	ctlList        int32 = 50008
	ctlRadioButton int32 = 50013
	ctlSlider      int32 = 50015
	ctlStatusBar   int32 = 50017
	ctlText        int32 = 50020
	ctlPane        int32 = 50033

	patternInvoke        = 10000
	patternSelection     = 10001
	patternSelectionItem = 10010
	patternToggle        = 10015

	eventFocusChanged = 20005

	navParent = 0
	navNext   = 1
	navPrev   = 2
	navFirst  = 3
	navLast   = 4

	runtimeIDToken = 3 // UiaAppendRuntimeIdToken

	hrOK          = 0
	hrNoInterface = 0x80004002
	hrPointer     = 0x80004003
	hrFail        = 0x80004005

	vtI4      = 3
	vtBSTR    = 8
	vtBool    = 11
	vtUnknown = 13
)

var (
	iidUnknown       = windows.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidSimple        = windows.GUID{Data1: 0xd6dd68d1, Data2: 0x86fd, Data3: 0x4332, Data4: [8]byte{0x86, 0x66, 0x9a, 0xbe, 0xde, 0xa2, 0xd2, 0x4c}}
	iidFragment      = windows.GUID{Data1: 0xf7063da8, Data2: 0x8359, Data3: 0x439c, Data4: [8]byte{0x92, 0x97, 0xbb, 0xc5, 0x29, 0x9a, 0x7d, 0x87}}
	iidFragmentRoot  = windows.GUID{Data1: 0x620ce2a5, Data2: 0xab8f, Data3: 0x40a9, Data4: [8]byte{0x86, 0xcb, 0xde, 0x3c, 0x75, 0x59, 0x9b, 0x58}}
	iidInvoke        = windows.GUID{Data1: 0x54fcb24b, Data2: 0xe18e, Data3: 0x47a2, Data4: [8]byte{0xb4, 0xd3, 0xec, 0xcb, 0xe7, 0x75, 0x99, 0xa2}}
	iidSelectionItem = windows.GUID{Data1: 0x2acad808, Data2: 0xb2d4, Data3: 0x452d, Data4: [8]byte{0xa4, 0x07, 0x91, 0xff, 0x1a, 0xd1, 0x67, 0xb2}}
	iidToggle        = windows.GUID{Data1: 0x56d00bd0, Data2: 0xc4f4, Data3: 0x433c, Data4: [8]byte{0xa8, 0x36, 0x1a, 0x52, 0xa5, 0x7e, 0x08, 0x92}}
	iidSelection     = windows.GUID{Data1: 0xfb8b03af, Data2: 0x3bdf, Data3: 0x48d4, Data4: [8]byte{0xbd, 0x36, 0x1a, 0x65, 0x79, 0x3b, 0xe1, 0x68}}
)

var (
	uiaCore           = windows.NewLazySystemDLL("UIAutomationCore.dll")
	procUiaReturnRaw  = uiaCore.NewProc("UiaReturnRawElementProvider")
	procUiaHostFrom   = uiaCore.NewProc("UiaHostProviderFromHwnd")
	procUiaRaiseEvent = uiaCore.NewProc("UiaRaiseAutomationEvent")
	procUiaListening  = uiaCore.NewProc("UiaClientsAreListening")
	procUiaDisconnect = uiaCore.NewProc("UiaDisconnectProvider")
	oleaut32          = windows.NewLazySystemDLL("oleaut32.dll")
	procSACreate      = oleaut32.NewProc("SafeArrayCreateVector")
	procSAPut         = oleaut32.NewProc("SafeArrayPutElement")
	user32UIA         = windows.NewLazySystemDLL("user32.dll")
	procGetCursorPos  = user32UIA.NewProc("GetCursorPos")
)

// accItem is one element a canvas exposes: a clickable region (also a hit) or
// a piece of text worth reading.
type accItem struct {
	r        rect // device pixels, client coordinates
	id, name string
	ctl      int32
	selected bool // ListItem / RadioButton selected, CheckBox on
	action   bool // reacts to a click (Invoke, Select, Toggle)
}

// uiaElement is one COM object: the root (the canvas) or one item.
type uiaElement struct {
	// Interface slots: each holds its vtable's address, so the address of the
	// slot is a valid COM interface pointer for that interface.
	vSimple, vFragment, vRoot, vInvoke, vSel, vToggle, vSelection uintptr

	refs int32
	root *uiaRoot
	id   string  // "" for the root
	last accItem // what the item looked like when last seen on screen
}

// uiaRoot ties a canvas window to its provider tree.
type uiaRoot struct {
	view *canvasView
	hwnd win.HWND
	prev uintptr // the window procedure below ours (Walk's)
	elem *uiaElement
	kids map[string]*uiaElement
	ids  map[string]int32 // runtime ids, handed out in order of first sight
	gone bool             // WM_DESTROY seen: providers disconnected
}

var (
	uiaOnce    sync.Once
	uiaReady   bool
	uiaHookCB  uintptr
	vtSimple   [7]uintptr
	vtFragment [9]uintptr
	vtRoot     [5]uintptr
	vtInvoke   [4]uintptr
	vtSelItem  [8]uintptr
	vtToggle   [5]uintptr
	vtSelect   [6]uintptr

	uiaMu    sync.Mutex
	uiaSlots = map[uintptr]*uiaElement{} // interface pointer -> object
	uiaRoots = map[win.HWND]*uiaRoot{}
)

// uiaInit builds the vtables once (Go callbacks are a scarce resource).
func uiaInit() {
	for _, p := range []*windows.LazyProc{procUiaReturnRaw, procUiaHostFrom, procUiaRaiseEvent, procUiaListening, procSACreate, procSAPut, procGetCursorPos} {
		if p.Find() != nil {
			return
		}
	}
	qi, ar, rel := syscall.NewCallback(uiaQueryInterface), syscall.NewCallback(uiaAddRef), syscall.NewCallback(uiaRelease)
	vtSimple = [7]uintptr{qi, ar, rel,
		syscall.NewCallback(uiaProviderOptions), syscall.NewCallback(uiaGetPatternProvider),
		syscall.NewCallback(uiaGetPropertyValue), syscall.NewCallback(uiaHostProvider)}
	vtFragment = [9]uintptr{qi, ar, rel,
		syscall.NewCallback(uiaNavigate), syscall.NewCallback(uiaGetRuntimeID), syscall.NewCallback(uiaBoundingRect),
		syscall.NewCallback(uiaEmbeddedRoots), syscall.NewCallback(uiaSetFocus), syscall.NewCallback(uiaFragmentRoot)}
	vtRoot = [5]uintptr{qi, ar, rel, syscall.NewCallback(uiaFromPoint), syscall.NewCallback(uiaGetFocus)}
	vtInvoke = [4]uintptr{qi, ar, rel, syscall.NewCallback(uiaInvoke)}
	vtSelItem = [8]uintptr{qi, ar, rel,
		syscall.NewCallback(uiaInvoke), syscall.NewCallback(uiaInvoke), syscall.NewCallback(uiaNoop),
		syscall.NewCallback(uiaIsSelected), syscall.NewCallback(uiaSelectionContainer)}
	vtToggle = [5]uintptr{qi, ar, rel, syscall.NewCallback(uiaInvoke), syscall.NewCallback(uiaToggleState)}
	vtSelect = [6]uintptr{qi, ar, rel, syscall.NewCallback(uiaGetSelection),
		syscall.NewCallback(uiaFalse), syscall.NewCallback(uiaFalse)}
	uiaHookCB = syscall.NewCallback(uiaHookProc)
	uiaReady = true
}

func slotAddr(p *uintptr) uintptr { return uintptr(unsafe.Pointer(p)) }

func newElement(root *uiaRoot, id string) *uiaElement {
	e := &uiaElement{root: root, id: id, refs: 1}
	e.vSimple = uintptr(unsafe.Pointer(&vtSimple[0]))
	e.vFragment = uintptr(unsafe.Pointer(&vtFragment[0]))
	e.vRoot = uintptr(unsafe.Pointer(&vtRoot[0]))
	e.vInvoke = uintptr(unsafe.Pointer(&vtInvoke[0]))
	e.vSel = uintptr(unsafe.Pointer(&vtSelItem[0]))
	e.vToggle = uintptr(unsafe.Pointer(&vtToggle[0]))
	e.vSelection = uintptr(unsafe.Pointer(&vtSelect[0]))
	uiaMu.Lock()
	for _, s := range e.slots() {
		uiaSlots[s] = e
	}
	uiaMu.Unlock()
	return e
}

func (e *uiaElement) slots() []uintptr {
	return []uintptr{slotAddr(&e.vSimple), slotAddr(&e.vFragment), slotAddr(&e.vRoot),
		slotAddr(&e.vInvoke), slotAddr(&e.vSel), slotAddr(&e.vToggle), slotAddr(&e.vSelection)}
}

// iface returns an AddRef'd interface pointer for one of e's slots.
func (e *uiaElement) iface(slot *uintptr) uintptr {
	atomic.AddInt32(&e.refs, 1)
	return slotAddr(slot)
}

func (e *uiaElement) release() int32 {
	n := atomic.AddInt32(&e.refs, -1)
	if n <= 0 {
		uiaMu.Lock()
		for _, s := range e.slots() {
			delete(uiaSlots, s)
		}
		uiaMu.Unlock()
	}
	return n
}

func elemOf(this uintptr) *uiaElement {
	uiaMu.Lock()
	defer uiaMu.Unlock()
	return uiaSlots[this]
}

// current returns the item as painted last, and whether it is on screen now.
func (e *uiaElement) current() (accItem, bool) {
	if e.id == "" {
		return accItem{}, true
	}
	if it, _, ok := e.root.item(e.id); ok {
		e.last = it
		return it, true
	}
	return e.last, false
}

func (e *uiaElement) hasFocus() bool {
	v := e.root.view
	if v.cw == nil || !v.cw.Focused() {
		return false
	}
	_, _, hit := e.root.item(v.keyHit)
	if e.id == "" {
		return !hit
	}
	return hit && v.keyHit == e.id
}

func (r *uiaRoot) item(id string) (accItem, int, bool) {
	if id == "" {
		return accItem{}, -1, false
	}
	for i, it := range r.view.items {
		if it.id == id {
			return it, i, true
		}
	}
	return accItem{}, -1, false
}

// child returns the element for an item, creating and caching it on first use.
func (r *uiaRoot) child(it accItem) *uiaElement {
	if e, ok := r.kids[it.id]; ok {
		e.last = it
		return e
	}
	if len(r.kids) > 512 { // forget items that left the screen (old process rows)
		for id, e := range r.kids {
			if _, _, ok := r.item(id); !ok {
				delete(r.kids, id)
				e.release()
			}
		}
	}
	e := newElement(r, it.id)
	e.last = it
	r.kids[it.id] = e
	return e
}

// disconnect tells UI Automation to drop every element of this canvas. The
// hook entry stays until WM_NCDESTROY so messages keep reaching Walk.
func (r *uiaRoot) disconnect() {
	if procUiaDisconnect.Find() == nil {
		procUiaDisconnect.Call(slotAddr(&r.elem.vSimple))
		for _, e := range r.kids {
			procUiaDisconnect.Call(slotAddr(&e.vSimple))
		}
	}
}

// release drops the provider's own references once the window is gone, so a
// canvas that is rebuilt later starts clean and nothing stays registered.
func (r *uiaRoot) release() {
	for id, e := range r.kids {
		delete(r.kids, id)
		e.release()
	}
	r.elem.release()
	if r.view.uia == r {
		r.view.uia = nil
	}
}

// runtimeID numbers items in order of first sight: unique per canvas, stable
// for the life of the window, and never a hash collision.
func (r *uiaRoot) runtimeID(id string) int32 {
	if n, ok := r.ids[id]; ok {
		return n
	}
	n := int32(len(r.ids) + 1)
	r.ids[id] = n
	return n
}

// ---- canvasView side ---------------------------------------------------------------

// ensureUIA installs the provider on the canvas window (once it exists).
func (v *canvasView) ensureUIA() {
	if v.uia != nil || v.cw == nil {
		return
	}
	uiaOnce.Do(uiaInit)
	if !uiaReady {
		return
	}
	h := win.HWND(v.cw.Handle())
	root := &uiaRoot{view: v, hwnd: h, kids: map[string]*uiaElement{}, ids: map[string]int32{}}
	root.elem = newElement(root, "")
	uiaMu.Lock()
	uiaRoots[h] = root
	uiaMu.Unlock()
	root.prev = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, uiaHookCB)
	v.uia = root
}

// announce raises a focus event when the keyboard focus inside the canvas
// moved to another item, so a screen reader reads the new item.
func (v *canvasView) announce() {
	if v.uia == nil {
		return
	}
	if v.cw == nil || !v.cw.Focused() {
		v.announced = ""
		return
	}
	id := v.keyHit
	if _, _, ok := v.uia.item(id); !ok {
		id = ""
	}
	if id == v.announced {
		return
	}
	v.announced = id
	if id == "" || !uiaListening() {
		return
	}
	it, _, _ := v.uia.item(id)
	e := v.uia.child(it)
	procUiaRaiseEvent.Call(slotAddr(&e.vSimple), eventFocusChanged)
}

func uiaListening() bool {
	if !uiaReady {
		return false
	}
	r, _, _ := procUiaListening.Call()
	return r != 0
}

// canvasAt returns the accessible canvas owning a window handle, if any.
func canvasAt(h win.HWND) *canvasView {
	uiaMu.Lock()
	defer uiaMu.Unlock()
	if r := uiaRoots[h]; r != nil {
		return r.view
	}
	return nil
}

// addItem registers a clickable item for both the mouse and assistive technology.
func (v *canvasView) addItem(r rect, id, name string, ctl int32, selected bool) {
	v.addHit(r, id)
	v.items = append(v.items, accItem{r: r.px(), id: id, name: name, ctl: ctl, selected: selected, action: true})
}

// addText registers read-only text (a value, a caption) for assistive technology.
func (v *canvasView) addText(r rect, name string) {
	if name == "" {
		return
	}
	id := "t:" + itoa(len(v.items))
	v.items = append(v.items, accItem{r: r.px(), id: id, name: name, ctl: ctlText})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// click activates an item the way a mouse click would.
func (v *canvasView) click(id string) {
	if id == "" {
		return
	}
	v.keyHit = id
	if v.onClick != nil {
		v.onClick(id, walk.LeftButton)
	}
	v.invalidate()
}

// ---- window procedure hook -------------------------------------------------------

func uiaHookProc(hwnd, msg, wp, lp uintptr) (ret uintptr) {
	defer uiaGuard(&ret, 0)
	h := win.HWND(hwnd)
	uiaMu.Lock()
	root := uiaRoots[h]
	uiaMu.Unlock()
	if root == nil {
		return win.DefWindowProc(h, uint32(msg), wp, lp)
	}
	switch msg {
	case win.WM_GETOBJECT:
		if int32(uint32(lp)) == uiaRootObjectID && !root.gone {
			r, _, _ := procUiaReturnRaw.Call(hwnd, wp, lp, slotAddr(&root.elem.vSimple))
			return r
		}
	case win.WM_DESTROY:
		// Tell UI Automation the window is going (it releases its references),
		// but keep chaining: Walk still needs WM_DESTROY and WM_NCDESTROY.
		if !root.gone {
			root.gone = true
			procUiaReturnRaw.Call(hwnd, 0, 0, 0)
			root.disconnect()
		}
	case win.WM_NCDESTROY:
		r := win.CallWindowProc(root.prev, h, uint32(msg), wp, lp)
		uiaMu.Lock()
		delete(uiaRoots, h)
		uiaMu.Unlock()
		root.release()
		return r
	}
	return win.CallWindowProc(root.prev, h, uint32(msg), wp, lp)
}

// uiaGuard keeps a panic in provider code from crossing into Windows, which
// would end the process: the call fails with the given result instead.
func uiaGuard(ret *uintptr, fail uintptr) {
	if e := recover(); e != nil {
		debugf("uia: recovered %v", e)
		*ret = fail
	}
}

// ---- IUnknown ------------------------------------------------------------------------

func uiaQueryInterface(this, riid, ppv uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if ppv == 0 {
		return hrPointer
	}
	out := (*uintptr)(ptrFrom(ppv))
	*out = 0
	e := elemOf(this)
	if e == nil || riid == 0 {
		return hrNoInterface
	}
	iid := *(*windows.GUID)(ptrFrom(riid))
	var slot *uintptr
	switch iid {
	case iidUnknown, iidSimple:
		slot = &e.vSimple
	case iidFragment:
		slot = &e.vFragment
	case iidFragmentRoot:
		if e.id == "" {
			slot = &e.vRoot
		}
	case iidInvoke:
		if e.id != "" {
			slot = &e.vInvoke
		}
	case iidSelectionItem:
		if e.id != "" {
			slot = &e.vSel
		}
	case iidToggle:
		if e.id != "" {
			slot = &e.vToggle
		}
	case iidSelection:
		if e.id == "" && e.root.view.accCtl == ctlList {
			slot = &e.vSelection
		}
	}
	if slot == nil {
		return hrNoInterface
	}
	*out = e.iface(slot)
	return hrOK
}

func uiaAddRef(this uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	e := elemOf(this)
	if e == nil {
		return 1
	}
	return uintptr(atomic.AddInt32(&e.refs, 1))
}

func uiaRelease(this uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	e := elemOf(this)
	if e == nil {
		return 0
	}
	n := e.release()
	if n < 0 {
		n = 0
	}
	return uintptr(n)
}

// ---- IRawElementProviderSimple ------------------------------------------------------

func uiaProviderOptions(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*int32)(ptrFrom(out)) = provServerSide | provUseComThread
	return hrOK
}

func uiaGetPatternProvider(this, pattern, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil {
		return hrOK
	}
	if e.id == "" {
		if pattern == patternSelection && e.root.view.accCtl == ctlList {
			*(*uintptr)(ptrFrom(out)) = e.iface(&e.vSelection)
		}
		return hrOK
	}
	it, _ := e.current()
	if !it.action {
		return hrOK
	}
	switch pattern {
	case patternInvoke:
		if it.ctl != ctlCheckBox {
			*(*uintptr)(ptrFrom(out)) = e.iface(&e.vInvoke)
		}
	case patternSelectionItem:
		if it.ctl == ctlListItem || it.ctl == ctlRadioButton {
			*(*uintptr)(ptrFrom(out)) = e.iface(&e.vSel)
		}
	case patternToggle:
		if it.ctl == ctlCheckBox {
			*(*uintptr)(ptrFrom(out)) = e.iface(&e.vToggle)
		}
	}
	return hrOK
}

type variant struct {
	vt  uint16
	_   [3]uint16
	val uint64
	_   uint64
}

func setBSTR(v *variant, s string) {
	v.vt = vtBSTR
	*(*uintptr)(unsafe.Pointer(&v.val)) = uintptr(unsafe.Pointer(win.SysAllocString(s)))
}

func setI4(v *variant, n int32) {
	v.vt = vtI4
	*(*int32)(unsafe.Pointer(&v.val)) = n
}

func setBool(v *variant, b bool) {
	v.vt = vtBool
	var x int16
	if b {
		x = -1 // VARIANT_TRUE
	}
	*(*int16)(unsafe.Pointer(&v.val)) = x
}

func uiaGetPropertyValue(this, prop, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	e := elemOf(this)
	if e == nil {
		return hrOK // VT_EMPTY: let UI Automation use its default
	}
	v := (*variant)(ptrFrom(out))
	view := e.root.view
	it, present := e.current()
	root := e.id == ""
	switch prop {
	case propName:
		if root {
			name := view.accName
			if name == "" {
				name = view.keyName
			}
			setBSTR(v, name)
		} else {
			setBSTR(v, it.name)
		}
	case propControlType:
		if root {
			ctl := view.accCtl
			if ctl == 0 {
				ctl = ctlPane
			}
			setI4(v, ctl)
		} else {
			setI4(v, it.ctl)
		}
	case propIsControlElement, propIsContentElement, propIsEnabled:
		setBool(v, true)
	case propIsKeyboardFocusable:
		setBool(v, view.keyboard && (root || it.action))
	case propHasKeyboardFocus:
		setBool(v, e.hasFocus())
	case propAutomationID:
		if root {
			setBSTR(v, "canvas:"+view.accName)
		} else {
			setBSTR(v, e.id)
		}
	case propHelpText:
		if root {
			setBSTR(v, view.keyName)
		}
	case propFrameworkID:
		setBSTR(v, "gotm")
	case propClassName:
		if root {
			setBSTR(v, "gotmCanvas")
		} else {
			setBSTR(v, "gotmItem")
		}
	case propIsOffscreen:
		setBool(v, !root && !present)
	case propIsSelected:
		if !root && (it.ctl == ctlListItem || it.ctl == ctlRadioButton) {
			setBool(v, it.selected)
		}
	case propToggleState:
		if !root && it.ctl == ctlCheckBox {
			setI4(v, boolToI4(it.selected))
		}
	}
	return hrOK
}

func boolToI4(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func uiaHostProvider(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil || e.id != "" {
		return hrOK
	}
	r, _, _ := procUiaHostFrom.Call(uintptr(e.root.hwnd), out)
	return r
}

// ---- IRawElementProviderFragment ---------------------------------------------------

func uiaNavigate(this, dir, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil {
		return hrOK
	}
	items := e.root.view.items
	var target *uiaElement
	if e.id == "" {
		switch dir {
		case navFirst:
			if len(items) > 0 {
				target = e.root.child(items[0])
			}
		case navLast:
			if len(items) > 0 {
				target = e.root.child(items[len(items)-1])
			}
		}
	} else {
		_, idx, ok := e.root.item(e.id)
		switch dir {
		case navParent:
			target = e.root.elem
		case navNext:
			if ok && idx+1 < len(items) {
				target = e.root.child(items[idx+1])
			}
		case navPrev:
			if ok && idx > 0 {
				target = e.root.child(items[idx-1])
			}
		}
	}
	if target != nil {
		*(*uintptr)(ptrFrom(out)) = target.iface(&target.vFragment)
	}
	return hrOK
}

func uiaGetRuntimeID(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil || e.id == "" {
		return hrOK // the host window supplies the root's id
	}
	psa, _, _ := procSACreate.Call(vtI4, 0, 2)
	if psa == 0 {
		return hrOK
	}
	vals := [2]int32{runtimeIDToken, e.root.runtimeID(e.id)}
	for i := int32(0); i < 2; i++ {
		idx := i
		procSAPut.Call(psa, uintptr(unsafe.Pointer(&idx)), uintptr(unsafe.Pointer(&vals[i])))
	}
	*(*uintptr)(ptrFrom(out)) = psa
	return hrOK
}

func uiaBoundingRect(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	rc := (*[4]float64)(ptrFrom(out))
	*rc = [4]float64{}
	e := elemOf(this)
	if e == nil || e.id == "" {
		return hrOK // the host window supplies the root's rectangle
	}
	it, present := e.current()
	if !present {
		return hrOK
	}
	pt := win.POINT{X: int32(it.r.X), Y: int32(it.r.Y)}
	win.ClientToScreen(e.root.hwnd, &pt)
	*rc = [4]float64{float64(pt.X), float64(pt.Y), float64(it.r.W), float64(it.r.H)}
	return hrOK
}

func uiaEmbeddedRoots(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	return hrOK
}

func uiaSetFocus(this uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	e := elemOf(this)
	if e == nil {
		return hrOK
	}
	v := e.root.view
	if e.id != "" {
		v.keyHit = e.id
	}
	if v.cw != nil {
		_ = v.cw.SetFocus()
	}
	v.invalidate()
	return hrOK
}

func uiaFragmentRoot(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	if e := elemOf(this); e != nil {
		*(*uintptr)(ptrFrom(out)) = e.root.elem.iface(&e.root.elem.vRoot)
	}
	return hrOK
}

// ---- IRawElementProviderFragmentRoot ------------------------------------------------

// uiaFromPoint receives its x and y in floating-point registers, which Go
// callbacks cannot read (on x64 the out pointer still lands in the fourth
// integer slot; the build is amd64-only); the query is almost always about
// the pointer, so the cursor position stands in.
func uiaFromPoint(this, _, _, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil {
		return hrOK
	}
	var pt win.POINT
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
		return hrOK
	}
	win.ScreenToClient(e.root.hwnd, &pt)
	items := e.root.view.items
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].r.contains(int(pt.X), int(pt.Y)) {
			c := e.root.child(items[i])
			*(*uintptr)(ptrFrom(out)) = c.iface(&c.vFragment)
			break
		}
	}
	return hrOK
}

func uiaGetFocus(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil {
		return hrOK
	}
	v := e.root.view
	if v.cw == nil || !v.cw.Focused() {
		return hrOK
	}
	if it, _, ok := e.root.item(v.keyHit); ok {
		c := e.root.child(it)
		*(*uintptr)(ptrFrom(out)) = c.iface(&c.vFragment)
	}
	return hrOK
}

// ---- patterns ------------------------------------------------------------------------

// uiaInvoke serves Invoke, Select, AddToSelection and Toggle: all activate the item.
func uiaInvoke(this uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	e := elemOf(this)
	if e == nil || e.id == "" {
		return hrOK
	}
	if it, _, ok := e.root.item(e.id); ok && it.action {
		e.root.view.click(e.id)
	}
	return hrOK
}

func uiaNoop(this uintptr) uintptr { return hrOK }

func uiaIsSelected(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*int32)(ptrFrom(out)) = 0
	if e := elemOf(this); e != nil {
		it, _ := e.current()
		*(*int32)(ptrFrom(out)) = boolToI4(it.selected)
	}
	return hrOK
}

func uiaSelectionContainer(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	if e := elemOf(this); e != nil {
		*(*uintptr)(ptrFrom(out)) = e.root.elem.iface(&e.root.elem.vSimple)
	}
	return hrOK
}

// uiaGetSelection lists the selected items of a list root (ISelectionProvider),
// which lets a screen reader say "Processes, 2 of 11".
func uiaGetSelection(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*uintptr)(ptrFrom(out)) = 0
	e := elemOf(this)
	if e == nil || e.id != "" {
		return hrOK
	}
	var sel []accItem
	for _, it := range e.root.view.items {
		if it.selected && (it.ctl == ctlListItem || it.ctl == ctlRadioButton) {
			sel = append(sel, it)
		}
	}
	psa, _, _ := procSACreate.Call(vtUnknown, 0, uintptr(len(sel)))
	if psa == 0 {
		return hrFail
	}
	for i, it := range sel {
		c := e.root.child(it)
		idx := int32(i)
		// SafeArrayPutElement takes its own reference on an IUnknown element
		procSAPut.Call(psa, uintptr(unsafe.Pointer(&idx)), slotAddr(&c.vSimple))
	}
	*(*uintptr)(ptrFrom(out)) = psa
	return hrOK
}

// uiaFalse answers CanSelectMultiple and IsSelectionRequired.
func uiaFalse(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*int32)(ptrFrom(out)) = 0
	return hrOK
}

func uiaToggleState(this, out uintptr) (ret uintptr) {
	defer uiaGuard(&ret, hrFail)
	if out == 0 {
		return hrPointer
	}
	*(*int32)(ptrFrom(out)) = 0
	if e := elemOf(this); e != nil {
		it, _ := e.current()
		*(*int32)(ptrFrom(out)) = boolToI4(it.selected)
	}
	return hrOK
}
