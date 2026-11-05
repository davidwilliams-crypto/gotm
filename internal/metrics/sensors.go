//go:build windows

package metrics

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SensorRow is one line of the Power & Freq sensor tree.
type SensorRow struct {
	ID    string
	Name  string
	Depth int
	Group bool
	Kind  string // pc, cpu, temp, load, clock, power, mem, gpu, disk, net, batt, data, note
	Value string
	Min   string
	Max   string
}

type minMax struct {
	min, max float64
	set      bool
}

// SensorHub turns snapshots into a labelled sensor tree and remembers the
// minimum and maximum each numeric sensor has shown since gotm started.
type SensorHub struct {
	mu           sync.Mutex
	mm           map[string]*minMax
	topo         CPUTopology
	power        PowerStatic
	drives       []DriveHealth
	maxMHz       []uint32
	currentMHz   []float64
	clocking     bool
	slowAt       time.Time
	slowing      bool
	powerAt      time.Time
	powering     bool
	ext          *ExternalSource
	ryzen        *RyzenSDK
	detailWanted atomic.Bool
	detailKnown  atomic.Bool // SetDetailWanted has been called
	apu          GPUAdapter  // the APU adapter from the last snapshot, for its CPU sensors
	clockAt      time.Time   // when the last CallNtPowerInformation read started
	drivesFull   bool        // drives came from a full read, not the start-up descriptor scan

	// Update's own state, guarded by updMu (never held while h.mu is wanted
	// by the UI): the reused entry list, cached IDs/names and formatted text.
	updMu   sync.Mutex
	entries []sensorEntry
	ids     sensorIDs
	rz      RyzenReading
	fmtc    map[string]*sensorFmt
	rows    []SensorRow // last formatted tree, returned while the page is hidden
	rowsAt  time.Time
}

// sensorRowsRefresh is how stale the formatted tree may get while the Power &
// Freq page is hidden. Min/max tracking is exact regardless; the page builds
// fresh rows on its first sample once it is shown.
const sensorRowsRefresh = 10 * time.Second

// cpuClockFallbackEvery throttles CallNtPowerInformation when PDH has no
// per-processor frequency and the hub supplies the clocks itself.
const cpuClockFallbackEvery = 2 * time.Second

// NewSensorHub starts empty; the first Update triggers the slow readers.
func NewSensorHub() *SensorHub {
	return NewSensorHubWith(nil)
}

// NewSensorHubWith is NewSensorHub with an explicit external source (or nil).
func NewSensorHubWith(ext *ExternalSource) *SensorHub {
	h := NewSensorHubDeferred()
	h.ext = ext
	h.power = ReadPowerStatic() // cheap; avoids showing "Battery" until the first slow read
	h.topo = ReadTopology()
	return h
}

// NewSensorHubDeferred creates an immediately usable hub without querying drivers.
// Initialize performs its static reads on the application's startup worker.
func NewSensorHubDeferred() *SensorHub { return &SensorHub{mm: map[string]*minMax{}} }

func (h *SensorHub) Initialize() {
	power, topo := ReadPowerStatic(), ReadTopology()
	h.mu.Lock()
	h.power, h.topo = power, topo
	h.powerAt = time.Now()
	h.mu.Unlock()
	h.clockSnapshot(ReadCPUClocks)
}

// External exposes the optional external sensor source (nil in the self-contained default configuration).
func (h *SensorHub) External() *ExternalSource { return h.ext }

// SetRyzen attaches (or detaches, with nil) the AMD Monitoring SDK reader.
func (h *SensorHub) SetRyzen(r *RyzenSDK) {
	h.mu.Lock()
	h.ryzen = r
	h.mu.Unlock()
}

// Ryzen returns the attached AMD SDK reader (may be nil).
func (h *SensorHub) Ryzen() *RyzenSDK {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ryzen
}

// CPUTemperature reports the best CPU temperature available: the AMD
// Monitoring SDK first, then an external program (LibreHardwareMonitor).
// It is called while painting: the hub lock is held only to copy three
// fields, and the AMD SDK is asked for the temperature alone.
func (h *SensorHub) CPUTemperature() (celsius float64, label string, ok bool) {
	h.mu.Lock()
	rz, ext, hasAPU, apuT := h.ryzen, h.ext, h.apu.HasCPUTemp, h.apu.CPUTempC
	h.mu.Unlock()
	return cpuTemperature(rz, ext, hasAPU, apuT)
}

// Package power in watts from the Windows Energy Meter, when available.
func EnergyPackage(s *Snapshot) (float64, bool) {
	for _, e := range s.Energy {
		if strings.HasSuffix(strings.ToUpper(e.Name), "_PKG") {
			return e.Value, true
		}
	}
	return 0, false
}

// Topology returns the cached CPU topology.
func (h *SensorHub) Topology() CPUTopology {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.topo
}

// Power returns the cached slow-changing power state.
func (h *SensorHub) Power() PowerStatic {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.power
}

// Drives returns the cached drive health.
func (h *SensorHub) Drives() []DriveHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]DriveHealth(nil), h.drives...)
}

