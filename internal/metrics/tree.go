//go:build windows

package metrics

import (
	"slices"
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

	fold *procFold // lower-case texts, shared by every build of this process
}

// procFold holds a process's lower-cased texts. It is immutable once a row
// refers to it: a changed name/user/path gets a fresh value.
type procFold struct {
	name, user, path, pid string
}

// NameFold, UserFold and PathFold return the lower-cased text used for
// case-insensitive sorting, computed once per process instead of on every
// comparison.
func (r *Row) NameFold() string {
	if r.fold != nil {
		return r.fold.name
	}
	return strings.ToLower(r.Name)
}

func (r *Row) UserFold() string {
	if r.fold != nil {
		return r.fold.user
	}
	return strings.ToLower(r.User)
}

func (r *Row) PathFold() string {
	if r.fold != nil {
		return r.fold.path
	}
	return strings.ToLower(r.Path)
}

// View describes how to turn a snapshot into rows.
type View struct {
	Layout    Layout
	Filter    string          // case-insensitive substring of name / PID / user / path
	HideIdle  bool            // drop PID 0 (System Idle Process)
	Collapsed map[uint32]bool // tree: PIDs whose children are hidden
	// Cmp orders rows (negative: a first). Equal rows keep snapshot order.
	// When nil, Less is used; when both are nil rows are ordered by PID.
	Cmp  func(a, b *Row) int
	Less func(a, b *Row) bool
}

func (v View) cmp() func(a, b *Row) int {
	if v.Cmp != nil {
		return v.Cmp
	}
	if less := v.Less; less != nil {
		return func(a, b *Row) int {
			if less(a, b) {
				return -1
			}
			if less(b, a) {
				return 1
			}
			return 0
		}
	}
	return func(a, b *Row) int {
		switch {
		case a.PID < b.PID:
			return -1
		case a.PID > b.PID:
			return 1
		}
		return 0
	}
}

// BuildRows arranges procs according to v. It never mutates procs. Callers
// that rebuild every tick should keep a RowBuilder, which reuses per-process
// derived text and scratch memory between builds.
func BuildRows(procs []Proc, v View) []Row {
	var b RowBuilder
	return b.Build(procs, v)
}

type procID struct {
	pid   uint32
	start int64
}

type procEntry struct {
	name, user, path string // source texts fold was derived from
	fold             procFold
	key              string
	seen             uint32 // last build that used this entry
}

// RowBuilder builds rows like BuildRows while caching, per process instance
// (PID + creation time), the lower-cased texts and the row key. Entries of
// exited processes are pruned. It is not safe for concurrent use; the
// returned rows never alias its internal memory and stay valid forever.
type RowBuilder struct {
	cache     map[procID]*procEntry
	groupKeys map[string]string
	builds    uint32

	// scratch, reused between builds and never handed out
	ids     []int32
	ents    []*procEntry
	perm    []int32
	byPID   map[uint32]int32
	byName  map[string]int32
	parent  []int32
	cs      []int32
	kids    []int32
	nsel    []int32
	ord     []int32
	keep    []bool
	seen    []bool
	scratch []Row
	top     []int32
}

func (b *RowBuilder) entry(p *Proc) *procEntry {
	id := procID{p.PID, p.Start}
	e := b.cache[id]
	if e == nil || e.name != p.Name || e.user != p.User || e.path != p.Path {
		n := &procEntry{name: p.Name, user: p.User, path: p.Path}
		n.fold = procFold{name: strings.ToLower(p.Name), user: strings.ToLower(p.User), path: strings.ToLower(p.Path)}
		if e != nil {
			n.fold.pid, n.key = e.fold.pid, e.key
		} else {
			n.fold.pid = strconv.FormatUint(uint64(p.PID), 10)
			n.key = "p:" + n.fold.pid + ":" + strconv.FormatInt(p.Start, 10)
		}
		e = n
		b.cache[id] = e
	}
	e.seen = b.builds
	return e
}

func (e *procEntry) matches(q string) bool {
	return strings.Contains(e.fold.name, q) || strings.Contains(e.fold.pid, q) ||
		strings.Contains(e.fold.user, q) || strings.Contains(e.fold.path, q)
}

