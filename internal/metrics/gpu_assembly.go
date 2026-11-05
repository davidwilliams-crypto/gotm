//go:build windows

package metrics

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ---- PDH assembly -------------------------------------------------------------------

type gpuInst struct {
	pid  uint32
	luid string
	eng  int
	typ  string
}

// luidKey extracts the canonical "0X........_0X........" from an instance name.
func luidKey(inst string) string {
	return canonicalLUID(rawLUIDKey(inst))
}

// canonicalLUID upper-cases a PDH LUID ("0x00000000_0x0000d1e2"). Input that
// is already canonical is returned without allocating.
func canonicalLUID(raw string) string {
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c >= 'a' && c <= 'z' {
			return strings.ToUpper(raw)
		}
	}
	return raw
}

// rawLUIDKey returns a view into the instance name without allocating. The
// discovery cache accepts these aliases; public adapter IDs stay canonical.
func rawLUIDKey(inst string) string {
	i := strings.Index(inst, "luid_")
	if i < 0 || len(inst) < i+5+21 {
		return ""
	}
	return inst[i+5 : i+5+21]
}

func parseGPUInst(inst string) (gpuInst, bool) {
	return parseGPUInstWith(inst, luidKey(inst))
}

// parseGPUInstWith parses everything but the LUID, which the caller supplies
// (canonicalised and cached).
func parseGPUInstWith(inst, luid string) (gpuInst, bool) {
	var g gpuInst
	g.luid = luid
	if g.luid == "" {
		return g, false
	}
	if strings.HasPrefix(inst, "pid_") {
		end := strings.Index(inst, "_luid_")
		if end > 4 {
			if n, err := strconv.ParseUint(inst[4:end], 10, 32); err == nil {
				g.pid = uint32(n)
			}
		}
	}
	if i := strings.Index(inst, "_eng_"); i >= 0 {
		rest := inst[i+5:]
		if j := strings.Index(rest, "_"); j > 0 {
			g.eng, _ = strconv.Atoi(rest[:j])
		}
	}
	if i := strings.Index(inst, "_engtype_"); i >= 0 {
		g.typ = inst[i+9:]
	}
	return g, true
}