// SetDetailWanted says whether the Power & Freq page is on screen. At start
// only drive names are read (one descriptor query per drive); temperatures
// and SMART are read only while the page is wanted: polling every drive every
// 15 s keeps disks and NVMe out of low-power states.
//
// It also gates formatting of the tree Update returns: until the UI has said
// which page is visible (detailKnown), every Update formats it, so a caller
// that does not track pages always gets a current tree.
func (h *SensorHub) SetDetailWanted(on bool) {
	h.detailWanted.Store(on)
	h.detailKnown.Store(true)
}

// rowsWanted reports whether this Update must format the tree.
func (h *SensorHub) rowsWanted(now time.Time) bool {
	return !h.detailKnown.Load() || h.detailWanted.Load() || h.rows == nil || now.Sub(h.rowsAt) >= sensorRowsRefresh
}

// refreshSlow reads rarely changing sensors off-thread. Power state must stay
// fresh on every page, independently of the drive-health polling gate.
func (h *SensorHub) refreshSlow() {
	h.refreshSlowWith(readDriveInventory, ReadPowerStatic, ReadCPUClocks)
}

// refreshSlowWith: readDrives(full) reads every drive's health (full) or just
// its model and bus.
func (h *SensorHub) refreshSlowWith(readDrives func(full bool) []DriveHealth, readPower func() PowerStatic, readClocks func() ([]uint32, []uint32)) {
	h.mu.Lock()
	now := time.Now()
	detail := h.detailWanted.Load()
	powerDue := !h.powering && now.Sub(h.powerAt) >= 15*time.Second
	drivesDue := !h.slowing && (h.slowAt.IsZero() ||
		(detail && (!h.drivesFull || now.Sub(h.slowAt) >= 15*time.Second)))
	if powerDue {
		h.powering = true
	}
	if drivesDue {
		h.slowing = true
	}
	needMax := h.maxMHz == nil // the rated maximum does not change: read it once
	h.mu.Unlock()
	if powerDue {
		if needMax {
			h.clockSnapshot(readClocks)
		}
		go func() {
			p := readPower()
			h.mu.Lock()
			h.power = p
			h.powerAt, h.powering = time.Now(), false
			h.mu.Unlock()
		}()
	}
	if drivesDue {
		go func() {
			d := readDrives(detail)
			h.mu.Lock()
			h.drives, h.drivesFull = d, detail
			h.slowAt, h.slowing = time.Now(), false
			h.mu.Unlock()
		}()
	}
}

// clockSnapshot never waits on the power driver. Cached slices are immutable;
// a single in-flight request bounds work even when a driver stops responding,
// and requests closer together than cpuClockFallbackEvery reuse the cache.
func (h *SensorHub) clockSnapshot(read func() ([]uint32, []uint32)) []float64 {
	h.mu.Lock()
	current := h.currentMHz
	now := time.Now()
	launch := !h.clocking && (h.clockAt.IsZero() || now.Sub(h.clockAt) >= cpuClockFallbackEvery)
	if launch {
		h.clocking, h.clockAt = true, now
	}
	h.mu.Unlock()
	if launch {
		go func() {
			cur, maxMHz := read()
			values := make([]float64, len(cur))
			for i, v := range cur {
				values[i] = float64(v)
			}
			h.mu.Lock()
			h.currentMHz, h.maxMHz = values, maxMHz
			h.clocking = false
			h.mu.Unlock()
		}()
	}
	return current
}

func (h *SensorHub) track(id string, v float64) *minMax {
	if h.mm == nil {
		h.mm = map[string]*minMax{}
	}
	m := h.mm[id]
	if m == nil {
		m = &minMax{}
		h.mm[id] = m
	}
	if !m.set {
		m.min, m.max, m.set = v, v, true
	} else {
		m.min, m.max = math.Min(m.min, v), math.Max(m.max, v)
	}
	return m
}

type fmtFn func(float64) string

func fPct(v float64) string     { return fmt.Sprintf("%.1f %%", v) }
func fTemp(v float64) string    { return fmt.Sprintf("%.1f °C", v) }
func fWatt(v float64) string    { return fmt.Sprintf("%.1f W", v) }
func fCount(v float64) string   { return fmt.Sprintf("%.0f", v) }
func fVolt(v float64) string    { return fmt.Sprintf("%.3f V", v) }
func fAmp(v float64) string     { return fmt.Sprintf("%.1f A", v) }
func fHours(v float64) string   { return fmt.Sprintf("%.0f h", v) }
func fMinutes(v float64) string { return fmt.Sprintf("%.0f min", v) }
func fClock(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2f GHz", v/1000)
	}
	return fmt.Sprintf("%.0f MHz", v)
}
func fRate(v float64) string { return rateStr(v) }
func fData(v float64) string { return bytesStr(v) }

