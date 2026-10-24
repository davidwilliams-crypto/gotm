//go:build windows

package metrics

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// startOf reads a live process's creation time the way the sampler does.
func startOf(t *testing.T, pid uint32) int64 {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		t.Fatal(err)
	}
	return c.Nanoseconds() / 1e6
}

func TestActionsRefuseAReusedPID(t *testing.T) {
	sys, _ := windows.GetSystemDirectory()
	cmd := exec.Command(filepath.Join(sys, "PING.EXE"), "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := uint32(cmd.Process.Pid)
	real := startOf(t, pid)

	// the same PID with a different creation time stands for another process
	stale := Target{PID: pid, Start: real - 5000, Name: "old.exe"}
	for name, err := range map[string]error{
		"kill":     KillProcess(stale),
		"suspend":  Suspend(stale),
		"priority": SetPriority(stale, "Idle"),
	} {
		if err == nil || !strings.Contains(err.Error(), "belongs to another program") {
			t.Errorf("%s on a reused PID: err = %v", name, err)
		}
	}
	if err := KillProcess(Target{PID: pid, Start: real, Name: "PING.EXE"}); err != nil {
		t.Fatalf("killing the real process: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process survived KillProcess")
	}
}

func TestCriticalWindowsProcessesAreRefused(t *testing.T) {
	procs, err := readProcesses(new([]byte))
	if err != nil {
		t.Skip(err)
	}
	sys, _ := windows.GetSystemDirectory()
	for _, p := range procs {
		if !strings.EqualFold(p.Name, "csrss.exe") && !strings.EqualFold(p.Name, "winlogon.exe") {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.PID)
		if err != nil {
			continue
		}
		why := criticalReason(h)
		windows.CloseHandle(h)
		if why == "" {
			t.Errorf("%s (PID %d) from %s not recognised as critical", p.Name, p.PID, sys)
		}
		return
	}
	t.Skip("no csrss/winlogon visible")
}

func TestDescendantsIgnoreStaleParents(t *testing.T) {
	procs := []Proc{
		{PID: 100, PPID: 1, Name: "launcher.exe", Start: 2000},
		{PID: 200, PPID: 100, Name: "child.exe", Start: 2500},
		{PID: 300, PPID: 200, Name: "grandchild.exe", Start: 2600},
		// started long before PID 100 existed: its real parent died and the PID was reused
		{PID: 400, PPID: 100, Name: "explorer.exe", Start: 500},
	}
	got := Descendants(Target{PID: 100, Start: 2000}, procs)
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "grandchild.exe,child.exe" {
		t.Errorf("descendants = %v, want grandchild.exe,child.exe (deepest first, no explorer.exe)", names)
	}
	if d := Descendants(Target{PID: 100, Start: 1999}, procs); d != nil {
		t.Errorf("a root that is now a different process must yield nothing, got %v", d)
	}
}
