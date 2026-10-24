//go:build windows

package metrics

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dblohm7/wingoes/com"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The tests in this file exercise what only an administrator can do: control
// a service, end a process that runs as SYSTEM, and read the AMD Ryzen
// Monitoring SDK. They skip under an ordinary token; tools/elevated-check.ps1
// runs the compiled test binary through a UAC prompt and saves the log.

const (
	helperFlag    = "-gotm-service-helper"
	selfTestSvc   = "gotm-selftest-svc"
	elevatedSkip  = "needs an elevated token: run tools\\elevated-check.ps1"
	svcTestChild  = "PING.EXE"
	svcTestChildN = "120" // the child pings for two minutes unless the test ends it
)

// TestMain lets the test binary double as the self-test service: the Service
// Control Manager starts it with helperFlag, and it then runs as SYSTEM.
func TestMain(m *testing.M) {
	for _, a := range os.Args[1:] {
		if a == helperFlag {
			runServiceHelper()
			return
		}
	}
	// GOTM_SDK_EMULATE reproduces, one at a time, what the app's process has
	// that this test binary does not: the System32-only DLL search and the GUI
	// toolkit's process-wide COM runtime (implicit MTA + CoInitializeSecurity).
	switch os.Getenv("GOTM_SDK_EMULATE") {
	case "dlldirs":
		_ = windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	case "comsec":
		_ = com.StartRuntime(com.GUIApp)
	case "both":
		_ = windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
		_ = com.StartRuntime(com.GUIApp)
	}
	os.Exit(m.Run())
}

// helperService is a minimal Windows service: it starts one long-lived child
// process (so the test has a SYSTEM-owned process to end) and stops on request.
type helperService struct{}

func (helperService) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	sys, _ := windows.GetSystemDirectory()
	child := exec.Command(filepath.Join(sys, svcTestChild), "-n", svcTestChildN, "127.0.0.1")
	child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = child.Start()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			if child.Process != nil {
				_ = child.Process.Kill()
			}
			s <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

func runServiceHelper() {
	_ = svc.Run(selfTestSvc, helperService{})
}

func requireElevated(t *testing.T) {
	t.Helper()
	if !IsElevated() {
		t.Skip(elevatedSkip)
	}
}

// childOf finds a running process by parent PID and image name, with the
// creation time gotm's actions verify.
func childOf(ppid uint32, name string) (Target, string, bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return Target{}, "", false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID != ppid || !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			return Target{}, "", false
		}
		var created, exited, k, u windows.Filetime
		_ = windows.GetProcessTimes(h, &created, &exited, &k, &u)
		user := processUser(h)
		windows.CloseHandle(h)
		return Target{PID: pe.ProcessID, Start: created.Nanoseconds() / 1e6, Name: name}, user, true
	}
	return Target{}, "", false
}

