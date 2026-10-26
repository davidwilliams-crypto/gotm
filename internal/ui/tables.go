//go:build windows

package ui

import (
	"sync"
	"syscall"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

const (
	lvmGetHeader = 0x101F // LVM_GETHEADER
	classBufLen  = 64
)

func colorref(c walk.Color) uintptr {
	return uintptr(uint32(c.R()) | uint32(c.G())<<8 | uint32(c.B())<<16)
}

// listViews returns the native SysListView32 children of a Walk TableView.
// They never change for a table's lifetime, so the answer is cached.
func listViews(tv *walk.TableView) []win.HWND {
	if hs, ok := lvCache[tv]; ok && allWindows(hs) {
		return hs
	}
	hs := childrenOfClass(win.HWND(tv.Handle()), "SysListView32")
	if len(hs) > 0 {
		lvCache[tv] = hs
	}
	return hs
}

var (
	lvCache      = map[*walk.TableView][]win.HWND{}
	procIsWindow = windows.NewLazySystemDLL("user32.dll").NewProc("IsWindow")
)

func allWindows(hs []win.HWND) bool {
	for _, h := range hs {
		if r, _, _ := procIsWindow.Call(uintptr(h)); r == 0 {
			return false
		}
	}
	return len(hs) > 0
}

// Go can create only about 2000 Windows callbacks per process and never frees
// them, so the enumeration callback is made once and reports through these
// package variables (only ever used on the UI thread).
var (
	enumOnce  sync.Once
	enumCB    uintptr
	enumClass string
	enumOut   []win.HWND
)

// childrenOfClass lists the descendants of parent whose window class is class.
func childrenOfClass(parent win.HWND, class string) []win.HWND {
	enumOnce.Do(func() {
		enumCB = syscall.NewCallback(func(child win.HWND, _ uintptr) uintptr {
			var buf [classBufLen]uint16
			n, _ := win.GetClassName(child, &buf[0], classBufLen)
			if n > 0 && syscall.UTF16ToString(buf[:n]) == enumClass {
				enumOut = append(enumOut, child)
			}
			return 1
		})
	})
	enumClass, enumOut = class, nil
	win.EnumChildWindows(parent, enumCB, 0)
	out := enumOut
	enumOut = nil
	return out
}

// styledTables lists every table gotm styled, so a palette change can recolour them.
var styledTables []*walk.TableView

// styleTable gives a Walk TableView gotm's dark look and turns on native
// double buffering, which is what stops rows blinking when values refresh.
func styleTable(tv *walk.TableView) {
	tv.SetFont(uiFont(10))
	for _, lv := range listViews(tv) {
		allowDark(lv)
		darkControl(lv, "DarkMode_Explorer")
		if hdr := win.HWND(win.SendMessage(lv, lvmGetHeader, 0, 0)); hdr != 0 {
			allowDark(hdr)
			darkControl(hdr, "DarkMode_ItemsView")
		}
		win.SendMessage(lv, win.LVM_SETEXTENDEDLISTVIEWSTYLE, win.LVS_EX_DOUBLEBUFFER, win.LVS_EX_DOUBLEBUFFER)
	}
	colorTable(tv)
	styledTables = append(styledTables, tv)
	tv.SetGridlines(false)
	tv.SetAlternatingRowBG(false)
	fillLastColumn(tv)
	tv.SizeChanged().Attach(func() { fillLastColumn(tv) })
}

// nameTable gives the native list views inside tv an accessible name, so a
// screen reader says "Processes table" rather than just "list".
func nameTable(tv *walk.TableView, name string) {
	var svc *win.IAccPropServices
	hr := win.CoCreateInstance(&win.CLSID_AccPropServices, nil, win.CLSCTX_INPROC_SERVER,
		&win.IID_IAccPropServices, (*unsafe.Pointer)(unsafe.Pointer(&svc)))
	if win.FAILED(hr) || svc == nil {
		return
	}
	defer svc.Release()
	for _, lv := range listViews(tv) {
		svc.SetHwndPropStr(lv, win.OBJID_CLIENT, win.CHILDID_SELF, &win.PROPID_ACC_NAME, name)
	}
}

// colorTable applies the palette's table colours (also after a theme change).
func colorTable(tv *walk.TableView) {
	for _, lv := range listViews(tv) {
		if hdr := win.HWND(win.SendMessage(lv, lvmGetHeader, 0, 0)); hdr != 0 {
			colorHeaderText(lv, hdr, T.Muted)
		}
		win.SendMessage(lv, win.LVM_SETBKCOLOR, 0, colorref(T.TableBG))
		win.SendMessage(lv, win.LVM_SETTEXTBKCOLOR, 0, colorref(T.TableBG))
		win.SendMessage(lv, win.LVM_SETTEXTCOLOR, 0, colorref(T.Text))
	}
}

func recolorTables() {
	for _, tv := range styledTables {
		colorTable(tv)
	}
}

// ---- tree indentation ---------------------------------------------------------

// Walk fills each virtual list-view item in LVN_GETDISPINFO but never its
// indent. The hook below sits above Walk's table procedure (the list views
// notify their parent, which is the TableView window) and adds the indent
// after Walk has filled the text and the icon, so in tree view the icon and
// the name move together, the way the native control does it.
var (
	tvHookOnce sync.Once
	tvHookCB   uintptr
	tvPrev     = map[win.HWND]uintptr{}
	tvIndent   = map[win.HWND]func(row int) int{}
)

// indentRows makes the first column of tv indent each row by depth(row) icon widths.
func indentRows(tv *walk.TableView, depth func(row int) int) {
	tvHookOnce.Do(func() { tvHookCB = syscall.NewCallback(tvHookProc) })
	h := win.HWND(tv.Handle())
	tvIndent[h] = depth
	if _, done := tvPrev[h]; done {
		return
	}
	tvPrev[h] = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, tvHookCB)
}

