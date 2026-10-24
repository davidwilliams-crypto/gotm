//go:build windows

package safepath

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRedirectedFoldersAreRefusedWhenElevated(t *testing.T) {
	old := elevated
	elevated = func() bool { return true }
	defer func() { elevated = old }()

	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "gotm")
	// a directory junction needs no privilege, which is exactly the threat
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, real).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if !Redirected(link) || !Redirected(filepath.Join(link, "recorder")) {
		t.Error("a junction (or a folder inside one) must count as redirected")
	}
	if Redirected(real) {
		t.Error("an ordinary folder is not redirected")
	}
	if WritableFile(filepath.Join(link, "config.json")) {
		t.Error("a file inside a junction must not be writable when elevated")
	}
	if !WritableFile(filepath.Join(real, "config.json")) {
		t.Error("an ordinary file must be writable")
	}

	elevated = func() bool { return false }
	if Redirected(link) {
		t.Error("unelevated, the user's own links are the user's business")
	}
}
