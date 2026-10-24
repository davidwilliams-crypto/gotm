//go:build windows

// Package safepath guards file writes an elevated gotm makes inside the
// user's own profile (%APPDATA%\gotm). Programs running as that user can
// replace folders there with junctions or symbolic links, which would aim an
// administrator's write or delete at a file of their choosing. When gotm is
// elevated, such redirected paths are simply not written to.
package safepath

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// elevated is a variable so tests can exercise the elevated behaviour.
var elevated = func() bool { return windows.GetCurrentProcessToken().IsElevated() }

// IsLink reports whether path itself is a reparse point (junction, symbolic
// link, mount point). A missing path is not a link.
func IsLink(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attrs, err := windows.GetFileAttributes(p)
	return err == nil && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// Redirected reports, in an elevated process only, whether dir or any of its
// three nearest parents (e.g. recorder, gotm, Roaming, AppData) is a link.
func Redirected(dir string) bool {
	if !elevated() {
		return false
	}
	p := filepath.Clean(dir)
	for i := 0; i < 4; i++ {
		if IsLink(p) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return false
}

// WritableFile reports whether an elevated process may create or append to
// path: its folder is not redirected and the file is not itself a link.
func WritableFile(path string) bool {
	if !elevated() {
		return true
	}
	if Redirected(filepath.Dir(path)) {
		return false
	}
	if st, err := os.Lstat(path); err == nil && st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return false
	}
	return !IsLink(path)
}
