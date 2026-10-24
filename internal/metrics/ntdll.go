//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modNtdll                     = windows.NewLazySystemDLL("ntdll.dll")
	procNtQuerySystemInformation = modNtdll.NewProc("NtQuerySystemInformation")
	procNtSuspendProcess         = modNtdll.NewProc("NtSuspendProcess")
	procNtResumeProcess          = modNtdll.NewProc("NtResumeProcess")

	modKernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64          = modKernel32.NewProc("GetTickCount64")
	procGetProcessAffinityMask  = modKernel32.NewProc("GetProcessAffinityMask")
	procSetProcessAffinityMask  = modKernel32.NewProc("SetProcessAffinityMask")
	procGetActiveProcessorCount = modKernel32.NewProc("GetActiveProcessorCount")

	modPsapi               = windows.NewLazySystemDLL("psapi.dll")
	procGetPerformanceInfo = modPsapi.NewProc("GetPerformanceInfo")
)

const (
	sysInfoProcess              = 5 // SystemProcessInformation
	sysInfoProcessorPerformance = 8 // SystemProcessorPerformanceInformation
	statusInfoLengthMismatch    = 0xC0000004

	// SYSTEM_PROCESS_INFORMATION field offsets (x64).
	offNextEntry   = 0x00
	offThreads     = 0x04
	offCreateTime  = 0x20
	offUserTime    = 0x28
	offKernelTime  = 0x30
	offImageLen    = 0x38
	offImagePtr    = 0x40
	offBasePrio    = 0x48
	offPID         = 0x50
	offPPID        = 0x58
	offHandles     = 0x60
	offSession     = 0x64
	offWorkingSet  = 0x90
	offPagefile    = 0xB8
	offPrivatePage = 0xC8
	offReadXfer    = 0xE8
	offWriteXfer   = 0xF0
	procInfoMinLen = 0x100

	coreEntrySize = 48
)

// rawProc is one process as reported by the kernel in a single snapshot.
type rawProc struct {
	PID, PPID  uint32
	Name       string
	Threads    uint32
	Handles    uint32
	Session    uint32
	BasePrio   int32
	CreateTime int64  // FILETIME (100ns since 1601)
	CPUTime    uint64 // user + kernel, 100ns units
	WorkingSet uint64
	Private    uint64
	Pagefile   uint64
	IOBytes    uint64 // read + write transfer bytes (cumulative)
}

// readProcesses calls NtQuerySystemInformation(SystemProcessInformation) and
// parses every process. *buf is reused between calls to avoid re-allocation.
func readProcesses(buf *[]byte) ([]rawProc, error) {
	size := len(*buf)
	if size < 1<<20 {
		size = 1 << 20
	}
	for attempt := 0; attempt < 8; attempt++ {
		if len(*buf) < size {
			*buf = make([]byte, size)
		}
		b := (*buf)[:size]
		var ret uint32
		r, _, _ := procNtQuerySystemInformation.Call(
			sysInfoProcess,
			uintptr(unsafe.Pointer(&b[0])),
			uintptr(size),
			uintptr(unsafe.Pointer(&ret)),
		)
		status := uint32(r)
		if status == statusInfoLengthMismatch {
			size = int(ret) + 128*1024
			continue
		}
		if status != 0 {
			return nil, fmt.Errorf("NtQuerySystemInformation(process): NTSTATUS 0x%08X", status)
		}
		return parseProcesses(b), nil
	}
	return nil, fmt.Errorf("NtQuerySystemInformation(process): buffer kept growing")
}

