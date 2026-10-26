//go:build windows

// Command setup is gotm's installer and uninstaller: one small, self-contained
// executable with gotm.exe embedded, so packaging needs no third-party tool.
//
//	gotm-setup.exe                install (asks first), or upgrade in place
//	gotm-setup.exe /S             silent install
//	gotm-setup.exe /D=<dir>       install somewhere other than Program Files\gotm
//	gotm-setup.exe /desktop       also put a shortcut on the desktop
//	uninstall.exe /uninstall [/S] remove gotm (installed next to gotm.exe)
//
// It installs for all users into Program Files, which only administrators can
// write. That matters for a task manager: gotm elevates itself on request, and
// an elevated program must never be started from a folder other users or
// malware running as the user could replace.
//
// Exit codes: 0 done, 1 failed, 2 cancelled or gotm still running.
package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// version is stamped by build.ps1: -ldflags "-X main.version=1.2.3".
var version = "0.0.0-dev"

//go:embed payload
var payloadFS embed.FS

const title = "gotm Setup"

type options struct {
	silent, uninstall, desktop bool
	dir                        string
	taskManager                string
}

func parseArgs(args []string) (options, error) {
	var o options
	// Respect /S even when an earlier argument is invalid, so scripted
	// maintenance failures never block on an interactive message box.
	for _, a := range args {
		switch strings.ToLower(a) {
		case "/s", "/silent", "/quiet":
			o.silent = true
		}
	}
	for _, a := range args {
		switch l := strings.ToLower(a); {
		case l == "/s" || l == "/silent" || l == "/quiet":
			o.silent = true
		case l == "/uninstall":
			o.uninstall = true
		case l == "/desktop":
			o.desktop = true
		case strings.HasPrefix(l, "/taskmanager="):
			mode := strings.TrimPrefix(l, "/taskmanager=")
			if o.taskManager != "" || (mode != "on" && mode != "off") {
				return o, fmt.Errorf("use exactly one /taskmanager=on or /taskmanager=off")
			}
			o.taskManager = mode
		case strings.HasPrefix(l, "/d="):
			var err error
			o.dir, err = normalizeDestinationPath(strings.Trim(a[3:], `"`))
			if err != nil {
				return o, fmt.Errorf("/D= needs an absolute folder, got %q", a[3:])
			}
		case l == "/?" || l == "/h" || l == "/help":
			return o, fmt.Errorf("usage: setup [/S] [/D=folder] [/desktop]   |   uninstall.exe /uninstall [/S]   |   uninstall.exe /taskmanager=on|off [/S]")
		default:
			return o, fmt.Errorf("unknown option %q", a)
		}
	}
	if (o.taskManager != "" && (o.uninstall || o.dir != "" || o.desktop)) || (o.uninstall && (o.dir != "" || o.desktop)) {
		return o, fmt.Errorf("install, uninstall and Task Manager maintenance options cannot be combined")
	}
	return o, nil
}

func main() {
	runtime.LockOSThread() // COM and message boxes stay on one thread
	os.Exit(run())
}

func run() int {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		msg(o.silent, err.Error(), windows.MB_ICONERROR)
		return 1
	}
	if o.taskManager != "" {
		self, err := os.Executable()
		if err == nil {
			err = machineLayout().maintainTaskManager(self, o.taskManager)
		}
		if err != nil {
			msg(o.silent, "Task Manager replacement could not be changed:\n\n"+err.Error(), windows.MB_ICONERROR)
			return 1
		}
		return 0
	}
	if o.uninstall {
		return runUninstall(o)
	}
	payload, err := openPayload(payloadFS)
	if err != nil {
		msg(o.silent, "This setup is damaged: "+err.Error(), windows.MB_ICONERROR)
		return 1
	}
	return runInstall(o, payload)
}

