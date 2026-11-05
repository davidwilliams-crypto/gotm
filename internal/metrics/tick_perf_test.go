//go:build windows

package metrics

import (
	"testing"
	"time"
)

// farFuture keeps the hub's slow refreshes from firing inside a benchmark.
var farFuture = time.Now().Add(24 * time.Hour)

// BenchmarkSamplerTick measures one complete live sampling pass on this
// machine: CPU, memory, PDH (disk, network, GPU), processes and metadata.
func BenchmarkSamplerTick(b *testing.B) {
	s := NewSampler(1000, 60)
	s.perf = newPerfCounters()
	defer s.perf.close()
	s.tick(false)
	s.tick(true)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.tick(true)
	}
}

// BenchmarkSamplerTickNoPDH isolates the sampler's own work (CPU, memory,
// processes, metadata) from the PDH collection.
func BenchmarkSamplerTickNoPDH(b *testing.B) {
	s := NewSampler(1000, 60)
	s.tick(false)
	s.tick(true)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.tick(true)
	}
}

func BenchmarkPerfCountersRead(b *testing.B) {
	pc := newPerfCounters()
	defer pc.close()
	pc.read()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		pc.read()
	}
}

func BenchmarkReadProcesses(b *testing.B) {
	var buf []byte
	if _, err := readProcesses(&buf); err != nil {
		b.Skip(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := readProcesses(&buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseProcesses(b *testing.B) {
	var buf []byte
	if _, err := readProcesses(&buf); err != nil {
		b.Skip(err)
	}
	// Steady state: the previous tick's names are reused, the slice recycled.
	prev := map[uint32]prevProc{}
	raws := parseProcessesInto(buf, nil, nil)
	for _, r := range raws {
		prev[r.PID] = prevProc{create: r.CreateTime, name: r.Name}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if raws = parseProcessesInto(buf, raws, prev); len(raws) == 0 {
			b.Fatal("no processes")
		}
	}
}

// BenchmarkParseProcessesCold decodes every name, as on the first tick.
func BenchmarkParseProcessesCold(b *testing.B) {
	var buf []byte
	if _, err := readProcesses(&buf); err != nil {
		b.Skip(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if len(parseProcesses(buf)) == 0 {
			b.Fatal("no processes")
		}
	}
}

func BenchmarkReadCoreTimes(b *testing.B) {
	n := logicalCPUs()
	var buf []byte
	var dst []coreTimes
	b.ReportAllocs()
	for b.Loop() {
		var err error
		if dst, err = readCoreTimesInto(n, dst, &buf); err != nil {
			b.Fatal(err)
		}
	}
}

func sensorBenchSnapshot() *Snapshot {
	s := &Snapshot{CPU: 12, Mem: MemInfo{Percent: 40, Used: 8 << 30, Available: 8 << 30, CommitTotal: 12 << 30}, CPUPerfPct: 101}
	for i := 0; i < 14; i++ {
		s.Cores = append(s.Cores, float64(i))
		s.CPUFreq = append(s.CPUFreq, 2400+float64(i))
	}
	s.Thermal = []NamedValue{{Name: `\_TZ.TZ01`, Value: 41.2}}
	s.PerDisk = []DiskStat{{Name: "0 C:", Percent: 3, ReadBps: 100, WriteBps: 200}}
	s.GPUs = []GPUAdapter{{ID: "0X00000000_0X00001234", Name: "Benchmark GPU", HasUtilization: true, HasDedicatedUsage: true, HasSharedUsage: true,
		Engines: []GPUEngine{{Name: "3D", Pct: 1}, {Name: "Copy", Pct: 0}, {Name: "Video Decode", Pct: 0}}}}
	return s
}

func benchmarkSensorsUpdate(b *testing.B, detail bool) {
	h := NewSensorHubDeferred()
	h.mu.Lock()
	h.slowAt, h.powerAt = farFuture, farFuture
	h.drives = []DriveHealth{{Index: 0, Model: "Virtual Disk", Bus: "SCSI"}}
	h.maxMHz = []uint32{2400}
	h.mu.Unlock()
	h.SetDetailWanted(detail)
	s := sensorBenchSnapshot()
	host := HostInfo{Hostname: "bench", CPUName: "Benchmark CPU"}
	h.Update(s, host)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.CPU++
		h.Update(s, host)
	}
}

func BenchmarkSensorsUpdateHidden(b *testing.B)  { benchmarkSensorsUpdate(b, false) }
func BenchmarkSensorsUpdateVisible(b *testing.B) { benchmarkSensorsUpdate(b, true) }
