//go:build windows

package metrics

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modPowrprof               = windows.NewLazySystemDLL("powrprof.dll")
	procCallNtPowerInfo       = modPowrprof.NewProc("CallNtPowerInformation")
	procPowerGetActiveScheme  = modPowrprof.NewProc("PowerGetActiveScheme")
	procPowerReadFriendlyName = modPowrprof.NewProc("PowerReadFriendlyName")
	procGetSystemPowerStatus  = modKernel32.NewProc("GetSystemPowerStatus")
	procGetLogProcInfoEx      = modKernel32.NewProc("GetLogicalProcessorInformationEx")
	procIsProcessorFeature    = modKernel32.NewProc("IsProcessorFeaturePresent")
)

const (
	infoLevelProcessorInformation = 11 // PROCESSOR_POWER_INFORMATION array
	relationAll                   = 0xFFFF
	pfVirtFirmwareEnabled         = 21
)

// ReadCPUClocks returns current and maximum MHz for every logical processor
// (CallNtPowerInformation, no elevation needed).
func ReadCPUClocks() (cur, max []uint32) {
	n := logicalCPUs()
	if n < 1 {
		return nil, nil
	}
	buf := make([]byte, n*24)
	r, _, _ := procCallNtPowerInfo.Call(infoLevelProcessorInformation, 0, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r != 0 {
		return nil, nil
	}
	cur = make([]uint32, n)
	max = make([]uint32, n)
	for i := 0; i < n; i++ {
		rec := buf[i*24:]
		max[i] = binary.LittleEndian.Uint32(rec[4:])
		cur[i] = binary.LittleEndian.Uint32(rec[8:])
	}
	return cur, max
}

// CPUTopology summarises packages, cores and caches.
type CPUTopology struct {
	Packages int    `json:"packages"`
	Cores    int    `json:"cores"`
	Logical  int    `json:"logical"`
	NUMA     int    `json:"numa"`
	L1       uint64 `json:"l1"`
	L2       uint64 `json:"l2"`
	L3       uint64 `json:"l3"`
	BaseMHz  uint32 `json:"baseMhz"`
	Virt     bool   `json:"virtualization"`
}

// ReadTopology walks GetLogicalProcessorInformationEx(RelationAll).
func ReadTopology() CPUTopology {
	t := CPUTopology{Logical: logicalCPUs()}
	var need uint32
	procGetLogProcInfoEx.Call(relationAll, 0, uintptr(unsafe.Pointer(&need)))
	if need > 0 {
		buf := make([]byte, need)
		r, _, _ := procGetLogProcInfoEx.Call(relationAll, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&need)))
		if r != 0 {
			le := binary.LittleEndian
			for off := 0; off+8 <= int(need); {
				rel := le.Uint32(buf[off:])
				size := int(le.Uint32(buf[off+4:]))
				if size < 8 {
					break
				}
				body := buf[off+8:]
				switch rel {
				case 0: // processor core
					t.Cores++
				case 1: // NUMA node
					t.NUMA++
				case 2: // cache: Level, Associativity, LineSize(2), CacheSize(4), Type(4)
					level := int(body[0])
					sz := uint64(le.Uint32(body[4:]))
					switch level {
					case 1:
						t.L1 += sz
					case 2:
						t.L2 += sz
					case 3:
						t.L3 += sz
					}
				case 3: // package
					t.Packages++
				}
				off += size
			}
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("~MHz"); err == nil {
			t.BaseMHz = uint32(v)
		}
		k.Close()
	}
	if r, _, _ := procIsProcessorFeature.Call(pfVirtFirmwareEnabled); r != 0 {
		t.Virt = true
	}
	return t
}

// PowerStatic is slow-changing power state.
type PowerStatic struct {
	Plan        string `json:"plan"`
	HasBattery  bool   `json:"hasBattery"`
	OnAC        bool   `json:"onAC"`
	BatteryPct  int    `json:"batteryPct"`
	BatteryMins int    `json:"batteryMins"` // -1 unknown
	FastStartup bool   `json:"fastStartup"`
}

type sysPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// ReadPowerStatic reads the active plan, battery and Fast Startup state.
func ReadPowerStatic() PowerStatic {
	p := PowerStatic{BatteryMins: -1}

	var st sysPowerStatus
	if r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&st))); r != 0 {
		p.OnAC = st.ACLineStatus == 1
		if st.BatteryFlag != 128 && st.BatteryFlag != 255 { // 128 = no battery
			p.HasBattery = true
			if st.BatteryLifePercent <= 100 {
				p.BatteryPct = int(st.BatteryLifePercent)
			}
			if st.BatteryLifeTime != 0xFFFFFFFF {
				p.BatteryMins = int(st.BatteryLifeTime / 60)
			}
		}
	}

	var guid *windows.GUID
	if r, _, _ := procPowerGetActiveScheme.Call(0, uintptr(unsafe.Pointer(&guid))); r == 0 && guid != nil {
		defer windows.LocalFree(windows.Handle(unsafe.Pointer(guid)))
		var size uint32
		procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(guid)), 0, 0, 0, uintptr(unsafe.Pointer(&size)))
		if size > 0 {
			buf := make([]byte, size)
			if r, _, _ := procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(guid)), 0, 0,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
				p.Plan = utf16At(buf, 0)
			}
		}
	}

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Power`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("HiberbootEnabled"); err == nil {
			p.FastStartup = v != 0
		}
		k.Close()
	}
	return p
}
