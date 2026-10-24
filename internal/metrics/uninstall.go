//go:build windows

package metrics

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUserScopeElevated is returned for a per-user app while gotm runs elevated:
// its uninstall entry lives in HKCU, which any program running as that user
// can rewrite, so running it as administrator would hand that program admin.
// ErrRestartNeeded reports an uninstaller that finished but wants a restart.
var ErrRestartNeeded = errors.New("the uninstaller finished; Windows needs a restart to complete it")

var ErrUserScopeElevated = errors.New("this app was installed for one user only, and its uninstall entry can be changed by any program that user runs, so gotm will not start it as administrator. Uninstall it from gotm without elevation, or from Settings > Apps")

// UninstallCommand resolves an app's UninstallString into the program to run
// and the exact command line, without a shell:
//   - REG_EXPAND_SZ values are expanded with system variables only, never the
//     user's (who could redefine %windir%);
//   - "quoted paths" are taken as written;
//   - unquoted paths with spaces are resolved the way CreateProcess does, by
//     trying successively longer prefixes until one names an existing file;
//   - a bare program name (MsiExec.exe) is looked up in System32 and the
//     Windows folder only, never the current directory or PATH.
func UninstallCommand(a InstalledApp, elevated bool) (exe, cmdline string, err error) {
	if elevated && a.Scope == "User" {
		return "", "", ErrUserScopeElevated
	}
	s := strings.TrimSpace(a.Uninstall)
	if a.UninstallExpand {
		if s, err = expandSystem(s); err != nil {
			return "", "", err
		}
	}
	if s == "" {
		return "", "", errors.New("the app does not register an uninstaller")
	}
	exe, args := splitCommand(s, !elevated)
	if exe == "" {
		return "", "", fmt.Errorf("cannot find the uninstaller in %q", a.Uninstall)
	}
	cmdline = `"` + exe + `"`
	if args != "" {
		cmdline += " " + args
	}
	return exe, cmdline, nil
}

// splitCommand finds the program at the start of a command line. allowPath
// permits a PATH lookup for bare names, which is only safe unelevated.
func splitCommand(s string, allowPath bool) (exe, args string) {
	if strings.HasPrefix(s, `"`) {
		end := strings.Index(s[1:], `"`)
		if end < 0 {
			return "", ""
		}
		return resolveProgram(s[1:1+end], allowPath), strings.TrimSpace(s[2+end:])
	}
	fields := strings.Split(s, " ")
	for i := 1; i <= len(fields); i++ {
		cand := strings.Join(fields[:i], " ")
		if p := resolveProgram(cand, allowPath); p != "" {
			return p, strings.TrimSpace(strings.Join(fields[i:], " "))
		}
	}
	return "", ""
}

// resolveProgram returns an absolute path to an existing file, or "".
func resolveProgram(name string, allowPath bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	try := func(p string) string {
		for _, c := range []string{p, p + ".exe"} {
			if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
				return c
			}
			// app execution aliases (WindowsApps\*.exe) are reparse points Stat cannot follow
			if st, err := os.Lstat(c); err == nil && !st.IsDir() {
				return c
			}
		}
		return ""
	}
	if filepath.IsAbs(name) {
		return try(name)
	}
	if strings.ContainsAny(name, `\/:`) {
		return "" // relative paths would depend on the current directory
	}
	sys, _ := windows.GetSystemDirectory()
	win, _ := windows.GetWindowsDirectory()
	for _, dir := range []string{sys, win} {
		if dir == "" {
			continue
		}
		if p := try(filepath.Join(dir, name)); p != "" {
			return p
		}
	}
	if allowPath {
		if p, err := exec.LookPath(name); err == nil && filepath.IsAbs(p) {
			return p
		}
	}
	return ""
}

var (
	userenv                       = windows.NewLazySystemDLL("userenv.dll")
	procExpandEnvironmentForUserW = userenv.NewProc("ExpandEnvironmentStringsForUserW")
)

// expandSystem expands %VARS% using the system environment block only
// (ExpandEnvironmentStringsForUser with no token).
func expandSystem(s string) (string, error) {
	src, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 32*1024)
	r, _, e := procExpandEnvironmentForUserW.Call(0, uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return "", fmt.Errorf("expand %q: %w", s, e)
	}
	return windows.UTF16ToString(buf), nil
}

// StartUninstaller runs an app's uninstaller in its own visible window and
// returns a channel that reports how it ended.
func StartUninstaller(a InstalledApp, elevated bool) (<-chan error, error) {
	exe, line, err := UninstallCommand(a, elevated)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", exe, err)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			switch code := ee.ExitCode(); code {
			case 1602: // Windows Installer: cancelled by the user
				done <- nil
			case 3010, 1641: // finished, restart required / started
				done <- ErrRestartNeeded
			default:
				done <- fmt.Errorf("the uninstaller exited with code %d", code)
			}
		default:
			done <- err
		}
	}()
	return done, nil
}

// IsDirectory reports whether p names an existing folder (not a file Explorer
// would run if handed it).
func IsDirectory(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
