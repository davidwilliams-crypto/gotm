//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"gotm/internal/taskmanager"
)

func TestParseTaskManagerModes(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		o, err := parseArgs([]string{"/S", "/taskmanager=" + mode})
		if err != nil || o.taskManager != mode || !o.silent {
			t.Errorf("mode %q: %+v, %v", mode, o, err)
		}
	}
	for _, args := range [][]string{
		{"/taskmanager=yes"}, {"/taskmanager="},
		{"/taskmanager=on", "/taskmanager=off"},
		{"/taskmanager=on", "/taskmanager=on"},
		{"/taskmanager=on", "/uninstall"},
		{"/taskmanager=off", `/D=C:\gotm`},
		{"/taskmanager=on", "/desktop"},
		{"/uninstall", `/D=C:\gotm`}, {"/uninstall", "/desktop"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted conflicting or invalid modes %q", args)
		}
	}
	if o, err := parseArgs([]string{"/taskmanager=yes", "/S"}); err == nil || !o.silent {
		t.Fatalf("silent errors must remain silent regardless of option order: %+v %v", o, err)
	}
}

func TestRelocationFailureLeavesOldRouteUsable(t *testing.T) {
	l := installedMaintenanceLayout(t)
	oldExe := filepath.Join(l.dir, "gotm.exe")
	if err := l.taskManagerStore.Enable(oldExe); err != nil {
		t.Fatal(err)
	}
	l.dir = filepath.Join(filepath.Dir(l.dir), "new")
	// The missing setup source fails after the new payload copy but before
	// routing/registration; the old route must still launch its old binary.
	if err := l.install(payloadV("new"), filepath.Join(t.TempDir(), "missing.exe"), "2", false); err == nil {
		t.Fatal("expected copy failure")
	}
	assertRouting(t, l, oldExe, true)
	if b, err := os.ReadFile(oldExe); err != nil || string(b) != "MZ fake gotm old" {
		t.Fatalf("old target damaged: %q %v", b, err)
	}
}