// Build arranges procs according to v. It never mutates procs.
func (b *RowBuilder) Build(procs []Proc, v View) []Row {
	if b.cache == nil {
		b.cache = make(map[procID]*procEntry, len(procs))
	}
	b.builds++
	q := strings.ToLower(strings.TrimSpace(v.Filter))
	cmp := v.cmp()

	// Included processes (PID 0 skipped without copying the snapshot).
	ids, ents := b.ids[:0], b.ents[:0]
	for i := range procs {
		p := &procs[i]
		if v.HideIdle && p.PID == 0 {
			continue
		}
		ids = append(ids, int32(i))
		ents = append(ents, b.entry(p))
	}
	b.ids, b.ents = ids, ents

	var out []Row
	switch v.Layout {
	case LayoutTree:
		out = b.buildTree(procs, q, v, cmp)
	case LayoutGroup:
		out = b.buildGroups(procs, q, cmp)
	default:
		out = b.buildFlat(procs, q, cmp)
	}
	clear(b.ents) // do not pin pruned entries
	b.prune(len(ids))
	return out
}

func (b *RowBuilder) prune(live int) {
	if len(b.cache) <= live+256 && b.builds%64 != 0 {
		return
	}
	for id, e := range b.cache {
		if e.seen != b.builds {
			delete(b.cache, id)
		}
	}
	if len(b.groupKeys) > 4096 {
		clear(b.groupKeys)
	}
}

// sortRows orders rows by cmp, ties by their current position (a stable
// sort), using an index permutation so the wide rows move only once.
func (b *RowBuilder) sortRows(rows []Row, cmp func(a, b *Row) int) {
	perm := slices.Grow(b.perm[:0], len(rows))[:len(rows)]
	for i := range perm {
		perm[i] = int32(i)
	}
	slices.SortFunc(perm, func(i, j int32) int {
		if c := cmp(&rows[i], &rows[j]); c != 0 {
			return c
		}
		return int(i - j)
	})
	// rows'[k] = rows[perm[k]], applied in place cycle by cycle
	for k := range perm {
		if perm[k] < 0 || int(perm[k]) == k {
			continue
		}
		tmp := rows[k]
		j := k
		for {
			src := int(perm[j])
			perm[j] = -1
			if src == k {
				rows[j] = tmp
				break
			}
			rows[j] = rows[src]
			j = src
		}
	}
	b.perm = perm
}

func (b *RowBuilder) buildFlat(procs []Proc, q string, cmp func(a, b *Row) int) []Row {
	n := 0
	hit := grow(b.keep, len(b.ids))
	for k := range b.ids {
		hit[k] = q == "" || b.ents[k].matches(q)
		if hit[k] {
			n++
		}
	}
	b.keep = hit
	rows := make([]Row, 0, n)
	pids := make([]uint32, n)
	starts := make([]int64, n)
	for k, i := range b.ids {
		if !hit[k] {
			continue
		}
		e := b.ents[k]
		p := &procs[i]
		at := len(rows)
		pids[at], starts[at] = p.PID, p.Start
		rows = append(rows, Row{Proc: *p, Key: e.key, PIDs: pids[at : at+1 : at+1], Starts: starts[at : at+1 : at+1],
			Count: 1, fold: &e.fold})
	}
	b.sortRows(rows, cmp)
	return rows
}

