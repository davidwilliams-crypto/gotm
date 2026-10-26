//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestUninstallLocationFailsBeforeDeletionWhenIdentityUnknown(t *testing.T) {
	for _, scenario := range []string{"missing-registration", "missing-helper", "missing-registered-directory"} {
		t.Run(scenario, func(t *testing.T) {
			l := installedMaintenanceLayout(t)
			self := filepath.Join(l.dir, "uninstall.exe")
			switch scenario {
			case "missing-registration":
				if err := registry.DeleteKey(l.regRoot, l.regPath); err != nil {
					t.Fatal(err)
				}
			case "missing-helper":
				if err := os.Remove(self); err != nil {
					t.Fatal(err)
				}
			case "missing-registered-directory":
				l.dir = filepath.Join(filepath.Dir(l.dir), "missing registered directory")
				if err := l.register("2", 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := l.uninstallLayout(self); err == nil {
				t.Fatal("uninstall accepted unknown native identity")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(self), "gotm.exe")); err != nil {
				t.Fatalf("payload changed before identity validation: %v", err)
			}
		})
	}
}

func TestUninstallLocationPreservesStaleHelperAndMissingAppRecovery(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing app", true: "stale helper"}[stale], func(t *testing.T) {
			l := installedMaintenanceLayout(t)
			old := l
			self, exe := filepath.Join(l.dir, "uninstall.exe"), filepath.Join(l.dir, "gotm.exe")
			if err := l.taskManagerStore.Enable(exe); err != nil {
				t.Fatal(err)
			}
			if stale {
				l.dir = filepath.Join(filepath.Dir(l.dir), "new location")
				if err := l.install(payloadV("new"), self, "2", false); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(exe); err != nil {
				t.Fatal(err)
			}
			resolved, err := l.uninstallLayout(self)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(resolved.dir, old.dir) {
				t.Fatalf("resolved %q want %q", resolved.dir, old.dir)
			}
			if err := resolved.uninstall(); err != nil {
				t.Fatal(err)
			}
			if stale {
				assertRouting(t, l, filepath.Join(l.dir, "gotm.exe"), true)
				if _, dir := l.installed(); !strings.EqualFold(dir, l.dir) {
					t.Fatal("stale helper removed new metadata")
				}
			} else {
				assertRouting(t, l, exe, false)
			}
		})
	}
}

func nativeShortDirectory(t *testing.T, dir string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Skipf("8.3 aliases unavailable: %v", err)
	}
	if n >= uint32(len(buf)) {
		t.Fatal("short path exceeds buffer")
	}
	short := windows.UTF16ToString(buf[:n])
	if strings.EqualFold(short, dir) {
		t.Skip("filesystem did not provide a distinct 8.3 path")
	}
	return short
}

func TestUninstallLocationNativeAliasRestoresRegisteredSpelling(t *testing.T) {
	for _, registeredShort := range []bool{false, true} {
		t.Run(map[bool]string{false: "short launch", true: "short registration"}[registeredShort], func(t *testing.T) {
			l := installedMaintenanceLayout(t)
			long := l.dir
			short := nativeShortDirectory(t, long)
			loadedDir := short
			if registeredShort {
				l.dir, loadedDir = short, long
				if err := l.register("1", 0); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(l.dir, "gotm.exe")
			if err := l.taskManagerStore.Enable(exe); err != nil {
				t.Fatal(err)
			}
			resolved, err := l.uninstallLayout(filepath.Join(loadedDir, "uninstall.exe"))
			if err != nil {
				t.Fatal(err)
			}
			if resolved.dir != l.dir {
				t.Fatalf("uninstall uses %q instead of exact registered path %q", resolved.dir, l.dir)
			}
			if err := resolved.uninstall(); err != nil {
				t.Fatal(err)
			}
			assertRouting(t, l, exe, false)
			if _, err := os.Stat(filepath.Join(long, "gotm.exe")); !os.IsNotExist(err) {
				t.Fatalf("payload was not removed: %v", err)
			}
			if _, dir := l.installed(); dir != "" {
				t.Fatal("same installation was mistaken for a newer install")
			}
		})
	}
}
