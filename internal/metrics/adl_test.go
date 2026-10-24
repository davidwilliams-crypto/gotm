//go:build windows

package metrics

import (
	"testing"
	"unsafe"
)

// The ADL structures are passed to a C library by pointer: their layout must
// match adl_structures.h exactly (AdapterInfo is 1572 bytes on Windows x64,
// ADLPMLogDataOutput 4 + 256*8).
func TestADLStructLayout(t *testing.T) {
	if s := unsafe.Sizeof(adlAdapterInfo{}); s != 1572 {
		t.Fatalf("AdapterInfo size %d, want 1572", s)
	}
	if s := unsafe.Sizeof(adlPMLog{}); s != 4+adlMaxSensors*8 {
		t.Fatalf("ADLPMLogDataOutput size %d", s)
	}
	var a adlAdapterInfo
	if off := unsafe.Offsetof(a.Bus); off != 264 {
		t.Fatalf("iBusNumber offset %d, want 264", off)
	}
	if off := unsafe.Offsetof(a.Present); off != 792 {
		t.Fatalf("iPresent offset %d, want 792", off)
	}
}

func TestADLPickAdaptersOnePerBus(t *testing.T) {
	mk := func(idx, bus, vendor, present int32, name string) adlAdapterInfo {
		a := adlAdapterInfo{Index: idx, Bus: bus, VendorID: vendor, Present: present}
		copy(a.Name[:], name)
		return a
	}
	all := []adlAdapterInfo{
		mk(0, 3, adlVendorAMD, 1, "AMD Radeon RX 7800 XT"),
		mk(1, 3, adlVendorAMD, 1, "AMD Radeon RX 7800 XT"), // second display output, same card
		mk(2, 9, 10, 1, "NVIDIA"),                          // not AMD
		mk(3, 7, adlVendorAMD, 0, "AMD Radeon(TM) Graphics"),
		mk(4, 7, adlVendorAMD, 1, "AMD Radeon(TM) Graphics"),
	}
	got := adlPickAdapters(all)
	if len(got) != 2 || got[0].index != 0 || got[0].bus != 3 || got[1].index != 4 || got[1].bus != 7 {
		t.Fatalf("picked %+v", got)
	}
	if got[1].name != "AMD Radeon(TM) Graphics" {
		t.Fatalf("name %q", got[1].name)
	}
}

func TestApplyPMLogMapsSensors(t *testing.T) {
	var tbl adlPMLog
	set := func(i int, v int32) { tbl.Sensors[i] = adlSensor{Supported: 1, Value: v} }
	set(pmlogTempEdge, 47)
	set(pmlogTempHotspot, 61)
	set(pmlogClkGfx, 2200)
	set(pmlogClkMem, 1000)
	set(pmlogAsicPower, 35)
	set(pmlogFanPct, 0)
	set(pmlogGfxVoltage, 1150)
	set(pmlogTempCPU, 58)
	set(pmlogCPUPower, 42)
	set(pmlogClkCPU, 4900)
	var a GPUAdapter
	if !applyPMLog(&tbl, &a) {
		t.Fatal("nothing applied")
	}
	if !a.HasTemp || a.TempC != 47 || !a.HasHotspot || a.HotspotC != 61 {
		t.Fatalf("temps %+v", a)
	}
	if !a.HasClock || a.ClockMHz != 2200 || a.MemClockMHz != 1000 {
		t.Fatalf("clocks %+v", a)
	}
	if a.PowerSource != "asic" {
		t.Fatalf("power source %q", a.PowerSource)
	}
	if !a.HasPower || a.PowerW != 35 || !a.HasFan || a.FanPct != 0 || !a.HasVoltage || a.VoltageV != 1.15 {
		t.Fatalf("power/fan/voltage %+v", a)
	}
	if !a.HasCPUTemp || a.CPUTempC != 58 || !a.HasCPUPower || a.CPUPowerW != 42 || !a.HasCPUClock || a.CPUClockMHz != 4900 {
		t.Fatalf("cpu side %+v", a)
	}
	// an unsupported table reports nothing rather than zeros
	var empty adlPMLog
	var b GPUAdapter
	if applyPMLog(&empty, &b) || b.HasTemp || b.HasClock {
		t.Fatal("empty table must not apply")
	}
}

