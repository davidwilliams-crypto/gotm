//go:build windows

package main

import (
	"archive/zip"
	"bytes"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/sys/windows/registry"
)

// testLayout installs into temporary folders and a scratch HKCU key, so the
// real install and uninstall code runs without touching Program Files or HKLM.
func testLayout(t *testing.T) layout {
	t.Helper()
	root := t.TempDir()
	l := layout{
		inspectSecurity: func(windows.Handle, bool) error { return nil },
		dir:             filepath.Join(root, "Program Files", "gotm"),
		startMenu:       filepath.Join(root, "Start Menu"),
		desktop:         filepath.Join(root, "Desktop"),
		regRoot:         registry.CURRENT_USER,
		regPath:         `Software\gotm-setup-test\` + strings.ReplaceAll(t.Name(), "/", "_"),
	}
	for _, d := range []string{l.startMenu, l.desktop} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, l.regPath)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\gotm-setup-test`)
	})
	return l
}

func payloadV(ver string) fstest.MapFS {
	return fstest.MapFS{
		"gotm.exe":                {Data: []byte("MZ fake gotm " + ver)},
		"README.txt":              {Data: []byte("readme " + ver)},
		"THIRD-PARTY-NOTICES.txt": {Data: []byte("notices")},
		"PLACEHOLDER.txt":         {Data: []byte("must not be installed")},
	}
}

func TestInstallUpgradeUninstall(t *testing.T) {
	l := testLayout(t)
	self := filepath.Join(t.TempDir(), "gotm-setup.exe")
	if err := os.WriteFile(self, []byte("MZ fake setup"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := l.install(payloadV("1"), self, "0.8.0", true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"gotm.exe", "README.txt", "THIRD-PARTY-NOTICES.txt", "uninstall.exe"} {
		if _, err := os.Stat(filepath.Join(l.dir, f)); err != nil {
			t.Errorf("%s not installed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.dir, "PLACEHOLDER.txt")); err == nil {
		t.Error("placeholder must not be installed")
	}
	for _, d := range []string{l.startMenu, l.desktop} {
		if st, err := os.Stat(filepath.Join(d, "gotm.lnk")); err != nil || st.Size() == 0 {
			t.Errorf("shortcut missing in %s: %v", d, err)
		}
	}
	// the shortcut must open gotm.exe (read back through the shell's own API)
	{
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(New-Object -ComObject WScript.Shell).CreateShortcut($env:LNK).TargetPath")
		cmd.Env = append(os.Environ(), "LNK="+filepath.Join(l.startMenu, "gotm.lnk"))
		got, err := cmd.Output()
		if err != nil {
			t.Fatalf("read shortcut: %v", err)
		}
		if !strings.EqualFold(strings.TrimSpace(string(got)), filepath.Join(l.dir, "gotm.exe")) {
			t.Errorf("shortcut target = %q", strings.TrimSpace(string(got)))
		}
	}
	ver, dir := l.installed()
	if ver != "0.8.0" || !strings.EqualFold(dir, l.dir) {
		t.Errorf("registry says %q in %q", ver, dir)
	}
	k, err := registry.OpenKey(l.regRoot, l.regPath, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	unin, _, _ := k.GetStringValue("UninstallString")
	quiet, _, _ := k.GetStringValue("QuietUninstallString")
	k.Close()
	if !strings.Contains(unin, `uninstall.exe" /uninstall`) || !strings.HasSuffix(quiet, "/uninstall /S") {
		t.Errorf("uninstall strings: %q / %q", unin, quiet)
	}

	// upgrade in place replaces the files
	if err := l.install(payloadV("2"), self, "0.8.1", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "gotm.exe")); string(b) != "MZ fake gotm 2" {
		t.Errorf("upgrade did not replace gotm.exe: %q", b)
	}
	if ver, _ := l.installed(); ver != "0.8.1" {
		t.Errorf("version after upgrade = %q", ver)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "gotm.exe.new")); err == nil {
		t.Error("temporary file left behind")
	}

	// a file the user added must survive uninstall
	mine := filepath.Join(l.dir, "my-notes.txt")
	if err := os.WriteFile(mine, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.uninstall(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"gotm.exe", "README.txt", "THIRD-PARTY-NOTICES.txt"} {
		if _, err := os.Stat(filepath.Join(l.dir, f)); err == nil {
			t.Errorf("%s still present after uninstall", f)
		}
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error("uninstall deleted a file it did not install")
	}
	for _, d := range []string{l.startMenu, l.desktop} {
		if _, err := os.Stat(filepath.Join(d, "gotm.lnk")); err == nil {
			t.Errorf("shortcut left in %s", d)
		}
	}
	if ver, _ := l.installed(); ver != "" {
		t.Error("Apps & features entry left behind")
	}
	// uninstalling twice is harmless
	if err := l.uninstall(); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestInstallRefusesWhileGotmRuns(t *testing.T) {
	// Run a copy of this test binary as "gotm.exe" from the install folder.
	l := testLayout(t)
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	me, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(me)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(l.dir, "gotm.exe")
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := execHelper(exe)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	waitRunning(t, exe)

	if err := l.install(payloadV("1"), "", "0.8.0", false); !errRunning(err) {
		t.Fatalf("install while running: err = %v, want errGotmRunning", err)
	}
	if err := l.uninstall(); !errRunning(err) {
		t.Fatalf("uninstall while running: err = %v, want errGotmRunning", err)
	}
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"/S", `/D="C:\Tools\gotm"`, "/desktop"})
	if err != nil || !o.silent || !o.desktop || o.dir != `C:\Tools\gotm` {
		t.Errorf("parse: %+v %v", o, err)
	}
	if _, err := parseArgs([]string{"/D=relative"}); err == nil {
		t.Error("relative /D= must be rejected")
	}
	if _, err := parseArgs([]string{"/bogus"}); err == nil {
		t.Error("unknown option must be rejected")
	}
	if o, _ := parseArgs([]string{"/uninstall", "/s"}); !o.uninstall || !o.silent {
		t.Errorf("uninstall flags: %+v", o)
	}
}

// The release installer carries its files as one zip; install must accept it.
func TestInstallFromZipPayload(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"gotm.exe": "MZ zipped", "README.txt": "r", "THIRD-PARTY-NOTICES.txt": "n"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	l := testLayout(t)
	l.startMenu, l.desktop = "", ""
	if err := l.install(zr, "", "0.8.0", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "gotm.exe")); string(b) != "MZ zipped" {
		t.Errorf("gotm.exe from zip = %q", b)
	}
	if err := l.uninstall(); err != nil {
		t.Fatal(err)
	}
}
