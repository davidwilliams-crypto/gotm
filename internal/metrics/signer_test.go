//go:build windows

package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSignerNames(t *testing.T) {
	sdk := filepath.Join(`D:\Claude\rea\gotm\third_party\RyzenMasterSDK-msi`, "platform.dll")
	if _, err := os.Stat(sdk); err == nil {
		n, _ := SignerName(sdk)
		t.Logf("Platform.dll signed by %q", n)
		if err := signedByAMD(sdk); err != nil {
			t.Errorf("AMD's own Platform.dll: %v", err)
		}
	}
	// A validly signed Windows file is still not AMD's.
	sys, _ := windows.GetSystemDirectory()
	for _, f := range []string{"ntoskrnl.exe", "notepad.exe", "cmd.exe"} {
		p := filepath.Join(sys, f)
		name, err := SignerName(p)
		if err != nil {
			continue // catalog-signed files carry no embedded signature
		}
		t.Logf("%s signed by %q", f, name)
		if !strings.Contains(name, "Microsoft") {
			t.Errorf("%s signer = %q", f, name)
		}
		if signedByAMD(p) == nil {
			t.Errorf("%s accepted as AMD-signed", f)
		}
		return
	}
	t.Log("no embedded-signature system file found to cross-check")
}
