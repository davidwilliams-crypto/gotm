//go:build windows

package ui

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGuardSingleInstanceClaimRelease(t *testing.T) {
	t.Cleanup(ReleaseSingleInstance)
	if !GuardSingleInstance() {
		t.Fatal("the first claim was refused")
	}
	ReleaseSingleInstance()
	if !GuardSingleInstance() {
		t.Fatal("claiming after release was refused")
	}
}

// A second claim from the SAME thread cannot be tested through the mutex
// (WaitForSingleObject reads a mutex owned by the calling thread as acquired,
// recursively); the refusals that matter come from another process.
// Note: the tests share the session-wide Local\gotm.* namespace with a real
// gotm running at the same time (dev machine reality) — that is exactly what
// the guard is for.
func TestGuardSingleInstanceRefusesSecondProcess(t *testing.T) {
	old, oldTakeover := activationWait, takeoverWait
	activationWait, takeoverWait = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { activationWait, takeoverWait = old, oldTakeover; ReleaseSingleInstance() })

	if os.Getenv("GOTM_TEST_INSTANCE_CHILD") == "1" {
		if !GuardSingleInstance() {
			os.Exit(3)
		}
		fmt.Println("READY")
		time.Sleep(30 * time.Second) // hold the slot; the parent kills us
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestGuardSingleInstanceRefusesSecondProcess$", "--")
	cmd.Env = append(os.Environ(), "GOTM_TEST_INSTANCE_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Skip(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("could not re-exec the test binary: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("the child process did not claim the instance slot: %q, %v", line, err)
	}
	if GuardSingleInstance() {
		cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatal("a second process's claim was accepted while the first held the slot")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if !GuardSingleInstance() {
		t.Fatal("claiming after the holder exited was refused")
	}
}
