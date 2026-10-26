//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// layout says where everything goes; tests point it at temporary folders and
// a throw-away registry key.
type layout struct {
	dir             string // install folder
	startMenu       string // folder that receives the Start menu shortcut
	desktop         string // folder that receives the optional desktop shortcut
	regRoot         registry.Key
	regPath         string                                                  // the Apps & features (Uninstall) entry
	inspectSecurity func(windows.Handle, bool) error                        // nil uses production ACL policy
	queryVolume     func(string, windows.Handle) (destinationVolume, error) // nil uses native volume APIs
}

const uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\gotm`

// files that belong to gotm in its folder; uninstall removes exactly these.
var ownedFiles = []string{"gotm.exe", "README.txt", "THIRD-PARTY-NOTICES.txt", "uninstall.exe"}

func machineLayout() layout {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		pf = os.Getenv("ProgramFiles")
	}
	sm, _ := windows.KnownFolderPath(windows.FOLDERID_CommonPrograms, 0)
	dt, _ := windows.KnownFolderPath(windows.FOLDERID_PublicDesktop, 0)
	return layout{
		dir:       filepath.Join(pf, "gotm"),
		startMenu: sm,
		desktop:   dt,
		regRoot:   registry.LOCAL_MACHINE,
		regPath:   uninstallKey,
	}
}

// installed reports the version and folder of an existing installation.
func (l layout) installed() (ver, dir string) {
	k, err := registry.OpenKey(l.regRoot, l.regPath, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	ver, _, _ = k.GetStringValue("DisplayVersion")
	dir, _, _ = k.GetStringValue("InstallLocation")
	if dir != "" {
		dir, _ = normalizeDestinationPath(dir)
	}
	return ver, dir
}

// errGotmRunning means a gotm.exe from the install folder is still running.
var errGotmRunning = errors.New("gotm is running from the install folder")

func errRunning(err error) bool { return errors.Is(err, errGotmRunning) }

// install copies the payload, the uninstaller, shortcuts and the Apps &
// features entry. Re-running it over an existing install upgrades in place.
func (l layout) install(payload fs.FS, self, ver string, desktop bool) error {
	var err error
	l.dir, err = normalizeDestinationPath(l.dir)
	if err != nil {
		return err
	}
	guard, err := l.protectDestination()
	if err != nil {
		return err
	}
	defer guard.close()
	exe := filepath.Join(l.dir, "gotm.exe")
	if runningFrom(exe) {
		return errGotmRunning
	}
	var size int64
	err = fs.WalkDir(payload, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !owned(d.Name()) {
			return err
		}
		data, err := fs.ReadFile(payload, p)
		if err != nil {
			return err
		}
		size += int64(len(data))
		return l.writePayload(filepath.Join(l.dir, d.Name()), data)
	})
	if err != nil {
		return err
	}

	// the uninstaller is this program, copied next to gotm.exe
	unin := filepath.Join(l.dir, "uninstall.exe")
	if self != "" && !strings.EqualFold(filepath.Clean(self), unin) {
		data, err := os.ReadFile(self)
		if err != nil {
			return fmt.Errorf("copy uninstaller: %w", err)
		}
		size += int64(len(data))
		if err := l.writePayload(unin, data); err != nil {
			return err
		}
	}

	if l.startMenu != "" {
		if err := createShortcut(filepath.Join(l.startMenu, "gotm.lnk"), exe, l.dir, "gotm task manager"); err != nil {
			return fmt.Errorf("Start menu shortcut: %w", err)
		}
	}
	if desktop && l.desktop != "" {
		if err := createShortcut(filepath.Join(l.desktop, "gotm.lnk"), exe, l.dir, "gotm task manager"); err != nil {
			return fmt.Errorf("desktop shortcut: %w", err)
		}
	}
	return l.register(ver, size)
}

func owned(name string) bool {
	for _, f := range ownedFiles {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

func (l layout) register(ver string, size int64) error {
	k, _, err := registry.CreateKey(l.regRoot, l.regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("Apps & features entry: %w", err)
	}
	defer k.Close()
	unin := filepath.Join(l.dir, "uninstall.exe")
	strs := map[string]string{
		"DisplayName":          "gotm",
		"DisplayVersion":       ver,
		"Publisher":            "gotm",
		"InstallLocation":      l.dir,
		"DisplayIcon":          filepath.Join(l.dir, "gotm.exe") + ",0",
		"UninstallString":      `"` + unin + `" /uninstall`,
		"QuietUninstallString": `"` + unin + `" /uninstall /S`,
		"InstallDate":          time.Now().Format("20060102"),
	}
	for n, v := range strs {
		if err := k.SetStringValue(n, v); err != nil {
			return err
		}
	}
	for n, v := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": uint32(size / 1024)} {
		if err := k.SetDWordValue(n, v); err != nil {
			return err
		}
	}
	return nil
}

// uninstall removes gotm's own files, shortcuts and registry entry. It never
// deletes anything else in the folder; the folder itself goes only once empty.
func (l layout) uninstall() error {
	if runningFrom(filepath.Join(l.dir, "gotm.exe")) {
		return errGotmRunning
	}
	var errs []error
	for _, f := range ownedFiles {
		if f == "uninstall.exe" {
			continue // the running uninstaller; removed after it exits
		}
		for _, p := range []string{filepath.Join(l.dir, f), filepath.Join(l.dir, f+".new")} {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	for _, d := range []string{l.startMenu, l.desktop} {
		if d == "" {
			continue
		}
		if err := os.Remove(filepath.Join(d, "gotm.lnk")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := registry.DeleteKey(l.regRoot, l.regPath); err != nil && !errors.Is(err, registry.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// writeAtomic writes data beside the target and renames it into place, so a
// failed copy never leaves a truncated gotm.exe behind.
func (l layout) writePayload(path string, data []byte) error {
	return writeAtomic(path, data, l.inspectSecurity)
}

func writeAtomic(path string, data []byte, inspect func(windows.Handle, bool) error) error {
	if inspect == nil {
		inspect = inspectDestinationSecurity
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gotm-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := inspect(windows.Handle(f.Fd()), true); err != nil {
		f.Close()
		return fmt.Errorf("unsafe payload file: %w", err)
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		var en syscall.Errno
		if errors.As(err, &en) && (en == windows.ERROR_ACCESS_DENIED || en == windows.ERROR_SHARING_VIOLATION) &&
			strings.EqualFold(filepath.Base(path), "gotm.exe") {
			return errGotmRunning
		}
		return err
	}
	return nil
}

// runningFrom reports whether any process's image is exactly exe.
func runningFrom(exe string) bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), filepath.Base(exe)) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		ok := windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil
		windows.CloseHandle(h)
		if ok && strings.EqualFold(windows.UTF16ToString(buf[:n]), exe) {
			return true
		}
	}
	return false
}

// launchAsUser starts gotm through Explorer, so it runs as the signed-in user
// rather than inheriting this installer's administrator token.
func launchAsUser(exe string) {
	windir, err := windows.GetWindowsDirectory()
	if err != nil {
		return
	}
	_ = exec.Command(filepath.Join(windir, "explorer.exe"), exe).Start()
}

// deleteAfterExit removes the running uninstaller once it has exited, and then
// the install folder if nothing else is left in it (rmdir without /s).
func deleteAfterExit(self, dir string) {
	if strings.ContainsAny(self+dir, `"%^!&|<>`) {
		return // never hand cmd.exe a path it could reinterpret; leave the file
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return
	}
	cmd := filepath.Join(sys, "cmd.exe")
	ping := filepath.Join(sys, "PING.EXE")
	line := fmt.Sprintf(`"%s" /d /c ""%s" -n 3 127.0.0.1 >nul & del /f /q "%s" & rmdir "%s""`, cmd, ping, self, dir)
	c := exec.Command(cmd)
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: line, HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	_ = c.Start()
}
