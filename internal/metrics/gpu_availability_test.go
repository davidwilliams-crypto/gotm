//go:build windows

package metrics

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGPUAvailabilityDistinguishesMissingAndIdle(t *testing.T) {
	const id = "0X00000000_0X00000001"
	static := []gpuStatic{{id: id, name: "Test GPU"}}
	missing := buildGPUs(static, nil, nil, nil, nil, nil)[0]
	if missing.HasUtilization || summarizeGPUs([]GPUAdapter{missing}).OK {
		t.Fatal("inventory alone is not utilization telemetry")
	}
	eng := []PDHInstanceValue{{Instance: "pid_12_luid_0x00000000_0x00000001_phys_0_eng_0_engtype_3D", Value: 0}}
	mem := []PDHInstanceValue{{Instance: "luid_0x00000000_0x00000001_phys_0", Value: 0}}
	idle := buildGPUs(static, eng, mem, mem, nil, nil)[0]
	if !idle.HasUtilization || !idle.HasDedicatedUsage || !idle.HasSharedUsage || !summarizeGPUs([]GPUAdapter{idle}).OK {
		t.Fatalf("valid zero counters must remain available: %+v", idle)
	}
	eng[0].Value = math.NaN()
	mem[0].Value = -1
	invalid := buildGPUs(static, eng, mem, mem, nil, nil)[0]
	if invalid.HasUtilization || invalid.HasDedicatedUsage || invalid.HasSharedUsage {
		t.Fatal("invalid counters became measured values")
	}
}

func TestGPUInventorySurvivesUnavailablePDH(t *testing.T) {
	pc := &perfCounters{gpuRead: &gpuReader{static: []gpuStatic{{id: "test", name: "Legacy display"}}, lastDiscovery: time.Now()}}
	s := pc.read()
	if len(s.GPUs) != 1 || s.GPUs[0].Name != "Legacy display" || s.GPU.OK {
		t.Fatalf("inventory must survive missing performance query: %+v", s)
	}
}

func TestRecorderPreservesMissingGPUAsUnavailable(t *testing.T) {
	r := NewRecorder(time.Hour, time.Second, "")
	r.SetEnabled(true)
	r.Add(&Snapshot{T: 1700000000000, GPU: GPUInfo{}})
	r.Add(&Snapshot{T: 1700000002000, GPU: GPUInfo{OK: true}})
	s := r.Samples(0, 1800000000000)
	if len(s) != 2 || !s[0].GPUUnavailable || s[1].GPUUnavailable {
		t.Fatalf("availability lost: %+v", s)
	}
	path := filepath.Join(t.TempDir(), "recording.csv")
	if err := r.ExportCSV(path, 0, 1800000000000); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil || len(rows) != 3 || rows[1][3] != "" || rows[2][3] != "0.0" {
		t.Fatalf("CSV must distinguish unavailable and idle: %q, %v", b, err)
	}
}
