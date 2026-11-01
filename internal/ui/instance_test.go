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

// ownInstanceNamespace points the guard at throw-away object names so a real
// gotm running in the session (which owns the production names) cannot make
// these tests fail — and the tests cannot make a real launch refuse to start.
// The suffix rides in an environment variable so the re-exec'd child process
// of the second-process test lands in the SAME namespace as its parent.
func ownInstanceNamespace(t *testing.T) {
	t.Helper()
	suffix := os.Getenv("GOTM_TEST_INSTANCE_NS")
	if suffix == "" {
		suffix = fmt.Sprint(os.Getpid(), "-", time.Now().UnixNano())
		os.Setenv("GOTM_TEST_INSTANCE_NS", suffix)
	}
	oldMutex, oldEvent := instanceMutexName, activationEventName
	instanceMutexName = `Local\gotm.test.` + suffix + ".mutex"
	activationEventName = `Local\gotm.test.` + suffix + ".ready"
	t.Cleanup(func() {
		instanceMutexName, activationEventName = oldMutex, oldEvent
		ReleaseSingleInstance()
	})
}

func TestGuardSingleInstanceClaimRelease(t *testing.T) {
	ownInstanceNamespace(t)
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
func TestGuardSingleInstanceRefusesSecondProcess(t *testing.T) {
	ownInstanceNamespace(t)
	old := activationWait
	activationWait = 10 * time.Millisecond
	t.Cleanup(func() { activationWait = old })

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
