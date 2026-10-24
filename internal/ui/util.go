//go:build windows

package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// hiddenWindowAttr runs a command line without flashing a console window.
func hiddenWindowAttr(cmdline string) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CmdLine: cmdline, HideWindow: true}
}

// openInExplorer reveals a file (selected, never run) or opens a folder.
// Paths that do not exist are ignored rather than handed to Explorer.
func openInExplorer(path string) {
	st, err := os.Stat(path)
	if path == "" || err != nil {
		return
	}
	windir, err := windows.GetWindowsDirectory()
	if err != nil {
		return
	}
	explorer := filepath.Join(windir, "explorer.exe")
	cmd := exec.Command(explorer)
	if st.IsDir() {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + explorer + `" "` + path + `"`}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + explorer + `" /select,"` + path + `"`}
	}
	_ = cmd.Start()
}
