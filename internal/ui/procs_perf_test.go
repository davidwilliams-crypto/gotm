//go:build windows

package ui

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"gotm/internal/metrics"
)

// oldProcLess are the column comparisons procs.go used before sorting moved
// to precomputed keys: the ordering oracle.
var oldProcLess = map[string]func(a, b *metrics.Row) bool{
	"name":    func(a, b *metrics.Row) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) },
	"pid":     func(a, b *metrics.Row) bool { return a.PID < b.PID },
	"ppid":    func(a, b *metrics.Row) bool { return a.PPID < b.PPID },
	"cpu":     func(a, b *metrics.Row) bool { return a.CPU < b.CPU },
	"mem":     func(a, b *metrics.Row) bool { return a.Mem < b.Mem },
	"private": func(a, b *metrics.Row) bool { return a.Private < b.Private },
	"io":      func(a, b *metrics.Row) bool { return a.IO < b.IO },
	"gpu":     func(a, b *metrics.Row) bool { return a.GPU < b.GPU },
	"threads": func(a, b *metrics.Row) bool { return a.Threads < b.Threads },
	"handles": func(a, b *metrics.Row) bool { return a.Handles < b.Handles },
	"prio":    func(a, b *metrics.Row) bool { return a.Prio < b.Prio },
	"user":    func(a, b *metrics.Row) bool { return strings.ToLower(a.User) < strings.ToLower(b.User) },
	"session": func(a, b *metrics.Row) bool { return a.Session < b.Session },
	"start":   func(a, b *metrics.Row) bool { return a.Start < b.Start },
	"path":    func(a, b *metrics.Row) bool { return strings.ToLower(a.Path) < strings.ToLower(b.Path) },
}

func oldModelLess(c func(a, b *metrics.Row) bool, desc bool) func(a, b *metrics.Row) bool {
	return func(a, b *metrics.Row) bool {
		if c(a, b) {
			return !desc
		}
		if c(b, a) {
			return desc
		}
		return a.PID < b.PID
	}
}

func tiedProcs(r *rand.Rand, n int) []metrics.Proc {
	names := []string{"svchost.exe", "SVCHOST.exe", "Code.exe", "code.exe", "Ä.exe", "ä.exe"}
	out := make([]metrics.Proc, n)
	perm := r.Perm(3 * n)
	for i := range out {
		out[i] = metrics.Proc{
			PID: uint32(perm[i]) + 1, Name: names[r.Intn(len(names))], User: []string{"A", "a", "", "SYSTEM"}[r.Intn(4)],
			Path: []string{`C:\X`, `c:\x`, `C:\Y`}[r.Intn(3)], CPU: float64(r.Intn(3)), Mem: uint64(r.Intn(3)),
			Private: uint64(r.Intn(2)), IO: float64(r.Intn(2)), GPU: float64(r.Intn(2)), Threads: uint32(r.Intn(3)),
			Handles: uint32(r.Intn(3)), Prio: []string{"High", "Normal"}[r.Intn(2)], Session: uint32(r.Intn(2)),
			Start: int64(r.Intn(4)),
		}
		out[i].PPID = uint32(perm[r.Intn(3*n)]) + 1
	}
	return out
}

func rowKeys(rows []metrics.Row) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = fmt.Sprintf("%s/%d/%d", rows[i].Key, rows[i].Depth, rows[i].Count)
	}
	return out
}

// Every column, both directions, every layout: identical order to before,
// including all the ties.
func TestProcessSortOrderUnchanged(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	m := &procModel{cols: colDefs()}
	var b metrics.RowBuilder
	for iter := 0; iter < 40; iter++ {
		procs := tiedProcs(r, 80)
		for col, c := range m.cols {
			old := oldProcLess[c.key]
			if old == nil {
				t.Fatalf("no oracle for column %s", c.key)
			}
			for _, order := range []walk.SortOrder{walk.SortAscending, walk.SortDescending} {
				m.sortCol, m.sortOrder = col, order
				for _, layout := range []metrics.Layout{metrics.LayoutFlat, metrics.LayoutTree, metrics.LayoutGroup} {
					v := metrics.View{Layout: layout, HideIdle: true, Filter: []string{"", "exe", "x"}[iter%3]}
					v.Less = oldModelLess(old, order == walk.SortDescending)
					want := metrics.BuildRows(procs, v)
					v.Less, v.Cmp = nil, m.cmp()
					got := b.Build(procs, v)
					if !reflect.DeepEqual(rowKeys(got), rowKeys(want)) {
						t.Fatalf("%s %v layout %d differs:\n got %v\nwant %v", c.key, order, layout, rowKeys(got), rowKeys(want))
					}
				}
			}
		}
	}
}

// oldGridResort is gridModel.resort before keys were precomputed.
func oldGridResort(rows [][]string, c int, desc, numeric bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := cell(rows[i], c), cell(rows[j], c)
		var less bool
		if numeric {
			av, aok := sortValue(a)
			bv, bok := sortValue(b)
			if aok != bok {
				return aok
			}
			if aok && bok {
				if av == bv {
					return false
				}
				less = av < bv
			} else {
				la, lb := strings.ToLower(a), strings.ToLower(b)
				if la == lb {
					return false
				}
				less = la < lb
			}
		} else {
			la, lb := strings.ToLower(a), strings.ToLower(b)
			if la == lb {
				return false
			}
			less = la < lb
		}
		if desc {
			return !less
		}
		return less
	})
}

