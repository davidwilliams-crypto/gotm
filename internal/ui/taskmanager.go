//go:build windows

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/walk"
	"golang.org/x/sys/windows/registry"

	"gotm/internal/installpath"
	"gotm/internal/taskmanager"
)

type taskManagerSetting struct {
	exe, helper string
	state       taskmanager.State
	err         error
	checkedAt   time.Time
	busy        bool
}

func taskManagerHelperPath(exe, installedDir string) (string, error) {
	dir, err := installpath.Normalize(installedDir)
	if err != nil {
		return "", fmt.Errorf("install gotm before changing the Windows Task Manager replacement")
	}
	exe, err = installpath.Normalize(exe)
	if err != nil || !strings.EqualFold(exe, filepath.Join(dir, "gotm.exe")) {
		return "", fmt.Errorf("open the installed copy of gotm to change the Windows Task Manager replacement")
	}
	return filepath.Join(dir, "uninstall.exe"), nil
}

func protectedTaskManagerHelper(exe, installedDir string) (string, *installpath.Guard, error) {
	helper, err := taskManagerHelperPath(exe, installedDir)
	if err != nil {
		return "", nil, err
	}
	for _, path := range []string{exe, helper} {
		st, err := os.Stat(path)
		if err != nil {
			return "", nil, fmt.Errorf("the installed gotm files are incomplete: %w", err)
		}
		if !st.Mode().IsRegular() {
			return "", nil, fmt.Errorf("the installed gotm files must be regular files")
		}
	}
	guard, err := installpath.Protect(filepath.Dir(helper),
		[]string{"gotm.exe", "README.txt", "THIRD-PARTY-NOTICES.txt", "uninstall.exe"}, false, installpath.Policy{})
	if err != nil {
		return "", nil, fmt.Errorf("Task Manager replacement requires a protected installation: %w", err)
	}
	return helper, guard, nil
}

func installedTaskManagerDir() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\gotm`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", fmt.Errorf("install gotm before changing the Windows Task Manager replacement")
	}
	defer k.Close()
	dir, typ, err := k.GetStringValue("InstallLocation")
	if err != nil || typ != registry.SZ {
		return "", fmt.Errorf("gotm's installed location is unavailable; run setup again")
	}
	return dir, nil
}

func (t *settingsTab) refreshTaskManager(force bool) {
	if t.taskManager.busy || !force && time.Since(t.taskManager.checkedAt) < 3*time.Second {
		return
	}
	s := taskManagerSetting{checkedAt: time.Now()}
	s.exe, s.err = os.Executable()
	if s.err == nil {
		s.state, s.err = taskmanager.MachineStore().Query(s.exe)
	}
	if s.err == nil {
		var dir string
		dir, s.err = installedTaskManagerDir()
		if s.err == nil {
			s.helper, s.err = taskManagerHelperPath(s.exe, dir)
		}
	}
	t.taskManager = s
}

func (t *settingsTab) taskManagerText() (label, note string) {
	s := t.taskManager
	label = "Replace Task Manager"
	note = "Off. Windows Task Manager opens normally."
	if s.state.Owned {
		label = "Restore Windows Task Manager"
		note = "On for this PC. Task Manager launches open gotm's Processes page."
		if s.state.Conflict {
			note = "A launch filter is also configured. Restore removes only gotm's replacement."
		}
	} else if s.state.Conflict {
		note = "Another replacement or launch filter is configured. It will be preserved."
	}
	if s.err != nil {
		note = s.err.Error()
	}
	if s.busy {
		label, note = "Applying change…", "Updating Windows Task Manager routing…"
	}
	return
}

func (t *settingsTab) changeTaskManager() {
	if t.taskManager.busy {
		return
	}
	a := t.app
	if !a.host.Elevated {
		walk.MsgBox(a.mw, "Windows Task Manager",
			"This setting applies to every user. Use “Run as administrator” first, then change it here.", walk.MsgBoxIconInformation)
		return
	}
	t.refreshTaskManager(true)
	s := t.taskManager
	if s.err != nil {
		a.errBox("Windows Task Manager", s.err)
		return
	}
	if s.state.Conflict && !s.state.Owned {
		a.errBox("Windows Task Manager", fmt.Errorf("another Task Manager replacement or launch filter is configured; restore it through that program first"))
		return
	}
	on := !s.state.Owned
	t.taskManager.busy = true
	t.v.invalidate()
	go func() {
		err := runTaskManagerHelper(s.exe, on)
		a.mw.Synchronize(func() {
			t.taskManager.busy = false
			t.refreshTaskManager(true)
			if err != nil {
				a.errBox("Windows Task Manager", err)
			}
			t.v.invalidate()
		})
	}()
}

func runTaskManagerHelper(exe string, on bool) error {
	dir, err := installedTaskManagerDir()
	if err != nil {
		return err
	}
	helper, guard, err := protectedTaskManagerHelper(exe, dir)
	if err != nil {
		return err
	}
	if guard != nil {
		defer guard.Close()
	}
	mode := "/taskmanager=off"
	if on {
		mode = "/taskmanager=on"
	}
	cmd := exec.Command(helper, mode, "/S")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return fmt.Errorf("%s", detail)
		}
		return fmt.Errorf("the installed helper could not change Task Manager: %w", err)
	}
	return nil
}
