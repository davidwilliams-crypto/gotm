//go:build windows

package installpath

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProtectExistingOnlyDoesNotCreateMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "install")
	policy := Policy{
		InspectSecurity: func(windows.Handle, bool) error { return nil },
		QueryVolume:     testPhysicalVolume,
	}
	g, err := Protect(missing, nil, false, policy)
	if g != nil {
		g.Close()
		t.Fatal("returned guard for a missing directory")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("missing directory error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("existing-only guard created a directory: %v", err)
	}
}

func TestProtectExistingOnlyRejectsNativeUnsafeUserFolder(t *testing.T) {
	// A temporary user-owned directory must fail the production owner/ACL
	// inspection, even when the directory already exists.
	dir := t.TempDir()
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := InspectSecurity(h, true); err == nil {
		t.Fatal("native ACL policy accepted user-owned directory")
	}
	g, err := Protect(dir, nil, false, Policy{QueryVolume: testPhysicalVolume})
	if g != nil {
		g.Close()
		t.Fatal("returned guard for unsafe user-owned directory")
	}
	if err == nil {
		t.Fatal("accepted unsafe user-owned directory")
	}
}

func testPhysicalVolume(string, windows.Handle) (Volume, error) {
	return Volume{
		DriveType:    windows.DRIVE_FIXED,
		DriveDevice:  `\Device\HarddiskVolume3`,
		VolumeDevice: `\Device\HarddiskVolume3`,
		MountVolume:  `\\?\Volume{physical}\`,
		FinalRoot:    `\\?\Volume{physical}\`,
	}, nil
}
