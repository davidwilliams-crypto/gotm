//go:build windows

package metrics

import (
	"fmt"
	"math"
	"sort"
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
	slowAt       time.Time
	slowing      bool
	powerAt      time.Time
	powering     bool
	ext          *ExternalSource
	ryzen        *RyzenSDK
	detailWanted atomic.Bool
	apu          GPUAdapter // the APU adapter from the last snapshot, for its CPU sensors
}

// NewSensorHub starts empty; the first Update triggers the slow readers.
func NewSensorHub() *SensorHub {
	ext := NewExternalSource(DefaultExternalURL)
	ext.Start()
	return NewSensorHubWith(ext)
}

// NewSensorHubWith is NewSensorHub with an explicit external source (or nil).
func NewSensorHubWith(ext *ExternalSource) *SensorHub {
	h := &SensorHub{mm: map[string]*minMax{}, ext: ext}
	h.power = ReadPowerStatic() // cheap; avoids showing "Battery" until the first slow read
	h.topo = ReadTopology()
	return h
}

// External exposes the optional external sensor source (never nil after NewSensorHub).
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
func (h *SensorHub) CPUTemperature() (celsius float64, label string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cpuTempLocked()
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

// SetDetailWanted says whether the Power & Freq page is on screen. Drive health
// is read once at start (for drive names) and then only while it is wanted:
// polling every drive every 15 s keeps disks and NVMe out of low-power states.
func (h *SensorHub) SetDetailWanted(on bool) { h.detailWanted.Store(on) }

// refreshSlow reads rarely changing sensors off-thread. Power state must stay
// fresh on every page, independently of the drive-health polling gate.
func (h *SensorHub) refreshSlow() {
	h.refreshSlowWith(ReadDriveHealth, ReadPowerStatic, ReadCPUClocks)
}

func (h *SensorHub) refreshSlowWith(readDrives func() []DriveHealth, readPower func() PowerStatic, readClocks func() ([]uint32, []uint32)) {
	h.mu.Lock()
	now := time.Now()
	powerDue := !h.powering && now.Sub(h.powerAt) >= 15*time.Second
	drivesDue := !h.slowing && now.Sub(h.slowAt) >= 15*time.Second && (h.slowAt.IsZero() || h.detailWanted.Load())
	if powerDue {
		h.powering = true
	}
	if drivesDue {
		h.slowing = true
	}
	h.mu.Unlock()
	if powerDue {
		go func() {
			p := readPower()
			_, mx := readClocks()
			h.mu.Lock()
			h.power, h.maxMHz = p, mx
			h.powerAt, h.powering = time.Now(), false
			h.mu.Unlock()
		}()
	}
	if drivesDue {
		go func() {
			d := readDrives()
			h.mu.Lock()
			h.drives = d
			h.slowAt, h.slowing = time.Now(), false
			h.mu.Unlock()
		}()
	}
}

func (h *SensorHub) track(id string, v float64) *minMax {
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

// Update rebuilds the tree for one snapshot.
func (h *SensorHub) Update(s *Snapshot, host HostInfo) []SensorRow {
	h.refreshSlow()
	h.mu.Lock()
	defer h.mu.Unlock()

	h.apu = GPUAdapter{}
	for i := range s.GPUs {
		if s.GPUs[i].HasCPUTemp || s.GPUs[i].HasCPUPower {
			h.apu = s.GPUs[i]
			break
		}
	}

	var rows []SensorRow
	group := func(id, name, kind string, depth int) {
		rows = append(rows, SensorRow{ID: id, Name: name, Kind: kind, Depth: depth, Group: true})
	}
	val := func(id, name, kind string, depth int, v float64, f fmtFn) {
		m := h.track(id, v)
		rows = append(rows, SensorRow{ID: id, Name: name, Kind: kind, Depth: depth,
			Value: f(v), Min: f(m.min), Max: f(m.max)})
	}
	text := func(id, name, kind string, depth int, v string) {
		rows = append(rows, SensorRow{ID: id, Name: name, Kind: kind, Depth: depth, Value: v})
	}

	group("pc", host.Hostname, "pc", 0)

	// ---- CPU ----
	cpuName := host.CPUName
	group("cpu", trim(cpuName), "cpu", 1)

	var rz RyzenReading
	rzOK := false
	rzState, rzDetail := RyzenOff, ""
	if h.ryzen != nil {
		rz, rzOK = h.ryzen.Reading()
		rzState, rzDetail = h.ryzen.State()
	}

	group("cpu.temp", "Temperatures", "temp", 2)
	if rzOK {
		val("cpu.temp.rz", "CPU Package (Tctl/Tdie)", "temp", 3, rz.TempC, fTemp)
		for _, c := range rz.Cores {
			if c.TempC > 0 && c.TempC < 150 {
				val(fmt.Sprintf("cpu.temp.rz.c%d", c.Index), fmt.Sprintf("Core #%d", c.Index), "temp", 3, c.TempC, fTemp)
			}
		}
		if rz.CHTCLimit > 0 {
			val("cpu.temp.rz.htc", "Thermal limit (cHTC)", "temp", 3, rz.CHTCLimit, fTemp)
		}
	} else if t, label, ok := h.cpuTempLocked(); ok {
		val("cpu.temp.ext", label+" (external)", "temp", 3, t, fTemp)
	} else {
		msg := "CPU die temperature needs the AMD Monitoring SDK (Settings) or an external sensor program"
		switch rzState {
		case RyzenStarting:
			msg = "AMD Monitoring SDK is starting…"
		case RyzenUnavailable:
			msg = "AMD Monitoring SDK: " + rzDetail
		}
		text("cpu.temp.die", msg, "note", 3, "—")
	}
	for i, tz := range s.Thermal {
		if thermalPlaceholder(tz.Value) {
			text(fmt.Sprintf("cpu.temp.%d", i), "ACPI zone "+trim(tz.Name)+" reports a fixed "+fTemp(tz.Value)+": a firmware placeholder, not a sensor", "note", 3, "—")
			continue
		}
		val(fmt.Sprintf("cpu.temp.%d", i), "ACPI zone "+trim(tz.Name)+" (board, not the CPU die)", "temp", 3, tz.Value, fTemp)
	}

	group("cpu.clk", "Clocks", "clock", 2)
	freqs := s.CPUFreq
	if len(freqs) == 0 {
		cur, _ := ReadCPUClocks()
		for _, c := range cur {
			freqs = append(freqs, float64(c))
		}
	}
	if len(freqs) > 0 {
		var sum float64
		for _, f := range freqs {
			sum += f
		}
		val("cpu.clk.avg", "Core Average", "clock", 3, sum/float64(len(freqs)), fClock)
		for i, f := range freqs {
			val(fmt.Sprintf("cpu.clk.%d", i), fmt.Sprintf("Core #%d", i), "clock", 3, f, fClock)
		}
	}
	if h.topo.BaseMHz > 0 {
		val("cpu.clk.base", "Base Clock (nominal)", "clock", 3, float64(h.topo.BaseMHz), fClock)
	}
	if len(h.maxMHz) > 0 {
		var mx uint32
		for _, v := range h.maxMHz {
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
				val(fmt.Sprintf("cpu.clk.rz.eff%d", c.Index), fmt.Sprintf("Core #%d effective", c.Index), "clock", 3, c.EffMHz, fClock)
			}
		}
		if rz.FCLKMHz > 0 {
			val("cpu.clk.rz.fclk", "Fabric clock (FCLK)", "clock", 3, rz.FCLKMHz, fClock)
		}
	}
	if h.apu.HasCPUClock {
		val("cpu.clk.apu", "CPU clock (AMD display driver)", "clock", 3, h.apu.CPUClockMHz, fClock)
	}

	group("cpu.pwr", "Powers", "power", 2)
	if len(s.Energy) == 0 && len(s.PowerMeter) == 0 {
		text("cpu.pwr.none", "Windows reports no CPU power meter on this system", "note", 3, "—")
	}
	if pkg, ok := EnergyPackage(s); ok {
		val("cpu.pwr.pkg", "CPU Package", "power", 3, pkg, fWatt)
	}
	type coreP struct {
		n int
		w float64
	}
	var cores []coreP
	var coreSum float64
	for _, e := range s.Energy {
		// names look like RAPL_Package0_Core12_CORE
		parts := strings.Split(strings.ToUpper(e.Name), "_")
		if len(parts) == 4 && parts[0] == "RAPL" && strings.HasPrefix(parts[1], "PACKAGE") &&
			strings.HasPrefix(parts[2], "CORE") && parts[3] == "CORE" {
			var n int
			if _, err := fmt.Sscanf(parts[2][4:], "%d", &n); err == nil {
				cores = append(cores, coreP{n, e.Value})
				coreSum += e.Value
			}
		}
	}
	if len(cores) > 0 {
		sort.Slice(cores, func(i, j int) bool { return cores[i].n < cores[j].n })
		val("cpu.pwr.cores", "Cores (sum)", "power", 3, coreSum, fWatt)
		for _, c := range cores {
			val(fmt.Sprintf("cpu.pwr.c%d", c.n), fmt.Sprintf("Core #%d", c.n), "power", 3, c.w, fWatt)
		}
	}
	for i, pm := range s.PowerMeter {
		val(fmt.Sprintf("cpu.pwr.%d", i), trim(pm.Name), "power", 3, pm.Value, fWatt)
	}
	if h.apu.HasCPUPower {
		val("cpu.pwr.apu", "CPU power (AMD display driver)", "power", 3, h.apu.CPUPowerW, fWatt)
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
		amp := func(id, name string, v, lim float64) {
			if v > 0 || lim > 0 {
				val(id, name, "power", 3, v, fAmp)
				if lim > 0 {
					val(id+"lim", name+" limit", "power", 3, lim, fAmp)
				}
			}
		}
		amp("cpu.elec.tdcvdd", "TDC (VDD)", rz.TDCVDD, rz.TDCVDDLimit)
		amp("cpu.elec.edcvdd", "EDC (VDD)", rz.EDCVDD, rz.EDCVDDLimit)
		amp("cpu.elec.tdcsoc", "TDC (SoC)", rz.TDCSOC, rz.TDCSOCLimit)
		amp("cpu.elec.edcsoc", "EDC (SoC)", rz.EDCSOC, rz.EDCSOCLimit)
	}

	group("cpu.load", "Utilization", "load", 2)
	val("cpu.load.total", "CPU Total", "load", 3, s.CPU, fPct)
	for i, c := range s.Cores {
		val(fmt.Sprintf("cpu.load.%d", i), fmt.Sprintf("CPU Core #%d", i), "load", 3, c, fPct)
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
		id := "gpu." + g.ID // the LUID keeps min/max with the adapter if the order changes
		if g.ID == "" {
			id = fmt.Sprintf("gpu%d", i)
		}
		group(id, fmt.Sprintf("GPU %d  %s", i, trim(g.Name)), "gpu", 1)
		if g.HasTemp || g.HasHotspot || g.HasSocTemp {
			group(id+".temp", "Temperatures", "temp", 2)
			if g.HasTemp {
				val(id+".temp.edge", "GPU", "temp", 3, g.TempC, fTemp)
			}
			if g.HasHotspot {
				val(id+".temp.hot", "GPU hotspot", "temp", 3, g.HotspotC, fTemp)
			}
			if g.HasSocTemp {
				val(id+".temp.soc", "SoC", "temp", 3, g.SocTempC, fTemp)
			}
		}
		if g.HasClock {
			group(id+".clk", "Clocks", "clock", 2)
			val(id+".clk.core", "GPU core", "clock", 3, g.ClockMHz, fClock)
			if g.MemClockMHz > 0 {
				val(id+".clk.mem", "GPU memory", "clock", 3, g.MemClockMHz, fClock)
			}
		}
		if g.HasPower || g.HasVoltage {
			group(id+".pwr", "Powers", "power", 2)
			if g.HasPower {
				val(id+".pwr.0", GPUPowerLabel(g), "power", 3, g.PowerW, fWatt)
				if g.PowerLimitW > 0 {
					val(id+".pwr.lim", "Power limit", "power", 3, g.PowerLimitW, fWatt)
				}
			}
			if g.HasVoltage {
				val(id+".pwr.v", "GPU core voltage", "power", 3, g.VoltageV, fVolt)
			}
		}
		if g.HasFan {
			group(id+".fan", "Fans", "load", 2)
			if g.FanRPM > 0 {
				val(id+".fan.rpm", "GPU fan", "load", 3, g.FanRPM, fCount)
			} else {
				val(id+".fan.pct", "GPU fan", "load", 3, g.FanPct, fPct)
			}
		}
		group(id+".load", "Utilization", "load", 2)
		val(id+".load.0", "Busiest engine", "gpu", 3, g.Pct, fPct)
		if g.HasDriverPct {
			val(id+".load.drv", "GPU busy (driver)", "gpu", 3, g.DriverPct, fPct)
		}
		for k, e := range g.Engines {
			val(fmt.Sprintf("%s.load.e%d", id, k), e.Name, "gpu", 3, e.Pct, fPct)
		}
		group(id+".mem", "Memory", "mem", 2)
		val(id+".mem.ded", "Dedicated in use", "mem", 3, float64(g.DedicatedUsed), fData)
		val(id+".mem.shr", "Shared in use", "mem", 3, float64(g.SharedUsed), fData)
		if !g.HasTemp && !g.HasClock {
			text(id+".none", gpuSensorNote(g), "note", 2, "—")
		}
	}
	if len(s.GPUs) == 0 && s.GPU.OK {
		group("gpu", "Graphics (all adapters)", "gpu", 1)
		val("gpu.load", "GPU Core (busiest engine)", "gpu", 2, s.GPU.Percent, fPct)
		val("gpu.mem", "GPU Memory Used", "gpu", 2, float64(s.GPU.MemBytes), fData)
	}

	// ---- drives ----
	byIndex := map[int]DiskStat{}
	for _, ds := range s.PerDisk {
		var n int
		if _, err := fmt.Sscanf(ds.Name, "%d", &n); err == nil {
			byIndex[n] = ds
		}
	}
	for _, d := range h.drives {
		id := fmt.Sprintf("disk%d", d.Index)
		group(id, fmt.Sprintf("%s  (%s)", d.Model, d.Bus), "disk", 1)
		if d.HasTemp {
			group(id+".temp", "Temperatures", "temp", 2)
			val(id+".temp.0", "Composite", "temp", 3, d.TempC, fTemp)
		}
		if st, ok := byIndex[d.Index]; ok {
			group(id+".load", "Utilization", "load", 2)
			val(id+".load.0", "Active time", "load", 3, st.Percent, fPct)
			group(id+".speed", "Speed", "net", 2)
			val(id+".speed.r", "Read", "net", 3, st.ReadBps, fRate)
			val(id+".speed.w", "Write", "net", 3, st.WriteBps, fRate)
		}
		if d.HasSMART {
			health := "Healthy"
			switch {
			case d.Critical != 0:
				health = "Warning"
			case d.SparePct < d.SpareThresh:
				health = "Spare below threshold"
			}
			group(id+".smart", "S.M.A.R.T.", "data", 2)
			text(id+".smart.health", "Health", "data", 3, health)
			val(id+".smart.spare", "Available Spare", "data", 3, float64(d.SparePct), fPct)
			val(id+".smart.thr", "Available Spare Threshold", "data", 3, float64(d.SpareThresh), fPct)
			val(id+".smart.used", "Percentage Used", "data", 3, float64(d.UsedPct), fPct)
			val(id+".smart.read", "Data Read", "data", 3, d.ReadBytes, fData)
			val(id+".smart.write", "Data Written", "data", 3, d.WrittenByte, fData)
			val(id+".smart.hr", "Host Read Commands", "data", 3, d.HostReads, fCount)
			val(id+".smart.hw", "Host Write Commands", "data", 3, d.HostWrites, fCount)
			val(id+".smart.busy", "Controller Busy Time", "data", 3, d.BusyMins, fMinutes)
			val(id+".smart.cyc", "Power Cycles", "data", 3, d.PowerCycles, fCount)
			val(id+".smart.poh", "Power On Hours", "data", 3, d.PowerOnHrs, fHours)
			val(id+".smart.unsafe", "Unsafe Shutdowns", "data", 3, d.Unsafe, fCount)
			val(id+".smart.err", "Media Errors", "data", 3, d.MediaErrors, fCount)
		} else {
			text(id+".smart.none", "S.M.A.R.T. not exposed for this bus (run as administrator or use the vendor tool)", "note", 2, "—")
		}
	}

	// ---- external sensors (optional) ----
	if h.ext != nil {
		if !h.ext.Connected() {
			group("ext", "External sensors", "note", 1)
			text("ext.none", "Not connected. Start LibreHardwareMonitor with its Remote Web Server on port 8085 to add CPU die temperature, voltages and fans.", "note", 2, "—")
		} else {
			group("ext", "External sensors (LibreHardwareMonitor)", "data", 1)
			lastHW, lastGrp := "", ""
			for i, e := range h.ext.Sensors() {
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
				rows = append(rows, SensorRow{ID: id, Name: e.Name, Kind: kindFor(e.Group), Depth: 4,
					Value: e.Value, Min: e.Min, Max: e.Max})
			}
		}
	}

	// ---- power ----
	group("power", "Power", "batt", 1)
	if h.power.Plan != "" {
		text("power.plan", "Active power plan", "power", 2, h.power.Plan)
	}
	src := "AC power"
	if !h.power.OnAC {
		src = "Battery"
	}
	text("power.src", "Power source", "power", 2, src)
	if h.power.HasBattery {
		val("power.batt", "Battery charge", "batt", 2, float64(h.power.BatteryPct), fPct)
	}
	fs := "Disabled"
	if h.power.FastStartup {
		fs = "Enabled"
	}
	text("power.fast", "Fast Startup", "power", 2, fs)
	return rows
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

// cpuTempLocked is CPUTemperature for use while h.mu is held: the AMD SDK,
// then an external program, then the CPU temperature an APU's display driver
// reports (no driver of our own, no administrator rights).
func (h *SensorHub) cpuTempLocked() (float64, string, bool) {
	if h.ryzen != nil {
		if rd, ok := h.ryzen.Reading(); ok {
			return rd.TempC, "CPU Package (Tctl/Tdie)", true
		}
	}
	if h.ext != nil {
		if t, label, ok := CPUTemperature(h.ext.Sensors()); ok {
			return t, label, true
		}
	}
	if h.apu.HasCPUTemp {
		return h.apu.CPUTempC, "CPU (AMD display driver)", true
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
	switch g.Vendor {
	case "NVIDIA":
		return "Clocks and temperature come from the NVIDIA driver's NVML; they appear while a GPU view is open"
	case "AMD":
		return "The AMD display library (atiadlxx.dll) reported no sensors for this adapter"
	case "Intel":
		return "Intel does not expose clocks or temperature through a public Windows API"
	}
	return "This adapter exposes no clock or temperature sensors"
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