func runInstall(o options, payload fs.FS) int {
	if _, err := fs.Stat(payload, "gotm.exe"); err != nil {
		msg(o.silent, "This setup was built without gotm.exe inside it. Rebuild it with build.ps1.", windows.MB_ICONERROR)
		return 1
	}
	l := machineLayout()
	prev, prevDir := l.installed()
	switch {
	case o.dir != "":
		l.dir = o.dir
	case prevDir != "":
		l.dir = prevDir // upgrade in place
	}

	if !o.silent {
		q := fmt.Sprintf("Install gotm %s for all users?\n\nFolder:  %s\n\nA Start menu shortcut is added, and gotm appears in Settings ▸ Apps for removal.", version, l.dir)
		if prev != "" {
			q = fmt.Sprintf("Upgrade gotm %s to %s?\n\nFolder:  %s", prev, version, l.dir)
		}
		if ask(q, windows.MB_YESNO|windows.MB_ICONQUESTION) != idYes {
			return 2
		}
	}

	self, _ := os.Executable()
	for {
		err := l.install(payload, self, version, o.desktop)
		if err == nil {
			break
		}
		if errRunning(err) && !o.silent &&
			ask("gotm is running from the install folder.\n\nClose every gotm window, then click Retry.", windows.MB_RETRYCANCEL|windows.MB_ICONWARNING) == idRetry {
			continue
		}
		msg(o.silent, "Setup could not finish:\n\n"+err.Error(), windows.MB_ICONERROR)
		if errRunning(err) {
			return 2
		}
		return 1
	}

	if !o.silent && ask(fmt.Sprintf("gotm %s is installed.\n\nOpen it now?", version), windows.MB_YESNO|windows.MB_ICONINFORMATION) == idYes {
		launchAsUser(filepath.Join(l.dir, "gotm.exe"))
	}
	return 0
}

func runUninstall(o options) int {
	self, err := os.Executable()
	if err != nil {
		msg(o.silent, err.Error(), windows.MB_ICONERROR)
		return 1
	}
	l := machineLayout()
	l, err = l.uninstallLayout(self)
	if err != nil {
		msg(o.silent, "gotm could not be removed safely:\n\n"+err.Error(), windows.MB_ICONERROR)
		return 1
	}
	if !o.silent && ask("Remove gotm from this computer?", windows.MB_YESNO|windows.MB_ICONQUESTION) != idYes {
		return 2
	}
	for {
		err = l.uninstall()
		if err == nil {
			break
		}
		if errRunning(err) && !o.silent &&
			ask("gotm is still running.\n\nClose every gotm window, then click Retry.", windows.MB_RETRYCANCEL|windows.MB_ICONWARNING) == idRetry {
			continue
		}
		msg(o.silent, "gotm could not be removed completely:\n\n"+err.Error(), windows.MB_ICONERROR)
		return 1
	}
	if !o.silent {
		if dir, ok := settingsDir(); ok && exists(dir) &&
			ask("Also delete your gotm settings and flight recordings?\n\n"+dir, windows.MB_YESNO|windows.MB_ICONQUESTION|windows.MB_DEFBUTTON2) == idYes {
			_ = os.RemoveAll(dir)
		}
	}
	// uninstall.exe cannot delete itself while it runs; a short-lived cmd.exe
	// removes it (and the folder, only if it is now empty) after we exit.
	deleteAfterExit(self, l.dir)
	if !o.silent {
		msg(false, "gotm has been removed.", windows.MB_ICONINFORMATION)
	}
	return 0
}

// settingsDir is gotm's per-user settings folder (%APPDATA%\gotm).
func settingsDir() (string, bool) {
	base := os.Getenv("APPDATA")
	if base == "" || !filepath.IsAbs(base) {
		return "", false
	}
	return filepath.Join(base, "gotm"), true
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func msg(silent bool, text string, icon uint32) {
	if silent {
		fmt.Fprintln(os.Stderr, text)
		return
	}
	ask(text, windows.MB_OK|icon)
}

func ask(text string, flags uint32) int32 {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	r, _ := windows.MessageBox(0, t, c, flags|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return r
}

// MessageBox results (winuser.h).
const (
	idRetry = 4
	idYes   = 6
)

// openPayload returns the files to install. build.ps1 packs them into
// payload/payload.zip (a third of the size); an unpacked payload folder works
// too, which is what development builds and tests use.
func openPayload(efs embed.FS) (fs.FS, error) {
	if data, err := efs.ReadFile("payload/payload.zip"); err == nil {
		return zip.NewReader(bytes.NewReader(data), int64(len(data)))
	}
	return fs.Sub(efs, "payload")
}
