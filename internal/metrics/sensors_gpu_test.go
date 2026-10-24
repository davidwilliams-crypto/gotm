//go:build windows

package metrics

import (
	"strings"
	"testing"
)

func rowByID(rows []SensorRow, id string) (SensorRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return SensorRow{}, false
}

// Each adapter with vendor sensors gets its own group in the tree, and an
// APU's CPU-side readings appear under the CPU with an honest source label.
func TestSensorRowsIncludeGPUAdapters(t *testing.T) {
	h := NewSensorHubWith(nil)
	s := &Snapshot{GPUs: []GPUAdapter{
		{ID: "a", Name: "AMD Radeon(TM) Graphics", Vendor: "AMD", DedicatedTotal: 512 << 20, Pct: 3, Integrated: true,
			HasTemp: true, TempC: 41, HasSocTemp: true, SocTempC: 45,
			HasClock: true, ClockMHz: 600, MemClockMHz: 3000,
			HasPower: true, PowerW: 57, PowerSource: "asic", HasVoltage: true, VoltageV: 1.14,
			HasDriverPct: true, DriverPct: 2,
			HasCPUTemp: true, CPUTempC: 58, HasCPUPower: true, CPUPowerW: 42,
			Engines: []GPUEngine{{Name: "3D", Pct: 3}}},
		{ID: "b", Name: "NVIDIA GeForce RTX 5080", Vendor: "NVIDIA", DedicatedTotal: 16 << 30,
			HasTemp: true, TempC: 40, HasClock: true, ClockMHz: 2500, HasPower: true, PowerW: 30, PowerLimitW: 360, HasFan: true, FanPct: 0},
		{ID: "c", Name: "Intel UHD", Vendor: "Intel"},
	}}
	rows := h.Update(s, HostInfo{Hostname: "pc", CPUName: "cpu"})

	want := map[string]string{
		"gpu.a.temp.edge": "41.0 °C", "gpu.a.temp.soc": "45.0 °C",
		"gpu.a.clk.core": "600 MHz", "gpu.a.clk.mem": "3.00 GHz",
		"gpu.a.pwr.0": "57.0 W", "gpu.a.pwr.v": "1.140 V", "gpu.a.load.drv": "2.0 %", "gpu.a.load.e0": "3.0 %",
		"gpu.b.temp.edge": "40.0 °C", "gpu.b.pwr.lim": "360.0 W", "gpu.b.fan.pct": "0.0 %",
		"cpu.pwr.apu": "42.0 W",
	}
	for id, v := range want {
		r, ok := rowByID(rows, id)
		if !ok {
			t.Errorf("row %s missing", id)
			continue
		}
		if r.Value != v {
			t.Errorf("row %s = %q, want %q", id, r.Value, v)
		}
	}
	if r, _ := rowByID(rows, "gpu.a.pwr.0"); r.Name != "Package power (CPU and GPU)" {
		t.Errorf("APU power label %q", r.Name)
	}
	if r, _ := rowByID(rows, "gpu.b.pwr.0"); r.Name != "Power draw" {
		t.Errorf("NVIDIA power label %q", r.Name)
	}
	if r, ok := rowByID(rows, "gpu.c.none"); !ok || !strings.Contains(r.Name, "Intel") {
		t.Errorf("adapter without sensors should carry a note, got %+v", r)
	}
	// the APU's CPU temperature is the fallback when no SDK or external source exists
	temp, label, ok := h.CPUTemperature()
	if !ok || temp != 58 || !strings.Contains(label, "display driver") {
		t.Errorf("CPU temperature fallback = %.0f %q %v", temp, label, ok)
	}
	if r, ok := rowByID(rows, "cpu.temp.ext"); !ok || r.Value != "58.0 °C" {
		t.Errorf("CPU temperature row %+v %v", r, ok)
	}
}

// A board's placeholder thermal zone (a constant 290 K) must not be shown as a
// temperature; a real zone still is.
func TestPlaceholderThermalZoneIsNotATemperature(t *testing.T) {
	h := NewSensorHubWith(nil)
	rows := h.Update(&Snapshot{Thermal: []NamedValue{{Name: `\_TZ.TZ10`, Value: 290 - 273.15}, {Name: `\_TZ.TZ01`, Value: 41.2}}}, HostInfo{})
	fake, _ := rowByID(rows, "cpu.temp.0")
	if fake.Kind != "note" || fake.Value != "—" || !strings.Contains(fake.Name, "placeholder") {
		t.Errorf("placeholder zone shown as %+v", fake)
	}
	real, _ := rowByID(rows, "cpu.temp.1")
	if real.Kind != "temp" || real.Value != "41.2 °C" {
		t.Errorf("real zone shown as %+v", real)
	}
}
