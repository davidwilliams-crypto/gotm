//go:build windows

package ui

import (
	"syscall"
	"time"
	"unsafe"

	"github.com/tailscale/walk"
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
// When the window stops being visible, graph memory is released: paints stop
// while hidden, so nothing else would free it.
func (a *App) setHidden(mask, value hideReason) {
	before := a.vis.hidden
	refresh := a.vis.update(mask, value)
	if a.vis.hidden != before {
		debugf("visibility: hidden reasons %06b -> %06b", before, a.vis.hidden)
		if a.vis.hidden&hideSession&^before != 0 {
			a.sessionSince = time.Now() // polls wait for the switch to settle
		}
		if before == 0 {
			releaseGraphMemory()
		}
		a.updateVisibilityTimer()
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
// newest snapshot after work was skipped while hidden. The cheap window
// state is read again first: a refresh queued by an event can run after the
// window was cloaked or minimized again without a notification.
func (a *App) refreshVisible() {
	if !a.vis.visible() || !a.vis.stale {
		return
	}
	if a.mw != nil {
		a.pollWindowVisibility(win.HWND(a.mw.Handle()))
		if !a.vis.visible() || !a.vis.stale {
			return
		}
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
	wasStale := a.vis.stale
	if a.mw != nil {
		a.pollWindowVisibility(win.HWND(a.mw.Handle()))
	}
	a.recheckSession(time.Now(), sessionRecheckGap)
	if wasStale && a.vis.visible() && !a.vis.stale {
		return true // a re-check found the window visible and refreshed it
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
	minimized, cloaked := windowState(hwnd)
	a.setHidden(hideMinimized|hideCloaked, reasonIf(hideMinimized, minimized)|reasonIf(hideCloaked, cloaked))
}

// windowState reads whether the window is minimized and whether the shell
// has cloaked it (another virtual desktop). Replaceable for tests.
var windowState = func(hwnd win.HWND) (minimized, cloaked bool) {
	return win.IsIconic(hwnd), windowCloaked(hwnd)
}

// userPresent clears the session reasons: something the user just did (a
// second launch, the tray icon, activating the window, input to one of its
// windows) proves they can see the screen, even if a lock or display
// notification was missed.
func (a *App) userPresent() { a.setHidden(hideSession, 0) }

// userInput is input (a key, a click, the wheel) delivered to one of the
// window's controls: someone is at the machine and the window is shown on
// their desktop, so the session and show/hide reasons are cleared and the
// window state is read again.
func (a *App) userInput(hwnd win.HWND) {
	a.setHidden(hideSession|hideHidden, 0)
	a.pollWindowVisibility(hwnd)
}

// paintSeen is a WM_PAINT (or frame paint) for the window or one of its
// controls while it is thought hidden. Windows paints a minimized or cloaked
// window rarely, and gotm itself invalidates nothing while hidden, so this is
// a cue that a notification may have been missed: the window state is read
// again and the session state re-checked (rate limited).
func (a *App) paintSeen(hwnd win.HWND) {
	if a.vis.visible() {
		return
	}
	a.pollWindowVisibility(hwnd)
	a.recheckSession(time.Now(), time.Second)
}

// Session reasons cannot all be polled, and a missed unlock, reconnect or
// display-on notification must not leave the page frozen while it is on
// screen. While one of them is set, the state is re-checked on evidence
// (paint, activation, input) and at least every sessionRecheckGap from the
// sampler tick and the visibility timer: a lock or disconnect is cleared when
// the session is found unlocked and connected again, and the display state
// is asked for again (Windows answers a new registration with the current
// state). Polls only ever clear reasons; setting them stays with the
// notifications, so a probe that cannot tell never hides a visible window.
// The first poll waits sessionRecheckGap after the notification, so a lock
// is never undone by a probe that ran before the desktop had switched.
const sessionRecheckGap = 5 * time.Second

func (a *App) recheckSession(now time.Time, minGap time.Duration) {
	if a.vis.hidden&hideSession == 0 || now.Sub(a.sessionChecked) < minGap || now.Sub(a.sessionSince) < sessionRecheckGap {
		return
	}
	a.sessionChecked = now
	var clear hideReason
	if a.vis.hidden&hideLocked != 0 && sessionUnlocked() {
		clear |= hideLocked
	}
	if a.vis.hidden&hideDisconnected != 0 && sessionConnected() {
		clear |= hideDisconnected
	}
	if clear != 0 {
		a.setHidden(clear, 0)
	}
	if a.vis.hidden&hideDisplayOff != 0 {
		requeryDisplayState(a)
	}
}

// visibility timer: while the window is cloaked or a session reason is set,
// re-read the state every second, so switching back to gotm's virtual desktop
// is noticed at once even when the cloak WinEvent does not arrive (and a
// missed session notification within sessionRecheckGap).
const (
	visTimerID     = 0x6F74 // "ot"
	visTimerPeriod = 1000   // ms
)

var (
	procSetTimer  = windows.NewLazySystemDLL("user32.dll").NewProc("SetTimer")
	procKillTimer = windows.NewLazySystemDLL("user32.dll").NewProc("KillTimer")
)

func (a *App) updateVisibilityTimer() {
	want := a.vis.hidden&(hideCloaked|hideSession) != 0
	if want == a.visTimer || a.mw == nil {
		return
	}
	hwnd := a.mw.Handle()
	if want {
		if r, _, _ := procSetTimer.Call(uintptr(hwnd), visTimerID, visTimerPeriod, 0); r == 0 {
			return
		}
	} else {
		procKillTimer.Call(uintptr(hwnd), visTimerID)
	}
	a.visTimer = want
}

func (a *App) visibilityTimerTick(hwnd win.HWND) {
	a.pollWindowVisibility(hwnd)
	a.recheckSession(time.Now(), sessionRecheckGap)
	a.updateVisibilityTimer()
}

const (
	wmMouseHWheel      = 0x020E
	desktopReadObjects = 0x0001
	uoiName            = 2
	wtsConnectState    = 8 // WTS_INFO_CLASS WTSConnectState
	wtsActive          = 0 // WTS_CONNECTSTATE_CLASS WTSActive
	wtsCurrentSession  = 0xFFFFFFFF
)

var (
	procOpenInputDesktop          = windows.NewLazySystemDLL("user32.dll").NewProc("OpenInputDesktop")
	procGetThreadDesktop          = windows.NewLazySystemDLL("user32.dll").NewProc("GetThreadDesktop")
	procCloseDesktop              = windows.NewLazySystemDLL("user32.dll").NewProc("CloseDesktop")
	procGetUserObjectInformation  = windows.NewLazySystemDLL("user32.dll").NewProc("GetUserObjectInformationW")
	procWTSQuerySessionInfomation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory             = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSFreeMemory")
)

// sessionUnlocked reports whether the desktop receiving input is this
// thread's desktop: false while the lock screen (the Winlogon desktop) or a
// secure prompt has it, or when that cannot be determined.
var sessionUnlocked = func() bool {
	if procOpenInputDesktop.Find() != nil || procGetUserObjectInformation.Find() != nil {
		return false
	}
	input, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if input == 0 {
		return false // access to the input desktop denied: a secure desktop
	}
	defer procCloseDesktop.Call(input)
	own, _, _ := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	name := func(h uintptr) string {
		var buf [64]uint16
		var n uint32
		if r, _, _ := procGetUserObjectInformation.Call(h, uoiName, uintptr(unsafe.Pointer(&buf[0])), unsafe.Sizeof(buf), uintptr(unsafe.Pointer(&n))); r == 0 {
			return ""
		}
		return windows.UTF16ToString(buf[:])
	}
	in := name(input)
	return own != 0 && in != "" && in == name(own)
}

// sessionConnected reports whether this session is attached to a console or
// a remote client (WTSActive).
var sessionConnected = func() bool {
	if procWTSQuerySessionInfomation.Find() != nil {
		return false
	}
	var buf uintptr
	var n uint32
	if r, _, _ := procWTSQuerySessionInfomation.Call(0, wtsCurrentSession, wtsConnectState, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n))); r == 0 || buf == 0 {
		return false
	}
	defer procWTSFreeMemory.Call(buf)
	return n >= 4 && *(*int32)(ptrFrom(buf)) == wtsActive
}

// requeryDisplayState registers for the console display state again;
// Windows answers a registration with the current state, which arrives as an
// ordinary WM_POWERBROADCAST.
var requeryDisplayState = func(a *App) {
	if a.mw == nil || a.visNotify.power == 0 || procRegisterPowerSettingNotification.Find() != nil {
		return
	}
	procUnregisterPowerSettingNotificaton.Call(a.visNotify.power)
	a.visNotify.power, _, _ = procRegisterPowerSettingNotification.Call(uintptr(a.mw.Handle()),
		uintptr(unsafe.Pointer(&guidConsoleDisplayState)), deviceNotifyWindow)
}

// visibilityWatcher sees every message gotm's message loop retrieves (Walk
// runs global pre-translate handlers first) and turns those that prove the
// window is in front of someone into visibility evidence.
type visibilityWatcher struct{ a *App }

func (w visibilityWatcher) OnPreTranslate(msg *win.MSG) bool {
	a := w.a
	// The common case, a visible window, costs one comparison per message:
	// evidence only matters while the window is thought hidden.
	if a.vis.visible() || a.mw == nil || msg.HWnd == 0 {
		return false
	}
	if !isUserInput(msg.Message) && msg.Message != win.WM_PAINT && msg.Message != win.WM_NCPAINT {
		return false
	}
	main := win.HWND(a.mw.Handle())
	if msg.HWnd != main && win.GetAncestor(msg.HWnd, win.GA_ROOT) != main {
		return false
	}
	a.observeLoopMessage(main, msg.Message)
	return false
}

// observeLoopMessage handles a message retrieved for one of the main
// window's windows (see visibilityWatcher).
func (a *App) observeLoopMessage(main win.HWND, msg uint32) {
	if isUserInput(msg) {
		a.userInput(main)
		return
	}
	a.paintSeen(main)
}

// isUserInput is keyboard, mouse button and wheel input. Mouse moves are not
// counted: Windows synthesizes them when a window moves under a still cursor.
func isUserInput(msg uint32) bool {
	switch msg {
	case win.WM_KEYDOWN, win.WM_SYSKEYDOWN, win.WM_CHAR,
		win.WM_LBUTTONDOWN, win.WM_RBUTTONDOWN, win.WM_MBUTTONDOWN, win.WM_XBUTTONDOWN,
		win.WM_LBUTTONDBLCLK, win.WM_RBUTTONDBLCLK,
		win.WM_MOUSEWHEEL, wmMouseHWheel,
		win.WM_NCLBUTTONDOWN, win.WM_NCRBUTTONDOWN:
		return true
	}
	return false
}

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
	watcher   bool // visibilityWatcher installed in the message loop
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
	if !a.visNotify.watcher {
		// Input and paints reach the controls, not the main window: watch
		// them in the message loop (registered for the life of the process).
		walk.App().AddGlobalPreTranslateHandler(visibilityWatcher{a})
		a.visNotify.watcher = true
	}
	graphOwnerShown = a.canvasShown
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
	if a.visTimer {
		procKillTimer.Call(uintptr(hwnd), visTimerID)
		a.visTimer = false
	}
	graphOwnerShown = canvasWindowShown
	stallHidden.Store(false)
}

// canvasShown is the App's graphOwnerShown: a canvas is shown while the
// window can be seen and the canvas sits, visible, on the current page (or
// in the shell chrome outside every page). It reads gotm's own state rather
// than IsWindowVisible on the canvas: Walk's layout suspend (WM_SETREDRAW
// FALSE on the client area) clears WS_VISIBLE on an ancestor without hiding
// anything, and must not throw away graph caches.
func (a *App) canvasShown(v *canvasView) bool {
	if !a.vis.visible() || v.cw == nil || v.cw.IsDisposed() {
		return false
	}
	canvas := win.HWND(v.cw.Handle())
	if !hasVisibleStyle(canvas) {
		return false
	}
	// Windows between the canvas and its page wrap are the page's own panels
	// (hidden by the page when not in use). Everything from the wrap up is
	// decided by the App: the current page, and a.vis for the window.
	hiddenBelow := false
	for h := win.GetParent(canvas); h != 0; h = win.GetParent(h) {
		for k, w := range a.wraps {
			if w != nil && win.HWND(w.Handle()) == h {
				return k == a.cur && !hiddenBelow
			}
		}
		if !hasVisibleStyle(h) {
			hiddenBelow = true
		}
	}
	return true // shell chrome, outside every page
}

func hasVisibleStyle(h win.HWND) bool {
	return uint32(win.GetWindowLong(h, win.GWL_STYLE))&win.WS_VISIBLE != 0
}

// observeVisibility is called from the main window hook for every message.
// It reports true for a message it consumed (the visibility timer).
func (a *App) observeVisibility(hwnd win.HWND, msg uint32, wp, lp uintptr) bool {
	switch msg {
	case win.WM_TIMER:
		if wp == visTimerID {
			a.visibilityTimerTick(hwnd)
			return true
		}
	case win.WM_ACTIVATEAPP:
		if wp != 0 { // gotm became the foreground application
			a.userPresent()
			a.pollWindowVisibility(hwnd)
		}
	case win.WM_NCPAINT, win.WM_PAINT, win.WM_ERASEBKGND:
		a.paintSeen(hwnd)
	case win.WM_SHOWWINDOW:
		if wp != 0 {
			a.paintSeen(hwnd)
		}
	case win.WM_SIZE:
		switch wp {
		case sizeMinimized:
			a.setHidden(hideMinimized, hideMinimized)
		case sizeRestored, sizeMaximized:
			a.setHidden(hideMinimized, 0)
		}
	case win.WM_WINDOWPOSCHANGED:
		if lp == 0 {
			return false
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
			a.pollWindowVisibility(hwnd)
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
	return false
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
