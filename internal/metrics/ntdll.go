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
	return readProcessesInto(buf, nil, nil)
}

// readProcessesInto is readProcesses appending to out[:0] and reusing the
// names of processes already in prev (same PID and creation time).
func readProcessesInto(buf *[]byte, out []rawProc, prev map[uint32]prevProc) ([]rawProc, error) {
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
			return out[:0], fmt.Errorf("NtQuerySystemInformation(process): NTSTATUS 0x%08X", status)
		}
		return parseProcessesInto(b, out, prev), nil
	}
	return out[:0], fmt.Errorf("NtQuerySystemInformation(process): buffer kept growing")
}

func parseProcesses(b []byte) []rawProc { return parseProcessesInto(b, nil, nil) }

// parseProcessesInto parses into out[:0]. A process whose PID and creation
// time match an entry of prev keeps that entry's name: image names never
// change, so only new processes are decoded (ASCII without a UTF-16 pass).
func parseProcessesInto(b []byte, out []rawProc, prev map[uint32]prevProc) []rawProc {
	out = out[:0]
	if len(b) == 0 {
		return out
	}
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

		if old, ok := prev[rp.PID]; ok && old.name != "" && old.create == rp.CreateTime {
			rp.Name = old.name
		} else {
			// ImageName is a UNICODE_STRING whose buffer lives inside b.
			nameLen := int(le.Uint16(p[offImageLen:]))
			namePtr := uintptr(le.Uint64(p[offImagePtr:]))
			if nameLen > 0 && namePtr >= base {
				no := int(namePtr - base)
				if no >= 0 && no+nameLen <= len(b) {
					rp.Name = decodeUTF16LE(b[no : no+nameLen&^1])
				}
			}
			switch {
			case rp.PID == 0:
				rp.Name = "System Idle Process"
			case rp.Name == "":
				rp.Name = fmt.Sprintf("pid %d", rp.PID)
			}
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

// decodeUTF16LE decodes little-endian UTF-16 bytes; pure ASCII (nearly every
// image name) costs one allocation.
func decodeUTF16LE(b []byte) string {
	n := len(b) / 2
	ascii := true
	for i := 0; i < n; i++ {
		if b[2*i+1] != 0 || b[2*i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		s := make([]byte, n)
		for i := range s {
			s[i] = b[2*i]
		}
		return unsafe.String(unsafe.SliceData(s), n) // s is never written again
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// coreTimes holds cumulative per-logical-CPU times in 100ns units.
// Kernel includes Idle, exactly as the kernel reports it.
type coreTimes struct{ Idle, Kernel, User uint64 }

// readCoreTimes returns one entry per logical CPU across all processor groups.
// NtQuerySystemInformation alone reports only the calling thread's group, so
// machines with more than 64 logical CPUs are read group by group.
func readCoreTimes(n int) ([]coreTimes, error) {
	var buf []byte
	return readCoreTimesInto(n, nil, &buf)
}

// readCoreTimesInto is readCoreTimes appending to dst[:0] and reusing *buf.
func readCoreTimesInto(n int, dst []coreTimes, buf *[]byte) ([]coreTimes, error) {
	dst = dst[:0]
	groups, _, _ := procGetActiveProcessorGroupCount.Call()
	if groups > 1 {
		for g := uint16(0); g < uint16(groups); g++ {
			cnt, _, _ := procGetActiveProcessorCount.Call(uintptr(g))
			var err error
			if dst, err = readGroupCoreTimes(g, int(cnt), dst, buf); err != nil {
				return dst[:0], err
			}
		}
		return dst, nil
	}
	if n < 1 {
		n = 1
	}
	if cap(*buf) < n*coreEntrySize {
		*buf = make([]byte, n*coreEntrySize)
	}
	b := (*buf)[:n*coreEntrySize]
	var ret uint32
	r, _, _ := procNtQuerySystemInformation.Call(
		sysInfoProcessorPerformance,
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		uintptr(unsafe.Pointer(&ret)),
	)
	if r != 0 {
		return dst, fmt.Errorf("NtQuerySystemInformation(cpu): NTSTATUS 0x%08X", uint32(r))
	}
	return appendCoreTimes(dst, b, int(ret)/coreEntrySize), nil
}

func appendCoreTimes(dst []coreTimes, b []byte, cnt int) []coreTimes {
	le := binary.LittleEndian
	for i := 0; i < cnt && (i+1)*coreEntrySize <= len(b); i++ {
		e := b[i*coreEntrySize:]
		dst = append(dst, coreTimes{Idle: le.Uint64(e[0:]), Kernel: le.Uint64(e[8:]), User: le.Uint64(e[16:])})
	}
	return dst
}

func readGroupCoreTimes(group uint16, n int, dst []coreTimes, buf *[]byte) ([]coreTimes, error) {
	if n < 1 {
		n = 1
	}
	if cap(*buf) < n*coreEntrySize {
		*buf = make([]byte, n*coreEntrySize)
	}
	b := (*buf)[:n*coreEntrySize]
	var ret uint32
	r, _, _ := procNtQuerySystemInformationEx.Call(
		sysInfoProcessorPerformance,
		uintptr(unsafe.Pointer(&group)), 2,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&ret)),
	)
	if r != 0 {
		return dst, fmt.Errorf("NtQuerySystemInformationEx(cpu, group %d): NTSTATUS 0x%08X", group, uint32(r))
	}
	return appendCoreTimes(dst, b, int(ret)/coreEntrySize), nil
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
