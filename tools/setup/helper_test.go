//go:build windows

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// When the test binary is started as a helper it just waits; the parent test
// uses it as a stand-in for a running gotm.exe.
func TestMain(m *testing.M) {
	if os.Getenv("GOTM_SETUP_HELPER") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func execHelper(exe string) *exec.Cmd {
	c := exec.Command(exe, "-test.run=^$")
	c.Env = append(os.Environ(), "GOTM_SETUP_HELPER=1")
	return c
}

func waitRunning(t *testing.T, exe string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if runningFrom(exe) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("helper process never showed up")
}
