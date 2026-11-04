//go:build windows

package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestSamplerBoundsProcessCPUWhenCounterWindowsDiffer(t *testing.T) {
	s := NewSampler(500, 60)
	s.tick(false)
	// Process counters and core counters are separate OS queries. Simulate
	// the process baseline lagging behind the core baseline, without PID reuse.
	for pid, prev := range s.prevProcs {
		prev.cpu = 0
		s.prevProcs[pid] = prev
	}
	time.Sleep(20 * time.Millisecond)
	s.tick(true)
	for _, p := range s.Latest().Procs {
		if p.CPU < 0 || p.CPU > 100 || math.IsNaN(p.CPU) {
			t.Fatalf("PID %d CPU=%v outside 0..100", p.PID, p.CPU)
		}
	}
}

func TestLiveGPUSummaryMatchesDisplayedAdapters(t *testing.T) {
	pc := newPerfCounters()
	if pc == nil {
		t.Skip("PDH unavailable")
	}
	defer pc.close()
	time.Sleep(1100 * time.Millisecond)
	s := pc.read()
	var busiest float64
	var memory uint64
	for _, g := range s.GPUs {
		busiest = math.Max(busiest, g.Pct)
		memory += g.DedicatedUsed
	}
	if s.GPU.Percent != busiest || s.GPU.MemBytes != memory {
		t.Fatalf("summary=%+v; visible adapters max=%v memory=%d adapters=%+v", s.GPU, busiest, memory, s.GPUs)
	}
}

func TestTemperatureGuidanceMatchesCPU(t *testing.T) {
	for _, tc := range []struct {
		name string
		amd  bool
	}{
		{"Intel(R) Xeon(R) CPU E5-2697 v3", false},
		{"AMD Ryzen 9 7950X3D", true},
		{"Unknown CPU", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SensorHub{mm: map[string]*minMax{}, slowAt: time.Now(), powerAt: time.Now()}
			rows := h.Update(&Snapshot{}, HostInfo{CPUName: tc.name})
			r, ok := rowByID(rows, "cpu.temp.die")
			if !ok || strings.Contains(r.Name, "external sensor") || !strings.Contains(r.Name, "unavailable") {
				t.Fatalf("wrong guidance for %q: %+v", tc.name, r)
			}
		})
	}
}
