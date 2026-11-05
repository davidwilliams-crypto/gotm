//go:build windows

package ui

import (
	"syscall"
	"unsafe"

	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// Presentation gating. While nobody can see the window (minimized, hidden,
// cloaked on another virtual desktop, session locked or disconnected, display
// off) the per-tick page refresh and chrome repaints are skipped. Sampling,
// history, recording, sensor extrema, alerts and the tray keep running; the
// first moment the window can be seen again the current page is ticked once
// with the newest snapshot, before Windows paints it.

// hideReason is one reason the window cannot currently be seen.
type hideReason uint8

const (
	hideMinimized hideReason = 1 << iota
	hideHidden
	hideCloaked
	hideLocked
	hideDisconnected
	hideDisplayOff

	// hideWindow is everything derived from the window itself; hideSession is
	// everything the user could only have ended by being at the machine.
	hideWindow  = hideMinimized | hideHidden | hideCloaked
	hideSession = hideLocked | hideDisconnected | hideDisplayOff
)

// visibilityGate is UI-thread state. The zero value is visible.
type visibilityGate struct {
	hidden hideReason
	stale  bool // presentation work was skipped while hidden
}

func (g *visibilityGate) visible() bool { return g.hidden == 0 }

// update sets the reasons in mask to the bits of value. It reports true when
// the window just became visible after presentation work had been skipped,
// i.e. the page now needs one immediate refresh.
func (g *visibilityGate) update(mask, value hideReason) bool {
	was := g.visible()
	g.hidden = g.hidden&^mask | value&mask
	return !was && g.visible() && g.stale
}

func reasonIf(r hideReason, on bool) hideReason {
	if on {
		return r
	}
	return 0
}

// setHidden updates the visibility state and, when the window can be seen
// again after skipped work, refreshes the current page on the UI thread.
func (a *App) setHidden(mask, value hideReason) {
	before := a.vis.hidden
	refresh := a.vis.update(mask, value)
	if a.vis.hidden != before {
		debugf("visibility: hidden reasons %06b -> %06b", before, a.vis.hidden)
	}
	stallHidden.Store(!a.vis.visible())
	if refresh {
		a.queueVisibleRefresh()
	}
}

// queueVisibleRefresh runs refreshVisible once, after the message that made
// the window visible has been handled (posted work runs before WM_PAINT, so
// the first frame the user sees is already current).
func (a *App) queueVisibleRefresh() {
	if a.mw == nil {
		a.refreshVisible()
		return
	}
	if a.visRefreshQueued {
		return
	}
	a.visRefreshQueued = true
	a.mw.Synchronize(func() {
		a.visRefreshQueued = false
		if a.pumpDone != nil {
			select {
			case <-a.pumpDone:
				return
			default:
			}
		}
		a.refreshVisible()
	})
}

// refreshVisible brings the visible page and chrome up to date with the
// newest snapshot after work was skipped while hidden.
func (a *App) refreshVisible() {
	if !a.vis.visible() || !a.vis.stale {
		return
	}
	a.vis.stale = false
	if a.side != nil {
		a.side.invalidate()
	}
	a.invalidateChrome()
	if a.snap != nil && a.cur >= 0 && a.cur < len(a.pages) && a.pages[a.cur] != nil && a.pages[a.cur].initialized {
		a.pages[a.cur].impl.tick(a.snap)
	}
}

// skipPresentation is called once per snapshot, after a.snap is updated. It
// re-reads the cheap window state (in case a notification was missed) and
// reports whether the caller's page tick and footer repaint should be
// skipped: either nobody can see the window, or it has just become visible
// and refreshVisible has already brought everything up to date.
func (a *App) skipPresentation() bool {
	if a.mw != nil {
		a.pollWindowVisibility(win.HWND(a.mw.Handle()))
	}
	if !a.vis.visible() {
		a.vis.stale = true
		return true
	}
	if a.vis.stale {
		a.refreshVisible()
		return true
	}
	return false
}

// pollWindowVisibility re-reads minimized and cloaked state. "Hidden" comes
// only from show/hide notifications: WM_SETREDRAW (Walk's SetSuspended)
// clears WS_VISIBLE too, and must not be mistaken for a hidden window.
func (a *App) pollWindowVisibility(hwnd win.HWND) {
	if hwnd == 0 {
		return
	}
	value := reasonIf(hideMinimized, win.IsIconic(hwnd)) | reasonIf(hideCloaked, windowCloaked(hwnd))
	a.setHidden(hideMinimized|hideCloaked, value)
}

// userPresent clears the session reasons: something the user just did (a
// second launch, the tray icon, activating the window) proves they can see
// the screen, even if a lock or display notification was missed.
func (a *App) userPresent() { a.setHidden(hideSession, 0) }

const (
	wmWTSSessionChange    = 0x02B1
	wtsConsoleConnect     = 0x1
	wtsConsoleDisconnect  = 0x2
	wtsRemoteConnect      = 0x3
	wtsRemoteDisconnect   = 0x4
	wtsSessionLock        = 0x7
	wtsSessionUnlock      = 0x8
	wmPowerBroadcast      = 0x0218
	pbtPowerSettingChange = 0x8013
	sizeRestored          = 0
	sizeMinimized         = 1
	sizeMaximized         = 2
	swpShowWindow         = 0x0040
	swpHideWindow         = 0x0080
	dwmwaCloaked          = 14
	eventObjectCloaked    = 0x8017
	eventObjectUncloaked  = 0x8018
	wineventOutOfContext  = 0
	deviceNotifyWindow    = 0
	notifyForThisSession  = 0
	waInactive            = 0
	objidWindow           = 0
)

// GUID_CONSOLE_DISPLAY_STATE: 0 off, 1 on, 2 dimmed.
var guidConsoleDisplayState = windows.GUID{Data1: 0x6FE69556, Data2: 0x704A, Data3: 0x47A0,
	Data4: [8]byte{0x8F, 0x24, 0xC2, 0x8D, 0x93, 0x6F, 0xDA, 0x47}}

var (
	procDwmGetWindowAttribute             = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmGetWindowAttribute")
	procWTSRegisterSessionNotification    = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSRegisterSessionNotification")
	procWTSUnRegisterSessionNotification  = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSUnRegisterSessionNotification")
	procRegisterPowerSettingNotification  = windows.NewLazySystemDLL("user32.dll").NewProc("RegisterPowerSettingNotification")
	procUnregisterPowerSettingNotificaton = windows.NewLazySystemDLL("user32.dll").NewProc("UnregisterPowerSettingNotification")
	procSetWinEventHook                   = windows.NewLazySystemDLL("user32.dll").NewProc("SetWinEventHook")
	procUnhookWinEvent                    = windows.NewLazySystemDLL("user32.dll").NewProc("UnhookWinEvent")

	cloakHookApp *App // the App whose window the cloak hook watches (UI thread only)
	cloakHookCB  uintptr
)

// windowCloaked reports DWMWA_CLOAKED: the window is on another virtual
// desktop or otherwise hidden by the shell while still nominally visible.
func windowCloaked(hwnd win.HWND) bool {
	if procDwmGetWindowAttribute.Find() != nil {
		return false
	}
	var cloaked uint32
	hr, _, _ := procDwmGetWindowAttribute.Call(uintptr(hwnd), dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), unsafe.Sizeof(cloaked))
	return hr == 0 && cloaked != 0
}

