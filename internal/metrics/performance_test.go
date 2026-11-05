//go:build windows

package metrics

import (
	"sort"
	"testing"
	"time"
)

func TestTopProcessesPreservesCPUAndMemoryLeaders(t *testing.T) {
	procs := make([]Proc, 5000)
	for i := range procs {
		procs[i] = Proc{PID: uint32(i + 1), CPU: float64((i * 37) % 5000), Mem: uint64(i * 91)}
	}
	cpu := append([]Proc(nil), procs...)
	mem := append([]Proc(nil), procs...)
	sort.Slice(cpu, func(i, j int) bool { return cpu[i].CPU > cpu[j].CPU })
	sort.Slice(mem, func(i, j int) bool { return mem[i].Mem > mem[j].Mem })
	want := map[uint32]bool{}
	for _, p := range cpu[:10] {
		want[p.PID] = true
	}
	for _, p := range mem[:3] {
		want[p.PID] = true
	}
	got := topProcs(procs)
	if len(got) != len(want) {
		t.Fatalf("got %d leaders, want %d", len(got), len(want))
	}
	for i, p := range got {
		if !want[p.PID] {
			t.Errorf("unexpected leader %d", p.PID)
		}
		if i > 0 && got[i-1].CPU < p.CPU {
			t.Fatal("leaders out of order")
		}
	}
	allocs := testing.AllocsPerRun(10, func() { topProcs(procs) })
	if allocs > 2 {
		t.Fatalf("top selection allocated %.0f objects; want only result storage", allocs)
	}
}

func TestRecorderWindowRemainsOrderedAfterEviction(t *testing.T) {
	r := NewRecorder(time.Minute, time.Second, "")
	r.SetEnabled(true)
	for i := int64(1); i <= 200; i++ {
		r.Add(&Snapshot{T: i * 1000})
	}
	s := r.Samples(0, 0)
	if len(s) != 60 || s[0].T != 141000 || s[59].T != 200000 {
		t.Fatalf("evicted window: %+v", s)
	}
	r.Add(&Snapshot{T: 170000})
	s = r.Samples(0, 0)
	if s[len(s)-1].T != 170000 {
		t.Fatal("clock rewind retained future data")
	}
}

func BenchmarkRecorderFullWindow(b *testing.B) {
	r := NewRecorder(3*time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	s := &Snapshot{T: 1700000000000, Procs: []Proc{{PID: 1, Name: "test", CPU: 2}}}
	for i := 0; i < r.capacity; i++ {
		s.T += 2000
		r.Add(s)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.T += 2000
		r.Add(s)
	}
}

func BenchmarkTopProcesses5000(b *testing.B) {
	procs := make([]Proc, 5000)
	for i := range procs {
		procs[i] = Proc{PID: uint32(i + 1), CPU: float64(i % 101), Mem: uint64(i * 137)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		topProcs(procs)
	}
}
