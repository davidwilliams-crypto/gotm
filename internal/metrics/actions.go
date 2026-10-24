//go:build windows

package metrics

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	processSuspendResume = 0x0800
	processSetInfo       = 0x0200
	processQueryInfo     = 0x0400
)

// PriorityClasses maps a UI name to the Win32 priority class flag.
var PriorityClasses = map[string]uint32{
	"Idle":        windows.IDLE_PRIORITY_CLASS,
	"BelowNormal": windows.BELOW_NORMAL_PRIORITY_CLASS,
	"Normal":      windows.NORMAL_PRIORITY_CLASS,
	"AboveNormal": windows.ABOVE_NORMAL_PRIORITY_CLASS,
	"High":        windows.HIGH_PRIORITY_CLASS,
	"Realtime":    windows.REALTIME_PRIORITY_CLASS,
}

// IsProtected reports PIDs that must never be targeted by process actions.
func IsProtected(pid uint32) bool {
	return pid == 0 || pid == 4 || pid == uint32(os.Getpid())
}

// Target is one process instance. Windows reuses process IDs, so every action
// also carries the creation time the user saw (Proc.Start, Unix ms) and is
// refused if the PID now belongs to a different process.
type Target struct {
	PID   uint32
	Start int64
	Name  string
}

func (t Target) String() string {
	if t.Name != "" {
		return fmt.Sprintf("“%s” (PID %d)", t.Name, t.PID)
	}
	return fmt.Sprintf("PID %d", t.PID)
}

// TargetOf names the process p.
func TargetOf(p Proc) Target { return Target{PID: p.PID, Start: p.Start, Name: p.Name} }

func friendly(err error, what string, t Target) error {
	if err == windows.ERROR_ACCESS_DENIED {
		return fmt.Errorf("access denied while trying to %s %s (try Run as administrator)", what, t)
	}
	return fmt.Errorf("%s %s: %w", what, t, err)
}

// criticalNames are the Windows processes whose loss or freeze takes the whole
// session down. They only count when they run from System32: malware may use
// the same names elsewhere, and it must stay killable.
var criticalNames = map[string]bool{
	"csrss.exe": true, "wininit.exe": true, "smss.exe": true, "services.exe": true,
	"lsass.exe": true, "winlogon.exe": true, "dwm.exe": true, "lsaiso.exe": true,
}

const processBreakOnTermination = 29 // PROCESSINFOCLASS

// criticalReason explains why gotm will not change process h, or returns "".
func criticalReason(h windows.Handle) string {
	var flag uint32
	if windows.NtQueryInformationProcess(h, processBreakOnTermination, unsafe.Pointer(&flag), 4, nil) == nil && flag != 0 {
		return "it is marked critical, and Windows stops with a blue screen if it ends"
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	path := windows.UTF16ToString(buf[:n])
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	if criticalNames[strings.ToLower(filepath.Base(path))] && strings.EqualFold(filepath.Dir(path), sys) {
		return "it is a core Windows process; ending or freezing it signs you out or crashes Windows"
	}
	return ""
}

// openTarget opens t with access, after checking it is still the same process
// and not one Windows depends on (unless allowCritical, used for Resume).
func openTarget(t Target, access uint32, what string, allowCritical bool) (windows.Handle, error) {
	if IsProtected(t.PID) {
		return 0, fmt.Errorf("%s is protected", t)
	}
	h, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, t.PID)
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return 0, fmt.Errorf("%s has already exited", t)
		}
		return 0, friendly(err, what, t)
	}
	if t.Start != 0 {
		var created, exited, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil || created.Nanoseconds()/1e6 != t.Start {
			windows.CloseHandle(h)
			return 0, fmt.Errorf("%s has exited; its process ID now belongs to another program, which was left alone", t)
		}
	}
	if !allowCritical {
		if why := criticalReason(h); why != "" {
			windows.CloseHandle(h)
			return 0, fmt.Errorf("gotm will not %s %s: %s", what, t, why)
		}
	}
	return h, nil
}

// KillProcess terminates one process instance.
func KillProcess(t Target) error {
	h, err := openTarget(t, windows.PROCESS_TERMINATE, "end", false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return friendly(err, "end", t)
	}
	return nil
}

