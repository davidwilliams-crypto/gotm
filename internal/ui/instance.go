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

	// activationWait bounds how long a second launch waits for the running
	// instance to confirm (window hook installed) before giving up.
	activationWait = 4 * time.Second
	// takeoverWait is how long a blocked launch keeps retrying the mutex
	// before treating the holder as permanent: a closing instance hands the
	// slot over within moments (Run as administrator), so a brief retry
	// covers the relaunch race instead of exiting after UAC was accepted.
	takeoverWait = 1500 * time.Millisecond
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
func GuardSingleInstance() bool {
	name, _ := windows.UTF16PtrFromString(`Local\gotm.single-instance`)
	h, err := windows.CreateMutex(nil, true, name)
	switch {
	case err == nil: // created and owned
		instanceMutex = h
		return true
	case err == windows.ERROR_ALREADY_EXISTS:
		// The holder may be an instance that is shutting down and handing the
		// slot over. Retry briefly; if the mutex frees or is abandoned, take
		// it over cleanly with a fresh create.
		start := time.Now()
		for {
			ev, werr := windows.WaitForSingleObject(h, 250)
			if werr == nil && (ev == windows.WAIT_OBJECT_0 || ev == windows.WAIT_ABANDONED) {
				windows.CloseHandle(h)
				if h2, err2 := windows.CreateMutex(nil, true, name); err2 == nil {
					instanceMutex = h2
					return true
				}
				return true // cannot recreate: do not block startup
			}
			if time.Since(start) >= takeoverWait {
				break
			}
		}
		if ev, werr := windows.WaitForSingleObject(h, 0); werr == nil && ev == uint32(windows.WAIT_TIMEOUT) {
			windows.CloseHandle(h)
			activateRunningInstance()
			return false
		}
		windows.CloseHandle(h)
	}
	return true // unexpected error: never refuse to start over the guard
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

// activateRunningInstance asks the running gotm to show its window. It keeps
// knocking while the other instance finishes starting (its window may not
// exist yet) and returns as soon as that instance confirms it heard us.
func activateRunningInstance() {
	msg := activationMsg()
	if msg == 0 {
		return
	}
	const (
		eventName = `Local\gotm.activation-ready`
		step      = 250 * time.Millisecond
	)
	deadline := time.Now().Add(activationWait)
	for {
		// The process the user just launched has foreground permission; hand
		// it to whoever is listening so the running instance's
		// SetForegroundWindow is honoured instead of flashing the taskbar.
		_, _, _ = procAllowSetForegroundWindow.Call(asfwAny)
		win.PostMessage(hwndBroadcast, msg, 0, 0)
		if name, err := windows.UTF16PtrFromString(eventName); err == nil {
			if ev, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name); err == nil {
				evWait, _ := windows.WaitForSingleObject(ev, uint32(step.Milliseconds()))
				windows.CloseHandle(ev)
				if evWait == windows.WAIT_OBJECT_0 {
					return // the running instance heard us
				}
			} else {
				time.Sleep(step) // the running instance has not come up yet
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
	}
}

// signalActivationReady lets second launches stop knocking: this instance's
// main window exists and its hook has seen the activation message. The event
// is manual-reset and initially set, and this handle stays open for the
// process lifetime, so every later launch is answered by its first knock.
func signalActivationReady() {
	const eventName = `Local\gotm.activation-ready`
	if readyEvent != 0 {
		return // already signaled by an earlier call
	}
	name, err := windows.UTF16PtrFromString(eventName)
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
	win.SetForegroundWindow(hwnd)
}
