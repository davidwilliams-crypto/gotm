//go:build windows

package metrics

import (
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// RyzenProbe starts the AMD Ryzen Monitoring SDK inside the calling process
// in several ways and writes one line per attempt to w. It exists because the
// SDK can behave differently inside the app (GUI COM runtime, System32-only
// DLL search) than in a test binary; "gotm.exe --ryzen-probe FILE", run as
// administrator, shows which. It never touches the machine-wide switch.
func RyzenProbe(w io.Writer, phase string) {
	fmt.Fprintf(w, "[%s] elevated=%v\n", phase, IsElevated())
	dir, err := ryzenSDKDir()
	if err != nil {
		fmt.Fprintf(w, "[%s] sdk dir: %v\n", phase, err)
		return
	}
	platformDLL := filepath.Join(dir, "Platform.dll")
	h, err := windows.LoadLibraryEx(platformDLL, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		fmt.Fprintf(w, "[%s] load %s: %v\n", phase, platformDLL, err)
		return
	}
	defer windows.FreeLibrary(h)
	getPlatform, err := windows.GetProcAddress(h, "GetPlatform")
	if err != nil {
		fmt.Fprintf(w, "[%s] GetPlatform export: %v\n", phase, err)
		return
	}
	platform, _, _ := syscall.SyscallN(getPlatform)
	if platform == 0 {
		fmt.Fprintf(w, "[%s] GetPlatform returned null\n", phase)
		return
	}
	r := newRyzenSDKWithMarker(regMarker{})
	vendor := boardVendor()
	// The SDK does not survive many UnInit/Init cycles in one process (the
	// third one here crashed it), so the probe stops at the first success.
	attempts := []struct {
		name    string
		vendor  string
		cpuOnly bool
	}{
		{"Init(NULL, false)", "", false}, // what the app does
		{"Init(NULL, true)", "", true},   // CPU only: no BIOS device
		{fmt.Sprintf("Init(%q, false)", vendor), vendor, false},
	}
	for _, a := range attempts {
		var vp *byte
		if a.vendor != "" {
			vp, _ = windows.BytePtrFromString(a.vendor)
		}
		cpu, why := r.initPlatform(platform, vp, a.cpuOnly)
		if cpu == 0 {
			fmt.Fprintf(w, "[%s] %s: FAIL: %s\n", phase, a.name, why)
			continue
		}
		var buf [ryzenParamsBuf]byte
		rc := int32(vcall(cpu, slotCPUGetCPUParams, uintptr(unsafe.Pointer(&buf[0]))))
		rd, derr := decodeRyzenParams((*ryzenParams)(unsafe.Pointer(&buf[0])), time.Now())
		name, pkg := r.Identity()
		fmt.Fprintf(w, "[%s] %s: OK %s (%s) GetCPUParameters rc=%d temp=%.1f ppt=%.1f cores=%d decode=%v\n",
			phase, a.name, name, pkg, rc, rd.TempC, rd.PPT, len(rd.Cores), derr)
		vcall(platform, slotPlatformUnInit)
		return
	}
}

// boardVendor is the motherboard manufacturer the firmware reports.
func boardVendor() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue("BaseBoardManufacturer")
	return v
}