func bytesStr(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 || n >= 100 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	if n >= 10 {
		return fmt.Sprintf("%.1f %s", n, units[i])
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}

func rateStr(n float64) string { return bytesStr(n) + "/s" }

// sensorEntry is one row before formatting. Values are tracked (min/max) on
// every sample; text is produced only when the tree is actually shown.
type sensorEntry struct {
	id, name, kind string
	depth          int
	group, isVal   bool
	pre            bool   // preformatted external row: value/min/max as given
	value          string // text rows and external rows
	min, max       string // external rows
	v, mn, mx      float64
	f              fmtFn
}

type sensorBuilder struct {
	full bool // names are needed (the tree will be formatted)
	e    []sensorEntry
}

func (b *sensorBuilder) group(id, name, kind string, depth int) {
	b.e = append(b.e, sensorEntry{id: id, name: name, kind: kind, depth: depth, group: true})
}

func (b *sensorBuilder) val(id, name, kind string, depth int, v float64, f fmtFn) {
	b.e = append(b.e, sensorEntry{id: id, name: name, kind: kind, depth: depth, isVal: true, v: v, f: f})
}

func (b *sensorBuilder) text(id, name, kind string, depth int, v string) {
	b.e = append(b.e, sensorEntry{id: id, name: name, kind: kind, depth: depth, value: v})
}

// sensorState is the hub state one Update reads, copied under h.mu so the
// tree is built without holding the lock the UI paints with. The slices are
// replaced wholesale by their writers, never modified in place.
type sensorState struct {
	baseMHz    uint32
	maxMHz     []uint32
	drives     []DriveHealth
	drivesFull bool
	power      PowerStatic
	ext        *ExternalSource
	ryzen      *RyzenSDK
	apu        *GPUAdapter
}

// noAPU stands in when no adapter reports CPU readings; it is only read.
var noAPU GPUAdapter

// sensorFmt caches one row's formatted text; it is re-formatted only when the
// value or an extreme changes.
type sensorFmt struct {
	v, mn, mx    float64
	vs, mns, mxs string
	set          bool
}

func (c *sensorFmt) format(v, mn, mx float64, f fmtFn) (string, string, string) {
	if !c.set || c.v != v {
		c.v, c.vs = v, f(v)
	}
	if !c.set || c.mn != mn {
		c.mn, c.mns = mn, f(mn)
	}
	if !c.set || c.mx != mx {
		c.mx, c.mxs = mx, f(mx)
	}
	c.set = true
	return c.vs, c.mns, c.mxs
}

// Update tracks every numeric sensor's minimum and maximum for one snapshot
// and returns the labelled tree. While the Power & Freq page is hidden only
// the tracking runs (on cached IDs); the tree returned is the last one built,
// refreshed every sensorRowsRefresh. The page's first sample once shown gets
// a freshly built tree.
func (h *SensorHub) Update(s *Snapshot, host HostInfo) []SensorRow {
	h.refreshSlow()
	// Read cached fallback clocks and request at most one background refresh.
	freqs := s.CPUFreq
	if len(freqs) == 0 {
		freqs = h.clockSnapshot(ReadCPUClocks)
	}
	h.updMu.Lock()
	defer h.updMu.Unlock()

	var apu *GPUAdapter
	for i := range s.GPUs {
		if s.GPUs[i].HasCPUTemp || s.GPUs[i].HasCPUPower {
			apu = &s.GPUs[i]
			break
		}
	}
	h.mu.Lock()
	h.apu = GPUAdapter{}
	if apu != nil {
		h.apu = *apu
	}
	st := sensorState{baseMHz: h.topo.BaseMHz, maxMHz: h.maxMHz, drives: h.drives, drivesFull: h.drivesFull,
		power: h.power, ext: h.ext, ryzen: h.ryzen, apu: &noAPU}
	h.mu.Unlock()
	if apu != nil {
		st.apu = apu
	}

	now := time.Now()
	b := sensorBuilder{full: h.rowsWanted(now), e: h.entries[:0]}
	h.collect(&b, s, host, freqs, &st)
	h.entries = b.e

	// Min/max are tracked on every sample, page shown or not.
	h.mu.Lock()
	for i := range b.e {
		if e := &b.e[i]; e.isVal {
			m := h.track(e.id, e.v)
			e.mn, e.mx = m.min, m.max
		}
	}
	h.mu.Unlock()

	if !b.full {
		return h.rows
	}
	h.rows, h.rowsAt = h.format(b.e), now
	return h.rows
}

// format turns entries into a new row slice; returned rows are never modified.
func (h *SensorHub) format(entries []sensorEntry) []SensorRow {
	if h.fmtc == nil || len(h.fmtc) > 8192 {
		h.fmtc = make(map[string]*sensorFmt, len(entries))
	}
	rows := make([]SensorRow, len(entries))
	for i := range entries {
		e := &entries[i]
		r := SensorRow{ID: e.id, Name: e.name, Kind: e.kind, Depth: e.depth, Group: e.group}
		switch {
		case e.isVal:
			c := h.fmtc[e.id]
			if c == nil {
				c = &sensorFmt{}
				h.fmtc[e.id] = c
			}
			r.Value, r.Min, r.Max = c.format(e.v, e.mn, e.mx, e.f)
		case e.pre:
			r.Value, r.Min, r.Max = e.value, e.min, e.max
		default:
			r.Value = e.value
		}
		rows[i] = r
	}
	return rows
}

// collect lists every row for one snapshot. The same control flow runs with
// and without b.full, so exactly the same sensors are tracked either way;
// only the work of naming rows is skipped when nobody will see them.
func (h *SensorHub) collect(b *sensorBuilder, s *Snapshot, host HostInfo, freqs []float64, st *sensorState) {
	ids := &h.ids
	full := b.full
	group, val, text := b.group, b.val, b.text

	name := ""
	if full {
		name = host.Hostname
	}
	group("pc", name, "pc", 0)

	// ---- CPU ----
	if full {
		name = trim(host.CPUName)
	}
	group("cpu", name, "cpu", 1)

	rz := &h.rz
	rzOK := false
	rzState, rzDetail := RyzenOff, ""
	if st.ryzen != nil {
		rzOK = st.ryzen.readingInto(rz)
		rzState, rzDetail = st.ryzen.State()
	}

	group("cpu.temp", "Temperatures", "temp", 2)
	if rzOK {
		val("cpu.temp.rz", "CPU Package (Tctl/Tdie)", "temp", 3, rz.TempC, fTemp)
		for _, c := range rz.Cores {
			if c.TempC > 0 && c.TempC < 150 {
				val(ids.idx("cpu.temp.rz.c", c.Index, ""), ids.idx("Core #", c.Index, ""), "temp", 3, c.TempC, fTemp)
			}
		}
		if rz.CHTCLimit > 0 {
			val("cpu.temp.rz.htc", "Thermal limit (cHTC)", "temp", 3, rz.CHTCLimit, fTemp)
		}
	} else if t, label, ok := cpuTemperature(nil, st.ext, st.apu.HasCPUTemp, st.apu.CPUTempC); ok {
		val("cpu.temp.ext", label, "temp", 3, t, fTemp)
	} else {
		msg := "CPU die temperature is unavailable from Windows and installed drivers"
		switch rzState {
		case RyzenStarting:
			msg = "AMD Monitoring SDK is starting…"
		case RyzenUnavailable:
			if full {
				msg = "AMD Monitoring SDK: " + rzDetail
			}
		}
		text("cpu.temp.die", msg, "note", 3, "—")
	}
	for i, tz := range s.Thermal {
		id := ids.idx("cpu.temp.", i, "")
		if thermalPlaceholder(tz.Value) {
			if full {
				name = "ACPI zone " + trim(tz.Name) + " reports a fixed " + fTemp(tz.Value) + ": a firmware placeholder, not a sensor"
			}
			text(id, name, "note", 3, "—")
			continue
		}
		if full {
			name = "ACPI zone " + trim(tz.Name) + " (board, not the CPU die)"
		}
		val(id, name, "temp", 3, tz.Value, fTemp)
	}

	group("cpu.clk", "Clocks", "clock", 2)
	if len(freqs) > 0 {
		var sum float64
		for _, f := range freqs {
			sum += f
		}
		val("cpu.clk.avg", "Core Average", "clock", 3, sum/float64(len(freqs)), fClock)
		for i, f := range freqs {
			val(ids.idx("cpu.clk.", i, ""), ids.idx("Core #", i, ""), "clock", 3, f, fClock)
		}
	}
	if st.baseMHz > 0 {
		val("cpu.clk.base", "Base Clock (nominal)", "clock", 3, float64(st.baseMHz), fClock)
	}
	if len(st.maxMHz) > 0 {
		var mx uint32
		for _, v := range st.maxMHz {
			if v > mx {
				mx = v
			}
		}
		val("cpu.clk.max", "Maximum Clock (rated)", "clock", 3, float64(mx), fClock)
	}
	if s.CPUPerfPct > 0 {
		val("cpu.clk.perf", "Processor Performance", "clock", 3, s.CPUPerfPct, fPct)
	}
	if rzOK {
		if rz.PeakMHz > 0 {
			val("cpu.clk.rz.peak", "Peak core speed (AMD SDK)", "clock", 3, rz.PeakMHz, fClock)
		}
		for _, c := range rz.Cores {
			if c.EffMHz > 0 {
				val(ids.idx("cpu.clk.rz.eff", c.Index, ""), ids.idx("Core #", c.Index, " effective"), "clock", 3, c.EffMHz, fClock)
			}
		}
		if rz.FCLKMHz > 0 {
			val("cpu.clk.rz.fclk", "Fabric clock (FCLK)", "clock", 3, rz.FCLKMHz, fClock)
		}
	}
	if st.apu.HasCPUClock {
		val("cpu.clk.apu", "CPU clock (AMD display driver)", "clock", 3, st.apu.CPUClockMHz, fClock)
	}

	group("cpu.pwr", "Powers", "power", 2)
	if len(s.Energy) == 0 && len(s.PowerMeter) == 0 {
		text("cpu.pwr.none", "Windows reports no CPU power meter on this system", "note", 3, "—")
	}
	if pkg, ok := EnergyPackage(s); ok {
		val("cpu.pwr.pkg", "CPU Package", "power", 3, pkg, fWatt)
	}
	// Core meters are named like RAPL_Package0_Core12_CORE. Sorted by core
	// number, then listed after their sum.
	cores := h.ids.cores[:0]
	var coreSum float64
	for _, e := range s.Energy {
		if n := ids.raplCore(e.Name); n >= 0 {
			cores = append(cores, raplCore{n, e.Value})
			coreSum += e.Value
		}
	}
	h.ids.cores = cores
	if len(cores) > 0 {
		slices.SortStableFunc(cores, func(a, b raplCore) int { return cmp.Compare(a.n, b.n) })
		val("cpu.pwr.cores", "Cores (sum)", "power", 3, coreSum, fWatt)
		for _, c := range cores {
			val(ids.idx("cpu.pwr.c", c.n, ""), ids.idx("Core #", c.n, ""), "power", 3, c.w, fWatt)
		}
	}
	for i, pm := range s.PowerMeter {
		if full {
			name = trim(pm.Name)
		}
		val(ids.idx("cpu.pwr.", i, ""), name, "power", 3, pm.Value, fWatt)
	}
	if st.apu.HasCPUPower {
		val("cpu.pwr.apu", "CPU power (AMD display driver)", "power", 3, st.apu.CPUPowerW, fWatt)
	}
	if rzOK {
		val("cpu.pwr.rz.ppt", "Package power, PPT (AMD SDK)", "power", 3, rz.PPT, fWatt)
		if rz.PPTLimit > 0 {
			val("cpu.pwr.rz.pptlim", "PPT limit", "power", 3, rz.PPTLimit, fWatt)
		}
		val("cpu.pwr.rz.vdd", "VDDCR_VDD (cores) power", "power", 3, rz.VDDPower, fWatt)
		val("cpu.pwr.rz.soc", "VDDCR_SOC power", "power", 3, rz.SOCPower, fWatt)
	}

	if rzOK {
		group("cpu.elec", "Voltages and currents (AMD SDK)", "power", 2)
		if rz.CoreVAvg > 0 {
			val("cpu.elec.vavg", "Core voltage, average", "power", 3, rz.CoreVAvg, fVolt)
		}
		if rz.CoreVPeak > 0 {
			val("cpu.elec.vpeak", "Core voltage, peak", "power", 3, rz.CoreVPeak, fVolt)
		}
		if rz.SocV > 0 {
			val("cpu.elec.vsoc", "SoC voltage", "power", 3, rz.SocV, fVolt)
		}
		amp := func(id, limID, name, limName string, v, lim float64) {
			if v > 0 || lim > 0 {
				val(id, name, "power", 3, v, fAmp)
				if lim > 0 {
					val(limID, limName, "power", 3, lim, fAmp)
				}
			}
		}
		amp("cpu.elec.tdcvdd", "cpu.elec.tdcvddlim", "TDC (VDD)", "TDC (VDD) limit", rz.TDCVDD, rz.TDCVDDLimit)
		amp("cpu.elec.edcvdd", "cpu.elec.edcvddlim", "EDC (VDD)", "EDC (VDD) limit", rz.EDCVDD, rz.EDCVDDLimit)
		amp("cpu.elec.tdcsoc", "cpu.elec.tdcsoclim", "TDC (SoC)", "TDC (SoC) limit", rz.TDCSOC, rz.TDCSOCLimit)
		amp("cpu.elec.edcsoc", "cpu.elec.edcsoclim", "EDC (SoC)", "EDC (SoC) limit", rz.EDCSOC, rz.EDCSOCLimit)
	}

	group("cpu.load", "Utilization", "load", 2)
	val("cpu.load.total", "CPU Total", "load", 3, s.CPU, fPct)
	for i, c := range s.Cores {
		val(ids.idx("cpu.load.", i, ""), ids.idx("CPU Core #", i, ""), "load", 3, c, fPct)
	}

	// ---- memory ----
	group("mem", "Memory", "mem", 1)
	val("mem.load", "Memory Used", "mem", 2, s.Mem.Percent, fPct)
	val("mem.used", "Memory Used (size)", "mem", 2, float64(s.Mem.Used), fData)
	val("mem.avail", "Memory Available", "mem", 2, float64(s.Mem.Available), fData)
	val("mem.commit", "Committed", "mem", 2, float64(s.Mem.CommitTotal), fData)

	// ---- GPU ----
	for i := range s.GPUs {
		g := &s.GPUs[i]
		id := ids.gpu(g.ID, i) // the LUID keeps min/max with the adapter if the order changes
		if full {
			name = "GPU " + strconv.Itoa(i) + "  " + trim(g.Name)
		}
		group(id.base, name, "gpu", 1)
		if g.HasTemp || g.HasHotspot || g.HasSocTemp {
			group(id.temp, "Temperatures", "temp", 2)
			if g.HasTemp {
				val(id.tempEdge, "GPU", "temp", 3, g.TempC, fTemp)
			}
			if g.HasHotspot {
				val(id.tempHot, "GPU hotspot", "temp", 3, g.HotspotC, fTemp)
			}
			if g.HasSocTemp {
				val(id.tempSoc, "SoC", "temp", 3, g.SocTempC, fTemp)
			}
		}
		if g.HasClock || g.HasMemClock {
			group(id.clk, "Clocks", "clock", 2)
			if g.HasClock {
				val(id.clkCore, "GPU core", "clock", 3, g.ClockMHz, fClock)
			}
			if g.HasMemClock || (g.HasClock && g.MemClockMHz > 0) {
				val(id.clkMem, "GPU memory", "clock", 3, g.MemClockMHz, fClock)
			}
		}
		if g.HasPower || g.HasVoltage {
			group(id.pwr, "Powers", "power", 2)
			if g.HasPower {
				val(id.pwr0, GPUPowerLabel(g), "power", 3, g.PowerW, fWatt)
				if g.PowerLimitW > 0 {
					val(id.pwrLim, "Power limit", "power", 3, g.PowerLimitW, fWatt)
				}
			}
			if g.HasVoltage {
				val(id.pwrV, "GPU core voltage", "power", 3, g.VoltageV, fVolt)
			}
		}
		if g.HasFan {
			group(id.fan, "Fans", "load", 2)
			if g.FanRPM > 0 {
				val(id.fanRPM, "GPU fan", "load", 3, g.FanRPM, fCount)
			} else {
				val(id.fanPct, "GPU fan", "load", 3, g.FanPct, fPct)
			}
		}
		group(id.load, "Utilization", "load", 2)
		if g.UtilizationAvailable() {
			val(id.load0, "Busiest engine", "gpu", 3, g.Pct, fPct)
		} else {
			text(id.load0, "Utilization unavailable from this driver", "note", 3, "—")
		}
		if g.HasDriverPct {
			val(id.loadDrv, "GPU busy (driver)", "gpu", 3, g.DriverPct, fPct)
		}
		for k, e := range g.Engines {
			val(id.engine(k), e.Name, "gpu", 3, e.Pct, fPct)
		}
		group(id.mem, "Memory", "mem", 2)
		if g.HasDedicatedUsage {
			val(id.memDed, "Dedicated in use", "mem", 3, float64(g.DedicatedUsed), fData)
		} else {
			text(id.memDed, "Dedicated usage unavailable", "note", 3, "—")
		}
		if g.HasSharedUsage {
			val(id.memShr, "Shared in use", "mem", 3, float64(g.SharedUsed), fData)
		} else {
			text(id.memShr, "Shared usage unavailable", "note", 3, "—")
		}
		if !g.HasTemp && !g.HasClock && !g.HasMemClock {
			text(id.none, gpuSensorNote(g), "note", 2, "—")
		}
	}
	if len(s.GPUs) == 0 && s.GPU.OK {
		group("gpu", "Graphics (all adapters)", "gpu", 1)
		val("gpu.load", "GPU Core (busiest engine)", "gpu", 2, s.GPU.Percent, fPct)
		val("gpu.mem", "GPU Memory Used", "gpu", 2, float64(s.GPU.MemBytes), fData)
	}

	// ---- drives ----
	for _, d := range st.drives {
		id := ids.drive(d.Index)
		if full {
			name = d.Model + "  (" + d.Bus + ")"
		}
		group(id.base, name, "disk", 1)
		if d.HasTemp {
			group(id.temp, "Temperatures", "temp", 2)
			val(id.temp0, "Composite", "temp", 3, d.TempC, fTemp)
		}
		if !d.HasTemp {
			note := "Temperature unavailable from this storage driver or controller"
			switch {
			case !st.drivesFull:
				note = "Reading drive temperature…"
			case d.stalled:
				note = "Drive did not answer in time; temperature unavailable"
			case d.Bus == "RAID":
				note = "RAID controller does not expose member-drive temperatures"
			}
			text(id.tempNone, note, "note", 2, "—")
		}
		if ds, ok := perDiskByIndex(s.PerDisk, d.Index); ok {
			group(id.load, "Utilization", "load", 2)
			val(id.load0, "Active time", "load", 3, ds.Percent, fPct)
			group(id.speed, "Speed", "net", 2)
			val(id.speedR, "Read", "net", 3, ds.ReadBps, fRate)
			val(id.speedW, "Write", "net", 3, ds.WriteBps, fRate)
		}
		if d.HasSMART {
			health := "Healthy"
			switch {
			case d.Critical != 0:
				health = "Warning"
			case d.SparePct < d.SpareThresh:
				health = "Spare below threshold"
			}
			group(id.smart, "S.M.A.R.T.", "data", 2)
			text(id.smartHealth, "Health", "data", 3, health)
			val(id.spare, "Available Spare", "data", 3, float64(d.SparePct), fPct)
			val(id.thr, "Available Spare Threshold", "data", 3, float64(d.SpareThresh), fPct)
			val(id.used, "Percentage Used", "data", 3, float64(d.UsedPct), fPct)
			val(id.read, "Data Read", "data", 3, d.ReadBytes, fData)
			val(id.write, "Data Written", "data", 3, d.WrittenByte, fData)
			val(id.hr, "Host Read Commands", "data", 3, d.HostReads, fCount)
			val(id.hw, "Host Write Commands", "data", 3, d.HostWrites, fCount)
			val(id.busy, "Controller Busy Time", "data", 3, d.BusyMins, fMinutes)
			val(id.cyc, "Power Cycles", "data", 3, d.PowerCycles, fCount)
			val(id.poh, "Power On Hours", "data", 3, d.PowerOnHrs, fHours)
			val(id.unsafe, "Unsafe Shutdowns", "data", 3, d.Unsafe, fCount)
			val(id.err, "Media Errors", "data", 3, d.MediaErrors, fCount)
		} else {
			note := "S.M.A.R.T. unavailable from this storage driver or controller"
			switch {
			case !st.drivesFull:
				note = "Reading S.M.A.R.T. health…"
			case d.stalled:
				note = "Drive did not answer in time; S.M.A.R.T. unavailable"
			}
			text(id.smartNone, note, "note", 2, "—")
		}
	}

	// ---- external sensors (optional; they carry their own min/max) ----
	if st.ext != nil && full {
		if !st.ext.Connected() {
			group("ext", "External sensors", "note", 1)
			text("ext.none", "Not connected. Start LibreHardwareMonitor with its Remote Web Server on port 8085 to add CPU die temperature, voltages and fans.", "note", 2, "—")
		} else {
			group("ext", "External sensors (LibreHardwareMonitor)", "data", 1)
			lastHW, lastGrp := "", ""
			for i, e := range st.ext.Sensors() {
				if e.Hardware != lastHW {
					lastHW, lastGrp = e.Hardware, ""
					group(fmt.Sprintf("ext.hw.%d", i), e.Hardware, "cpu", 2)
				}
				if e.Group != lastGrp {
					lastGrp = e.Group
					group(fmt.Sprintf("ext.grp.%d", i), e.Group, kindFor(e.Group), 3)
				}
				id := "ext." + e.SensorID
				if e.SensorID == "" {
					id = fmt.Sprintf("ext.s.%d", i)
				}
				b.e = append(b.e, sensorEntry{id: id, name: e.Name, kind: kindFor(e.Group), depth: 4, pre: true,
					value: e.Value, min: e.Min, max: e.Max})
			}
		}
	}

	// ---- power ----
	group("power", "Power", "batt", 1)
	if st.power.Plan != "" {
		text("power.plan", "Active power plan", "power", 2, st.power.Plan)
	}
	src := "AC power"
	if !st.power.OnAC {
		src = "Battery"
	}
	text("power.src", "Power source", "power", 2, src)
	if st.power.HasBattery {
		val("power.batt", "Battery charge", "batt", 2, float64(st.power.BatteryPct), fPct)
	}
	fs := "Disabled"
	if st.power.FastStartup {
		fs = "Enabled"
	}
	text("power.fast", "Fast Startup", "power", 2, fs)
}

// perDiskByIndex finds the PDH disk whose instance ("0 C: D:") starts with
// index; the last match wins, as a map keyed by index would.
func perDiskByIndex(disks []DiskStat, index int) (DiskStat, bool) {
	var out DiskStat
	found := false
	for _, ds := range disks {
		if n, ok := leadingInt(ds.Name); ok && n == index {
			out, found = ds, true
		}
	}
	return out, found
}

// leadingInt parses the integer at the start of s (after spaces), like
// fmt.Sscanf(s, "%d"), without allocating.
func leadingInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := i
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j {
		return 0, false
	}
	n, err := strconv.Atoi(s[i:k])
	return n, err == nil
}

