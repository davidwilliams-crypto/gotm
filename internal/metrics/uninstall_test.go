//go:build windows

package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestUninstallCommandResolvesLikeWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files", "My App")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	unin := filepath.Join(dir, "uninst.exe")
	if err := os.WriteFile(unin, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	sys, _ := windows.GetSystemDirectory()

	cases := []struct {
		in, exe, args string
	}{
		{unin + " /S", unin, "/S"},                      // unquoted path with spaces
		{`"` + unin + `" /x & calc`, unin, "/x & calc"}, // & is just an argument, no shell
		{"MsiExec.exe /X{1234}", filepath.Join(sys, "MsiExec.exe"), "/X{1234}"},
	}
	for _, c := range cases {
		exe, line, err := UninstallCommand(InstalledApp{Uninstall: c.in, Scope: "Machine"}, true)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !strings.EqualFold(exe, c.exe) {
			t.Errorf("%q: exe = %q, want %q", c.in, exe, c.exe)
		}
		if want := `"` + exe + `" ` + c.args; line != want {
			t.Errorf("%q: line = %q, want %q", c.in, line, want)
		}
	}

	if _, _, err := UninstallCommand(InstalledApp{Uninstall: unin, Scope: "User"}, true); err != ErrUserScopeElevated {
		t.Errorf("per-user entry while elevated: err = %v", err)
	}
	if _, _, err := UninstallCommand(InstalledApp{Uninstall: unin, Scope: "User"}, false); err != nil {
		t.Errorf("per-user entry without elevation should run: %v", err)
	}
	if _, _, err := UninstallCommand(InstalledApp{Uninstall: `relative\uninst.exe`, Scope: "Machine"}, false); err == nil {
		t.Error("relative paths must not resolve against the current directory")
	}
}

func TestUninstallExpandsWithSystemVariablesOnly(t *testing.T) {
	win, _ := windows.GetWindowsDirectory()
	t.Setenv("windir", `C:\Users\Public\evil`) // what malware could set in HKCU\Environment
	got, err := expandSystem(`%windir%\system32\sdbinst.exe -u x`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.ToLower(got), strings.ToLower(win)+`\system32\`) {
		t.Errorf("expanded to %q, want the real Windows folder %q", got, win)
	}
}

func TestEveryUninstallEntryOnThisPCResolves(t *testing.T) {
	var bad []string
	n := 0
	for _, a := range ReadInstalledApps() {
		if a.Uninstall == "" {
			continue
		}
		n++
		if _, _, err := UninstallCommand(a, false); err != nil {
			bad = append(bad, a.Name+": "+a.Uninstall)
		}
	}
	t.Logf("%d uninstall entries, %d unresolved", n, len(bad))
	for _, b := range bad {
		t.Log("  unresolved:", b)
	}
}
