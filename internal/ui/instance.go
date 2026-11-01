//go:build windows

package ui

import (
	"sync"
	"time"

	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// Single-instance guard. With the Windows Task Manager replacement enabled,
// every Ctrl+Shift+Esc launches gotm again; a second copy must not fight the
// first over config.json and the recorder files. A named per-session mutex
// decides who runs; the loser asks the running instance to show its window
// and exits.

var (
	instanceMutex windows.Handle
	// readyEvent is held open for the whole process lifetime: a named kernel
	// object is destroyed when its last handle closes, and later launches
	// wait on it to learn this instance is listening.
	readyEvent    windows.Handle
	activateMsgId uint32
	activateOnce  sync.Once

	// activationWait bounds how long a second launch keeps asking the running
	// instance to show itself before giving up (the running instance may
	// still be building its window, and a closing one may hand the slot
	// over — see secondLaunch).
	activationWait = 4 * time.Second

	// Object names are variables so tests can use their own namespace
	// instead of fighting a real gotm running in the session.
	instanceMutexName   = `Local\gotm.single-instance`
	activationEventName = `Local\gotm.activation-ready`
)

const (
	hwndBroadcast = 0xFFFF      // HWND_BROADCAST
	asfwAny       = ^uintptr(0) // AllowSetForegroundWindow(ASFW_ANY)
)

// activationMsg is the registered window message a second instance
// broadcasts; RegisterWindowMessage returns the same id in every process.
func activationMsg() uint32 {
	activateOnce.Do(func() {
		name, _ := windows.UTF16PtrFromString("gotm.ActivateWindow")
		activateMsgId = win.RegisterWindowMessage(name)
	})
	return activateMsgId
}

// GuardSingleInstance claims this session's gotm slot. It returns true when
// this process may run (and holds the slot until the process exits or
// ReleaseSingleInstance); when another gotm already runs it asks that
// instance to show itself and returns false. A guard failure never refuses to
// start the app.
//
// The named mutex is created WITHOUT ownership: Go's scheduler may retire the
// thread that took it, and an owned mutex then reads "abandoned" while its
// process is perfectly alive (which once made this guard hand a live
// instance's slot to a second launch). The object's EXISTENCE is the slot —
// a process's handles all close when it exits, and elevate() releases
// explicitly before relaunching its replacement.
func GuardSingleInstance() bool {
	name, _ := windows.UTF16PtrFromString(instanceMutexName)
	h, err := windows.CreateMutex(nil, false, name)
	switch err {
	case nil: // created: the slot is ours
		instanceMutex = h
		return true
	case windows.ERROR_ALREADY_EXISTS:
		secondLaunch(h)
		return false
	}
	return true // unexpected error: never refuse to start over the guard
}

// secondLaunch serves a launch that found the slot taken. The running
// instance is asked to show itself IMMEDIATELY (the healthy case is a Ctrl+
// Shift+Esc: the window must come up at once, not after any retry), and it
// keeps asking briefly while that instance finishes building its window.
func secondLaunch(h windows.Handle) {
	defer windows.CloseHandle(h)
	knock()
	deadline := time.Now().Add(activationWait)
	for time.Now().Before(deadline) {
		if activationConfirmed() {
			return // the running instance heard us: this launch is done
		}
		time.Sleep(100 * time.Millisecond)
		knock()
	}
}

// knock asks the running gotm to show its window. The process the user just
// launched holds foreground permission; handing it to whoever is listening
// lets the running instance's SetForegroundWindow be honoured instead of
// only flashing the taskbar.
func knock() {
	msg := activationMsg()
	if msg == 0 {
		return
	}
	_, _, _ = procAllowSetForegroundWindow.Call(asfwAny)
	win.PostMessage(hwndBroadcast, msg, 0, 0)
}

// activationConfirmed reports whether a running instance has said "I hear
// you" (its main window hook is installed and the ready event is set).
func activationConfirmed() bool {
	name, err := windows.UTF16PtrFromString(activationEventName)
	if err != nil {
		return false
	}
	ev, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(ev)
	evWait, _ := windows.WaitForSingleObject(ev, 0)
	return evWait == windows.WAIT_OBJECT_0
}

// ReleaseSingleInstance drops the slot: used by tests, and by elevate() to
// hand the slot to the elevated replacement before it is launched.
func ReleaseSingleInstance() {
	if instanceMutex != 0 {
		windows.CloseHandle(instanceMutex)
		instanceMutex = 0
	}
}

var procAllowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

// signalActivationReady lets second launches stop knocking: this instance's
// main window exists and its hook has seen the activation message. The event
// is manual-reset and initially set, and this handle stays open for the
// process lifetime, so every later launch is answered by its first knock.
func signalActivationReady() {
	if readyEvent != 0 {
		return // already signaled by an earlier call
	}
	name, err := windows.UTF16PtrFromString(activationEventName)
	if err != nil {
		return
	}
	// On ERROR_ALREADY_EXISTS the returned handle is still valid (x/sys sets
	// err while returning it): set it rather than opening a second handle.
	ev, err := windows.CreateEventEx(nil, name, windows.CREATE_EVENT_MANUAL_RESET|windows.CREATE_EVENT_INITIAL_SET, windows.EVENT_MODIFY_STATE)
	if err == nil || err == windows.ERROR_ALREADY_EXISTS {
		_ = windows.SetEvent(ev)
		readyEvent = ev // kept open until the process exits
	}
}

// activateMainWindow is called from the main window's hook when a second
// instance asks this one to show itself.
func activateMainWindow(hwnd win.HWND) {
	if hwnd == 0 {
		return
	}
	win.ShowWindow(hwnd, win.SW_RESTORE) // unhides, un-minimizes and activates
	if win.SetForegroundWindow(hwnd) {
		return // the launching process handed us its foreground permission
	}
	// No permission was handed over (the launch may not have had any itself,
	// and Windows otherwise only flashes the taskbar): briefly attach to the
	// foreground thread — the standard fallback — and bring the window up.
	if fg := win.GetForegroundWindow(); fg != 0 {
		mine := win.GetCurrentThreadId()
		if other := win.GetWindowThreadProcessId(fg, nil); other != 0 && other != mine {
			win.AttachThreadInput(int32(mine), int32(other), true)
			win.BringWindowToTop(hwnd)
			win.SetForegroundWindow(hwnd)
			win.AttachThreadInput(int32(mine), int32(other), false)
		}
	}
}
