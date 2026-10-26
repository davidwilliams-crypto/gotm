//go:build windows

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/walk"
	"gotm/internal/config"
	"gotm/internal/metrics"
	"gotm/internal/taskmanager"
)

func TestTaskManagerHelperRejectsUnprotectedCopy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gotm.exe")
	for _, name := range []string{"gotm.exe", "uninstall.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not executed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, guard, err := protectedTaskManagerHelper(exe, dir)
	if guard != nil {
		guard.Close()
	}
	if err == nil {
		t.Fatal("an ordinary-user writable helper was accepted for elevated execution")
	}
}

func TestTaskManagerHelperMatchesRegisteredCopy(t *testing.T) {
	dir := `C:\Program Files\gotm`
	got, err := taskManagerHelperPath(`c:\program files\GOTM\gotm.exe`, dir)
	if err != nil || got != filepath.Join(dir, "uninstall.exe") {
		t.Fatalf("installed helper = %q, %v", got, err)
	}
	for _, exe := range []string{`C:\Downloads\gotm.exe`, `C:\Program Files\gotm\other.exe`, `C:\Program Files\gotm-old\gotm.exe`} {
		if _, err := taskManagerHelperPath(exe, dir); err == nil {
			t.Errorf("noninstalled copy accepted: %s", exe)
		}
	}
}

func TestTaskManagerHelperDoesNotCreateMissingInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, guard, err := protectedTaskManagerHelper(filepath.Join(dir, "gotm.exe"), dir); err == nil {
		if guard != nil {
			guard.Close()
		}
		t.Fatal("missing installation accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("helper check created the missing installation: %v", err)
	}
}

func TestTaskManagerSettingReportsRestoreAndConflict(t *testing.T) {
	tab := newSettingsTab(&App{})
	tab.app.host.Elevated = true
	tab.taskManager.state = taskmanager.State{Owned: true}
	label, _ := tab.taskManagerText()
	if label != "Restore Windows Task Manager" {
		t.Fatalf("owned replacement offers %q", label)
	}
	tab.taskManager.state = taskmanager.State{Owned: true, Conflict: true}
	label, note := tab.taskManagerText()
	if label != "Restore Windows Task Manager" || !strings.Contains(note, "launch filter") {
		t.Fatalf("owned replacement with an active filter hides its conflict: %q, %q", label, note)
	}
	tab.taskManager.state = taskmanager.State{Conflict: true}
	label, note = tab.taskManagerText()
	if label == "Restore Windows Task Manager" || note == "" {
		t.Fatalf("foreign replacement offers restore or no explanation: %q, %q", label, note)
	}
	tab.taskManager.busy = true
	label, _ = tab.taskManagerText()
	if label != "Applying change…" {
		t.Fatalf("pending maintenance offers another change: %q", label)
	}
}

func TestTaskManagerSettingIsAccessibleAndKeyboardReachable(t *testing.T) {
	runNativeTest(t, func() {
		oldScale := gs
		gs = 1
		defer func() { gs = oldScale }()
		a := &App{cfg: config.Default(), store: config.OpenAt(t.TempDir()), sensors: &metrics.SensorHub{}}
		a.host.Elevated = true
		tab := newSettingsTab(a)
		tab.taskManager.state = taskmanager.State{Owned: true}
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 900, Height: 1400}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		tab.paint(cv, 900, 1400)
		cv.Dispose()
		found := false
		for _, item := range tab.v.items {
			if item.id == "taskmanager" {
				found = item.ctl == ctlCheckBox && item.name == "Restore Windows Task Manager" && item.selected && item.action
			}
		}
		if !found {
			t.Error("replacement control is missing its accessible restore action")
		}
		tab.viewH = 400
		tab.v.keyHit = "recdisk"
		pageKeyForTest(t, tab, walk.KeyTab, 0)
		if tab.v.keyHit != "taskmanager" || tab.scroll == 0 {
			t.Errorf("Tab did not reveal Task Manager setting: focus=%q scroll=%d", tab.v.keyHit, tab.scroll)
		}
		// A second activation during an in-flight change must be a no-op.
		// This path uses the real page handler without launching any helper.
		tab.taskManager.busy = true
		pageKeyForTest(t, tab, walk.KeyReturn, 0)
		if !tab.taskManager.busy {
			t.Error("keyboard activation replaced the pending operation")
		}
	})
}
