//go:build windows

package metrics

import "testing"

func fixture() []Proc {
	// 4 System
	//   100 launcher            (2 children)
	//     101 worker  cpu 10
	//     102 worker  cpu 30
	//   200 orphan (parent 999 does not exist)
	//   300 reused-pid child: its "parent" 50 started AFTER it, so it is a root
	// 50 unrelated (started later than 300)
	return []Proc{
		{PID: 4, PPID: 0, Name: "System", Start: 1},
		{PID: 100, PPID: 4, Name: "launcher.exe", Start: 10, CPU: 1, Mem: 100},
		{PID: 101, PPID: 100, Name: "worker.exe", Start: 20, CPU: 10, Mem: 10},
		{PID: 102, PPID: 100, Name: "worker.exe", Start: 21, CPU: 30, Mem: 20},
		{PID: 200, PPID: 999, Name: "orphan.exe", Start: 30},
		{PID: 300, PPID: 50, Name: "child.exe", Start: 40},
		{PID: 50, PPID: 0, Name: "unrelated.exe", Start: 90},
	}
}

func byCPUDesc(a, b *Row) bool {
	if a.CPU != b.CPU {
		return a.CPU > b.CPU
	}
	return a.PID < b.PID
}

func pids(rows []Row) []uint32 {
	out := make([]uint32, len(rows))
	for i, r := range rows {
		out[i] = r.PID
	}
	return out
}

func equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTreeNestingAndSiblingOrder(t *testing.T) {
	rows := BuildRows(fixture(), View{Layout: LayoutTree, Less: byCPUDesc})
	got := pids(rows)
	// roots: 4(sys), 200, 300, 50 ; all have cpu 0 here so ordered by PID
	// under System: launcher; under launcher: workers by CPU desc (102, 101)
	want := []uint32{4, 100, 102, 101, 50, 200, 300}
	if !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	depth := map[uint32]int{}
	for _, r := range rows {
		depth[r.PID] = r.Depth
	}
	if depth[4] != 0 || depth[100] != 1 || depth[101] != 2 || depth[102] != 2 {
		t.Errorf("bad depths: %v", depth)
	}
	if depth[300] != 0 {
		t.Errorf("PID-reuse child must be a root, depth=%d", depth[300])
	}
}

func TestCollapsedRowShowsSubtreeTotals(t *testing.T) {
	rows := BuildRows(fixture(), View{Layout: LayoutTree, Less: byCPUDesc, Collapsed: map[uint32]bool{100: true}})
	var l *Row
	for i := range rows {
		if rows[i].PID == 100 {
			l = &rows[i]
		}
		if rows[i].PID == 101 || rows[i].PID == 102 {
			t.Fatalf("child %d should be hidden when parent collapsed", rows[i].PID)
		}
	}
	if l == nil || !l.Collapsed || !l.HasKids || l.Desc != 2 {
		t.Fatalf("launcher row wrong: %+v", l)
	}
	if l.CPU != 41 || l.Mem != 130 {
		t.Errorf("subtree totals cpu=%v mem=%v, want 41 / 130", l.CPU, l.Mem)
	}
}

func TestFilterKeepsAncestorsAndOpensCollapsed(t *testing.T) {
	rows := BuildRows(fixture(), View{Layout: LayoutTree, Less: byCPUDesc, Filter: "worker",
		Collapsed: map[uint32]bool{100: true}})
	got := pids(rows)
	want := []uint32{4, 100, 102, 101}
	if !equal(got, want) {
		t.Fatalf("filtered = %v, want %v", got, want)
	}
}

func TestParentCycleDoesNotHangOrDropRows(t *testing.T) {
	cyc := []Proc{
		{PID: 1, PPID: 2, Name: "a", Start: 1},
		{PID: 2, PPID: 1, Name: "b", Start: 1},
	}
	rows := BuildRows(cyc, View{Layout: LayoutTree})
	if len(rows) != 2 {
		t.Fatalf("cycle rows = %d, want 2", len(rows))
	}
}

func TestGroupAndFlatLayouts(t *testing.T) {
	g := BuildRows(fixture(), View{Layout: LayoutGroup, Less: byCPUDesc})
	var w *Row
	for i := range g {
		if g[i].Name == "worker.exe" {
			w = &g[i]
		}
	}
	if w == nil || w.Count != 2 || w.CPU != 40 || len(w.PIDs) != 2 {
		t.Fatalf("worker group wrong: %+v", w)
	}
	f := BuildRows(fixture(), View{Layout: LayoutFlat, HideIdle: true, Less: byCPUDesc})
	if len(f) != len(fixture()) {
		t.Errorf("flat rows = %d", len(f))
	}
	if f[0].PID != 102 {
		t.Errorf("flat not sorted by cpu desc: first=%d", f[0].PID)
	}
}