func TestInstallAndUninstallPreserveForeignRouting(t *testing.T) {
	l := installedMaintenanceLayout(t)
	k, _, err := registry.CreateKey(l.taskManagerStore.Root, l.taskManagerStore.Path, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetStringValue("Debugger", "foreign debugger"); err != nil {
		t.Fatal(err)
	}
	if err := l.install(payloadV("new"), "", "2", false); err != nil {
		t.Fatal(err)
	}
	if err := l.uninstall(); err != nil {
		t.Fatal(err)
	}
	v, _, err := k.GetStringValue("Debugger")
	if err != nil || v != "foreign debugger" {
		t.Fatalf("foreign route changed: %q %v", v, err)
	}
}

func installedMaintenanceLayout(t *testing.T) layout {
	t.Helper()
	l := testLayout(t)
	l.startMenu, l.desktop = "", ""
	self := filepath.Join(t.TempDir(), "setup.exe")
	if err := os.WriteFile(self, []byte("fake setup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.install(payloadV("old"), self, "1", false); err != nil {
		t.Fatal(err)
	}
	state, err := l.taskManagerStore.Query(filepath.Join(l.dir, "gotm.exe"))
	if err != nil || state.Owned || state.Conflict || state.Command != "" {
		t.Fatalf("installation enabled routing without opting in: %+v %v", state, err)
	}
	return l
}

func assertRouting(t *testing.T, l layout, exe string, owned bool) {
	t.Helper()
	s, err := l.taskManagerStore.Query(exe)
	if err != nil || s.Owned != owned {
		t.Fatalf("routing owned=%v want %v: %+v, %v", s.Owned, owned, s, err)
	}
}

// A portable/setup copy, a stale helper and an unsafe payload must never gain
// machine launch authority, even if the caller is already elevated.
func TestTaskManagerMaintenanceAuthorization(t *testing.T) {
	for _, scenario := range []string{"portable", "setup", "stale", "unsafe", "missing-payload", "alias", "volume"} {
		t.Run(scenario, func(t *testing.T) {
			l := installedMaintenanceLayout(t)
			self := filepath.Join(l.dir, "uninstall.exe")
			switch scenario {
			case "portable":
				if err := registry.DeleteKey(l.regRoot, l.regPath); err != nil {
					t.Fatal(err)
				}
			case "setup":
				self = filepath.Join(l.dir, "setup.exe")
			case "stale":
				self = filepath.Join(filepath.Dir(l.dir), "old", "uninstall.exe")
			case "unsafe":
				l.inspectSecurity = func(windows.Handle, bool) error { return errors.New("unsafe ACL") }
			case "missing-payload":
				if err := os.Remove(filepath.Join(l.dir, "gotm.exe")); err != nil {
					t.Fatal(err)
				}
			case "alias":
				self = l.dir + `\..\gotm\uninstall.exe`
			case "volume":
				l.queryVolume = func(string, windows.Handle) (destinationVolume, error) { return destinationVolume{}, nil }
			}
			for _, mode := range []string{"on", "off"} {
				if scenario == "missing-payload" && mode == "off" {
					continue // recovery must work even if gotm.exe was removed
				}
				if err := l.maintainTaskManager(self, mode); err == nil {
					t.Fatalf("accepted %s helper for %s", scenario, mode)
				}
			}
			assertRouting(t, l, filepath.Join(l.dir, "gotm.exe"), false)
		})
	}
}

func TestTaskManagerRestoreMissingPayload(t *testing.T) {
	l := installedMaintenanceLayout(t)
	exe, self := filepath.Join(l.dir, "gotm.exe"), filepath.Join(l.dir, "uninstall.exe")
	if err := l.maintainTaskManager(self, "on"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := l.maintainTaskManager(self, "off"); err != nil {
		t.Fatalf("restore must recover routing when the app is missing: %v", err)
	}
	assertRouting(t, l, exe, false)
	if _, err := os.Stat(self); err != nil {
		t.Fatalf("recovery removed the helper: %v", err)
	}
}

func TestTaskManagerMaintenanceOwnedAndForeign(t *testing.T) {
	l := installedMaintenanceLayout(t)
	self, exe := filepath.Join(l.dir, "uninstall.exe"), filepath.Join(l.dir, "gotm.exe")
	if err := l.maintainTaskManager(self, "on"); err != nil {
		t.Fatal(err)
	}
	assertRouting(t, l, exe, true)
	if err := l.maintainTaskManager(self, "off"); err != nil {
		t.Fatal(err)
	}
	assertRouting(t, l, exe, false)
	k, _, err := registry.CreateKey(l.taskManagerStore.Root, l.taskManagerStore.Path, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetStringValue("Debugger", "foreign debugger"); err != nil {
		t.Fatal(err)
	}
	if err := l.maintainTaskManager(self, "on"); err == nil {
		t.Fatal("foreign replacement overwritten")
	}
	if err := l.maintainTaskManager(self, "off"); err != nil {
		t.Fatal(err)
	}
	value, _, err := k.GetStringValue("Debugger")
	if err != nil || value != "foreign debugger" {
		t.Fatalf("foreign routing changed: %q %v", value, err)
	}
}

func TestUninstallRestoresBeforeDeletingPayload(t *testing.T) {
	l := installedMaintenanceLayout(t)
	exe := filepath.Join(l.dir, "gotm.exe")
	if err := l.taskManagerStore.Enable(exe); err != nil {
		t.Fatal(err)
	}
	// Lock the binary against deletion. Restoration must have happened even
	// when deletion fails at the very first executable.
	p, _ := windows.UTF16PtrFromString(exe)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := l.uninstall(); err == nil {
		t.Fatal("expected locked executable deletion error")
	}
	assertRouting(t, l, exe, false)
}

func TestUninstallRestorationErrorPreservesAllPayload(t *testing.T) {
	l := installedMaintenanceLayout(t)
	store := *l.taskManagerStore
	l.taskManagerStore = &taskmanager.Store{Root: registry.Key(0), Path: store.Path}
	if err := l.uninstall(); err == nil {
		t.Fatal("expected routing access failure")
	}
	for _, f := range ownedFiles {
		if _, err := os.Stat(filepath.Join(l.dir, f)); err != nil {
			t.Fatalf("deleted %s before restoring routing: %v", f, err)
		}
	}
	if _, dir := l.installed(); dir != l.dir {
		t.Fatal("deleted install metadata before routing restored")
	}
}

func TestUpgradeRoutingAndOldUninstaller(t *testing.T) {
	l := installedMaintenanceLayout(t)
	old := l
	oldExe := filepath.Join(old.dir, "gotm.exe")
	if err := l.taskManagerStore.Enable(oldExe); err != nil {
		t.Fatal(err)
	}
	if err := l.install(payloadV("same"), "", "2", false); err != nil {
		t.Fatal(err)
	}
	assertRouting(t, l, oldExe, true)
	l.dir = filepath.Join(filepath.Dir(l.dir), "new gotm")
	if err := l.install(payloadV("new"), filepath.Join(old.dir, "uninstall.exe"), "3", false); err != nil {
		t.Fatal(err)
	}
	newExe := filepath.Join(l.dir, "gotm.exe")
	assertRouting(t, l, newExe, true)
	// Shared links now belong to the newer registration too.
	l.startMenu = filepath.Join(filepath.Dir(l.dir), "Shared Start Menu")
	old.startMenu = l.startMenu
	if err := os.MkdirAll(l.startMenu, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l.startMenu, "gotm.lnk")
	if err := os.WriteFile(link, []byte("new shortcut"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := old.uninstall(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(link); err != nil || string(b) != "new shortcut" {
		t.Fatalf("old helper removed newer shortcut: %q %v", b, err)
	}
	assertRouting(t, l, newExe, true)
	if ver, dir := l.installed(); ver != "3" || !strings.EqualFold(dir, l.dir) {
		t.Fatalf("old uninstaller deleted newer metadata: %q %q", ver, dir)
	}
	if b, err := os.ReadFile(newExe); err != nil || string(b) != "MZ fake gotm new" {
		t.Fatalf("new payload %q %v", b, err)
	}
}

func TestRelocationCommitRollback(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign-change=", changed), func(t *testing.T) {
			l := installedMaintenanceLayout(t)
			oldDir := l.dir
			oldExe := filepath.Join(oldDir, "gotm.exe")
			if err := l.taskManagerStore.Enable(oldExe); err != nil {
				t.Fatal(err)
			}
			l.dir = filepath.Join(filepath.Dir(l.dir), "new")
			if err := os.MkdirAll(l.dir, 0700); err != nil {
				t.Fatal(err)
			}
			newExe := filepath.Join(l.dir, "gotm.exe")
			if err := os.WriteFile(newExe, []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			commitErr := errors.New("metadata denied")
			err := l.commitInstall(oldDir, func() error {
				assertRouting(t, l, newExe, true)
				if changed {
					k, err := registry.OpenKey(l.taskManagerStore.Root, l.taskManagerStore.Path, registry.SET_VALUE)
					if err != nil {
						t.Fatal(err)
					}
					defer k.Close()
					if err := k.SetStringValue("Debugger", "foreign debugger"); err != nil {
						t.Fatal(err)
					}
				}
				return commitErr
			})
			if !errors.Is(err, commitErr) {
				t.Fatalf("lost commit failure: %v", err)
			}
			assertRouting(t, l, oldExe, !changed)
			if changed {
				state, err := l.taskManagerStore.Query(oldExe)
				if err != nil || state.Command != "foreign debugger" {
					t.Fatalf("rollback overwrote foreign change: %+v %v", state, err)
				}
			}
			if _, err := os.Stat(oldExe); err != nil {
				t.Fatalf("old route target removed: %v", err)
			}
			if _, err := os.Stat(newExe); err != nil {
				t.Fatalf("rollback deleted potential active target: %v", err)
			}
		})
	}
}
