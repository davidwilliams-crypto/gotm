//go:build windows

package metrics

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// benchProcs makes a realistic process list: a shallow tree under a few
// launchers, repeated executable names (for grouping) and mixed-case text.
func benchProcs(n int, seed int64) []Proc {
	r := rand.New(rand.NewSource(seed))
	names := make([]string, 60)
	for i := range names {
		names[i] = fmt.Sprintf("Program%02d.exe", i)
	}
	users := []string{`NT AUTHORITY\SYSTEM`, `NT AUTHORITY\LOCAL SERVICE`, `HOST\Administrator`, `HOST\User`}
	out := make([]Proc, n)
	for i := range out {
		pid := uint32(4 * (i + 1))
		var ppid uint32
		if i > 0 {
			ppid = out[r.Intn(i)].PID // parents always earlier: a tree
			if r.Intn(10) == 0 {
				ppid = uint32(99999 + r.Intn(100)) // dead parent
			}
		}
		name := names[r.Intn(len(names))]
		out[i] = Proc{
			PID: pid, PPID: ppid, Name: name,
			Path: `C:\Program Files\Vendor\` + name,
			User: users[r.Intn(len(users))],
			CPU:  r.Float64() * 5, Mem: uint64(r.Intn(1 << 28)), Private: uint64(r.Intn(1 << 27)),
			Threads: uint32(r.Intn(80)), Handles: uint32(r.Intn(2000)),
			Prio: "Normal", Session: uint32(r.Intn(2)), Start: int64(1000 + i),
		}
	}
	return out
}

func benchCPUDesc(a, b *Row) int {
	switch {
	case a.CPU > b.CPU:
		return -1
	case a.CPU < b.CPU:
		return 1
	}
	return cmpU32(a.PID, b.PID)
}

func benchNameAsc(a, b *Row) int {
	if c := strings.Compare(a.NameFold(), b.NameFold()); c != 0 {
		return c
	}
	return cmpU32(a.PID, b.PID)
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func benchmarkBuild(b *testing.B, n int, v View) {
	procs := benchProcs(n, 1)
	r := rand.New(rand.NewSource(2))
	var rb RowBuilder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range procs {
			procs[j].CPU = r.Float64() * 5 // values change every tick
		}
		_ = rb.Build(procs, v)
	}
}

func BenchmarkBuildRows(b *testing.B) {
	for _, n := range []int{300, 2000} {
		b.Run(fmt.Sprintf("flat-cpu/%d", n), func(b *testing.B) {
			benchmarkBuild(b, n, View{Layout: LayoutFlat, HideIdle: true, Cmp: benchCPUDesc})
		})
		b.Run(fmt.Sprintf("flat-name/%d", n), func(b *testing.B) {
			benchmarkBuild(b, n, View{Layout: LayoutFlat, HideIdle: true, Cmp: benchNameAsc})
		})
		b.Run(fmt.Sprintf("flat-filter/%d", n), func(b *testing.B) {
			benchmarkBuild(b, n, View{Layout: LayoutFlat, HideIdle: true, Filter: "Program1", Cmp: benchCPUDesc})
		})
		b.Run(fmt.Sprintf("tree/%d", n), func(b *testing.B) {
			benchmarkBuild(b, n, View{Layout: LayoutTree, HideIdle: true, Cmp: benchCPUDesc})
		})
		b.Run(fmt.Sprintf("group/%d", n), func(b *testing.B) {
			benchmarkBuild(b, n, View{Layout: LayoutGroup, HideIdle: true, Cmp: benchCPUDesc})
		})
	}
}