// ---- cached row IDs and names ---------------------------------------------------------

type raplCore struct {
	n int
	w float64
}

type sensorIdxKey struct{ prefix, suffix string }

// sensorIDs builds each indexed or per-device ID and name once.
type sensorIDs struct {
	indexed map[sensorIdxKey][]string
	gpus    map[string]*gpuSensorIDs
	drives  map[int]*driveSensorIDs
	rapl    map[string]int // energy-meter instance -> core number, -1 not a core meter
	cores   []raplCore
}

// idx returns prefix + i + suffix.
func (c *sensorIDs) idx(prefix string, i int, suffix string) string {
	if i < 0 || i > 1<<16 {
		return prefix + strconv.Itoa(i) + suffix
	}
	k := sensorIdxKey{prefix, suffix}
	l := c.indexed[k]
	if i < len(l) && l[i] != "" {
		return l[i]
	}
	if c.indexed == nil {
		c.indexed = map[sensorIdxKey][]string{}
	}
	if i >= len(l) {
		l = append(l, make([]string, i+1-len(l))...)
	}
	l[i] = prefix + strconv.Itoa(i) + suffix
	c.indexed[k] = l
	return l[i]
}

// raplCore reads "RAPL_Package0_Core12_CORE" as core 12 (-1 for other meters).
func (c *sensorIDs) raplCore(name string) int {
	if n, ok := c.rapl[name]; ok {
		return n
	}
	n := -1
	parts := strings.Split(strings.ToUpper(name), "_")
	if len(parts) == 4 && parts[0] == "RAPL" && strings.HasPrefix(parts[1], "PACKAGE") &&
		strings.HasPrefix(parts[2], "CORE") && parts[3] == "CORE" {
		var v int
		if _, err := fmt.Sscanf(parts[2][4:], "%d", &v); err == nil {
			n = v
		}
	}
	if c.rapl == nil || len(c.rapl) > 4096 {
		c.rapl = map[string]int{}
	}
	c.rapl[name] = n
	return n
}

