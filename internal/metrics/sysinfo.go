//go:build windows

package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// HostInfo is static information about the machine.
type HostInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	CPUName  string `json:"cpuName"`
	NumCPU   int    `json:"numCpu"`
	TotalMem uint64 `json:"totalMem"`
	Elevated bool   `json:"elevated"`
	Self     int    `json:"selfPid"`
}

// Drive is a logical volume with capacity figures.
type Drive struct {
	Letter string `json:"letter"`
	Kind   string `json:"kind"`
	Total  uint64 `json:"total"`
	Free   uint64 `json:"free"`
}

// ReadHostInfo gathers the static machine description.
func ReadHostInfo() HostInfo {
	h := HostInfo{NumCPU: logicalCPUs(), Elevated: IsElevated(), Self: os.Getpid()}
	h.Hostname, _ = os.Hostname()
	if m, ok := readMemory(); ok {
		h.TotalMem = m.Total
	}

	h.OS = "Windows"
	if v := windows.RtlGetVersion(); v != nil {
		name := "Windows 10"
		if v.MajorVersion == 10 && v.BuildNumber >= 22000 {
			name = "Windows 11"
		}
		h.OS = fmt.Sprintf("%s (build %d)", name, v.BuildNumber)
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
			`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
			if dv, _, err := k.GetStringValue("DisplayVersion"); err == nil && dv != "" {
				h.OS = fmt.Sprintf("%s %s (build %d)", name, dv, v.BuildNumber)
			}
			k.Close()
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		h.CPUName, _, _ = k.GetStringValue("ProcessorNameString")
		k.Close()
	}
	return h
}

// ReadDrives lists fixed, removable and network volumes with free space.
func ReadDrives() []Drive {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Drive
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(letter)
		kind := ""
		switch windows.GetDriveType(p) {
		case windows.DRIVE_FIXED:
			kind = "Fixed"
		case windows.DRIVE_REMOVABLE:
			kind = "Removable"
		case windows.DRIVE_REMOTE:
			kind = "Network"
		case windows.DRIVE_CDROM:
			kind = "Optical"
		default:
			continue
		}
		var free, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
			continue
		}
		out = append(out, Drive{Letter: filepath.VolumeName(letter), Kind: kind, Total: total, Free: totalFree})
	}
	return out
}

var _ = unsafe.Sizeof(0)