// TestADLLive reads the real sensors when an AMD adapter and driver are
// present; elsewhere the library simply reports no adapters.
func TestADLLive(t *testing.T) {
	l := openADL()
	defer l.close()
	if !l.ok {
		t.Skip("no AMD display driver (atiadlxx.dll) on this PC")
	}
	for _, r := range l.records {
		t.Logf("record idx=%d vendor=%d bus=%d dev=%d fn=%d present=%d exist=%d name=%q", r.Index, r.VendorID, r.Bus, r.Device, r.Function, r.Present, r.Exist, cString(r.Name[:]))
	}
	if len(l.adapters) == 0 {
		t.Skip("ADL lists no AMD adapters")
	}
	for _, ad := range l.adapters {
		var a GPUAdapter
		ok := l.fill(ad, &a)
		t.Logf("adapter %d bus %d %q integrated=%v", ad.index, ad.bus, ad.name, ad.integrated)
		if isAPUName(ad.name) && !ad.integrated {
			t.Errorf("%q looks like an APU but the driver did not flag it integrated", ad.name)
		}
		t.Logf("adapter %d bus %d %q: read=%v temp=%v/%.0f hotspot=%v/%.0f clock=%v/%.0f mem=%.0f power=%v/%.1f fan=%v/%.0f volt=%v/%.3f busy=%v/%.0f cpu: temp=%v/%.0f power=%v/%.1f clock=%v/%.0f",
			ad.index, ad.bus, ad.name, ok, a.HasTemp, a.TempC, a.HasHotspot, a.HotspotC, a.HasClock, a.ClockMHz, a.MemClockMHz,
			a.HasPower, a.PowerW, a.HasFan, a.FanPct, a.HasVoltage, a.VoltageV, a.HasDriverPct, a.DriverPct,
			a.HasCPUTemp, a.CPUTempC, a.HasCPUPower, a.CPUPowerW, a.HasCPUClock, a.CPUClockMHz)
		if !ok {
			t.Errorf("adapter %q: no sensor could be read", ad.name)
		}
		if a.HasTemp && (a.TempC < 0 || a.TempC > 120) {
			t.Errorf("implausible temperature %.0f", a.TempC)
		}
		if a.HasClock && (a.ClockMHz < 50 || a.ClockMHz > 5000) {
			t.Errorf("implausible clock %.0f MHz", a.ClockMHz)
		}
	}
	// the DXGI/D3DKMT bus must line up with ADL's, or sensors land on the wrong card
	matched := 0
	for _, st := range enumerateGPUs() {
		if st.vendorID != pciVendorAMD || st.software {
			continue
		}
		matched++
		if _, ok := l.find(st.bus, st.name); !ok {
			t.Errorf("DXGI adapter %q on bus %d has no ADL match (ADL: %+v)", st.name, st.bus, l.adapters)
		}
	}
	if matched == 0 {
		t.Error("DXGI lists no AMD adapter although ADL does")
	}
}

const pciVendorAMD = 0x1002 // DXGI reports the PCI vendor id in hex

func TestAPUNameHeuristic(t *testing.T) {
	for n, want := range map[string]bool{
		"AMD Radeon(TM) Graphics": true, "AMD Radeon(TM) 780M Graphics": true, "AMD Radeon Graphics": true,
		"AMD Radeon RX 7800 XT": false, "AMD Radeon PRO W7800": false, "Radeon RX Vega 11 Graphics": false,
	} {
		if got := isAPUName(n); got != want {
			t.Errorf("isAPUName(%q) = %v, want %v", n, got, want)
		}
	}
}
