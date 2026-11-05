//go:build windows

package metrics

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// randomProcs makes snapshots full of ties, mixed case, PID reuse, dead and
// cyclic parents, and the idle process, to compare against the old builder.
func randomProcs(r *rand.Rand, n int) []Proc {
	names := []string{"svchost.exe", "SvcHost.exe", "chrome.exe", "Chrome.EXE", "a.exe", "B.exe", "Ünïcode.exe", "ÜNÏCODE.exe", "System", ""}
	users := []string{"", "SYSTEM", "system", "Alice", "alice", "Bob"}
	perm := r.Perm(n * 3)
	out := make([]Proc, n)
	for i := range out {
		pid := uint32(perm[i]) + 1 // unique, as in a real snapshot
		if i == 0 {
			pid = 0
		}
		out[i] = Proc{
			PID: pid, Name: names[r.Intn(len(names))], User: users[r.Intn(len(users))],
			Path: `C:\Bin\` + names[r.Intn(len(names))],
			CPU:  float64(r.Intn(4)), Mem: uint64(r.Intn(3)), IO: float64(r.Intn(2)), GPU: float64(r.Intn(70)),
			Threads: uint32(r.Intn(3)), Handles: uint32(r.Intn(3)), Prio: []string{"Normal", "High", "Idle"}[r.Intn(3)],
			Session: uint32(r.Intn(2)), Start: int64(r.Intn(50)), GPUUnavailable: r.Intn(9) == 0,
		}
	}
	for i := range out {
		switch r.Intn(5) {
		case 0:
			out[i].PPID = uint32(r.Intn(n * 3)) // often dead, sometimes reused
		default:
			out[i].PPID = out[r.Intn(n)].PID // includes cycles and self-parents
		}
	}
	return out
}

type cmpCase struct {
	name string
	cmp  func(a, b *Row) int
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func withPID(c func(a, b *Row) int, desc bool) func(a, b *Row) int {
	return func(a, b *Row) int {
		v := c(a, b)
		if desc {
			v = -v
		}
		if v != 0 {
			return v
		}
		return cmpU32(a.PID, b.PID)
	}
}

func equivCases() []cmpCase {
	name := func(a, b *Row) int { return strings.Compare(a.NameFold(), b.NameFold()) }
	user := func(a, b *Row) int { return strings.Compare(a.UserFold(), b.UserFold()) }
	cpu := func(a, b *Row) int { return cmpF(a.CPU, b.CPU) }
	return []cmpCase{
		{"name-asc", withPID(name, false)},
		{"name-desc", withPID(name, true)},
		{"user-desc", withPID(user, true)},
		{"cpu-desc", withPID(cpu, true)},
		{"cpu-asc", withPID(cpu, false)},
		{"all-equal", func(a, b *Row) int { return 0 }}, // only snapshot order decides
	}
}

// oldLess is how the old builder saw the comparator: case-folding by
// strings.ToLower on every comparison, exactly as procs.go used to.
func oldLess(c func(a, b *Row) int) func(a, b *Row) bool {
	return func(a, b *Row) bool {
		x, y := *a, *b
		x.fold, y.fold = nil, nil
		return c(&x, &y) < 0
	}
}

func stripFold(rows []Row) []Row {
	out := append([]Row(nil), rows...)
	for i := range out {
		out[i].fold = nil
	}
	return out
}

func TestRowBuilderMatchesPreviousOrdering(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var b RowBuilder
	for iter := 0; iter < 300; iter++ {
		procs := randomProcs(r, 1+r.Intn(60))
		if iter%3 == 1 { // same identities, changed texts: cached folds must refresh
			for i := range procs {
				procs[i].Name = strings.ToUpper(procs[i].Name)
			}
		}
		collapsed := map[uint32]bool{}
		for _, p := range procs {
			if r.Intn(4) == 0 {
				collapsed[p.PID] = true
			}
		}
		filter := []string{"", "", "svc", "HOST", "1", "ü", " A ", "bin"}[r.Intn(8)]
		for _, layout := range []Layout{LayoutFlat, LayoutTree, LayoutGroup} {
			for _, c := range equivCases() {
				for _, hide := range []bool{false, true} {
					v := View{Layout: layout, Filter: filter, HideIdle: hide, Collapsed: collapsed}
					vOld := v
					vOld.Less = oldLess(c.cmp)
					want := refBuildRows(procs, vOld)
					v.Cmp = c.cmp
					got := stripFold(b.Build(procs, v))
					if len(want) == 0 && len(got) == 0 {
						continue
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("iter %d layout %d %s hide=%v filter %q:\n got %v\nwant %v", iter, layout, c.name, hide, filter, keys(got), keys(want))
					}
					// a nil Cmp falls back to Less, and BuildRows matches too
					if got2 := stripFold(BuildRows(procs, vOld)); !reflect.DeepEqual(got2, want) {
						t.Fatalf("Less fallback differs: iter %d layout %d %s", iter, layout, c.name)
					}
				}
			}
		}
	}
}

func keys(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%s/%d", r.Key, r.Depth)
	}
	return out
}

func TestRowBuilderPrunesExitedProcesses(t *testing.T) {
	var b RowBuilder
	procs := benchProcs(400, 3)
	b.Build(procs, View{})
	for round := 0; round < 3; round++ {
		b.Build(procs[:10], View{})
	}
	if len(b.cache) > 10+256 {
		t.Fatalf("cache kept %d entries for 10 live processes", len(b.cache))
	}
	for i := 0; i < 64; i++ {
		b.Build(procs[:10], View{})
	}
	if len(b.cache) != 10 {
		t.Fatalf("periodic prune left %d entries, want 10", len(b.cache))
	}
}

func TestRowBuilderRowsAreIndependentOfLaterBuilds(t *testing.T) {
	var b RowBuilder
	procs := benchProcs(50, 4)
	first := b.Build(procs, View{Layout: LayoutTree})
	snapshot := stripFold(first)
	for i := range procs {
		procs[i].Name = "renamed.exe"
		procs[i].CPU += 7
	}
	_ = b.Build(procs, View{Layout: LayoutTree})
	_ = b.Build(procs, View{Layout: LayoutGroup})
	_ = b.Build(procs, View{Layout: LayoutFlat})
	if !reflect.DeepEqual(stripFold(first), snapshot) {
		t.Fatal("a later build changed rows handed out earlier")
	}
	for i := range first {
		if first[i].NameFold() != strings.ToLower(snapshot[i].Name) {
			t.Fatal("cached fold of an earlier row changed")
		}
		if cap(first[i].PIDs) != 1 {
			t.Fatal("single-process PIDs slice can be appended into its neighbour")
		}
	}
}

func TestBuildRowsAllocations(t *testing.T) {
	procs := benchProcs(2000, 5)
	var b RowBuilder
	v := View{Layout: LayoutFlat, HideIdle: true, Cmp: benchNameAsc}
	b.Build(procs, v)
	allocs := testing.AllocsPerRun(5, func() { b.Build(procs, v) })
	if allocs > 10 {
		t.Fatalf("flat build allocates %.0f times per tick", allocs)
	}
	v.Layout = LayoutTree
	b.Build(procs, v)
	if allocs = testing.AllocsPerRun(5, func() { b.Build(procs, v) }); allocs > 10 {
		t.Fatalf("tree build allocates %.0f times per tick", allocs)
	}
}