// prettyEngine makes "VideoDecode", "Compute_0", "High Priority 3D" readable.
func prettyEngine(t string) string {
	t = strings.ReplaceAll(t, "_", " ")
	var b strings.Builder
	rs := []rune(t)
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' && rs[i-1] >= 'a' && rs[i-1] <= 'z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func engineRank(n string) int {
	l := strings.ToLower(n)
	switch {
	case l == "3d":
		return 0
	case strings.HasPrefix(l, "high priority 3d"):
		return 1
	case strings.HasPrefix(l, "compute"), strings.HasPrefix(l, "high priority compute"):
		return 2
	case strings.HasPrefix(l, "copy"):
		return 3
	case strings.HasPrefix(l, "video"):
		return 4
	}
	return 5
}

// ---- parse cache -----------------------------------------------------------------

// gpuInstInfo is one PDH instance name, parsed once. PDH instance strings are
// interned per counter, so lookups by name hash an existing string.
type gpuInstInfo struct {
	gpuInst
	ok     bool
	hasPID bool // "pid_<n>_…" with a valid PID: a per-process instance
	pretty gpuEngName
	seen   uint32
}

type gpuEngName struct {
	name string
	rank int
}

type gpuNumbered struct {
	name string
	n    int
}

// gpuParseCacheSweepEvery bounds the cache: instances (processes) absent
// from the latest sample are forgotten every this many samples.
const gpuParseCacheSweepEvery = 64

type gpuParseCache struct {
	inst     map[string]*gpuInstInfo
	canon    map[string]string // raw PDH LUID -> canonical adapter ID
	pretty   map[string]gpuEngName
	numbered map[gpuNumbered]string
	gen      uint32
}

func (c *gpuParseCache) init() {
	if c.inst == nil {
		c.inst = map[string]*gpuInstInfo{}
		c.canon = map[string]string{}
		c.pretty = map[string]gpuEngName{}
		c.numbered = map[gpuNumbered]string{}
	}
}

// canonical returns the cached canonical form of a raw PDH LUID.
func (c *gpuParseCache) canonical(raw string) string {
	c.init()
	if s, ok := c.canon[raw]; ok {
		return s
	}
	s := canonicalLUID(raw)
	if len(c.canon) > 4096 {
		clear(c.canon) // adapters do not churn; this only bounds pathological input
	}
	c.canon[raw] = s
	return s
}

func (c *gpuParseCache) prettyName(typ string) gpuEngName {
	if p, ok := c.pretty[typ]; ok {
		return p
	}
	name := prettyEngine(typ)
	p := gpuEngName{name: name, rank: engineRank(name)}
	if len(c.pretty) > 1024 {
		clear(c.pretty)
	}
	c.pretty[typ] = p
	return p
}

// numberedName is "Copy 1" for the second of several engines named "Copy".
func (c *gpuParseCache) numberedName(name string, n int) string {
	k := gpuNumbered{name, n}
	if s, ok := c.numbered[k]; ok {
		return s
	}
	s := name + " " + strconv.Itoa(n)
	if len(c.numbered) > 1024 {
		clear(c.numbered)
	}
	c.numbered[k] = s
	return s
}

func (c *gpuParseCache) lookup(inst string) *gpuInstInfo {
	c.init()
	if e := c.inst[inst]; e != nil {
		e.seen = c.gen
		return e
	}
	e := &gpuInstInfo{seen: c.gen}
	if raw := rawLUIDKey(inst); raw != "" {
		e.gpuInst, e.ok = parseGPUInstWith(inst, c.canonical(raw))
		if strings.HasPrefix(inst, "pid_") {
			end := strings.Index(inst, "_luid_")
			if end > 4 {
				_, err := strconv.ParseUint(inst[4:end], 10, 32)
				e.hasPID = err == nil
			}
		}
		if e.typ != "" {
			e.pretty = c.prettyName(e.typ)
		}
	}
	c.inst[inst] = e
	return e
}

// next starts a sample; sweep runs after it.
func (c *gpuParseCache) next() { c.init(); c.gen++ }

func (c *gpuParseCache) sweep() {
	if c.gen%gpuParseCacheSweepEvery != 0 {
		return
	}
	for k, e := range c.inst {
		if e.seen != c.gen {
			delete(c.inst, k)
		}
	}
}

// ---- assembly ----------------------------------------------------------------------

type gpuEngKey struct {
	luid string
	eng  int
	typ  string
}

type gpuProcTypKey struct {
	luid string
	pid  uint32
	typ  string
}

type gpuProcKey struct {
	luid string
	pid  uint32
}

type gpuPIDTypKey struct {
	pid uint32
	typ string
}

type gpuMemAcc struct{ ded, shr uint64 }

type gpuEngRow struct {
	ad   int
	idx  int
	name gpuEngName
	pct  float64
}

// gpuScratch holds the per-sample working maps of buildGPUs. They are
// cleared, not reallocated, every sample. Everything the result references
// is freshly allocated: snapshots are published and must never change.
type gpuScratch struct {
	parse    gpuParseCache
	index    map[string]int // adapter ID -> position in the result; -1 hidden (software)
	sums     map[gpuEngKey]float64
	procTyp  map[gpuProcTypKey]float64
	procBest map[gpuProcKey]float64
	mem      map[gpuProcKey]gpuMemAcc
	pidTyp   map[gpuPIDTypKey]float64
	unknown  map[string]struct{} // LUIDs in PDH that the inventory does not list
	drivers  map[string]string   // NVIDIA driver display strings
	engs     []gpuEngRow
	counts   []int
}

func (sc *gpuScratch) reset() {
	if sc.index == nil {
		sc.index = map[string]int{}
		sc.sums = map[gpuEngKey]float64{}
		sc.procTyp = map[gpuProcTypKey]float64{}
		sc.procBest = map[gpuProcKey]float64{}
		sc.mem = map[gpuProcKey]gpuMemAcc{}
		sc.pidTyp = map[gpuPIDTypKey]float64{}
		sc.unknown = map[string]struct{}{}
		sc.drivers = map[string]string{}
	}
	clear(sc.index)
	clear(sc.sums)
	clear(sc.procTyp)
	clear(sc.procBest)
	clear(sc.mem)
	clear(sc.pidTyp)
	clear(sc.unknown)
	sc.engs = sc.engs[:0]
}

// buildGPUs merges static identity with one PDH collection. eng are the
// "GPU Engine" utilisations; ded/shr the per-adapter memory; pded/pshr the
// per-process memory. Only adapters PDH reports (or DXGI lists) appear.
func buildGPUs(static []gpuStatic, eng, ded, shr, pded, pshr []PDHInstanceValue) []GPUAdapter {
	var sc gpuScratch
	return sc.build(static, eng, ded, shr, pded, pshr, nil)
}

// build is buildGPUs reusing sc's working memory. When byPID is non-nil it is
// cleared and filled with each process's busiest engine type summed across
// the displayed adapters (the process table's GPU column), in the same pass.
func (sc *gpuScratch) build(static []gpuStatic, eng, ded, shr, pded, pshr []PDHInstanceValue, byPID map[uint32]float64) []GPUAdapter {
	sc.reset()
	sc.parse.next()
	defer sc.parse.sweep()

	out := make([]GPUAdapter, 0, len(static))
	for _, st := range static {
		i, ok := sc.index[st.id]
		if !ok {
			i = len(out)
			out = append(out, GPUAdapter{ID: st.id})
			sc.index[st.id] = i
		}
		a := &out[i]
		a.Name, a.Vendor, a.Software = st.name, vendorName(st.vendorID), st.software
		a.DedicatedTotal, a.SharedTotal = st.ded, st.shr
		a.Driver, a.DriverDate, a.Location = st.driver, st.date, st.location
		if a.Vendor == "NVIDIA" {
			a.Driver = sc.nvidiaDriver(st.driver)
		}
	}
	// Software rasterizers stay known (their LUIDs are not "new devices") but hidden.
	n := 0
	for i := range out {
		if out[i].Software {
			sc.index[out[i].ID] = -1
			continue
		}
		out[n] = out[i]
		sc.index[out[n].ID] = n
		n++
	}
	clear(out[n:])
	out = out[:n]

	// visible returns the result position of a parsed instance's adapter, or
	// -1 for software and unknown (compute-only, not yet discovered) devices.
	visible := func(e *gpuInstInfo, record bool) int {
		i, ok := sc.index[e.luid]
		if !ok {
			if record {
				sc.unknown[e.luid] = struct{}{}
			}
			return -1
		}
		return i
	}

	// engines: sum across processes per (adapter, engine index, type), cap 100
	for _, it := range eng {
		e := sc.parse.lookup(it.Instance)
		if !e.ok {
			continue
		}
		ai := visible(e, true)
		if ai < 0 || e.typ == "" || !validGPUCounter(it.Value) {
			continue
		}
		out[ai].HasUtilization = true
		sc.sums[gpuEngKey{e.luid, e.eng, e.typ}] += it.Value
		if e.pid != 0 {
			sc.procTyp[gpuProcTypKey{e.luid, e.pid, e.typ}] += it.Value
		}
		if byPID != nil && e.hasPID {
			sc.pidTyp[gpuPIDTypKey{e.pid, e.typ}] += it.Value
		}
	}
	for k, v := range sc.sums {
		e := sc.parse.prettyName(k.typ)
		sc.engs = append(sc.engs, gpuEngRow{ad: sc.index[k.luid], idx: k.eng, name: e, pct: math.Min(v, 100)})
	}
	slices.SortFunc(sc.engs, func(a, b gpuEngRow) int {
		if c := cmp.Compare(a.ad, b.ad); c != 0 {
			return c
		}
		if c := cmp.Compare(a.name.rank, b.name.rank); c != 0 {
			return c
		}
		if c := strings.Compare(a.name.name, b.name.name); c != 0 {
			return c
		}
		return cmp.Compare(a.idx, b.idx)
	})
	for s := 0; s < len(sc.engs); {
		ad, e := sc.engs[s].ad, s
		for e < len(sc.engs) && sc.engs[e].ad == ad {
			e++
		}
		a := &out[ad]
		a.Engines = make([]GPUEngine, 0, e-s)
		for i := s; i < e; {
			j := i
			for j < e && sc.engs[j].name.name == sc.engs[i].name.name {
				j++
			}
			for k := i; k < j; k++ {
				name := sc.engs[k].name.name
				if j-i > 1 {
					name = sc.parse.numberedName(name, k-i)
				}
				a.Engines = append(a.Engines, GPUEngine{Name: name, Pct: round1(sc.engs[k].pct)})
				if sc.engs[k].pct > a.Pct {
					a.Pct = round1(sc.engs[k].pct)
				}
			}
			i = j
		}
		s = e
	}

	for _, it := range ded {
		e := sc.parse.lookup(it.Instance)
		if !e.ok {
			continue
		}
		if ai := visible(e, true); ai >= 0 && validGPUCounter(it.Value) && it.Value < 1<<60 {
			out[ai].DedicatedUsed = uint64(it.Value)
			out[ai].HasDedicatedUsage = true
		}
	}
	for _, it := range shr {
		e := sc.parse.lookup(it.Instance)
		if !e.ok {
			continue
		}
		if ai := visible(e, false); ai >= 0 && validGPUCounter(it.Value) && it.Value < 1<<60 {
			out[ai].SharedUsed = uint64(it.Value)
			out[ai].HasSharedUsage = true
		}
	}

	// per-process memory
	for pass, list := range [2][]PDHInstanceValue{pded, pshr} {
		for _, it := range list {
			e := sc.parse.lookup(it.Instance)
			if !e.ok || !strings.HasPrefix(it.Instance, "pid_") || visible(e, false) < 0 {
				continue
			}
			k := gpuProcKey{e.luid, e.pid}
			m := sc.mem[k]
			if validGPUCounter(it.Value) && it.Value > 0 && it.Value < 1<<60 {
				if pass == 0 {
					m.ded += uint64(it.Value)
				} else {
					m.shr += uint64(it.Value)
				}
			}
			sc.mem[k] = m
		}
	}
	for k, v := range sc.procTyp {
		pk := gpuProcKey{k.luid, k.pid}
		if cur, ok := sc.procBest[pk]; !ok || v > cur {
			sc.procBest[pk] = v
		}
	}
	sc.counts = slices.Grow(sc.counts[:0], len(out))[:len(out)]
	clear(sc.counts)
	for k, m := range sc.mem {
		if _, has := sc.procBest[k]; m.ded == 0 && m.shr == 0 && !has {
			continue
		}
		sc.counts[sc.index[k.luid]]++
	}
	for k, best := range sc.procBest {
		if _, inMem := sc.mem[k]; !inMem && best > 0 {
			sc.counts[sc.index[k.luid]]++
		}
	}
	for i, c := range sc.counts {
		if c > 0 {
			out[i].Procs = make([]GPUProcMem, 0, c)
		}
	}
	for k, m := range sc.mem {
		best, has := sc.procBest[k]
		if m.ded == 0 && m.shr == 0 && !has {
			continue
		}
		pr := GPUProcMem{PID: k.pid, Dedicated: m.ded, Shared: m.shr}
		if best > 0 {
			pr.Pct = math.Min(round1(best), 100)
		}
		a := &out[sc.index[k.luid]]
		a.Procs = append(a.Procs, pr)
	}
	for k, best := range sc.procBest { // engine-only processes with no memory instance
		if _, inMem := sc.mem[k]; inMem || best <= 0 {
			continue
		}
		a := &out[sc.index[k.luid]]
		a.Procs = append(a.Procs, GPUProcMem{PID: k.pid, Pct: math.Min(round1(best), 100)})
	}
	for i := range out {
		slices.SortFunc(out[i].Procs, func(a, b GPUProcMem) int {
			if c := cmp.Compare(b.Dedicated+b.Shared, a.Dedicated+a.Shared); c != 0 {
				return c
			}
			return cmp.Compare(b.Pct, a.Pct)
		})
	}

	if byPID != nil {
		clear(byPID)
		for k, v := range sc.pidTyp {
			if cur, ok := byPID[k.pid]; !ok || v > cur {
				byPID[k.pid] = v
			}
		}
		for pid, v := range byPID {
			if v > 100 {
				byPID[pid] = 100
			}
		}
	}
	return out
}

// nvidiaDriver shows NVIDIA's own version next to Windows' driver version.
func (sc *gpuScratch) nvidiaDriver(v string) string {
	if s, ok := sc.drivers[v]; ok {
		return s
	}
	s := v
	if m := nvidiaMarketingVersion(v); m != "" {
		s = m + " (" + v + ")"
	}
	if len(sc.drivers) > 64 {
		clear(sc.drivers)
	}
	sc.drivers[v] = s
	return s
}