func parseProcesses(b []byte) []rawProc {
	out := make([]rawProc, 0, 256)
	le := binary.LittleEndian
	base := uintptr(unsafe.Pointer(&b[0]))
	off := 0
	for off+procInfoMinLen <= len(b) {
		p := b[off:]
		var rp rawProc
		rp.Threads = le.Uint32(p[offThreads:])
		rp.CreateTime = int64(le.Uint64(p[offCreateTime:]))
		rp.CPUTime = le.Uint64(p[offUserTime:]) + le.Uint64(p[offKernelTime:])
		rp.BasePrio = int32(le.Uint32(p[offBasePrio:]))
		rp.PID = uint32(le.Uint64(p[offPID:]))
		rp.PPID = uint32(le.Uint64(p[offPPID:]))
		rp.Handles = le.Uint32(p[offHandles:])
		rp.Session = le.Uint32(p[offSession:])
		rp.WorkingSet = le.Uint64(p[offWorkingSet:])
		rp.Pagefile = le.Uint64(p[offPagefile:])
		rp.Private = le.Uint64(p[offPrivatePage:])
		rp.IOBytes = le.Uint64(p[offReadXfer:]) + le.Uint64(p[offWriteXfer:])

		// ImageName is a UNICODE_STRING whose buffer lives inside b.
		nameLen := int(le.Uint16(p[offImageLen:]))
		namePtr := uintptr(le.Uint64(p[offImagePtr:]))
		if nameLen > 0 && namePtr >= base {
			no := int(namePtr - base)
			if no >= 0 && no+nameLen <= len(b) {
				u := make([]uint16, nameLen/2)
				for i := range u {
					u[i] = le.Uint16(b[no+i*2:])
				}
				rp.Name = string(utf16.Decode(u))
			}
		}
		switch {
		case rp.PID == 0:
			rp.Name = "System Idle Process"
		case rp.Name == "":
			rp.Name = fmt.Sprintf("pid %d", rp.PID)
		}
		out = append(out, rp)

		next := int(le.Uint32(p[offNextEntry:]))
		if next == 0 {
			break
		}
		off += next
	}
	return out
}

// coreTimes holds cumulative per-logical-CPU times in 100ns units.
// Kernel includes Idle, exactly as the kernel reports it.
type coreTimes struct{ Idle, Kernel, User uint64 }

// readCoreTimes returns one entry per logical CPU across all processor groups.
// NtQuerySystemInformation alone reports only the calling thread's group, so
// machines with more than 64 logical CPUs are read group by group.
func readCoreTimes(n int) ([]coreTimes, error) {
	groups, _, _ := procGetActiveProcessorGroupCount.Call()
	if groups > 1 {
		var all []coreTimes
		for g := uint16(0); g < uint16(groups); g++ {
			cnt, _, _ := procGetActiveProcessorCount.Call(uintptr(g))
			part, err := readGroupCoreTimes(g, int(cnt))
			if err != nil {
				return nil, err
			}
			all = append(all, part...)
		}
		return all, nil
	}
	if n < 1 {
		n = 1
	}
	b := make([]byte, n*coreEntrySize)
	var ret uint32
	r, _, _ := procNtQuerySystemInformation.Call(
		sysInfoProcessorPerformance,
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		uintptr(unsafe.Pointer(&ret)),
	)
	if r != 0 {
		return nil, fmt.Errorf("NtQuerySystemInformation(cpu): NTSTATUS 0x%08X", uint32(r))
	}
	cnt := int(ret) / coreEntrySize
	le := binary.LittleEndian
	out := make([]coreTimes, cnt)
	for i := 0; i < cnt; i++ {
		e := b[i*coreEntrySize:]
		out[i] = coreTimes{
			Idle:   le.Uint64(e[0:]),
			Kernel: le.Uint64(e[8:]),
			User:   le.Uint64(e[16:]),
		}
	}
	return out, nil
}

func readGroupCoreTimes(group uint16, n int) ([]coreTimes, error) {
	if n < 1 {
		n = 1
	}
	b := make([]byte, n*coreEntrySize)
	var ret uint32
	r, _, _ := procNtQuerySystemInformationEx.Call(
		sysInfoProcessorPerformance,
		uintptr(unsafe.Pointer(&group)), 2,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&ret)),
	)
	if r != 0 {
		return nil, fmt.Errorf("NtQuerySystemInformationEx(cpu, group %d): NTSTATUS 0x%08X", group, uint32(r))
	}
	le := binary.LittleEndian
	out := make([]coreTimes, int(ret)/coreEntrySize)
	for i := range out {
		e := b[i*coreEntrySize:]
		out[i] = coreTimes{Idle: le.Uint64(e[0:]), Kernel: le.Uint64(e[8:]), User: le.Uint64(e[16:])}
	}
	return out, nil
}

var (
	procNtQuerySystemInformationEx   = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformationEx")
	procGetActiveProcessorGroupCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetActiveProcessorGroupCount")
)

// logicalCPUs returns the number of active logical processors across all groups.
func logicalCPUs() int {
	r, _, _ := procGetActiveProcessorCount.Call(0xFFFF)
	if r == 0 {
		return 1
	}
	return int(r)
}

func uptimeSeconds() uint64 {
	r, _, _ := procGetTickCount64.Call()
	return uint64(r) / 1000
}
