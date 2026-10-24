//go:build windows

package metrics

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// NamedValue is one instance of a wildcard PDH counter.
type NamedValue struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// DiskStat is the per-physical-disk activity ("0 C: D:" style instance names).
type DiskStat struct {
	Name     string  `json:"name"`
	Percent  float64 `json:"percent"`
	ReadBps  float64 `json:"readBps"`
	WriteBps float64 `json:"writeBps"`
}

// perfSample is everything one PDH collection yields.
type perfSample struct {
	Disk       DiskInfo
	Net        NetInfo
	GPU        GPUInfo
	GPUByPID   map[uint32]float64
	GPUs       []GPUAdapter // per adapter: engines, memory, per-process use
	CPUFreq    []float64    // MHz per logical processor, in group/index order
	CPUPerfPct float64      // % Processor Performance (100 = base clock)
	Thermal    []NamedValue
	PowerMeter []NamedValue
	Energy     []NamedValue // Windows Energy Meter (RAPL-style) power in W: package and per core
	PerDisk    []DiskStat
}

type perfCounters struct {
	q                       *pdhQuery
	diskR, diskW            *pdhCounter
	netRx, netTx            *pdhCounter
	gpu, gpuMem             *pdhCounter
	gpuShared               *pdhCounter
	gpuPDed, gpuPShr        *pdhCounter
	gpuRead                 *gpuReader
	cpuFreq, cpuPerf        *pdhCounter
	thermal, powerMeter     *pdhCounter
	energy                  *pdhCounter
	pdTime, pdRead, pdWrite *pdhCounter
}

func newPerfCounters() *perfCounters {
	q, err := openPdh()
	if err != nil {
		return nil
	}
	pc := &perfCounters{q: q}
	pc.diskR, _ = q.add(`\PhysicalDisk(_Total)\Disk Read Bytes/sec`)
	pc.diskW, _ = q.add(`\PhysicalDisk(_Total)\Disk Write Bytes/sec`)
	pc.netRx, _ = q.add(`\Network Interface(*)\Bytes Received/sec`)
	pc.netTx, _ = q.add(`\Network Interface(*)\Bytes Sent/sec`)
	pc.gpu, _ = q.add(`\GPU Engine(*)\Utilization Percentage`)
	pc.gpuMem, _ = q.add(`\GPU Adapter Memory(*)\Dedicated Usage`)
	pc.gpuShared, _ = q.add(`\GPU Adapter Memory(*)\Shared Usage`)
	// Per-process "Dedicated Usage" counts virtual allocations (dwm.exe reads 46 GB on a
	// 15 GB card); "Local Usage" is what is actually resident in video memory, and is
	// what Task Manager shows per process.
	pc.gpuPDed, _ = q.add(`\GPU Process Memory(*)\Local Usage`)
	pc.gpuPShr, _ = q.add(`\GPU Process Memory(*)\Shared Usage`)
	pc.gpuRead = newGPUReader()
	pc.cpuFreq, _ = q.add(`\Processor Information(*)\Processor Frequency`)
	// Processor Frequency is the nominal clock (4.2 GHz here, never the boost);
	// % Processor Performance is relative to it and goes above 100 while boosting.
	pc.cpuPerf, _ = q.add(`\Processor Information(*)\% Processor Performance`)
	pc.thermal, _ = q.add(`\Thermal Zone Information(*)\Temperature`)
	pc.powerMeter, _ = q.add(`\Power Meter(*)\Power`)
	pc.energy, _ = q.add(`\Energy Meter(*)\Power`)
	// "% Disk Time" is really queue length x 100 (an NVMe at queue depth 4
	// reads 100 % while 30 % busy); active time is 100 - % Idle Time.
	pc.pdTime, _ = q.add(`\PhysicalDisk(*)\% Idle Time`)
	pc.pdRead, _ = q.add(`\PhysicalDisk(*)\Disk Read Bytes/sec`)
	pc.pdWrite, _ = q.add(`\PhysicalDisk(*)\Disk Write Bytes/sec`)
	q.collect() // baseline for rate counters
	return pc
}

func (pc *perfCounters) close() {
	if pc != nil {
		pc.q.close()
	}
}