type gpuSensorIDs struct {
	base, temp, tempEdge, tempHot, tempSoc, clk, clkCore, clkMem string
	pwr, pwr0, pwrLim, pwrV, fan, fanRPM, fanPct                 string
	load, load0, loadDrv, mem, memDed, memShr, none              string
	engines                                                      []string
}

func (g *gpuSensorIDs) engine(k int) string {
	for len(g.engines) <= k {
		g.engines = append(g.engines, g.base+".load.e"+strconv.Itoa(len(g.engines)))
	}
	return g.engines[k]
}

// gpu returns the IDs for adapter id (or, without one, its position i).
func (c *sensorIDs) gpu(id string, i int) *gpuSensorIDs {
	key := id
	if key == "" {
		key = c.idx("gpu", i, "")
	}
	if g := c.gpus[key]; g != nil {
		return g
	}
	base := key
	if id != "" {
		base = "gpu." + id
	}
	g := &gpuSensorIDs{base: base,
		temp: base + ".temp", tempEdge: base + ".temp.edge", tempHot: base + ".temp.hot", tempSoc: base + ".temp.soc",
		clk: base + ".clk", clkCore: base + ".clk.core", clkMem: base + ".clk.mem",
		pwr: base + ".pwr", pwr0: base + ".pwr.0", pwrLim: base + ".pwr.lim", pwrV: base + ".pwr.v",
		fan: base + ".fan", fanRPM: base + ".fan.rpm", fanPct: base + ".fan.pct",
		load: base + ".load", load0: base + ".load.0", loadDrv: base + ".load.drv",
		mem: base + ".mem", memDed: base + ".mem.ded", memShr: base + ".mem.shr", none: base + ".none"}
	if c.gpus == nil || len(c.gpus) > 256 {
		c.gpus = map[string]*gpuSensorIDs{}
	}
	c.gpus[key] = g
	return g
}