func TestGridSortOrderUnchanged(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	values := []string{"", "1 KB", "1,024", "2 MB", "2.0 MB", "0.5 GB", "n/a", "Apps", "apps", "2026-10-11", "-3", "10 %", "1.4 GHz", "x"}
	for iter := 0; iter < 200; iter++ {
		rows := make([][]string, 1+r.Intn(40))
		for i := range rows {
			rows[i] = []string{fmt.Sprint("k", i), values[r.Intn(len(values))]}
		}
		for _, numeric := range []bool{false, true} {
			for _, desc := range []bool{false, true} {
				want := append([][]string(nil), rows...)
				oldGridResort(want, 1, desc, numeric)
				m := &gridModel{sortCol: 1, sortDesc: desc, numeric: map[int]bool{1: numeric}}
				got := append([][]string(nil), rows...)
				m.sortRows(got)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("numeric=%v desc=%v:\n got %v\nwant %v", numeric, desc, got, want)
				}
			}
		}
	}
}

func TestGridUpdateSortsOnce(t *testing.T) {
	m := &gridModel{keyCol: 0, sortCol: 1, numeric: map[int]bool{1: true}, rows: [][]string{{"A", "1"}, {"B", "2"}}}
	rows := [][]string{{"B", "0"}, {"A", "5"}}
	if m.update(rows) {
		t.Fatal("changed order must not update in place")
	}
	if !m.presorted(rows) {
		t.Fatal("update did not leave its sorted rows for set")
	}
	m.set(rows)
	if m.rows[0][0] != "B" || m.presorted(rows) {
		t.Fatalf("set lost order or kept a stale presort mark: %v", m.rows)
	}
}

// The selection follows its process through a reorder and through rows
// appearing and disappearing, without a model reset.
func TestProcessSelectionFollowsReorder(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		mw.SetFont(uiFont(10))
		a := navigationTestApp(mw)
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		procs := make([]metrics.Proc, 50)
		for i := range procs {
			procs[i] = metrics.Proc{PID: uint32(100 + i), Name: fmt.Sprint("p", i), CPU: float64(i), Start: 1}
		}
		a.snap = &metrics.Snapshot{Procs: procs}
		a.cfg.SortKey, a.cfg.SortDesc = "cpu", true
		a.procs = newProcsTab(a)
		a.pages[pgProcesses].impl = a.procs
		if e := a.ensurePage(pgProcesses); e != nil {
			panic(e)
		}
		p := a.procs
		resets := 0
		p.model.RowsReset().Attach(func() { resets++ })
		p.refresh()
		if len(p.model.rows) != 50 || p.model.rows[0].PID != 149 {
			t.Errorf("initial order wrong")
			return
		}
		_ = p.tv.SetCurrentIndex(2) // PID 147
		if p.selKey == "" || p.selected().PID != 147 {
			t.Errorf("selection not tracked: %q", p.selKey)
			return
		}
		// Reverse the CPU ranking: PID 147 moves to index 47.
		next := append([]metrics.Proc(nil), procs...)
		for i := range next {
			next[i].CPU = float64(100 - i)
		}
		a.snap = &metrics.Snapshot{Procs: next}
		p.refresh()
		if r := p.selected(); r == nil || r.PID != 147 || p.tv.CurrentIndex() != 47 || p.model.rows[47].PID != 147 {
			t.Errorf("selection lost on reorder: index %d", p.tv.CurrentIndex())
			return
		}
		lv := listViews(p.tv)
		nativeSel := -1
		for _, h := range lv {
			if i := int32(win.SendMessage(h, win.LVM_GETNEXTITEM, ^uintptr(0), win.LVNI_SELECTED)); i >= 0 {
				nativeSel = int(i)
			}
		}
		if nativeSel != 47 {
			t.Errorf("native selection at %d, want 47", nativeSel)
		}
		// Processes exit and start: the selected one stays selected.
		fewer := append([]metrics.Proc(nil), next[:10]...)
		fewer = append(fewer, next[40:]...)
		a.snap = &metrics.Snapshot{Procs: fewer}
		p.refresh()
		if r := p.selected(); r == nil || r.PID != 147 || p.model.rows[p.tv.CurrentIndex()].PID != 147 {
			t.Errorf("selection lost when rows were removed")
		}
		more := append(fewer, metrics.Proc{PID: 999, Name: "new", CPU: 1000, Start: 2})
		a.snap = &metrics.Snapshot{Procs: more}
		p.refresh()
		if r := p.selected(); r == nil || r.PID != 147 || p.model.rows[p.tv.CurrentIndex()].PID != 147 {
			t.Errorf("selection lost when a row was added")
		}
		// The selected process exits: nothing is selected.
		var gone []metrics.Proc
		for _, pr := range more {
			if pr.PID != 147 {
				gone = append(gone, pr)
			}
		}
		a.snap = &metrics.Snapshot{Procs: gone}
		p.refresh()
		if p.selKey != "" || p.tv.CurrentIndex() != -1 {
			t.Errorf("exited process still selected at %d", p.tv.CurrentIndex())
		}
		if resets != 0 {
			t.Errorf("refresh reset the model %d times", resets)
		}
	})
}