func tvHookProc(hwnd, msg, wp, lp uintptr) (ret uintptr) {
	defer uiaGuard(&ret, 0)
	h := win.HWND(hwnd)
	r := win.CallWindowProc(tvPrev[h], h, uint32(msg), wp, lp)
	if msg == win.WM_NCDESTROY {
		delete(tvPrev, h)
		delete(tvIndent, h)
		return r
	}
	if msg == win.WM_NOTIFY && lp != 0 {
		nmh := (*win.NMHDR)(ptrFrom(lp))
		if nmh.Code == win.LVN_GETDISPINFO {
			di := (*win.NMLVDISPINFO)(ptrFrom(lp))
			if di.Item.ISubItem == 0 {
				if f := tvIndent[h]; f != nil {
					di.Item.IIndent = int32(f(int(di.Item.IItem)))
					di.Item.Mask |= win.LVIF_INDENT
				}
			}
		}
	}
	return r
}

// flexBase remembers, per list view, which column absorbs spare width and how
// wide it was designed to be.
type flexInfo struct {
	col  int
	base int32
}

var flexOf = map[win.HWND]*flexInfo{}

// fillLastColumn hands the table's spare width to its widest text column (the
// one whose content benefits most), so a wide window shows no empty band on
// the right and right-aligned numbers stay next to their neighbours.
func fillLastColumn(tv *walk.TableView) {
	for _, lv := range listViews(tv) {
		if !win.IsWindowVisible(lv) {
			continue
		}
		hdr := win.HWND(win.SendMessage(lv, lvmGetHeader, 0, 0))
		n := int(win.SendMessage(hdr, win.HDM_GETITEMCOUNT, 0, 0))
		if n <= 0 {
			continue
		}
		widths := make([]int32, n)
		for i := range widths {
			widths[i] = int32(win.SendMessage(lv, win.LVM_GETCOLUMNWIDTH, uintptr(i), 0))
		}
		fi := flexOf[lv]
		if fi == nil || fi.col >= n {
			fi = &flexInfo{col: 0}
			for i, w := range widths {
				if w > widths[fi.col] {
					fi.col = i
				}
			}
			fi.base = widths[fi.col]
			flexOf[lv] = fi
		}
		var rc win.RECT
		win.GetClientRect(lv, &rc)
		var others int32
		for i, w := range widths {
			if i != fi.col {
				others += w
			}
		}
		want := rc.Right - rc.Left - others - 4
		if want < fi.base {
			want = fi.base
		}
		if want != widths[fi.col] {
			win.SendMessage(lv, win.LVM_SETCOLUMNWIDTH, uintptr(fi.col), uintptr(want))
		}
	}
}

// redrawVisible repaints just the rows currently on screen, without erasing
// the control: the cheap path for "values changed, order did not".
func redrawVisible(tv *walk.TableView) {
	for _, lv := range listViews(tv) {
		first := win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0)
		per := win.SendMessage(lv, win.LVM_GETCOUNTPERPAGE, 0, 0)
		win.SendMessage(lv, win.LVM_REDRAWITEMS, first, first+per+1)
	}
}

// rowStyle returns the colours for a row in a gotm table.
func rowStyle(row, selected int) (bg, fg walk.Color) {
	switch {
	case row == selected:
		return T.RowSel, T.RowSelText
	case row%2 == 1:
		return T.RowAlt, T.Text
	}
	return T.TableBG, T.Text
}

// Walk's SetCurrentIndex synchronizes its current row, native selection and
// notifications, but unconditionally ensures the row is visible. Suppress
// only that message while restoring selection, including notifications that
// synchronize the frozen and normal halves. All calls run on the UI thread.
var (
	quietSelectOnce sync.Once
	quietSelectCB   uintptr
	quietSelectPrev = map[win.HWND]uintptr{}
)

func quietSelectProc(hwnd, msg, wp, lp uintptr) (ret uintptr) {
	defer uiaGuard(&ret, 0)
	if msg == win.LVM_ENSUREVISIBLE {
		return win.TRUE
	}
	return win.CallWindowProc(quietSelectPrev[win.HWND(hwnd)], win.HWND(hwnd), uint32(msg), wp, lp)
}

// selectQuietly uses the toolkit API so both Walk and the native controls
// agree on selection, without moving the reader's viewport.
func selectQuietly(tv *walk.TableView, i int) {
	quietSelectOnce.Do(func() { quietSelectCB = syscall.NewCallback(quietSelectProc) })
	var hooked []win.HWND
	for _, lv := range listViews(tv) {
		if _, alreadyHooked := quietSelectPrev[lv]; alreadyHooked {
			continue
		}
		if prev := win.SetWindowLongPtr(lv, win.GWLP_WNDPROC, quietSelectCB); prev != 0 {
			quietSelectPrev[lv] = prev
			hooked = append(hooked, lv)
		}
	}
	defer func() {
		for _, lv := range hooked {
			win.SetWindowLongPtr(lv, win.GWLP_WNDPROC, quietSelectPrev[lv])
			delete(quietSelectPrev, lv)
		}
	}()
	_ = tv.SetCurrentIndex(i)
}
