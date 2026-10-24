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

// Windows has no documented switch for dark menus or dark list-view headers.
// The uxtheme exports below (by ordinal, Windows 10 1903+) are what Explorer,
// Notepad++ and others use; gotm looks them up defensively and does nothing
// when they are missing.
var (
	darkOnce            sync.Once
	pAllowDarkForWindow uintptr // ordinal 133: bool AllowDarkModeForWindow(HWND, bool)
	pSetPreferredMode   uintptr // ordinal 135: PreferredAppMode SetPreferredAppMode(mode)
	pFlushMenuThemes    uintptr // ordinal 136: void FlushMenuThemes()

	gdi32            = windows.NewLazySystemDLL("gdi32.dll")
	procSetTextColor = gdi32.NewProc("SetTextColor")
)

const preferForceDark = 2

// enableDarkMode opts the process into dark context menus and dark common
// controls. Call before the first window is created.
func enableDarkMode() {
	darkOnce.Do(func() {
		if windows.RtlGetVersion().BuildNumber < 18362 || highContrast {
			return // high-contrast themes keep the system look for menus and headers
		}
		h, err := windows.LoadLibraryEx("uxtheme.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
		if err != nil {
			return
		}
		pAllowDarkForWindow, _ = windows.GetProcAddressByOrdinal(h, 133)
		pSetPreferredMode, _ = windows.GetProcAddressByOrdinal(h, 135)
		pFlushMenuThemes, _ = windows.GetProcAddressByOrdinal(h, 136)
		if pSetPreferredMode != 0 {
			syscall.SyscallN(pSetPreferredMode, preferForceDark)
		}
		if pFlushMenuThemes != 0 {
			syscall.SyscallN(pFlushMenuThemes)
		}
	})
}

// allowDark lets one window (and its scroll bars, header, tooltips) use the
// dark variants of its visual style.
func allowDark(hwnd win.HWND) {
	if pAllowDarkForWindow != 0 && hwnd != 0 {
		syscall.SyscallN(pAllowDarkForWindow, uintptr(hwnd), 1)
	}
}

// ---- list-view header text colour ----------------------------------------------

// Walk replaces each list view's window procedure and forwards header
// custom-draw notifications away from comctl32's subclass chain, so the hook
// has to sit above Walk's procedure: it is installed with SetWindowLongPtr
// after Walk, and passes everything it does not handle down to Walk.
var (
	lvHookOnce sync.Once
	lvHookCB   uintptr
	lvPrev     = map[win.HWND]uintptr{}    // list view -> the procedure below ours
	headerInk  = map[win.HWND]walk.Color{} // list view -> header text colour
	headerOf   = map[win.HWND]win.HWND{}   // list view -> its header
)

// colorHeaderText makes the header of list view lv draw its captions in ink.
// The dark item-view theme otherwise leaves them near-black on a dark band.
func colorHeaderText(lv, hdr win.HWND, ink walk.Color) {
	lvHookOnce.Do(func() { lvHookCB = syscall.NewCallback(lvHookProc) })
	headerInk[lv], headerOf[lv] = ink, hdr
	if _, done := lvPrev[lv]; done {
		return
	}
	lvPrev[lv] = win.SetWindowLongPtr(lv, win.GWLP_WNDPROC, lvHookCB)
}

func lvHookProc(hwnd, msg, wp, lp uintptr) (ret uintptr) {
	defer uiaGuard(&ret, 0)
	h := win.HWND(hwnd)
	if msg == win.WM_NCDESTROY {
		r := win.CallWindowProc(lvPrev[h], h, uint32(msg), wp, lp)
		delete(lvPrev, h)
		delete(headerInk, h)
		delete(headerOf, h)
		return r
	}
	// Walk re-applies system colours to every table on WM_SETTINGCHANGE /
	// WM_THEMECHANGED (a wallpaper change, an accent change, a policy refresh),
	// which would paint the empty part of each table white.
	switch msg {
	case win.LVM_SETBKCOLOR, win.LVM_SETTEXTBKCOLOR:
		lp = colorref(T.TableBG)
	case win.LVM_SETTEXTCOLOR:
		lp = colorref(T.Text)
	}
	if msg == win.WM_NOTIFY && lp != 0 {
		nmh := (*win.NMHDR)(ptrFrom(lp))
		if nmh.Code == win.NM_CUSTOMDRAW && nmh.HwndFrom == headerOf[h] {
			nmcd := (*win.NMCUSTOMDRAW)(ptrFrom(lp))
			switch nmcd.DwDrawStage {
			case win.CDDS_PREPAINT:
				return win.CDRF_NOTIFYITEMDRAW
			case win.CDDS_ITEMPREPAINT:
				procSetTextColor.Call(uintptr(nmcd.Hdc), colorref(headerInk[h]))
				return win.CDRF_DODEFAULT
			}
		}
	}
	return win.CallWindowProc(lvPrev[h], h, uint32(msg), wp, lp)
}

// ptrFrom turns a message parameter that Windows documents as a pointer back
// into one (the memory belongs to the sender, not to Go).
func ptrFrom(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }
