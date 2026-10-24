//go:build windows

package metrics

import (
	"sort"
	"strconv"
	"strings"
)

// Layout selects how BuildRows arranges the process list.
type Layout int

const (
	LayoutFlat  Layout = iota // one row per process
	LayoutTree                // indented under the parent process
	LayoutGroup               // one row per executable name
)

// Row is one displayed line: a process, or a group of same-named processes.
type Row struct {
	Proc
	Key       string   // stable identity used to restore the selection
	PIDs      []uint32 // every process this row stands for
	Starts    []int64  // creation time of each PID (Unix ms), same order
	Count     int      // len(PIDs)
	Depth     int      // tree indentation level
	HasKids   bool     // tree: has child processes
	Collapsed bool     // tree: children hidden, values are subtree totals
	Desc      int      // tree: number of descendants
}

// View describes how to turn a snapshot into rows.
type View struct {
	Layout    Layout
	Filter    string          // case-insensitive substring of name / PID / user / path
	HideIdle  bool            // drop PID 0 (System Idle Process)
	Collapsed map[uint32]bool // tree: PIDs whose children are hidden
	Less      func(a, b *Row) bool
}

func (v View) matches(p *Proc, q string) bool {
	return strings.Contains(strings.ToLower(p.Name), q) ||
		strings.Contains(strconv.FormatUint(uint64(p.PID), 10), q) ||
		strings.Contains(strings.ToLower(p.User), q) ||
		strings.Contains(strings.ToLower(p.Path), q)
}

// BuildRows arranges procs according to v. It never mutates procs.
func BuildRows(procs []Proc, v View) []Row {
	list := procs
	if v.HideIdle {
		list = make([]Proc, 0, len(procs))
		for _, p := range procs {
			if p.PID != 0 {
				list = append(list, p)
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(v.Filter))
	if v.Less == nil {
		v.Less = func(a, b *Row) bool { return a.PID < b.PID }
	}

	switch v.Layout {
	case LayoutTree:
		return buildTree(list, q, v)
	case LayoutGroup:
		return buildGroups(list, q, v)
	}
	rows := make([]Row, 0, len(list))
	for i := range list {
		p := &list[i]
		if q != "" && !v.matches(p, q) {
			continue
		}
		rows = append(rows, Row{Proc: *p, Key: procKeyOf(p), PIDs: []uint32{p.PID}, Starts: []int64{p.Start}, Count: 1})
	}
	sort.SliceStable(rows, func(i, j int) bool { return v.Less(&rows[i], &rows[j]) })
	return rows
}

func buildGroups(list []Proc, q string, v View) []Row {
	idx := map[string]int{}
	var rows []Row
	for i := range list {
		p := &list[i]
		if q != "" && !v.matches(p, q) {
			continue
		}
		k := strings.ToLower(p.Name)
		at, ok := idx[k]
		if !ok {
			at = len(rows)
			idx[k] = at
			rows = append(rows, Row{Proc: Proc{Name: p.Name, Path: p.Path, User: p.User, Prio: p.Prio, PID: p.PID, Session: p.Session}, Key: "g:" + k})
		}
		r := &rows[at]
		r.PIDs = append(r.PIDs, p.PID)
		r.Starts = append(r.Starts, p.Start)
		r.Count++
		r.CPU += p.CPU
		r.Mem += p.Mem
		r.Private += p.Private
		r.IO += p.IO
		r.GPU = minF(100, r.GPU+p.GPU)
		r.Threads += p.Threads
		r.Handles += p.Handles
	}
	sort.SliceStable(rows, func(i, j int) bool { return v.Less(&rows[i], &rows[j]) })
	return rows
}

type treeNode struct {
	row      Row
	children []*treeNode
}

// buildTree indents each process under its parent. A parent PID is only
// trusted when that process started before the child: Windows reuses PIDs, so
// a dead parent's PID may now belong to something unrelated. Collapsed rows
// carry the totals of their whole subtree so a busy branch stays visible.
func buildTree(list []Proc, q string, v View) []Row {
	byPID := make(map[uint32]*Proc, len(list))
	for i := range list {
		byPID[list[i].PID] = &list[i]
	}
	parentOf := make(map[uint32]uint32, len(list))
	kids := make(map[uint32][]*Proc, len(list))
	var roots []*Proc
	for i := range list {
		p := &list[i]
		par, ok := byPID[p.PPID]
		if ok && par.PID != p.PID && !(par.Start > 0 && p.Start > 0 && par.Start > p.Start) {
			parentOf[p.PID] = par.PID
			kids[par.PID] = append(kids[par.PID], p)
		} else {
			roots = append(roots, p)
		}
	}

	// A filter keeps the matches plus every ancestor, so context is visible.
	var keep map[uint32]bool
	if q != "" {
		keep = map[uint32]bool{}
		for i := range list {
			if !v.matches(&list[i], q) {
				continue
			}
			id := list[i].PID
			for n := 0; n < 256; n++ {
				if keep[id] {
					break
				}
				keep[id] = true
				par, ok := parentOf[id]
				if !ok {
					break
				}
				id = par
			}
		}
	}

	seen := make(map[uint32]bool, len(list))
	var build func(p *Proc, depth int) (*treeNode, Proc)
	build = func(p *Proc, depth int) (*treeNode, Proc) {
		seen[p.PID] = true
		n := &treeNode{}
		agg := *p
		desc := 0
		for _, c := range kids[p.PID] {
			if seen[c.PID] || (keep != nil && !keep[c.PID]) {
				continue
			}
			cn, ca := build(c, depth+1)
			n.children = append(n.children, cn)
			agg.CPU += ca.CPU
			agg.Mem += ca.Mem
			agg.Private += ca.Private
			agg.IO += ca.IO
			agg.GPU = minF(100, agg.GPU+ca.GPU)
			agg.Threads += ca.Threads
			agg.Handles += ca.Handles
			desc += 1 + cn.row.Desc
		}
		hasKids := len(n.children) > 0
		// while filtering, always open the path down to the matches
		collapsed := hasKids && keep == nil && v.Collapsed[p.PID]
		row := Row{Proc: *p, Key: procKeyOf(p), PIDs: []uint32{p.PID}, Starts: []int64{p.Start},
			Count: 1, Depth: depth, HasKids: hasKids, Collapsed: collapsed, Desc: desc}
		if collapsed {
			row.CPU, row.Mem, row.Private, row.IO = agg.CPU, agg.Mem, agg.Private, agg.IO
			row.GPU, row.Threads, row.Handles = agg.GPU, agg.Threads, agg.Handles
		}
		n.row = row
		return n, agg
	}

	var top []*treeNode
	for _, p := range roots {
		if keep != nil && !keep[p.PID] {
			continue
		}
		n, _ := build(p, 0)
		top = append(top, n)
	}
	// processes caught in a parent cycle are never reached from a root
	for i := range list {
		p := &list[i]
		if !seen[p.PID] && (keep == nil || keep[p.PID]) {
			n, _ := build(p, 0)
			top = append(top, n)
		}
	}

	out := make([]Row, 0, len(list))
	var emit func(ns []*treeNode)
	emit = func(ns []*treeNode) {
		sort.SliceStable(ns, func(i, j int) bool { return v.Less(&ns[i].row, &ns[j].row) })
		for _, n := range ns {
			out = append(out, n.row)
			if !n.row.Collapsed {
				emit(n.children)
			}
		}
	}
	emit(top)
	return out
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// procKeyOf identifies a process instance: a reused PID gets a new key, so the
// selection does not silently jump to an unrelated process.
func procKeyOf(p *Proc) string {
	return "p:" + strconv.FormatUint(uint64(p.PID), 10) + ":" + strconv.FormatInt(p.Start, 10)
}

// Targets lists the process instances a row stands for.
func (r *Row) Targets() []Target {
	out := make([]Target, len(r.PIDs))
	for i, pid := range r.PIDs {
		out[i] = Target{PID: pid, Name: r.Name}
		if i < len(r.Starts) {
			out[i].Start = r.Starts[i]
		}
	}
	return out
}