func (pc *perfCounters) read() perfSample {
	var out perfSample
	if pc == nil || !pc.q.collect() {
		return out
	}
	if v, ok := pc.diskR.value(); ok {
		out.Disk.ReadBps = math.Round(v)
	}
	if v, ok := pc.diskW.value(); ok {
		out.Disk.WriteBps = math.Round(v)
	}
	for _, it := range pc.netRx.values() {
		if !skipNIC(it.Instance) {
			out.Net.RxBps += it.Value
		}
	}
	for _, it := range pc.netTx.values() {
		if !skipNIC(it.Instance) {
			out.Net.TxBps += it.Value
		}
	}
	out.Net.RxBps, out.Net.TxBps = math.Round(out.Net.RxBps), math.Round(out.Net.TxBps)

	// GPU engines are named
	//   pid_<pid>_luid_<hi>_<lo>_phys_<n>_eng_<n>_engtype_<type>
	// A process's utilisation is the busiest engine type (summed within a type);
	// the system figure is the busiest single engine across all processes.
	byPID := map[uint32]map[string]float64{}
	byEngine := map[string]float64{}
	engVals := pc.gpu.values()
	for _, it := range engVals {
		out.GPU.OK = true
		pid, eng, typ, ok := parseGPUInstance(it.Instance)
		if !ok {
			continue
		}
		if byPID[pid] == nil {
			byPID[pid] = map[string]float64{}
		}
		byPID[pid][typ] += it.Value
		byEngine[eng] += it.Value
	}
	out.GPUByPID = make(map[uint32]float64, len(byPID))
	for pid, types := range byPID {
		var best float64
		for _, v := range types {
			if v > best {
				best = v
			}
		}
		out.GPUByPID[pid] = math.Min(best, 100)
	}
	var sys float64
	for _, v := range byEngine {
		if v > sys {
			sys = v
		}
	}
	out.GPU.Percent = round1(math.Min(sys, 100))
	dedVals := pc.gpuMem.values()
	for _, it := range dedVals {
		out.GPU.OK = true
		out.GPU.MemBytes += uint64(it.Value)
	}
	out.GPUs = pc.gpuRead.build(engVals, dedVals, pc.gpuShared.values(), pc.gpuPDed.values(), pc.gpuPShr.values())

	// per-logical-processor clock: instances are "<group>,<index>" plus totals
	type fq struct {
		order int
		mhz   float64
	}
	perfBy := map[int]float64{}
	var perfTotal float64
	var haveTotal bool
	for _, it := range pc.cpuPerf.values() {
		if strings.EqualFold(it.Instance, "_Total") {
			perfTotal, haveTotal = it.Value, true
			continue
		}
		if g, i, ok := parseCPUInstance(it.Instance); ok {
			perfBy[g*1024+i] = it.Value
		}
	}
	var fqs []fq
	for _, it := range pc.cpuFreq.values() {
		g, i, ok := parseCPUInstance(it.Instance)
		if !ok {
			continue
		}
		mhz := it.Value
		if p, ok := perfBy[g*1024+i]; ok && p > 0 {
			mhz = it.Value * p / 100
		}
		fqs = append(fqs, fq{g*1024 + i, mhz})
	}
	sort.Slice(fqs, func(i, j int) bool { return fqs[i].order < fqs[j].order })
	for _, f := range fqs {
		out.CPUFreq = append(out.CPUFreq, math.Round(f.mhz))
	}
	if haveTotal {
		out.CPUPerfPct = round1(perfTotal)
	}
	for _, it := range pc.thermal.values() {
		out.Thermal = append(out.Thermal, NamedValue{Name: it.Instance, Value: it.Value - 273.15})
	}
	for _, it := range pc.powerMeter.values() {
		out.PowerMeter = append(out.PowerMeter, NamedValue{Name: it.Instance, Value: it.Value / 1000}) // mW -> W
	}
	for _, it := range pc.energy.values() {
		if it.Instance == "_Total" || it.Instance == "_total" {
			continue
		}
		out.Energy = append(out.Energy, NamedValue{Name: it.Instance, Value: it.Value / 1000}) // mW -> W
	}
	for _, it := range pc.pdTime.values() {
		if it.Instance == "_Total" {
			continue
		}
		active := round1(math.Max(0, math.Min(100, 100-it.Value)))
		out.PerDisk = append(out.PerDisk, DiskStat{Name: it.Instance, Percent: active})
		out.Disk.Percent = math.Max(out.Disk.Percent, active) // the busiest disk, as Task Manager's summary
	}
	sort.Slice(out.PerDisk, func(i, j int) bool { return out.PerDisk[i].Name < out.PerDisk[j].Name })
	rd, wr := map[string]float64{}, map[string]float64{}
	for _, it := range pc.pdRead.values() {
		rd[it.Instance] = it.Value
	}
	for _, it := range pc.pdWrite.values() {
		wr[it.Instance] = it.Value
	}
	for i := range out.PerDisk {
		out.PerDisk[i].ReadBps = math.Round(rd[out.PerDisk[i].Name])
		out.PerDisk[i].WriteBps = math.Round(wr[out.PerDisk[i].Name])
	}
	return out
}

// parseCPUInstance reads "0,12" -> group 0, index 12 ("_Total" variants fail).
func parseCPUInstance(s string) (group, idx int, ok bool) {
	c := strings.IndexByte(s, ',')
	if c <= 0 {
		return
	}
	g, err1 := strconv.Atoi(s[:c])
	i, err2 := strconv.Atoi(s[c+1:])
	if err1 != nil || err2 != nil {
		return
	}
	return g, i, true
}

// skipNIC drops interfaces whose bytes are also counted on a physical adapter:
// loopback and IPv6 transition pseudo-interfaces, Hyper-V/WSL virtual switches
// (vEthernet) and VPN tunnels, whose traffic crosses the real NIC as well.
func skipNIC(name string) bool {
	l := strings.ToLower(name)
	for _, s := range []string{"loopback", "isatap", "teredo", "6to4", "vethernet", "hyper-v virtual",
		"virtual ethernet adapter", "wan miniport", "tap-windows", "wintun", "wireguard", "tunnel",
		"vpn", "anyconnect", "pangp", "fortinet", "zscaler", "kernel debug"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func parseGPUInstance(s string) (pid uint32, engine, engType string, ok bool) {
	if !strings.HasPrefix(s, "pid_") {
		return
	}
	rest := s[4:]
	i := strings.IndexByte(rest, '_')
	if i <= 0 {
		return
	}
	p, err := strconv.ParseUint(rest[:i], 10, 32)
	if err != nil {
		return
	}
	engine = rest[i+1:]
	j := strings.LastIndex(s, "engtype_")
	if j < 0 {
		return
	}
	return uint32(p), engine, s[j+len("engtype_"):], true
}