func (b *RowBuilder) buildGroups(procs []Proc, q string, cmp func(a, b *Row) int) []Row {
	if b.byName == nil {
		b.byName = map[string]int32{}
	}
	if b.groupKeys == nil {
		b.groupKeys = map[string]string{}
	}
	clear(b.byName)
	// pass 1: group index per matching process (in perm), sizes in nsel
	gi := slices.Grow(b.perm[:0], len(b.ids))[:len(b.ids)]
	sizes := b.nsel[:0]
	for k := range b.ids {
		e := b.ents[k]
		if q != "" && !e.matches(q) {
			gi[k] = -1
			continue
		}
		at, ok := b.byName[e.fold.name]
		if !ok {
			at = int32(len(sizes))
			b.byName[e.fold.name] = at
			sizes = append(sizes, 0)
		}
		gi[k] = at
		sizes[at]++
	}
	rows := make([]Row, len(sizes))
	total := 0
	for _, s := range sizes {
		total += int(s)
	}
	pids := make([]uint32, total)
	starts := make([]int64, total)
	off := 0
	for g, s := range sizes {
		rows[g].PIDs = pids[off : off : off+int(s)]
		rows[g].Starts = starts[off : off : off+int(s)]
		off += int(s)
	}
	for k, i := range b.ids {
		g := gi[k]
		if g < 0 {
			continue
		}
		p := &procs[i]
		r := &rows[g]
		if r.Count == 0 {
			e := b.ents[k]
			key, ok := b.groupKeys[e.fold.name]
			if !ok {
				key = "g:" + e.fold.name
				b.groupKeys[e.fold.name] = key
			}
			r.Proc = Proc{Name: p.Name, Path: p.Path, User: p.User, Prio: p.Prio, PID: p.PID, Session: p.Session}
			r.Key, r.fold = key, &e.fold
		}
		r.PIDs = append(r.PIDs, p.PID)
		r.Starts = append(r.Starts, p.Start)
		r.Count++
		r.CPU += p.CPU
		r.Mem += p.Mem
		r.Private += p.Private
		r.IO += p.IO
		r.GPU = minF(100, r.GPU+p.GPU)
		r.GPUUnavailable = r.GPUUnavailable || p.GPUUnavailable
		r.Threads += p.Threads
		r.Handles += p.Handles
	}
	b.perm, b.nsel = gi, sizes
	b.sortRows(rows, cmp)
	return rows
}

type subtotal struct {
	cpu, io, gpu     float64
	mem, private     uint64
	threads, handles uint32
	gpuUnavailable   bool
}

func grow[T any](s []T, n int) []T {
	return slices.Grow(s[:0], n)[:n]
}

// buildTree indents each process under its parent. A parent PID is only
// trusted when that process started before the child: Windows reuses PIDs, so
// a dead parent's PID may now belong to something unrelated. Collapsed rows
// carry the totals of their whole subtree so a busy branch stays visible.
//
// Nodes are positions in b.ids; children are kept in one flat array grouped
// by parent (in snapshot order), so no per-node allocation is made.
func (b *RowBuilder) buildTree(procs []Proc, q string, v View, cmp func(a, b *Row) int) []Row {
	n := len(b.ids)
	at := func(i int32) *Proc { return &procs[b.ids[i]] }
	if b.byPID == nil {
		b.byPID = make(map[uint32]int32, n)
	}
	clear(b.byPID)
	for i := 0; i < n; i++ {
		b.byPID[at(int32(i)).PID] = int32(i)
	}
	parent := grow(b.parent, n)
	cs := grow(b.cs, n+1)
	clear(cs)
	for i := 0; i < n; i++ {
		p := at(int32(i))
		parent[i] = -1
		if j, ok := b.byPID[p.PPID]; ok {
			par := at(j)
			if par.PID != p.PID && !(par.Start > 0 && p.Start > 0 && par.Start > p.Start) {
				parent[i] = j
				cs[j+1]++
			}
		}
	}
	for i := 0; i < n; i++ {
		cs[i+1] += cs[i]
	}
	kids := grow(b.kids, n)
	nsel := grow(b.nsel, n)
	clear(nsel)
	for i := 0; i < n; i++ { // nsel doubles as the fill cursor here
		if j := parent[i]; j >= 0 {
			kids[cs[j]+nsel[j]] = int32(i)
			nsel[j]++
		}
	}
	clear(nsel)

	// A filter keeps the matches plus every ancestor, so context is visible.
	var keep []bool
	if q != "" {
		keep = grow(b.keep, n)
		clear(keep)
		for i := 0; i < n; i++ {
			if !b.ents[i].matches(q) {
				continue
			}
			id := int32(i)
			for step := 0; step < 256; step++ {
				if keep[id] {
					break
				}
				keep[id] = true
				if parent[id] < 0 {
					break
				}
				id = parent[id]
			}
		}
		b.keep = keep
	}

	seen := grow(b.seen, n)
	clear(seen)
	rows := grow(b.scratch, n)
	pids := make([]uint32, n)
	starts := make([]int64, n)
	t := treeBuild{b: b, procs: procs, parent: parent, cs: cs, kids: kids, nsel: nsel, keep: keep, seen: seen,
		rows: rows, pids: pids, starts: starts, collapsed: v.Collapsed}

	top := b.top[:0]
	for i := 0; i < n; i++ {
		if parent[i] >= 0 || (keep != nil && !keep[i]) {
			continue
		}
		t.build(int32(i), 0)
		top = append(top, int32(i))
	}
	// processes caught in a parent cycle are never reached from a root
	for i := 0; i < n; i++ {
		if !seen[i] && (keep == nil || keep[i]) {
			t.build(int32(i), 0)
			top = append(top, int32(i))
		}
	}

	// Siblings tie in insertion order: snapshot order for children, the order
	// above for top-level rows.
	ord := grow(b.ord, n)
	for i := range ord {
		ord[i] = int32(i)
	}
	for k, i := range top {
		ord[i] = int32(k)
	}
	t.ord, t.cmp = ord, cmp
	t.out = make([]Row, 0, n)
	t.emit(top)

	clear(rows) // drop string references held by the scratch rows
	b.parent, b.cs, b.kids, b.nsel, b.seen, b.scratch, b.top, b.ord = parent, cs, kids, nsel, seen, rows, top, ord
	return t.out
}

