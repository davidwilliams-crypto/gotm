//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows preserves an 8.3 spelling in os.Executable. Registry ownership is
// intentionally textual, so select the recorded spelling only after proving
// that both the loaded directory and helper are the recorded native objects.
func (l layout) uninstallLayout(self string) (layout, error) {
	self, err := normalizeDestinationPath(self)
	if err != nil {
		return l, err
	}
	if !strings.EqualFold(filepath.Base(self), "uninstall.exe") {
		return l, fmt.Errorf("uninstall must run through the installed uninstall.exe")
	}
	l.dir = filepath.Dir(self)
	_, registeredDir := l.installed()
	if registeredDir == "" {
		return l, fmt.Errorf("cannot safely determine the registered installation before uninstalling")
	}
	loadedDir, err := os.Stat(l.dir)
	if err != nil {
		return l, fmt.Errorf("inspect loaded installation directory: %w", err)
	}
	registered, err := os.Stat(registeredDir)
	if err != nil {
		return l, fmt.Errorf("inspect registered installation directory: %w", err)
	}
	if !loadedDir.IsDir() || !registered.IsDir() {
		return l, fmt.Errorf("cannot establish installation directory identity")
	}
	helper, err := os.Stat(self)
	if err != nil {
		return l, fmt.Errorf("inspect loaded uninstall helper: %w", err)
	}
	if !helper.Mode().IsRegular() {
		return l, fmt.Errorf("uninstall helper is not a regular file")
	}
	if os.SameFile(loadedDir, registered) {
		registeredHelper, err := os.Stat(filepath.Join(registeredDir, "uninstall.exe"))
		if err != nil {
			return l, fmt.Errorf("inspect registered uninstall helper: %w", err)
		}
		if !os.SameFile(helper, registeredHelper) {
			return l, fmt.Errorf("loaded helper does not match the registered uninstall helper")
		}
		l.dir = registeredDir
		return l, nil
	}
	// A genuinely stale helper can remove its old payload while preserving
	// the newer registration. Refuse an aliased stale location: its historical
	// registry spelling is unknown, so deleting files could strand its route.
	p, err := windows.UTF16PtrFromString(self)
	if err != nil {
		return l, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		return l, fmt.Errorf("resolve stale uninstall helper: %w", err)
	}
	if n >= uint32(len(buf)) || !strings.EqualFold(self, windows.UTF16ToString(buf[:n])) {
		return l, fmt.Errorf("cannot safely uninstall an aliased stale installation; use its full original folder path")
	}
	return l, nil
}