// visibilityNotifications are the registrations made for the main window.
type visibilityNotifications struct {
	wts       bool
	power     uintptr
	cloakHook uintptr
}

// registerVisibility subscribes the main window to session lock, display
// power and cloak notifications. Each is optional: failure only means that
// reason is never reported, and the window stays treated as visible.
func (a *App) registerVisibility(hwnd win.HWND) {
	if procWTSRegisterSessionNotification.Find() == nil {
		if r, _, _ := procWTSRegisterSessionNotification.Call(uintptr(hwnd), notifyForThisSession); r != 0 {
			a.visNotify.wts = true
		}
	}
	if procRegisterPowerSettingNotification.Find() == nil {
		// Windows answers immediately with the current display state.
		a.visNotify.power, _, _ = procRegisterPowerSettingNotification.Call(uintptr(hwnd),
			uintptr(unsafe.Pointer(&guidConsoleDisplayState)), deviceNotifyWindow)
	}
	if procSetWinEventHook.Find() == nil {
		if cloakHookCB == 0 {
			cloakHookCB = syscall.NewCallback(func(hook, event, hwnd, idObject, idChild, thread, ms uintptr) uintptr {
				if a := cloakHookApp; a != nil && a.mw != nil && win.HWND(hwnd) == win.HWND(a.mw.Handle()) && int32(idObject) == objidWindow {
					a.setHidden(hideCloaked, reasonIf(hideCloaked, windowCloaked(win.HWND(hwnd))))
				}
				return 0
			})
		}
		cloakHookApp = a
		// Out of context: delivered on this (the UI) thread by its message loop.
		a.visNotify.cloakHook, _, _ = procSetWinEventHook.Call(eventObjectCloaked, eventObjectUncloaked, 0, cloakHookCB,
			uintptr(windows.GetCurrentProcessId()), uintptr(windows.GetCurrentThreadId()), wineventOutOfContext)
	}
	a.pollWindowVisibility(hwnd)
}

