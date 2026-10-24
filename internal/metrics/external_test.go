//go:build windows

package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A trimmed LibreHardwareMonitor data.json: Computer > Hardware > Group > Sensor.
const sampleLHM = `{
 "id": 0, "Text": "Sensor", "Min": "Min", "Value": "Value", "Max": "Max", "ImageURL": "",
 "Children": [{
  "id": 1, "Text": "TESTPC", "Value": "", "Children": [
   {"id": 2, "Text": "AMD Ryzen 9 7950X3D", "Value": "", "Children": [
     {"id": 3, "Text": "Temperatures", "Value": "", "Children": [
       {"id": 4, "Text": "Core (Tctl/Tdie)", "Value": "61,4 °C", "Min": "38,2 °C", "Max": "78,6 °C",
        "Type": "Temperature", "SensorId": "/amdcpu/0/temperature/2", "Children": []},
       {"id": 5, "Text": "CCD1 (Tdie)", "Value": "59.0 °C", "Min": "37.0 °C", "Max": "70.1 °C",
        "Type": "Temperature", "SensorId": "/amdcpu/0/temperature/3", "Children": []}]},
     {"id": 6, "Text": "Powers", "Value": "", "Children": [
       {"id": 7, "Text": "Package", "Value": "54.8 W", "Min": "12.1 W", "Max": "92.6 W",
        "Type": "Power", "SensorId": "/amdcpu/0/power/0", "Children": []}]}]},
   {"id": 8, "Text": "NVIDIA GeForce RTX", "Value": "", "Children": [
     {"id": 9, "Text": "Temperatures", "Value": "", "Children": [
       {"id": 10, "Text": "GPU Core", "Value": "44.0 °C", "Min": "30.0 °C", "Max": "70.0 °C",
        "Type": "Temperature", "SensorId": "/gpu-nvidia/0/temperature/0", "Children": []}]}]}
  ]}]}`

func TestFlattenLHMAndPickCPUTemperature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleLHM))
	}))
	defer srv.Close()

	src := NewExternalSource(srv.URL + "/data.json")
	if src.Connected() {
		t.Fatal("must not report connected before the first poll")
	}
	if err := src.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !src.Connected() {
		t.Fatal("not connected after a successful poll")
	}
	sensors := src.Sensors()
	if len(sensors) != 4 {
		t.Fatalf("sensors = %d, want 4: %+v", len(sensors), sensors)
	}
	first := sensors[0]
	if first.Hardware != "AMD Ryzen 9 7950X3D" || first.Group != "Temperatures" || first.Name != "Core (Tctl/Tdie)" {
		t.Errorf("path wrong: %+v", first)
	}
	if !first.HasNum || first.Num != 61.4 { // decimal comma must parse
		t.Errorf("value = %v (%v), want 61.4", first.Num, first.HasNum)
	}
	temp, label, ok := CPUTemperature(sensors)
	if !ok || temp != 61.4 || !strings.Contains(label, "Tctl") {
		t.Errorf("CPU temp = %v %q %v, want 61.4 Tctl", temp, label, ok)
	}
}

func TestExternalSourceOnlyTalksToLoopback(t *testing.T) {
	for _, u := range []string{"http://example.com/data.json", "https://127.0.0.1:8085/data.json", "http://192.168.1.5:8085/data.json", "file:///c:/x"} {
		src := NewExternalSource(u)
		if err := src.PollOnce(context.Background()); err == nil {
			t.Errorf("%s should be refused", u)
		}
	}
	if !loopbackOnly("http://127.0.0.1:8085/data.json") || !loopbackOnly("http://localhost:8085/data.json") || !loopbackOnly("http://[::1]:8085/x") {
		t.Error("loopback URLs must be accepted")
	}
}

func TestExternalSourceSurvivesGarbageAndMissingServer(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	src := NewExternalSource(bad.URL)
	if err := src.PollOnce(context.Background()); err == nil || src.Connected() {
		t.Error("non-JSON body must be a failed poll")
	}
	bad.Close()

	gone := NewExternalSource(bad.URL) // server is closed now
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := gone.PollOnce(ctx); err == nil || gone.Connected() {
		t.Error("closed server must be a failed poll, not a hang")
	}
}

func TestWindowsEnergyMeterGivesPackagePower(t *testing.T) {
	pc := newPerfCounters()
	if pc == nil {
		t.Skip("PDH unavailable")
	}
	defer pc.close()
	time.Sleep(1100 * time.Millisecond)
	ps := pc.read()
	if len(ps.Energy) == 0 {
		t.Skip("this machine exposes no Energy Meter counters")
	}
	s := &Snapshot{Energy: ps.Energy}
	pkg, ok := EnergyPackage(s)
	if !ok {
		t.Fatalf("no package instance among %+v", ps.Energy)
	}
	if pkg <= 0 || pkg > 600 {
		t.Errorf("package power %.1f W is implausible", pkg)
	}
	t.Logf("package %.1f W across %d energy instances", pkg, len(ps.Energy))
}

func TestSensorTreeShowsPowerAndExternalGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleLHM))
	}))
	defer srv.Close()
	src := NewExternalSource(srv.URL)
	if err := src.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewSensorHubWith(src)
	s := &Snapshot{
		CPU: 5, Cores: []float64{5, 6}, Mem: MemInfo{Percent: 40},
		Energy: []NamedValue{
			{Name: "RAPL_Package0_PKG", Value: 48.1},
			{Name: "RAPL_Package0_Core1_CORE", Value: 2.5},
			{Name: "RAPL_Package0_Core0_CORE", Value: 4.7},
		},
	}
	rows := h.Update(s, ReadHostInfo())
	byID := map[string]SensorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r, ok := byID["cpu.pwr.pkg"]; !ok || r.Value != "48.1 W" {
		t.Errorf("package row: %+v", r)
	}
	if r, ok := byID["cpu.pwr.cores"]; !ok || r.Value != "7.2 W" {
		t.Errorf("core sum row: %+v", r)
	}
	if byID["cpu.pwr.c0"].Value != "4.7 W" || byID["cpu.pwr.c1"].Value != "2.5 W" {
		t.Errorf("per-core rows: %+v %+v", byID["cpu.pwr.c0"], byID["cpu.pwr.c1"])
	}
	if r, ok := byID["cpu.temp.ext"]; !ok || !strings.HasPrefix(r.Value, "61.4") {
		t.Errorf("external CPU temperature row: %+v", r)
	}
	if _, ok := byID["ext"]; !ok {
		t.Error("external group missing")
	}
	// order: core 0 must come before core 1 in the flattened tree
	i0, i1 := -1, -1
	for i, r := range rows {
		switch r.ID {
		case "cpu.pwr.c0":
			i0 = i
		case "cpu.pwr.c1":
			i1 = i
		}
	}
	if !(i0 >= 0 && i0 < i1) {
		t.Errorf("per-core power rows out of order: %d, %d", i0, i1)
	}
}

// Windows' "Processor Frequency" never exceeds the nominal clock; the sample
// must scale it by % Processor Performance so boost shows up.
func TestCPUFreqIncludesBoost(t *testing.T) {
	pc := newPerfCounters()
	if pc == nil {
		t.Skip("PDH unavailable")
	}
	defer pc.close()
	done := make(chan struct{})
	for i := 0; i < 8; i++ { // enough load to leave the idle clocks
		go func() {
			for x := 0; ; x++ {
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	time.Sleep(1200 * time.Millisecond)
	ps := pc.read()
	close(done)
	if len(ps.CPUFreq) == 0 || ps.CPUPerfPct <= 0 {
		t.Skip("no frequency counters")
	}
	var maxF float64
	for _, f := range ps.CPUFreq {
		if f > maxF {
			maxF = f
		}
	}
	t.Logf("perf %.1f%%, max core %.0f MHz over %d logical CPUs", ps.CPUPerfPct, maxF, len(ps.CPUFreq))
	if base := float64(ReadTopology().BaseMHz); base > 0 && ps.CPUPerfPct > 105 && maxF <= base {
		t.Errorf("perf %.1f%% but max clock %.0f MHz is not above base %.0f MHz", ps.CPUPerfPct, maxF, base)
	}
}

// A local program on port 8085 must not be able to send gotm elsewhere.
func TestExternalSourceDoesNotFollowRedirects(t *testing.T) {
	hit := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(sampleLHM))
	}))
	defer elsewhere.Close()
	bouncer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/data.json", http.StatusFound)
	}))
	defer bouncer.Close()
	src := NewExternalSource(bouncer.URL + "/data.json")
	if err := src.PollOnce(context.Background()); err == nil || hit {
		t.Fatalf("redirect was followed (err=%v, hit=%v)", err, hit)
	}
}

// A Radeon or Intel GPU temperature is not a CPU temperature.
func TestCPUTemperatureIgnoresGPUs(t *testing.T) {
	sensors := []ExtSensor{
		{Hardware: "AMD Radeon RX 7900 XTX", Group: "Temperatures", Name: "GPU Core", Type: "Temperature",
			SensorID: "/gpu-amd/0/temperature/0", Value: "55.0 °C", Num: 55, HasNum: true},
		{Hardware: "INTEL SSDPEKNW010T8", Group: "Temperatures", Name: "Temperature", Type: "Temperature",
			SensorID: "/nvme/0/temperature/0", Value: "40.0 °C", Num: 40, HasNum: true},
	}
	if v, label, ok := CPUTemperature(sensors); ok {
		t.Fatalf("picked %v from %q as the CPU temperature", v, label)
	}
}
