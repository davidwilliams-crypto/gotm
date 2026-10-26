//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Reject aliases before filepath.Clean can erase evidence of an unsafe input.
// Machine installs only accept ordinary local drive paths.
func normalizeDestinationPath(path string) (string, error) {
	// Slash direction and one final directory separator are ordinary Windows
	// spellings. Do not clean dots, spaces, streams or parent components.
	path = strings.ReplaceAll(path, "/", `\`)
	path = strings.TrimSuffix(path, `\`)
	if len(path) < 4 || path[1] != ':' || path[2] != '\\' ||
		!((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) {
		return "", fmt.Errorf("installation needs an absolute local drive folder: %q", path)
	}
	for _, part := range strings.Split(path[3:], `\`) {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, "<>:\"/|?*\x00") {
			return "", fmt.Errorf("ambiguous installation folder: %q", path)
		}
		for _, c := range part {
			if c < 32 {
				return "", fmt.Errorf("invalid installation folder: %q", path)
			}
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL"
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			switch base[3:] {
			case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
				reserved = true
			}
		}
		if reserved {
			return "", fmt.Errorf("reserved installation folder: %q", path)
		}
	}
	return path, nil
}

type destinationGuard struct{ handles []windows.Handle }

type destinationVolume struct {
	driveType                                         uint32
	driveDevice, volumeDevice, mountVolume, finalRoot string
}

// A drive letter can name a SUBST folder or mapped network share. Its root
// handle can therefore hide real, mutable ancestors from the traversal below.
// Accept only a fixed local volume whose DOS device and pinned root both agree
// with the volume mount manager's identity; never persist an alias as /D=.
func checkDestinationVolume(info destinationVolume) error {
	if info.driveType != windows.DRIVE_FIXED || info.driveDevice == "" || info.volumeDevice == "" ||
		info.mountVolume == "" || info.finalRoot == "" ||
		!strings.EqualFold(info.driveDevice, info.volumeDevice) || !strings.EqualFold(info.mountVolume, info.finalRoot) {
		return fmt.Errorf("installation needs a physical local fixed-volume drive, not a substituted or network drive")
	}
	return nil
}

func queryDestinationVolume(root string, h windows.Handle) (destinationVolume, error) {
	var info destinationVolume
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return info, err
	}
	info.driveType = windows.GetDriveType(p)
	if info.driveType != windows.DRIVE_FIXED {
		return info, nil
	}
	queryDevice := func(name string) (string, error) {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return "", err
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		if _, err = windows.QueryDosDevice(p, &buf[0], uint32(len(buf))); err != nil {
			return "", err
		}
		return windows.UTF16ToString(buf), nil
	}
	info.driveDevice, err = queryDevice(root[:2])
	if err != nil {
		return info, err
	}
	volume := make([]uint16, 128)
	if err = windows.GetVolumeNameForVolumeMountPoint(p, &volume[0], uint32(len(volume))); err != nil {
		return info, err
	}
	info.mountVolume = windows.UTF16ToString(volume)
	if !strings.HasPrefix(info.mountVolume, `\\?\Volume{`) || !strings.HasSuffix(info.mountVolume, `}\`) {
		return info, fmt.Errorf("unexpected installation volume identity")
	}
	info.volumeDevice, err = queryDevice(strings.TrimSuffix(strings.TrimPrefix(info.mountVolume, `\\?\`), `\`))
	if err != nil {
		return info, err
	}
	final := make([]uint16, windows.MAX_LONG_PATH)
	const volumeNameGUID = 1
	n, err := windows.GetFinalPathNameByHandle(h, &final[0], uint32(len(final)), volumeNameGUID)
	if err != nil {
		return info, err
	}
	if n >= uint32(len(final)) {
		return info, fmt.Errorf("installation root identity exceeds path limit")
	}
	info.finalRoot = windows.UTF16ToString(final[:n])
	return info, nil
}

func (g *destinationGuard) close() {
	for i := len(g.handles) - 1; i >= 0; i-- {
		windows.CloseHandle(g.handles[i])
	}
}

// Every ancestor remains open without FILE_SHARE_DELETE until registration is
// complete. A checked directory cannot be renamed or replaced during writes.
// inspectSecurity is an unexported ACL-inspection seam for temporary HKCU tests;
// path syntax, reparse checks and handle pinning always run.
func (l layout) protectDestination() (_ *destinationGuard, err error) {
	l.dir, err = normalizeDestinationPath(l.dir)
	if err != nil {
		return nil, err
	}
	g := &destinationGuard{}
	defer func() {
		if err != nil {
			g.close()
		}
	}()
	inspect := l.inspectSecurity
	if inspect == nil {
		inspect = inspectDestinationSecurity
	}
	open := func(path string, target, directory bool) error {
		p, e := windows.UTF16PtrFromString(path)
		if e != nil {
			return e
		}
		h, e := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_DATA,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if e != nil {
			return e
		}
		g.handles = append(g.handles, h)
		var info windows.ByHandleFileInformation
		if e = windows.GetFileInformationByHandle(h, &info); e != nil {
			return e
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("reparse point is not a safe installation destination: %s", path)
		}
		if (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
			return fmt.Errorf("unexpected installation object type: %s", path)
		}
		if e = inspect(h, target); e != nil {
			return fmt.Errorf("unsafe installation path %s: %w", path, e)
		}
		return nil
	}
	path := l.dir[:3]
	if err = open(path, false, true); err != nil {
		return nil, err
	}
	queryVolume := l.queryVolume
	if queryVolume == nil {
		queryVolume = queryDestinationVolume
	}
	volume, err := queryVolume(path, g.handles[0])
	if err != nil {
		return nil, fmt.Errorf("inspect installation volume: %w", err)
	}
	if err = checkDestinationVolume(volume); err != nil {
		return nil, err
	}
	parts := strings.Split(l.dir[3:], `\`)
	for i, part := range parts {
		path = filepath.Join(path, part)
		err = open(path, i == len(parts)-1, true)
		if os.IsNotExist(err) {
			// The verified parent is pinned before creating each component.
			if err = os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
				return nil, err
			}
			err = open(path, i == len(parts)-1, true)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, name := range ownedFiles {
		if err = open(filepath.Join(l.dir, name), true, false); os.IsNotExist(err) {
			err = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		// The parent disallows untrusted replacement; release the file so our
		// own atomic rename can replace it. Its ACL disallows untrusted writes.
		windows.CloseHandle(g.handles[len(g.handles)-1])
		g.handles = g.handles[:len(g.handles)-1]
	}
	return g, nil
}

func inspectDestinationSecurity(h windows.Handle, target bool) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkDestinationSecurity(sd, target)
}

func trustedInstallerPrincipal(sid *windows.SID) bool {
	if sid == nil || !sid.IsValid() {
		return false
	}
	switch sid.String() {
	case "S-1-5-18", "S-1-5-32-544", "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464":
		return true
	}
	return false
}

// Inspect the descriptor rather than testing the elevated installer's token:
// an administrator can write both secure and insecure destinations.
func checkDestinationSecurity(sd *windows.SECURITY_DESCRIPTOR, target bool) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !trustedInstallerPrincipal(owner) {
		return fmt.Errorf("owner is not SYSTEM, Administrators or TrustedInstaller")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("missing discretionary ACL")
	}
	// Ancestors may grant create-subdirectory (C:\ commonly does); they may
	// not grant deletion/replacement, descriptor changes or other mutation.
	mutation := uint32(windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.FILE_WRITE_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | 0x40) // FILE_DELETE_CHILD
	if target {
		mutation |= windows.FILE_APPEND_DATA
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		inheritOnly := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0
		if inheritOnly && !target {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue // conservative: do not subtract denies
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Header.AceSize < uint16(unsafe.Sizeof(*ace)) {
				return fmt.Errorf("invalid ACL entry")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			// CREATOR OWNER is replaced with the new object's owner. Newly
			// created files are inspected separately before writing payload data.
			if inheritOnly && sid.IsValid() && sid.String() == "S-1-3-0" {
				continue
			}
			if !trustedInstallerPrincipal(sid) && uint32(ace.Mask)&mutation != 0 {
				return fmt.Errorf("ACL grants untrusted mutation")
			}
		default:
			return fmt.Errorf("unsupported ACL entry type %d", ace.Header.AceType)
		}
	}
	return nil
}
