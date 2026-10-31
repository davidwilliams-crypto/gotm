//go:build windows

package installpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormalizeRejectsShortNameAliases(t *testing.T) {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		t.Skip("no Program Files folder")
	}
	if long, ok := longPathOf(`C:\PROGRA~1`); !ok || !strings.EqualFold(long, pf) {
		t.Skip("this volume has no 8.3 alias for Program Files")
	}
	if _, err := NormalizeInstall(`C:\PROGRA~1\gotm`); err == nil || !strings.Contains(err.Error(), "8.3") {
		t.Fatalf("an 8.3-spelled destination was accepted: err = %v", err)
	}
	if _, err := Normalize(`C:\PROGRA~1\gotm`); err != nil {
		// resolving existing installs must keep accepting aliased spellings
		t.Fatalf("Normalize rejected a path that resolution stages must accept: %v", err)
	}
}

func TestNormalizeAcceptsLiteralTildeNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools~2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// "tools~2" is the directory's real long name (created by us): a tilde
	// alone must not be mistaken for an 8.3 alias.
	if _, err := Normalize(dir); err != nil {
		t.Fatalf("a literal tilde name was rejected: %v", err)
	}
}