type driveSensorIDs struct {
	base, temp, temp0, tempNone, load, load0, speed, speedR, speedW string
	smart, smartHealth, spare, thr, used, read, write, hr, hw       string
	busy, cyc, poh, unsafe, err, smartNone                          string
}

func (c *sensorIDs) drive(index int) *driveSensorIDs {
	if d := c.drives[index]; d != nil {
		return d
	}
	base := "disk" + strconv.Itoa(index)
	d := &driveSensorIDs{base: base,
		temp: base + ".temp", temp0: base + ".temp.0", tempNone: base + ".temp.none",
		load: base + ".load", load0: base + ".load.0", speed: base + ".speed", speedR: base + ".speed.r", speedW: base + ".speed.w",
		smart: base + ".smart", smartHealth: base + ".smart.health", spare: base + ".smart.spare", thr: base + ".smart.thr",
		used: base + ".smart.used", read: base + ".smart.read", write: base + ".smart.write", hr: base + ".smart.hr", hw: base + ".smart.hw",
		busy: base + ".smart.busy", cyc: base + ".smart.cyc", poh: base + ".smart.poh", unsafe: base + ".smart.unsafe",
		err: base + ".smart.err", smartNone: base + ".smart.none"}
	if c.drives == nil || len(c.drives) > 1024 {
		c.drives = map[int]*driveSensorIDs{}
	}
	c.drives[index] = d
	return d
}

