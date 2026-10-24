//go:build windows

package metrics

import (
	"golang.org/x/sys/windows/registry"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestRyzenParamsLayoutMatchesSDKHeader(t *testing.T) {
	var p ryzenParams
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"size", unsafe.Sizeof(p), 192},
		{"FreqLen", unsafe.Offsetof(p.FreqLen), 8},
		{"Freq", unsafe.Offsetof(p.Freq), 16},
		{"CurTemp", unsafe.Offsetof(p.CurTemp), 40},
		{"PeakV", unsafe.Offsetof(p.PeakV), 48},
		{"Temp", unsafe.Offsetof(p.Temp), 72},
		{"PeakSpeed", unsafe.Offsetof(p.PeakSpeed), 96},
		{"PPTLimit", unsafe.Offsetof(p.PPTLimit), 104},
		{"PPTValue", unsafe.Offsetof(p.PPTValue), 108},
		{"TDCLimVDD", unsafe.Offsetof(p.TDCLimVDD), 112},
		{"FCLK", unsafe.Offsetof(p.FCLK), 140},
		{"VDDPower", unsafe.Offsetof(p.VDDPower), 164},
		{"EDCValCCD", unsafe.Offsetof(p.EDCValCCD), 184},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestDecodeRyzenParams(t *testing.T) {
	freq := []float64{4500, 0, 5050}
	state := []float64{80, 0, 99}
	cur := []float64{4600, 0, 5100}
	temp := []float64{55.5, 0, 61.25}
	p := &ryzenParams{
		FreqLen: 3,
		Freq:    unsafe.Pointer(&freq[0]), State: unsafe.Pointer(&state[0]),
		CurFreq: unsafe.Pointer(&cur[0]), CurTemp: unsafe.Pointer(&temp[0]),
		Temp: 63.5, PeakSpeed: 5150, PPTValue: 88.5, PPTLimit: 162, SOCPower: 12.5,
	}
	r, err := decodeRyzenParams(p, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if r.TempC != 63.5 || r.PPT != 88.5 || r.PPTLimit != 162 || r.SOCPower != 12.5 || r.PeakMHz != 5150 {
		t.Errorf("scalars wrong: %+v", r)
	}
	if len(r.Cores) != 3 || r.Cores[2].EffMHz != 5050 || r.Cores[2].TempC != 61.25 || r.Cores[0].C0Pct != 80 {
		t.Errorf("cores wrong: %+v", r.Cores)
	}

	for _, bad := range []*ryzenParams{
		{Temp: 0},
		{Temp: 300},
		{Temp: 50, FreqLen: 100000},
	} {
		if _, err := decodeRyzenParams(bad, time.Now()); err == nil {
			t.Errorf("should reject %+v", bad)
		}
	}
}

// fakeSDK stands in for Platform.dll/Device.dll: C++-style objects whose first
// word points at a table of stdcall/x64 callbacks.
type fakeSDK struct {
	platform, mgr, cpu []uintptr
	vtP, vtM, vtC      [24]uintptr
	name, pkg          []uint16
	calls              map[string]int
	initOK             bool
}

func newFakeSDK(initOK bool) *fakeSDK {
	f := &fakeSDK{calls: map[string]int{}, initOK: initOK}
	f.name = syscall.StringToUTF16("AMD Ryzen 9 7950X3D 16-Core Processor")
	f.pkg = syscall.StringToUTF16("Socket AM5")

	f.vtP[slotPlatformInit] = syscall.NewCallback(func(this, vendor, cpuOnly uintptr) uintptr {
		f.calls["Init"]++
		if f.initOK {
			return 1
		}
		return 0
	})
	f.vtP[slotPlatformUnInit] = syscall.NewCallback(func(this uintptr) uintptr { f.calls["UnInit"]++; return 1 })
	f.vtP[slotPlatformDevMgr] = syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(unsafe.Pointer(&f.mgr[0]))
	})
	f.vtM[slotMgrGetDevice] = syscall.NewCallback(func(this, typ, idx uintptr) uintptr {
		f.calls["GetDevice"]++
		if typ == dtCPU && idx == 0 {
			return uintptr(unsafe.Pointer(&f.cpu[0]))
		}
		return 0
	})
	f.vtC[slotDeviceGetType] = syscall.NewCallback(func(this uintptr) uintptr { return dtCPU })
	f.vtC[slotDeviceGetName] = syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(unsafe.Pointer(&f.name[0]))
	})
	f.vtC[slotCPUGetPackage] = syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(unsafe.Pointer(&f.pkg[0]))
	})
	f.vtC[slotCPUGetCPUParams] = syscall.NewCallback(func(this, out uintptr) uintptr {
		f.calls["GetCPUParameters"]++
		p := (*ryzenParams)(rawPtr(out))
		p.Temp, p.PPTValue, p.PPTLimit = 71.0, 120.5, 162
		return 0
	})

	f.platform = []uintptr{uintptr(unsafe.Pointer(&f.vtP[0]))}
	f.mgr = []uintptr{uintptr(unsafe.Pointer(&f.vtM[0]))}
	f.cpu = []uintptr{uintptr(unsafe.Pointer(&f.vtC[0]))}
	return f
}

