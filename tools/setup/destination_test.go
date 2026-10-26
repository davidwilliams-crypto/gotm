//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestInstallRejectsDriveAliasesBeforeWrites(t *testing.T) {
	physical := destinationVolume{windows.DRIVE_FIXED, `\Device\HarddiskVolume3`, `\Device\HarddiskVolume3`, `\\?\Volume{physical}\`, `\\?\Volume{physical}\`}
	for _, tt := range []struct {
		name     string
		info     destinationVolume
		queryErr error
		safe     bool
	}{
		{"ordinary physical drive", physical, nil, true},
		{"SUBST hides physical ancestors", destinationVolume{windows.DRIVE_FIXED, `\??\C:\Users\user\protected`, physical.volumeDevice, physical.mountVolume, physical.mountVolume + `Users\user\protected\`}, nil, false},
		{"mapped network root", destinationVolume{windows.DRIVE_REMOTE, `\Device\LanmanRedirector\server\share`, physical.volumeDevice, physical.mountVolume, physical.finalRoot}, nil, false},
		{"alias to volume root", destinationVolume{windows.DRIVE_FIXED, `\??\C:\`, physical.volumeDevice, physical.mountVolume, physical.finalRoot}, nil, false},
		{"different pinned root", destinationVolume{windows.DRIVE_FIXED, physical.driveDevice, physical.volumeDevice, physical.mountVolume, `\\?\Volume{different}\`}, nil, false},
		{"unknown root type", destinationVolume{windows.DRIVE_UNKNOWN, physical.driveDevice, physical.volumeDevice, physical.mountVolume, physical.finalRoot}, nil, false},
		{"volume API failure", destinationVolume{}, errors.New("volume lookup failed"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := testLayout(t)
			l.startMenu, l.desktop = "", ""
			queried := false
			l.queryVolume = func(root string, h windows.Handle) (destinationVolume, error) {
				queried = true
				return tt.info, tt.queryErr
			}
			err := l.install(payloadV("1"), "", "1", false)
			if (err == nil) != tt.safe {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tt.safe, err)
			}
			if !queried {
				t.Fatal("install skipped root-volume inspection")
			}
			if !tt.safe {
				if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
					t.Fatalf("unsafe volume created destination: %v", err)
				}
				k, err := registry.OpenKey(l.regRoot, l.regPath, registry.QUERY_VALUE)
				if err == nil {
					k.Close()
					t.Fatal("unsafe volume registered install")
				}
			}
		})
	}
}

func TestInstallRejectsUntrustedDestinationBeforeWrites(t *testing.T) {
	l := testLayout(t)
	l.inspectSecurity = nil
	// Use the production inspection policy rather than the fixture trust seam.
	if err := l.install(payloadV("unsafe"), "", "1.0.0", true); err == nil {
		t.Fatal("installer accepted a user-owned temporary destination")
	}
	if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
		t.Fatalf("unsafe install created destination: %v", err)
	}
	k, err := registry.OpenKey(l.regRoot, l.regPath, registry.QUERY_VALUE)
	if err == nil {
		k.Close()
		t.Fatal("unsafe install registered itself")
	}
}

func TestDestinationACLPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, sddl       string
		target, wantSafe bool
	}{
		{"protected custom", "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)", true, true},
		{"trusted installer owner", "O:S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464D:P(A;;FA;;;SY)", true, true},
		{"user owner despite protected DACL", "O:BUG:BAD:P(A;;FA;;;BA)", true, false},
		{"user write target", "O:BAD:P(A;;FA;;;BA)(A;;FW;;;BU)", true, false},
		{"ancestor create directory", "O:BAD:P(A;;FA;;;BA)(A;;0x4;;;AU)", false, true},
		{"target create directory", "O:BAD:P(A;;FA;;;BA)(A;;0x4;;;AU)", true, false},
		{"ancestor delete child", "O:BAD:P(A;;FA;;;BA)(A;;0x40;;;AU)", false, false},
		{"ancestor delete", "O:BAD:P(A;;FA;;;BA)(A;;SD;;;AU)", false, false},
		{"ancestor change DACL", "O:BAD:P(A;;FA;;;BA)(A;;WD;;;AU)", false, false},
		{"ancestor change owner", "O:BAD:P(A;;FA;;;BA)(A;;WO;;;AU)", false, false},
		{"ancestor reparse mutation", "O:BAD:P(A;;FA;;;BA)(A;;0x100;;;AU)", false, false},
		{"inherited child write", "O:BAD:P(A;OIIO;FW;;;BU)(A;;FA;;;BA)", true, false},
		{"unknown conditional grant", "O:BAD:P(XA;;FA;;;BU;(Member_of {SID(S-1-5-32-545)}))", true, false},
		{"null DACL", "O:BAD:NO_ACCESS_CONTROL", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tt.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = checkDestinationSecurity(sd, tt.target)
			if (err == nil) != tt.wantSafe {
				t.Fatalf("safe=%v, err=%v", tt.wantSafe, err)
			}
		})
	}
}

func TestDestinationNativeProtectedACL(t *testing.T) {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.VolumeName(pf) + `\`, pf} {
		p, _ := windows.UTF16PtrFromString(path)
		h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = inspectDestinationSecurity(h, path == pf)
		if err == nil && path != pf {
			var volume destinationVolume
			volume, err = queryDestinationVolume(path, h)
			if err == nil {
				err = checkDestinationVolume(volume)
			}
		}
		windows.CloseHandle(h)
		if err != nil {
			t.Fatalf("protected system location %s rejected: %v", path, err)
		}
	}
}

func TestDestinationPinsEveryAncestor(t *testing.T) {
	l := testLayout(t)
	g, err := l.protectDestination()
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	for _, path := range []string{l.dir, filepath.Dir(l.dir), filepath.Dir(filepath.Dir(l.dir))} {
		if err := os.Rename(path, path+"-moved"); err == nil {
			os.Rename(path+"-moved", path)
			t.Fatalf("checked path could be replaced: %s", path)
		}
	}
}

func TestInstallRejectsExistingOwnedFileSecurity(t *testing.T) {
	l := testLayout(t)
	if err := os.MkdirAll(l.dir, 0755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(l.dir, "gotm.exe")
	if err := os.WriteFile(exe, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	l.inspectSecurity = func(h windows.Handle, target bool) error {
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &info); err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return inspectDestinationSecurity(h, target)
		}
		return nil
	}
	if err := l.install(payloadV("replacement"), "", "1", false); err == nil {
		t.Fatal("untrusted existing executable accepted")
	}
	if data, _ := os.ReadFile(exe); string(data) != "original" {
		t.Fatal("existing executable changed")
	}
}

func TestInstallRejectsReparseDestinationAndAncestor(t *testing.T) {
	for _, atTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "ancestor", true: "target"}[atTarget], func(t *testing.T) {
			l := testLayout(t)
			redirect := t.TempDir()
			link := filepath.Dir(l.dir)
			if atTarget {
				if err := os.MkdirAll(link, 0755); err != nil {
					t.Fatal(err)
				}
				link = l.dir
			}
			out, err := exec.Command(os.Getenv("ComSpec"), "/d", "/c", "mklink", "/J", link, redirect).CombinedOutput()
			if err != nil {
				t.Fatalf("create junction: %v: %s", err, out)
			}
			if err := l.install(payloadV("redirected"), "", "1", true); err == nil || !strings.Contains(err.Error(), "reparse") {
				t.Fatalf("junction accepted: %v", err)
			}
			entries, err := os.ReadDir(redirect)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote redirected files: %v %v", entries, err)
			}
		})
	}
}

func TestInstalledRejectsAmbiguousRegistryPath(t *testing.T) {
	l := testLayout(t)
	k, _, err := registry.CreateKey(l.regRoot, l.regPath, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	for _, path := range []string{`C:\gotm.`, `C:\gotm:stream`, `\\server\share\gotm`} {
		if err := k.SetStringValue("InstallLocation", path); err != nil {
			t.Fatal(err)
		}
		if _, dir := l.installed(); dir != "" {
			t.Fatalf("accepted registry path %q", dir)
		}
	}
}

func TestParseRejectsAmbiguousDestinationPaths(t *testing.T) {
	for _, p := range []string{`\\server\share\gotm`, `\\?\C:\gotm`, `C:\gotm:stream`, `C:\gotm.`, `C:\gotm `, `C:\a\..\gotm`, `C:\a\.\gotm`, `C:\CON`, `C:\COM¹`, `C:\a.\gotm`} {
		t.Run(p, func(t *testing.T) {
			if _, err := parseArgs([]string{"/D=" + p}); err == nil {
				t.Fatalf("accepted ambiguous path %q", p)
			}
		})
	}
}

func TestParseProtectedCustomPathSpellings(t *testing.T) {
	for _, path := range []string{`C:\Tools\gotm`, `C:/Tools/gotm`, `C:\Tools\gotm\`, `C:/Tools/gotm/`} {
		o, err := parseArgs([]string{"/D=" + path})
		if err != nil || o.dir != `C:\Tools\gotm` {
			t.Fatalf("valid custom path %q: %+v %v", path, o, err)
		}
	}
}

func TestAtomicWriteDoesNotTouchPredictableStagingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gotm.exe")
	sentinel := p + ".new"
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(p, []byte("payload"), func(windows.Handle, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("predictable staging file changed: %q, %v", got, err)
	}
}

func TestPayloadRejectsUntrustedStagedFile(t *testing.T) {
	l := testLayout(t)
	l.inspectSecurity = nil
	if err := os.MkdirAll(l.dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := l.writePayload(filepath.Join(l.dir, "gotm.exe"), []byte("payload")); err == nil {
		t.Fatal("accepted user-owned staged payload")
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsafe staged payload was retained: %v %v", entries, err)
	}
}