func processUser(h windows.Handle) string {
	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) != nil {
		return ""
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return ""
	}
	acct, dom, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return tu.User.Sid.String()
	}
	return dom + `\` + acct
}

func processAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

func waitState(t *testing.T, s *mgr.Service, want svc.State) svc.Status {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := s.Query()
		if err != nil {
			t.Fatalf("query service: %v", err)
		}
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("service state %d, want %d", st.State, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestElevatedServiceControlAndSystemProcess installs a throw-away service
// that runs this test binary as SYSTEM, drives it through gotm's
// ServiceControl (start, restart, stop), and ends the SYSTEM-owned child it
// spawns through gotm's KillProcess. The service is removed afterwards.
func TestElevatedServiceControlAndSystemProcess(t *testing.T) {
	requireElevated(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatalf("connect to the service manager: %v", err)
	}
	defer m.Disconnect()
	if old, err := m.OpenService(selfTestSvc); err == nil { // a previous run that died
		_, _ = old.Control(svc.Stop)
		_ = old.Delete()
		old.Close()
		// a service marked for deletion disappears only once it has stopped
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			s, err := m.OpenService(selfTestSvc)
			if err != nil {
				break
			}
			s.Close()
			time.Sleep(250 * time.Millisecond)
		}
	}
	s, err := m.CreateService(selfTestSvc, exe, mgr.Config{
		DisplayName: "gotm self-test service (safe to delete)",
		Description: "Created by gotm's elevated self-test; removed when the test ends.",
		StartType:   mgr.StartManual,
	}, helperFlag)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	defer func() {
		_, _ = s.Control(svc.Stop)
		if err := s.Delete(); err != nil {
			t.Errorf("delete service: %v", err)
		}
		s.Close()
	}()

	if err := ServiceControl(selfTestSvc, "start"); err != nil {
		t.Fatalf("ServiceControl start: %v", err)
	}
	st := waitState(t, s, svc.Running)
	t.Logf("service running as PID %d", st.ProcessId)

	var child Target
	var user string
	for i := 0; i < 50; i++ {
		var ok bool
		if child, user, ok = childOf(st.ProcessId, svcTestChild); ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if child.PID == 0 {
		t.Fatal("the service did not start its child process")
	}
	t.Logf("SYSTEM child: %s owned by %q", child, user)
	if !strings.HasSuffix(strings.ToUpper(user), `\SYSTEM`) {
		t.Errorf("child owner %q, want the SYSTEM account", user)
	}
	// a stale creation time must be refused even with an elevated token
	stale := child
	stale.Start--
	if err := KillProcess(stale); err == nil {
		t.Error("KillProcess accepted a process whose creation time did not match")
	}
	if err := KillProcess(child); err != nil {
		t.Fatalf("KillProcess on a SYSTEM process: %v", err)
	}
	for i := 0; i < 50 && processAlive(child.PID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(child.PID) {
		t.Fatal("the SYSTEM child is still running")
	}

	if err := ServiceControl(selfTestSvc, "restart"); err != nil {
		t.Fatalf("ServiceControl restart: %v", err)
	}
	st2 := waitState(t, s, svc.Running)
	if st2.ProcessId == st.ProcessId {
		t.Errorf("restart kept PID %d", st.ProcessId)
	}
	if err := ServiceControl(selfTestSvc, "stop"); err != nil {
		t.Fatalf("ServiceControl stop: %v", err)
	}
	waitState(t, s, svc.Stopped)
	t.Log("start, restart and stop through ServiceControl all worked")
}

// TestElevatedProcessActions runs every process action on a child of this
// test with an elevated token.
func TestElevatedProcessActions(t *testing.T) {
	requireElevated(t)
	sys, _ := windows.GetSystemDirectory()
	cmd := exec.Command(filepath.Join(sys, "PING.EXE"), "-n", "60", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	tg, _, ok := childOf(uint32(os.Getpid()), "PING.EXE")
	if !ok {
		t.Fatal("child not found")
	}
	for _, class := range []string{"High", "BelowNormal", "Normal"} {
		if err := SetPriority(tg, class); err != nil {
			t.Errorf("SetPriority %s: %v", class, err)
		}
	}
	if err := SetAffinity(tg, 1); err != nil {
		t.Errorf("SetAffinity: %v", err)
	}
	if mask, _, err := Affinity(tg); err != nil || mask != 1 {
		t.Errorf("Affinity = %x, %v; want 1", mask, err)
	}
	if err := Suspend(tg); err != nil {
		t.Errorf("Suspend: %v", err)
	}
	if err := Resume(tg); err != nil {
		t.Errorf("Resume: %v", err)
	}
	if err := KillProcess(tg); err != nil {
		t.Errorf("KillProcess: %v", err)
	}
	t.Log("priority, affinity, suspend, resume and end all worked elevated")
}

// TestElevatedRyzenSDK starts the AMD Ryzen Monitoring SDK the way the app
// does (HKLM crash marker, signed Platform.dll) and checks for a plausible
// reading. Like the app, it sets HKLM\SOFTWARE\gotm\RyzenSDKPending while
// the SDK initialises; if the SDK crashed the process here, the app would
// refuse to start the SDK until Settings clears the marker, which is the
// intended protection.
func TestElevatedRyzenSDK(t *testing.T) {
	requireElevated(t)
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\AMD\RyzenMasterMonitoringSDK`, registry.QUERY_VALUE); err != nil {
		t.Skip("AMD Ryzen Master Monitoring SDK is not installed")
	} else {
		k.Close()
	}
	t.Logf("emulation: %q", os.Getenv("GOTM_SDK_EMULATE"))
	r := NewRyzenSDK()
	r.Start()
	defer r.Stop()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, detail := r.State()
		if st == RyzenRunning {
			break
		}
		if st == RyzenUnavailable {
			t.Fatalf("SDK unavailable: %s", detail)
		}
		if time.Now().After(deadline) {
			t.Fatalf("SDK did not start: state %d (%s)", st, detail)
		}
		time.Sleep(250 * time.Millisecond)
	}
	var rd RyzenReading
	ok := false
	for i := 0; i < 40 && !ok; i++ {
		rd, ok = r.Reading()
		time.Sleep(250 * time.Millisecond)
	}
	if !ok {
		t.Fatal("no reading from the SDK")
	}
	cpu, pkg := r.Identity()
	t.Logf("SDK running: %s (%s) temp=%.1f °C ppt=%.1f W peak=%.0f MHz cores=%d", cpu, pkg, rd.TempC, rd.PPT, rd.PeakMHz, len(rd.Cores))
	if rd.TempC < 10 || rd.TempC > 120 {
		t.Errorf("implausible Tctl %.1f", rd.TempC)
	}
	if len(rd.Cores) == 0 {
		t.Error("no per-core readings")
	}
	fmt.Fprintf(os.Stderr, "ryzen sdk ok: %s %.1f C\n", cpu, rd.TempC)
}