func TestRyzenVtableCallsAgainstFakeSDK(t *testing.T) {
	f := newFakeSDK(true)
	r := newRyzenSDKWithMarker(regMarker{})
	rd, err := r.pollFake(uintptr(unsafe.Pointer(&f.platform[0])))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rd.TempC-71) > 1e-9 || rd.PPT != 120.5 || rd.PPTLimit != 162 {
		t.Errorf("reading = %+v", rd)
	}
	cpu, pkg := r.Identity()
	if !strings.Contains(cpu, "7950X3D") || pkg != "Socket AM5" {
		t.Errorf("identity = %q / %q", cpu, pkg)
	}
	if f.calls["Init"] != 1 || f.calls["GetDevice"] != 1 || f.calls["GetCPUParameters"] != 1 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestRyzenInitFailureIsReportedNotFatal(t *testing.T) {
	f := newFakeSDK(false)
	_, err := newRyzenSDKWithMarker(regMarker{}).pollFake(uintptr(unsafe.Pointer(&f.platform[0])))
	if err == nil || !strings.Contains(err.Error(), "Init failed") {
		t.Fatalf("err = %v", err)
	}
	if f.calls["GetCPUParameters"] != 0 {
		t.Error("must not read after a failed Init")
	}
}

func TestRyzenCrashMarkerDisablesStart(t *testing.T) {
	m := regMarker{registry.CURRENT_USER, `Software\gotm-test\` + t.Name(), "RyzenSDKPending"}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, m.path)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\gotm-test`)
	})
	if err := m.set(true); err != nil {
		t.Fatal(err)
	}
	r := newRyzenSDKWithMarker(m)
	r.Start()
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := r.State(); st == RyzenUnavailable {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, detail := r.State()
	if st != RyzenUnavailable || !strings.Contains(detail, "previous start-up") {
		t.Fatalf("state = %v %q", st, detail)
	}
	if _, ok := r.Reading(); ok {
		t.Error("no reading may be offered")
	}
}

func TestRyzenRefusesUnsignedAndMissingSDK(t *testing.T) {
	if IsElevated() {
		t.Skip("elevated test run would try the real SDK path")
	}
	// Not elevated: the worker must stop at the privilege check, loading nothing.
	r := newRyzenSDKWithMarker(regMarker{})
	r.Start()
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := r.State(); st == RyzenUnavailable {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, d := r.State(); st != RyzenUnavailable || !strings.Contains(d, "administrator") {
		t.Fatalf("state = %v %q", st, d)
	}
}

func TestVerifySignedDetectsTampering(t *testing.T) {
	src := filepath.Join(`D:\Claude\rea\gotm\third_party\RyzenMasterSDK-msi`, "platform.dll")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Skip("extracted AMD SDK not present")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "Platform.dll")
	if err := os.WriteFile(good, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySigned(good); err != nil {
		t.Fatalf("genuine AMD Platform.dll should verify: %v", err)
	}
	b[len(b)/3]++
	bad := filepath.Join(dir, "Tampered.dll")
	if err := os.WriteFile(bad, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySigned(bad); err == nil {
		t.Fatal("tampered DLL must fail signature verification")
	}
}

func TestSensorTreeUsesRyzenSDKWhenRunning(t *testing.T) {
	r := newRyzenSDKWithMarker(regMarker{})
	r.mu.Lock()
	r.state, r.have = RyzenRunning, true
	r.last = RyzenReading{
		TempC: 66.5, PPT: 101.2, PPTLimit: 162, VDDPower: 70.1, SOCPower: 11.9,
		CoreVAvg: 1.231, SocV: 1.05, TDCVDD: 60, TDCVDDLimit: 110, FCLKMHz: 2000,
		Cores: []RyzenCore{{Index: 0, EffMHz: 5100, TempC: 58}, {Index: 1, EffMHz: 4800, TempC: 0}},
	}
	r.mu.Unlock()

	h := NewSensorHubWith(nil)
	h.SetRyzen(r)
	rows := h.Update(&Snapshot{CPU: 3, Cores: []float64{3, 4}, Mem: MemInfo{Percent: 10}}, ReadHostInfo())
	by := map[string]SensorRow{}
	for _, row := range rows {
		by[row.ID] = row
	}
	want := map[string]string{
		"cpu.temp.rz":        "66.5 °C",
		"cpu.temp.rz.c0":     "58.0 °C",
		"cpu.pwr.rz.ppt":     "101.2 W",
		"cpu.pwr.rz.pptlim":  "162.0 W",
		"cpu.elec.vavg":      "1.231 V",
		"cpu.elec.tdcvddlim": "110.0 A",
		"cpu.clk.rz.eff0":    "5.10 GHz",
		"cpu.clk.rz.fclk":    "2.00 GHz",
	}
	for id, v := range want {
		if got := by[id].Value; got != v {
			t.Errorf("%s = %q, want %q", id, got, v)
		}
	}
	if _, bad := by["cpu.temp.rz.c1"]; bad {
		t.Error("a zero (parked) core temperature must not be listed")
	}
	if temp, label, ok := h.CPUTemperature(); !ok || temp != 66.5 || !strings.Contains(label, "Tctl") {
		t.Errorf("CPUTemperature = %v %q %v", temp, label, ok)
	}
}

// If Init succeeds but no CPU device is found, the SDK must be UnInit'ed
// before gotm unloads it (its threads and driver handle are live).
func TestRyzenUnInitsAfterPartialInit(t *testing.T) {
	f := newFakeSDK(true)
	f.cpu = nil // GetDevice will hand back a pointer to nothing; make it return 0
	f.vtM[slotMgrGetDevice] = syscall.NewCallback(func(this, typ, idx uintptr) uintptr { return 0 })
	_, err := newRyzenSDKWithMarker(regMarker{}).pollFake(uintptr(unsafe.Pointer(&f.platform[0])))
	if err == nil || !strings.Contains(err.Error(), "no CPU device") {
		t.Fatalf("err = %v", err)
	}
	if f.calls["UnInit"] != 1 {
		t.Errorf("UnInit called %d times after a partial init, want 1", f.calls["UnInit"])
	}
}