// Descendants returns the processes started, directly or indirectly, by root,
// deepest first (so callers can end children before parents). A process only
// counts as a child when its parent started before it: Windows reuses PIDs, so
// an orphan can name a dead parent's PID that now belongs to someone else.
func Descendants(root Target, procs []Proc) []Target {
	byPID := make(map[uint32]*Proc, len(procs))
	for i := range procs {
		byPID[procs[i].PID] = &procs[i]
	}
	if r, ok := byPID[root.PID]; !ok || (root.Start != 0 && r.Start != root.Start) {
		return nil // root is gone or is now a different process
	}
	children := map[uint32][]*Proc{}
	for i := range procs {
		p := &procs[i]
		par, ok := byPID[p.PPID]
		if !ok || p.PID == p.PPID || (par.Start > 0 && p.Start > 0 && par.Start > p.Start) {
			continue
		}
		children[p.PPID] = append(children[p.PPID], p)
	}
	seen := map[uint32]bool{root.PID: true}
	var out []Target
	var walk func(uint32, int)
	walk = func(id uint32, depth int) {
		if depth > 64 {
			return
		}
		for _, c := range children[id] {
			if seen[c.PID] {
				continue
			}
			seen[c.PID] = true
			walk(c.PID, depth+1)
			out = append(out, TargetOf(*c))
		}
	}
	walk(root.PID, 0)
	return out
}

// KillTree ends exactly the processes in desc (as shown to the user), then
// root. Each one is checked again before it is ended.
func KillTree(root Target, desc []Target) error {
	var errs []error
	for _, t := range append(append([]Target(nil), desc...), root) {
		if err := KillProcess(t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetPriority changes a process priority class by UI name.
func SetPriority(t Target, class string) error {
	flag, ok := PriorityClasses[class]
	if !ok {
		return fmt.Errorf("unknown priority %q", class)
	}
	h, err := openTarget(t, processSetInfo, "change the priority of", false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.SetPriorityClass(h, flag); err != nil {
		return friendly(err, "change the priority of", t)
	}
	return nil
}

// Affinity returns the process and system affinity masks.
func Affinity(t Target) (process, system uint64, err error) {
	h, err := openTarget(t, 0, "read the affinity of", true)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	r, _, e := procGetProcessAffinityMask.Call(uintptr(h),
		uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&system)))
	if r == 0 {
		return 0, 0, friendly(e, "read the affinity of", t)
	}
	return process, system, nil
}

// SetAffinity restricts a process to the logical CPUs in mask (processor group 0).
func SetAffinity(t Target, mask uint64) error {
	if mask == 0 {
		return fmt.Errorf("affinity mask must include at least one CPU")
	}
	h, err := openTarget(t, processSetInfo, "change the affinity of", false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if r, _, e := procSetProcessAffinityMask.Call(uintptr(h), uintptr(mask)); r == 0 {
		if e == windows.ERROR_INVALID_PARAMETER {
			return fmt.Errorf("Windows rejected that affinity for %s (it may use processors in more than one group)", t)
		}
		return friendly(e, "change the affinity of", t)
	}
	return nil
}

// Suspend / Resume use the native NtSuspendProcess / NtResumeProcess. Resume is
// allowed on critical processes so a mistake can always be undone.
func Suspend(t Target) error { return suspendResume(t, true) }
func Resume(t Target) error  { return suspendResume(t, false) }

func suspendResume(t Target, suspend bool) error {
	what := "resume"
	if suspend {
		what = "suspend"
	}
	h, err := openTarget(t, processSuspendResume, what, !suspend)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	proc := procNtResumeProcess
	if suspend {
		proc = procNtSuspendProcess
	}
	if r, _, _ := proc.Call(uintptr(h)); r != 0 {
		return fmt.Errorf("%s %s: NTSTATUS 0x%08X", what, t, uint32(r))
	}
	return nil
}

// OpenFileLocation reveals a process image in Explorer.
func OpenFileLocation(pid uint32) error {
	p := ProcessPath(pid)
	if p == "" {
		return fmt.Errorf("image path for PID %d is not accessible", pid)
	}
	// Explorer needs the whole /select argument as one token.
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + p + `"`}
	return cmd.Start()
}

// IsElevated reports whether this process runs with an elevated token.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// RelaunchElevated starts a new elevated copy of this executable (UAC prompt).
func RelaunchElevated(args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	var a *uint16
	if args != "" {
		a, _ = windows.UTF16PtrFromString(args)
	}
	return windows.ShellExecute(0, verb, file, a, nil, windows.SW_NORMAL)
}