// thermalPlaceholder recognises ACPI zones that return a constant below any
// temperature a running board can have. Desktop firmware often fills an
// unused zone with 290 K (16.85 °C) or 0 K; Windows exposes it anyway.
func thermalPlaceholder(celsius float64) bool { return celsius < 20 }

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == 0) {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}

// cpuTemperature picks the best CPU temperature: the AMD SDK, then an
// external program, then the CPU temperature an APU's display driver reports
// (no driver of our own, no administrator rights).
func cpuTemperature(rz *RyzenSDK, ext *ExternalSource, hasAPU bool, apuC float64) (float64, string, bool) {
	if rz != nil {
		if t, ok := rz.Temperature(); ok {
			return t, "CPU Package (Tctl/Tdie)", true
		}
	}
	if ext != nil {
		if t, label, ok := CPUTemperature(ext.Sensors()); ok {
			return t, label, true
		}
	}
	if hasAPU {
		return apuC, "CPU (AMD display driver)", true
	}
	return 0, "", false
}

// GPUPowerLabel names what an adapter's power figure covers.
func GPUPowerLabel(g *GPUAdapter) string {
	switch g.PowerSource {
	case "board":
		return "Board power"
	case "asic", "soc":
		if g.Integrated || g.HasCPUTemp || g.HasCPUPower {
			return "Package power (CPU and GPU)" // an APU shares one power budget
		}
		return "Chip power"
	case "gfx":
		return "Graphics power"
	}
	return "Power draw"
}

func gpuSensorNote(g *GPUAdapter) string {
	return "Windows and the installed display driver report no clock or temperature readings"
}

func kindFor(group string) string {
	g := strings.ToLower(group)
	switch {
	case strings.Contains(g, "temp"):
		return "temp"
	case strings.Contains(g, "power"):
		return "power"
	case strings.Contains(g, "clock"):
		return "clock"
	case strings.Contains(g, "load"), strings.Contains(g, "util"):
		return "load"
	case strings.Contains(g, "data"):
		return "data"
	}
	return "data"
}
