//go:build windows

package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSplitCommandKeepsConsecutiveSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My  App") // two spaces: a legal NTFS spelling
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	unin := filepath.Join(dir, "un.exe")
	if err := os.WriteFile(unin, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, args := splitCommand(unin+" /S", false)
	if !strings.EqualFold(exe, unin) || args != "/S" {
		t.Fatalf("splitCommand = (%q, %q), want (%q, /S)", exe, args, unin)
	}
}

func TestUninstallCommandRefusesProfileTargetsWhileElevated(t *testing.T) {
	if _, err := windows.KnownFolderPath(windows.FOLDERID_UserProfiles, 0); err != nil {
		t.Skip("no profiles root on this machine")
	}
	// t.TempDir() lives under the profiles root, which is the point here.
	unin := filepath.Join(t.TempDir(), "un.exe")
	if err := os.WriteFile(unin, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := UninstallCommand(InstalledApp{Uninstall: unin + " /S", Scope: "Machine"}, true)
	if err == nil || !strings.Contains(err.Error(), "user profile") {
		t.Fatalf("an elevated run of a profile-resident uninstaller was not refused: err = %v", err)
	}
	if _, _, err := UninstallCommand(InstalledApp{Uninstall: unin + " /S", Scope: "Machine"}, false); err != nil {
		t.Fatalf("the same uninstaller without elevation should be allowed: %v", err)
	}
}

func TestInUserProfile(t *testing.T) {
	profiles, err := windows.KnownFolderPath(windows.FOLDERID_UserProfiles, 0)
	if err != nil || profiles == "" {
		t.Skip("no profiles root on this machine")
	}
	if !inUserProfile(filepath.Join(profiles, "someone", "App", "un.exe")) {
		t.Error("a path under the profiles root was not detected as user-profile")
	}
	if !inUserProfile(filepath.Join(profiles, "someone")) {
		t.Error("the profiles root prefix alone should be handled")
	}
	if inUserProfile(profiles + "-elsewhere\\un.exe") {
		t.Error("a sibling of the profiles root was misclassified")
	}
	if pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0); err == nil && inUserProfile(filepath.Join(pf, "App", "un.exe")) {
		t.Error("Program Files was misclassified as a user profile")
	}
	if inUserProfile("C:\\") {
		t.Error("a drive root was misclassified")
	}
}