// unregisterVisibility undoes registerVisibility (on WM_DESTROY).
func (a *App) unregisterVisibility(hwnd win.HWND) {
	if a.visNotify.wts {
		procWTSUnRegisterSessionNotification.Call(uintptr(hwnd))
		a.visNotify.wts = false
	}
	if a.visNotify.power != 0 {
		procUnregisterPowerSettingNotificaton.Call(a.visNotify.power)
		a.visNotify.power = 0
	}
	if a.visNotify.cloakHook != 0 {
		procUnhookWinEvent.Call(a.visNotify.cloakHook)
		a.visNotify.cloakHook = 0
	}
	if cloakHookApp == a {
		cloakHookApp = nil
	}
	stallHidden.Store(false)
}

// observeVisibility is called from the main window hook for every message.
func (a *App) observeVisibility(hwnd win.HWND, msg uint32, wp, lp uintptr) {
	switch msg {
	case win.WM_SIZE:
		switch wp {
		case sizeMinimized:
			a.setHidden(hideMinimized, hideMinimized)
		case sizeRestored, sizeMaximized:
			a.setHidden(hideMinimized, 0)
		}
	case win.WM_WINDOWPOSCHANGED:
		if lp == 0 {
			return
		}
		flags := (*win.WINDOWPOS)(ptrFrom(lp)).Flags
		switch {
		case flags&swpHideWindow != 0:
			a.setHidden(hideHidden, hideHidden)
		case flags&swpShowWindow != 0:
			a.setHidden(hideHidden|hideMinimized, reasonIf(hideMinimized, win.IsIconic(hwnd)))
		}
	case win.WM_ACTIVATE:
		if wp&0xFFFF != waInactive {
			a.userPresent()
		}
	case wmWTSSessionChange:
		if r, value, ok := sessionChange(wp); ok {
			a.setHidden(r, value)
		}
	case wmPowerBroadcast:
		if wp == pbtPowerSettingChange && lp != 0 {
			if off, ok := displayOff(lp); ok {
				// The "console" display is the physical one even for a remote
				// desktop session, whose user sees it on another machine.
				a.setHidden(hideDisplayOff, reasonIf(hideDisplayOff, off && !isRemoteSession()))
			}
		}
	case win.WM_DESTROY:
		a.unregisterVisibility(hwnd)
	}
}

// sessionChange maps a WM_WTSSESSION_CHANGE code to the reason it sets or clears.
func sessionChange(code uintptr) (hideReason, hideReason, bool) {
	switch code {
	case wtsSessionLock:
		return hideLocked, hideLocked, true
	case wtsSessionUnlock:
		return hideLocked, 0, true
	case wtsConsoleDisconnect, wtsRemoteDisconnect:
		return hideDisconnected, hideDisconnected, true
	case wtsConsoleConnect, wtsRemoteConnect:
		// A newly attached display is on; the console state reported before
		// may belong to a display this session no longer uses.
		return hideDisconnected | hideDisplayOff, 0, true
	}
	return 0, 0, false
}

const smRemoteSession = 0x1000

// isRemoteSession reports whether this session is shown over Remote Desktop.
var isRemoteSession = func() bool { return win.GetSystemMetrics(smRemoteSession) != 0 }

// displayOff decodes a POWERBROADCAST_SETTING for GUID_CONSOLE_DISPLAY_STATE.
// Data is only read once DataLength says it holds the DWORD state.
func displayOff(lp uintptr) (off, ok bool) {
	return decodeDisplayState((*powerBroadcastSetting)(ptrFrom(lp)))
}

// powerBroadcastSetting is POWERBROADCAST_SETTING with a DWORD of data.
type powerBroadcastSetting struct {
	guid  windows.GUID
	size  uint32
	state uint32
}

func decodeDisplayState(p *powerBroadcastSetting) (off, ok bool) {
	if p == nil || p.guid != guidConsoleDisplayState || p.size < 4 {
		return false, false
	}
	return p.state == 0, true
}