type treeBuild struct {
	b                      *RowBuilder
	procs                  []Proc
	parent, cs, kids, nsel []int32
	ord                    []int32
	keep, seen             []bool
	rows                   []Row
	pids                   []uint32
	starts                 []int64
	collapsed              map[uint32]bool
	cmp                    func(a, b *Row) int
	out                    []Row
}

func (t *treeBuild) build(i int32, depth int) subtotal {
	t.seen[i] = true
	p := &t.procs[t.b.ids[i]]
	agg := subtotal{cpu: p.CPU, io: p.IO, gpu: p.GPU, mem: p.Mem, private: p.Private,
		threads: p.Threads, handles: p.Handles, gpuUnavailable: p.GPUUnavailable}
	desc := 0
	base := t.cs[i]
	for _, c := range t.kids[base:t.cs[i+1]] {
		if t.seen[c] || (t.keep != nil && !t.keep[c]) {
			continue
		}
		ca := t.build(c, depth+1)
		t.kids[base+t.nsel[i]] = c // selected children, compacted in order
		t.nsel[i]++
		agg.cpu += ca.cpu
		agg.mem += ca.mem
		agg.private += ca.private
		agg.io += ca.io
		agg.gpu = minF(100, agg.gpu+ca.gpu)
		agg.gpuUnavailable = agg.gpuUnavailable || ca.gpuUnavailable
		agg.threads += ca.threads
		agg.handles += ca.handles
		desc += 1 + t.rows[c].Desc
	}
	hasKids := t.nsel[i] > 0
	// while filtering, always open the path down to the matches
	collapsed := hasKids && t.keep == nil && t.collapsed[p.PID]
	e := t.b.ents[i]
	t.pids[i], t.starts[i] = p.PID, p.Start
	r := &t.rows[i]
	*r = Row{Proc: *p, Key: e.key, PIDs: t.pids[i : i+1 : i+1], Starts: t.starts[i : i+1 : i+1],
		Count: 1, Depth: depth, HasKids: hasKids, Collapsed: collapsed, Desc: desc, fold: &e.fold}
	if collapsed {
		r.CPU, r.Mem, r.Private, r.IO = agg.cpu, agg.mem, agg.private, agg.io
		r.GPU, r.Threads, r.Handles = agg.gpu, agg.threads, agg.handles
		r.GPUUnavailable = agg.gpuUnavailable
	}
	return agg
}

func (t *treeBuild) emit(ns []int32) {
	slices.SortFunc(ns, func(x, y int32) int {
		if c := t.cmp(&t.rows[x], &t.rows[y]); c != 0 {
			return c
		}
		return int(t.ord[x] - t.ord[y])
	})
	for _, i := range ns {
		t.out = append(t.out, t.rows[i])
		if !t.rows[i].Collapsed {
			t.emit(t.kids[t.cs[i] : t.cs[i]+t.nsel[i]])
		}
	}
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
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
