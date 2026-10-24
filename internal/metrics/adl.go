//go:build windows

package metrics

import (
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AMD Display Library (ADL) ships with every Radeon driver as atiadlxx.dll in
// System32. It is the public, documented way to read what Windows itself does
// not expose for AMD adapters: clocks, temperatures, power and fan speed. gotm
// only calls read-only entry points, never the overdrive setters, and matches
// each ADL adapter to a DXGI adapter by PCI bus number.
//
// Modern drivers answer ADL2_New_QueryPMLogData_Get with a table of sensors
// (edge/hotspot temperature, graphics and memory clock, ASIC power, fan, and
// on APUs the CPU temperature, power and clock as well). Older drivers fall
// back to the Overdrive 5 temperature and activity calls.

const (
	adlOK          = 0
	adlMaxPath     = 256
	adlMaxSensors  = 256
	adlVendorAMD   = 1002   // ADL gives iVendorID in decimal (0x1002 = 4098)
	adlLMEMZeroInt = 0x0040 // LMEM_ZEROINIT
)

// Sensor indices of ADLPMLogDataOutput (adl_defines.h, ADLSensorType).
const (
	pmlogClkGfx      = 1
	pmlogClkMem      = 2
	pmlogTempEdge    = 8
	pmlogFanRPM      = 14
	pmlogFanPct      = 15
	pmlogSocPower    = 17
	pmlogActivityGfx = 19
	pmlogGfxVoltage  = 21 // mV
	pmlogAsicPower   = 23
	pmlogTempHotspot = 27
	pmlogTempGfx     = 28
	pmlogTempSoc     = 29
	pmlogGfxPower    = 30
	pmlogTempCPU     = 32
	pmlogCPUPower    = 33
	pmlogClkCPU      = 34
	pmlogBoardPower  = 73
)

// ADL2_Adapter_ASICFamilyType_Get flags (adl_defines.h).
const (
	adlASICIntegrated = 1 << 1
	adlASICFusion     = 1 << 5 // older name for an APU

	adlErrNotSupported = -8
	adlErrInvalidIndex = -5
)

// adlAdapterInfo is AdapterInfo (1572 bytes on Windows x64).
type adlAdapterInfo struct {
	Size, Index               int32
	UDID                      [adlMaxPath]byte
	Bus, Device, Function     int32
	VendorID                  int32
	Name, DisplayName         [adlMaxPath]byte
	Present, Exist            int32
	DriverPath, DriverPathExt [adlMaxPath]byte
	PNPString                 [adlMaxPath]byte
	OSDisplayIndex            int32
}

type adlSensor struct{ Supported, Value int32 }

// adlPMLog is ADLPMLogDataOutput.
type adlPMLog struct {
	Size    int32
	Sensors [adlMaxSensors]adlSensor
}

type adlAdapter struct {
	index      int32
	bus        int
	name       string
	integrated bool // an APU's graphics: the same silicon as the CPU, never asleep
}

type adlLib struct {
	dll                    *windows.LazyDLL
	ctx                    uintptr
	adapters               []adlAdapter
	pmlog, od5Temp, od5Act *windows.LazyProc
	destroy                *windows.LazyProc
	ok                     bool
	apu                    map[int32]bool   // adapters whose sensor table carried CPU readings
	noPMLog                map[int32]bool   // adapters the driver answers "not supported" for
	records                []adlAdapterInfo // raw list, for diagnostics
}

var (
	adlMallocOnce sync.Once
	adlMallocCB   uintptr
)

// adlMalloc is the allocator ADL asks for at init. Only entry points that
// return driver-allocated buffers use it (the caller then frees them); none
// of the calls gotm makes do, every buffer here is gotm's own.
func adlMalloc(size uintptr) uintptr {
	p, _ := windows.LocalAlloc(adlLMEMZeroInt, uint32(size))
	return uintptr(p)
}

// openADL loads the library and lists the AMD adapters it knows. A missing
// DLL (no AMD driver) simply yields a reader that reports nothing.
func openADL() *adlLib {
	l := &adlLib{dll: windows.NewLazySystemDLL("atiadlxx.dll"), noPMLog: map[int32]bool{}, apu: map[int32]bool{}}
	if l.dll.Load() != nil {
		return l
	}
	create := l.dll.NewProc("ADL2_Main_Control_Create")
	l.destroy = l.dll.NewProc("ADL2_Main_Control_Destroy")
	count := l.dll.NewProc("ADL2_Adapter_NumberOfAdapters_Get")
	info := l.dll.NewProc("ADL2_Adapter_AdapterInfo_Get")
	l.pmlog = l.dll.NewProc("ADL2_New_QueryPMLogData_Get")
	l.od5Temp = l.dll.NewProc("ADL2_Overdrive5_Temperature_Get")
	l.od5Act = l.dll.NewProc("ADL2_Overdrive5_CurrentActivity_Get")
	for _, p := range []*windows.LazyProc{create, l.destroy, count, info, l.pmlog, l.od5Temp, l.od5Act} {
		if p.Find() != nil {
			return l
		}
	}
	adlMallocOnce.Do(func() { adlMallocCB = syscall.NewCallback(adlMalloc) })
	// 0 = every adapter, not just those with a display attached: an APU's
	// graphics with the monitors on a discrete card would otherwise be missing
	if r, _, _ := create.Call(adlMallocCB, 0, uintptr(unsafe.Pointer(&l.ctx))); int32(r) != adlOK || l.ctx == 0 {
		return l
	}
	var n int32
	if r, _, _ := count.Call(l.ctx, uintptr(unsafe.Pointer(&n))); int32(r) != adlOK || n <= 0 || n > 64 {
		l.close()
		return l
	}
	buf := make([]adlAdapterInfo, n)
	if r, _, _ := info.Call(l.ctx, uintptr(unsafe.Pointer(&buf[0])), uintptr(n)*unsafe.Sizeof(buf[0])); int32(r) != adlOK {
		l.close()
		return l
	}
	l.records = buf
	l.adapters = adlPickAdapters(buf)
	// An APU's graphics may be polled at any time (nothing to wake), a
	// discrete card only while a GPU view is open: ask the driver which is which.
	asic := l.dll.NewProc("ADL2_Adapter_ASICFamilyType_Get")
	for i := range l.adapters {
		a := &l.adapters[i]
		if asic.Find() == nil {
			var types, valid int32
			if r, _, _ := asic.Call(l.ctx, uintptr(a.index), uintptr(unsafe.Pointer(&types)), uintptr(unsafe.Pointer(&valid))); int32(r) == adlOK {
				mask := valid & (adlASICIntegrated | adlASICFusion)
				a.integrated = types&mask != 0
				if valid&adlASICIntegrated != 0 || valid&adlASICFusion != 0 {
					continue // the driver answered; no need to guess from the name
				}
			}
		}
		a.integrated = isAPUName(a.name)
	}
	l.ok = l.ctx != 0
	return l
}

// isAPUName recognises the generic names Windows gives integrated Radeon
// graphics ("AMD Radeon(TM) Graphics", "AMD Radeon(TM) 780M Graphics").
func isAPUName(n string) bool {
	n = strings.ToLower(shortName(n))
	return strings.HasSuffix(n, " graphics") && !strings.Contains(n, " rx ") && !strings.Contains(n, " pro ")
}

// isAPU reports whether an adapter shares its silicon with the CPU.
func (l *adlLib) isAPU(ad adlAdapter) bool {
	return ad.integrated || (l != nil && l.apu[ad.index])
}

// adlPickAdapters keeps one entry per physical AMD adapter: ADL lists one
// record per display output, all sharing the PCI bus number. Records that
// are neither present nor existing describe hardware that was removed.
func adlPickAdapters(all []adlAdapterInfo) []adlAdapter {
	var out []adlAdapter
	seen := map[int32]bool{}
	for i := range all {
		a := &all[i]
		if a.VendorID != adlVendorAMD || (a.Present == 0 && a.Exist == 0) || seen[a.Bus] {
			continue
		}
		seen[a.Bus] = true
		out = append(out, adlAdapter{index: a.Index, bus: int(a.Bus), name: cString(a.Name[:])})
	}
	return out
}

func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func (l *adlLib) close() {
	if l != nil && l.ctx != 0 && l.destroy != nil {
		l.destroy.Call(l.ctx)
		l.ctx = 0
	}
	if l != nil {
		l.ok = false
	}
}

// find returns the ADL adapter on this PCI bus, else one with this name.
func (l *adlLib) find(bus int, name string) (adlAdapter, bool) {
	if l == nil || !l.ok {
		return adlAdapter{}, false
	}
	if bus >= 0 {
		for _, a := range l.adapters {
			if a.bus == bus {
				return a, true
			}
		}
	}
	for _, a := range l.adapters {
		if strings.EqualFold(shortName(a.name), shortName(name)) {
			return a, true
		}
	}
	return adlAdapter{}, false
}

func shortName(n string) string {
	n = strings.ReplaceAll(n, "(TM)", "")
	n = strings.ReplaceAll(n, "(R)", "")
	return strings.Join(strings.Fields(n), " ")
}

// fill reads the adapter's sensors into a. It reports whether anything was read.
func (l *adlLib) fill(ad adlAdapter, a *GPUAdapter) bool {
	if l == nil || !l.ok {
		return false
	}
	if !l.noPMLog[ad.index] {
		var out adlPMLog
		r, _, _ := l.pmlog.Call(l.ctx, uintptr(ad.index), uintptr(unsafe.Pointer(&out)))
		switch int32(r) {
		case adlOK:
			if applyPMLog(&out, a) {
				if a.HasCPUTemp || a.HasCPUPower {
					l.apu[ad.index] = true
				}
				a.Integrated = l.isAPU(ad)
				return true
			}
		case adlErrNotSupported, adlErrInvalidIndex:
			l.noPMLog[ad.index] = true // this driver has no sensor table for it: use Overdrive 5
		}
		// any other failure (a driver reset, resume from sleep) is tried again next time
	}
	ok := l.fillOD5(ad, a)
	a.Integrated = l.isAPU(ad)
	return ok
}

// applyPMLog copies the supported sensors of a PMLog table into a.
func applyPMLog(t *adlPMLog, a *GPUAdapter) bool {
	get := func(i int) (float64, bool) {
		if i < 0 || i >= len(t.Sensors) || t.Sensors[i].Supported == 0 {
			return 0, false
		}
		return float64(t.Sensors[i].Value), true
	}
	any := false
	if v, ok := get(pmlogTempEdge); ok && v > -50 && v < 150 {
		a.TempC, a.HasTemp, any = v, true, true
	} else if v, ok := get(pmlogTempGfx); ok && v > -50 && v < 150 {
		a.TempC, a.HasTemp, any = v, true, true
	} else if v, ok := get(pmlogTempSoc); ok && v > -50 && v < 150 {
		a.TempC, a.HasTemp, any = v, true, true
	}
	if v, ok := get(pmlogTempHotspot); ok && v > -50 && v < 150 {
		a.HotspotC, a.HasHotspot, any = v, true, true
	}
	if v, ok := get(pmlogClkGfx); ok && v > 0 {
		a.ClockMHz, a.HasClock, any = v, true, true
	}
	if v, ok := get(pmlogClkMem); ok && v > 0 {
		a.MemClockMHz, any = v, true
	}
	for _, p := range []struct {
		idx int
		src string
	}{{pmlogBoardPower, "board"}, {pmlogAsicPower, "asic"}, {pmlogGfxPower, "gfx"}, {pmlogSocPower, "soc"}} {
		if v, ok := get(p.idx); ok && v >= 0 {
			a.PowerW, a.HasPower, a.PowerSource, any = v, true, p.src, true
			break
		}
	}
	if v, ok := get(pmlogTempSoc); ok && v > -50 && v < 150 {
		a.SocTempC, a.HasSocTemp, any = v, true, true
	}
	if v, ok := get(pmlogFanPct); ok {
		a.FanPct, a.HasFan, any = v, true, true
	} else if v, ok := get(pmlogFanRPM); ok && v > 0 {
		a.FanRPM, a.HasFan, any = v, true, true
	}
	if v, ok := get(pmlogGfxVoltage); ok && v > 0 {
		a.VoltageV, a.HasVoltage, any = v/1000, true, true
	}
	if v, ok := get(pmlogActivityGfx); ok {
		a.DriverPct, a.HasDriverPct, any = v, true, true
	}
	// APUs report the CPU side through the same table.
	if v, ok := get(pmlogTempCPU); ok && v > 0 && v < 150 {
		a.CPUTempC, a.HasCPUTemp, any = v, true, true
	}
	if v, ok := get(pmlogCPUPower); ok && v >= 0 {
		a.CPUPowerW, a.HasCPUPower, any = v, true, true
	}
	if v, ok := get(pmlogClkCPU); ok && v > 0 {
		a.CPUClockMHz, a.HasCPUClock, any = v, true, true
	}
	return any
}

// fillOD5 is the legacy path: one temperature and the current activity block.
func (l *adlLib) fillOD5(ad adlAdapter, a *GPUAdapter) bool {
	any := false
	var t struct{ Size, Temp int32 }
	t.Size = int32(unsafe.Sizeof(t))
	if r, _, _ := l.od5Temp.Call(l.ctx, uintptr(ad.index), 0, uintptr(unsafe.Pointer(&t))); int32(r) == adlOK && t.Temp > 0 {
		a.TempC, a.HasTemp, any = float64(t.Temp)/1000, true, true
	}
	var act struct {
		Size, EngineClock, MemoryClock, Vddc, ActivityPct    int32
		PerfLevel, BusSpeed, BusLanes, MaxBusLanes, Reserved int32
	}
	act.Size = int32(unsafe.Sizeof(act))
	if r, _, _ := l.od5Act.Call(l.ctx, uintptr(ad.index), uintptr(unsafe.Pointer(&act))); int32(r) == adlOK {
		if act.EngineClock > 0 {
			a.ClockMHz, a.HasClock, any = float64(act.EngineClock)/100, true, true // 10 kHz units
		}
		if act.MemoryClock > 0 {
			a.MemClockMHz, any = float64(act.MemoryClock)/100, true
		}
		if act.Vddc > 0 {
			a.VoltageV, a.HasVoltage, any = float64(act.Vddc)/1000, true, true
		}
		a.DriverPct, a.HasDriverPct, any = float64(act.ActivityPct), true, true
	}
	return any
}
