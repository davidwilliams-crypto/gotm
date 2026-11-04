//go:build windows

package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// GPUEngine is one hardware engine of an adapter (3D, Copy, Video Decode …).
type GPUEngine struct {
	Name string
	Pct  float64
}

// GPUProcMem is one process's use of an adapter's memory and engines.
type GPUProcMem struct {
	PID       uint32
	Name      string
	Dedicated uint64
	Shared    uint64
	Pct       float64 // busiest engine type of this process on this adapter
}

// GPUAdapter is one display adapter with everything we can learn about it.
type GPUAdapter struct {
	ID       string // PDH LUID key, "0X00000000_0X00015516"
	Name     string
	Vendor   string
	Software bool

	DedicatedTotal uint64
	SharedTotal    uint64
	DedicatedUsed  uint64
	SharedUsed     uint64

	Pct                               float64 // busiest engine
	HasUtilization                    bool
	HasDedicatedUsage, HasSharedUsage bool
	Engines                           []GPUEngine
	Procs                             []GPUProcMem // by dedicated+shared, descending

	Driver     string
	DriverDate string
	Location   string // bus:device.function

	// vendor sensors (NVML for NVIDIA, ADL for AMD); Has* says whether the value is real
	HasTemp, HasPower, HasClock, HasFan bool
	HasMemClock                         bool
	TempC, PowerW, PowerLimitW          float64
	ClockMHz, MemClockMHz               float64
	FanPct, FanRPM                      float64
	HasHotspot                          bool
	HotspotC                            float64 // AMD: hottest point on the die
	HasSocTemp                          bool
	SocTempC                            float64 // AMD APU: the SoC/uncore temperature
	PowerSource                         string  // AMD: board, asic, gfx or soc (what PowerW covers)
	Integrated                          bool    // an APU's graphics (shares the CPU's power budget)
	HasVoltage                          bool
	VoltageV                            float64
	HasDriverPct                        bool
	DriverPct                           float64 // the driver's own busy figure
	// APUs report the CPU side of the package through the display driver too.
	HasCPUTemp, HasCPUPower, HasCPUClock bool
	CPUTempC, CPUPowerW, CPUClockMHz     float64
}

// gpuStatic is what rarely changes: DXGI identity, memory totals, driver.
type gpuStatic struct {
	id       string
	name     string
	vendorID uint32
	deviceID uint32
	software bool
	ded, shr uint64
	driver   string
	date     string
	location string
	bus      int // PCI bus number, -1 when unknown
}

func vendorName(id uint32) string {
	switch id {
	case 0x1002:
		return "AMD"
	case 0x10DE:
		return "NVIDIA"
	case 0x8086:
		return "Intel"
	case 0x1414:
		return "Microsoft"
	case 0x102b:
		return "Matrox"
	case 0x1a03:
		return "ASPEED"
	case 0x15ad:
		return "VMware"
	}
	return ""
}

// UtilizationAvailable distinguishes measured idle from a detected adapter
// whose installed driver supplies no utilization counters.
func (g GPUAdapter) UtilizationAvailable() bool {
	return g.HasUtilization || len(g.Engines) > 0
}

func validGPUCounter(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// ---- DXGI enumeration (vtable calls, no cgo) -----------------------------------

var (
	dxgiDLL              = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFact1  = dxgiDLL.NewProc("CreateDXGIFactory1")
	iidDXGIFactory1      = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	dxgiFactoryEnumAd1   = 12 // IDXGIFactory1::EnumAdapters1
	dxgiAdapter1GetDesc1 = 10 // IDXGIAdapter1::GetDesc1
)

// dxgiDesc1 is DXGI_ADAPTER_DESC1 (x64): 312 bytes.
type dxgiDesc1 struct {
	Description     [128]uint16
	VendorID        uint32
	DeviceID        uint32
	SubSysID        uint32
	Revision        uint32
	DedicatedVideo  uintptr
	DedicatedSystem uintptr
	SharedSystem    uintptr
	LuidLow         uint32
	LuidHigh        int32
	Flags           uint32
}

func enumerateDXGIGPUs() []gpuStatic {
	if procCreateDXGIFact1.Find() != nil {
		return nil
	}
	var fac uintptr
	hr, _, _ := procCreateDXGIFact1.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&fac)))
	if int32(hr) < 0 || fac == 0 {
		return nil
	}
	defer vcall(fac, 2) // Release

	var out []gpuStatic
	for i := 0; i < 16; i++ {
		var ad uintptr
		if int32(vcall(fac, dxgiFactoryEnumAd1, uintptr(i), uintptr(unsafe.Pointer(&ad)))) < 0 || ad == 0 {
			break
		}
		var d dxgiDesc1
		hr := int32(vcall(ad, dxgiAdapter1GetDesc1, uintptr(unsafe.Pointer(&d))))
		vcall(ad, 2) // Release
		if hr < 0 {
			continue
		}
		st := gpuStatic{
			id:       fmt.Sprintf("0X%08X_0X%08X", uint32(d.LuidHigh), d.LuidLow),
			name:     strings.TrimSpace(windows.UTF16ToString(d.Description[:])),
			vendorID: d.VendorID, deviceID: d.DeviceID,
			software: d.Flags&2 != 0,
			ded:      uint64(d.DedicatedVideo),
			shr:      uint64(d.SharedSystem),
		}
		// enumerateGPUs attaches exact devnode metadata and the LUID's PCI
		// address. Avoid registry/model guesses that it would discard anyway.
		st.bus = -1
		out = append(out, st)
	}
	return out
}

// driverInfo reads the display-class registry key that matches the adapter.
func driverInfo(ven, dev uint32, name string) (version, date string) {
	const cls = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, cls, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	subs, _ := k.ReadSubKeyNames(-1)
	want := strings.ToUpper(fmt.Sprintf("VEN_%04X&DEV_%04X", ven, dev))
	var byName [2]string
	for _, s := range subs {
		sk, err := registry.OpenKey(k, s, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		match, _, _ := sk.GetStringValue("MatchingDeviceId")
		desc, _, _ := sk.GetStringValue("DriverDesc")
		v, _, _ := sk.GetStringValue("DriverVersion")
		dt, _, _ := sk.GetStringValue("DriverDate")
		sk.Close()
		if v == "" {
			continue
		}
		if strings.Contains(strings.ToUpper(match), want) {
			return v, dt
		}
		if byName[0] == "" && desc != "" && strings.EqualFold(desc, name) {
			byName = [2]string{v, dt}
		}
	}
	return byName[0], byName[1]
}

// pciLocation returns "bus:device.function" in hex, from Enum\PCI LocationInformation.
func pciLocation(ven, dev uint32) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\PCI`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer k.Close()
	subs, _ := k.ReadSubKeyNames(-1)
	prefix := strings.ToUpper(fmt.Sprintf("VEN_%04X&DEV_%04X", ven, dev))
	for _, s := range subs {
		if !strings.HasPrefix(strings.ToUpper(s), prefix) {
			continue
		}
		dk, err := registry.OpenKey(k, s, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		insts, _ := dk.ReadSubKeyNames(-1)
		dk.Close()
		for _, in := range insts {
			ik, err := registry.OpenKey(k, s+`\`+in, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			li, _, _ := ik.GetStringValue("LocationInformation")
			ik.Close()
			if loc := parseLocation(li); loc != "" {
				return loc
			}
		}
	}
	return ""
}

// parseLocation understands "...;PCI bus %1, device %2, function %3;(17,0,0)".
func parseLocation(s string) string {
	i := strings.LastIndex(s, "(")
	j := strings.LastIndex(s, ")")
	if i < 0 || j < i {
		return ""
	}
	p := strings.Split(s[i+1:j], ",")
	if len(p) != 3 {
		return ""
	}
	n := make([]int, 3)
	for k := range p {
		v, err := strconv.Atoi(strings.TrimSpace(p[k]))
		if err != nil {
			return ""
		}
		n[k] = v
	}
	return fmt.Sprintf("%02X:%02X.%X", n[0], n[1], n[2])
}

// ---- NVML (NVIDIA, public API shipped with the driver) -----------------------------

type nvmlLib struct {
	dll                                   *windows.LazyDLL
	getCount, getHandle, getName          *windows.LazyProc
	getTemp, getPower, getLimit, getClock *windows.LazyProc
	getFan                                *windows.LazyProc
	getPCI                                *windows.LazyProc
	ok                                    bool
}

type nvmlDev struct {
	handle uintptr
	name   string
	bus    int
}

type vendorGPUIdentity struct {
	bus  int
	name string
}

func vendorGPUIdentities(static []gpuStatic, vendor uint32) []vendorGPUIdentity {
	var out []vendorGPUIdentity
	for _, st := range static {
		if st.vendorID == vendor && !st.software {
			out = append(out, vendorGPUIdentity{st.bus, st.name})
		}
	}
	return out
}

// matchVendorGPU requires uniqueness on both sides. Bus numbers alone are not
// globally unique across PCI domains, and enumeration order is never identity.
// With no unambiguous match, native Windows queries still use the exact LUID.
func matchVendorGPU(bus int, name string, local, remote []vendorGPUIdentity) int {
	if bus >= 0 {
		lc, rc, found := 0, 0, -1
		for _, d := range local {
			if d.bus == bus {
				lc++
			}
		}
		for i, d := range remote {
			if d.bus == bus {
				rc++
				found = i
			}
		}
		if lc == 1 && rc == 1 {
			return found
		}
	}
	name = shortName(name)
	if name == "" {
		return -1
	}
	lc, rc, found := 0, 0, -1
	for _, d := range local {
		if strings.EqualFold(shortName(d.name), name) {
			lc++
		}
	}
	for i, d := range remote {
		if strings.EqualFold(shortName(d.name), name) {
			rc++
			found = i
		}
	}
	if lc == 1 && rc == 1 {
		// A friendly-name match cannot override contradictory PCI evidence.
		if bus >= 0 && remote[found].bus >= 0 && bus != remote[found].bus {
			return -1
		}
		return found
	}
	return -1
}

func openNVML() *nvmlLib {
	l := &nvmlLib{dll: windows.NewLazySystemDLL("nvml.dll")}
	if l.dll.Load() != nil {
		return l
	}
	init := l.dll.NewProc("nvmlInit_v2")
	if r, _, _ := init.Call(); r != 0 {
		return l
	}
	l.getCount = l.dll.NewProc("nvmlDeviceGetCount_v2")
	l.getHandle = l.dll.NewProc("nvmlDeviceGetHandleByIndex_v2")
	l.getName = l.dll.NewProc("nvmlDeviceGetName")
	l.getTemp = l.dll.NewProc("nvmlDeviceGetTemperature")
	l.getPower = l.dll.NewProc("nvmlDeviceGetPowerUsage")
	l.getLimit = l.dll.NewProc("nvmlDeviceGetEnforcedPowerLimit")
	l.getClock = l.dll.NewProc("nvmlDeviceGetClockInfo")
	l.getFan = l.dll.NewProc("nvmlDeviceGetFanSpeed")
	l.getPCI = l.dll.NewProc("nvmlDeviceGetPciInfo_v3")
	for _, p := range []*windows.LazyProc{l.getCount, l.getHandle, l.getName, l.getTemp, l.getPower, l.getLimit, l.getClock, l.getFan, l.getPCI} {
		if p.Find() != nil {
			return l // an old or partial NVML: no NVIDIA sensors rather than a crash
		}
	}
	l.ok = true
	return l
}

func (l *nvmlLib) devices() []nvmlDev {
	if l == nil || !l.ok {
		return nil
	}
	var n uint32
	if r, _, _ := l.getCount.Call(uintptr(unsafe.Pointer(&n))); r != 0 {
		return nil
	}
	var out []nvmlDev
	for i := uint32(0); i < n && i < 16; i++ {
		var h uintptr
		if r, _, _ := l.getHandle.Call(uintptr(i), uintptr(unsafe.Pointer(&h))); r != 0 {
			continue
		}
		var nm [96]byte
		l.getName.Call(h, uintptr(unsafe.Pointer(&nm[0])), uintptr(len(nm)))
		d := nvmlDev{handle: h, name: strings.TrimRight(string(nm[:]), "\x00"), bus: -1}
		// nvmlPciInfo_t: busIdLegacy[16], domain, bus, device, pciDeviceId, subsystem, busId[32]
		var pci [80]byte
		if r, _, _ := l.getPCI.Call(h, uintptr(unsafe.Pointer(&pci[0]))); r == 0 {
			d.bus = int(*(*uint32)(unsafe.Pointer(&pci[20])))
		}
		out = append(out, d)
	}
	return out
}

func (l *nvmlLib) fill(d nvmlDev, a *GPUAdapter) {
	if l == nil || !l.ok {
		return
	}
	var u uint32
	if r, _, _ := l.getTemp.Call(d.handle, 0, uintptr(unsafe.Pointer(&u))); r == 0 {
		a.TempC, a.HasTemp = float64(u), true
	}
	if r, _, _ := l.getPower.Call(d.handle, uintptr(unsafe.Pointer(&u))); r == 0 {
		a.PowerW, a.HasPower = float64(u)/1000, true
	}
	if r, _, _ := l.getLimit.Call(d.handle, uintptr(unsafe.Pointer(&u))); r == 0 {
		a.PowerLimitW = float64(u) / 1000
	}
	if r, _, _ := l.getClock.Call(d.handle, 0, uintptr(unsafe.Pointer(&u))); r == 0 { // graphics
		a.ClockMHz, a.HasClock = float64(u), true
	}
	if r, _, _ := l.getClock.Call(d.handle, 2, uintptr(unsafe.Pointer(&u))); r == 0 { // memory
		a.MemClockMHz, a.HasMemClock = float64(u), true
	}
	if r, _, _ := l.getFan.Call(d.handle, uintptr(unsafe.Pointer(&u))); r == 0 {
		a.FanPct, a.HasFan = float64(u), true
	}
}

// close ends the NVML session (only when the entry point exists in this
// driver) so a stopped sampler leaves no session in the driver.
func (l *nvmlLib) close() {
	if l == nil || !l.ok || l.dll == nil {
		return
	}
	if shutdown := l.dll.NewProc("nvmlShutdown"); shutdown.Find() == nil {
		_, _, _ = shutdown.Call()
	}
	l.ok = false
}

// nvidiaMarketingVersion turns Windows' 32.0.16.1714 into NVIDIA's 617.14.
func nvidiaMarketingVersion(v string) string {
	p := strings.Split(v, ".")
	if len(p) != 4 {
		return ""
	}
	digits := p[2] + p[3]
	if len(digits) < 5 {
		return ""
	}
	digits = digits[len(digits)-5:]
	return strings.TrimLeft(digits[:3], "0") + "." + digits[3:]
}

// ---- PDH assembly -------------------------------------------------------------------

type gpuInst struct {
	pid  uint32
	luid string
	eng  int
	typ  string
}

// luidKey extracts "0X........_0X........" from an instance name.
func luidKey(inst string) string {
	return strings.ToUpper(rawLUIDKey(inst))
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
	var g gpuInst
	g.luid = luidKey(inst)
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

// buildGPUs merges static identity with one PDH collection. eng are the
// "GPU Engine" utilisations; ded/shr the per-adapter memory; pded/pshr the
// per-process memory. Only adapters PDH reports (or DXGI lists) appear.
func buildGPUs(static []gpuStatic, eng, ded, shr, pded, pshr []PDHInstanceValue) []GPUAdapter {
	byID := map[string]*GPUAdapter{}
	var order []string
	get := func(id string) *GPUAdapter {
		if a, ok := byID[id]; ok {
			return a
		}
		a := &GPUAdapter{ID: id, Name: "GPU " + id}
		byID[id] = a
		order = append(order, id)
		return a
	}
	known := map[string]bool{}
	for _, st := range static {
		known[st.id] = true
		a := get(st.id)
		a.Name, a.Vendor, a.Software = st.name, vendorName(st.vendorID), st.software
		a.DedicatedTotal, a.SharedTotal = st.ded, st.shr
		a.Driver, a.DriverDate, a.Location = st.driver, st.date, st.location
		if a.Vendor == "NVIDIA" {
			if m := nvidiaMarketingVersion(st.driver); m != "" {
				a.Driver = m + " (" + st.driver + ")"
			}
		}
	}

	// engines: sum across processes per (adapter, engine index, type), cap 100
	type ek struct {
		luid string
		eng  int
		typ  string
	}
	sums := map[ek]float64{}
	procPct := map[string]map[uint32]map[string]float64{} // luid -> pid -> type -> pct
	for _, it := range eng {
		g, ok := parseGPUInst(it.Instance)
		if !ok || g.typ == "" || !validGPUCounter(it.Value) {
			continue
		}
		get(g.luid).HasUtilization = true
		sums[ek{g.luid, g.eng, g.typ}] += it.Value
		if g.pid != 0 {
			if procPct[g.luid] == nil {
				procPct[g.luid] = map[uint32]map[string]float64{}
			}
			if procPct[g.luid][g.pid] == nil {
				procPct[g.luid][g.pid] = map[string]float64{}
			}
			procPct[g.luid][g.pid][g.typ] += it.Value
		}
	}
	type eng3 struct {
		idx  int
		name string
		pct  float64
	}
	perAd := map[string][]eng3{}
	for k, v := range sums {
		perAd[k.luid] = append(perAd[k.luid], eng3{k.eng, prettyEngine(k.typ), math.Min(v, 100)})
	}
	for id, list := range perAd {
		sort.Slice(list, func(i, j int) bool {
			ri, rj := engineRank(list[i].name), engineRank(list[j].name)
			if ri != rj {
				return ri < rj
			}
			if list[i].name != list[j].name {
				return list[i].name < list[j].name
			}
			return list[i].idx < list[j].idx
		})
		count := map[string]int{}
		for _, e := range list {
			count[e.name]++
		}
		seen := map[string]int{}
		a := get(id)
		for _, e := range list {
			name := e.name
			if count[name] > 1 {
				name = fmt.Sprintf("%s %d", name, seen[name])
			}
			seen[e.name]++
			a.Engines = append(a.Engines, GPUEngine{Name: name, Pct: round1(e.pct)})
			if e.pct > a.Pct {
				a.Pct = round1(e.pct)
			}
		}
	}

	for _, it := range ded {
		if id := luidKey(it.Instance); id != "" && validGPUCounter(it.Value) && it.Value < 1<<60 {
			get(id).DedicatedUsed = uint64(it.Value)
			get(id).HasDedicatedUsage = true
		}
	}
	for _, it := range shr {
		if id := luidKey(it.Instance); id != "" && validGPUCounter(it.Value) && it.Value < 1<<60 {
			get(id).SharedUsed = uint64(it.Value)
			get(id).HasSharedUsage = true
		}
	}

	// per-process memory
	type pm struct{ ded, shr uint64 }
	mem := map[string]map[uint32]*pm{}
	add := func(list []PDHInstanceValue, dedicated bool) {
		for _, it := range list {
			g, ok := parseGPUInst(it.Instance)
			if !ok || !strings.HasPrefix(it.Instance, "pid_") {
				continue
			}
			if mem[g.luid] == nil {
				mem[g.luid] = map[uint32]*pm{}
			}
			p := mem[g.luid][g.pid]
			if p == nil {
				p = &pm{}
				mem[g.luid][g.pid] = p
			}
			if validGPUCounter(it.Value) && it.Value > 0 && it.Value < 1<<60 {
				if dedicated {
					p.ded += uint64(it.Value)
				} else {
					p.shr += uint64(it.Value)
				}
			}
		}
	}
	add(pded, true)
	add(pshr, false)
	for id, procs := range mem {
		a := get(id)
		for pid, m := range procs {
			if m.ded == 0 && m.shr == 0 && procPct[id][pid] == nil {
				continue
			}
			pr := GPUProcMem{PID: pid, Dedicated: m.ded, Shared: m.shr}
			for _, v := range procPct[id][pid] {
				if v > pr.Pct {
					pr.Pct = math.Min(round1(v), 100)
				}
			}
			a.Procs = append(a.Procs, pr)
		}
	}
	for id, byPID := range procPct { // engine-only processes with no memory instance
		a := get(id)
		have := map[uint32]bool{}
		for _, p := range a.Procs {
			have[p.PID] = true
		}
		for pid, types := range byPID {
			if have[pid] {
				continue
			}
			var best float64
			for _, v := range types {
				best = math.Max(best, v)
			}
			if best > 0 {
				a.Procs = append(a.Procs, GPUProcMem{PID: pid, Pct: math.Min(round1(best), 100)})
			}
		}
	}

	out := make([]GPUAdapter, 0, len(order))
	for _, id := range order {
		a := byID[id]
		if a.Software || !known[id] {
			continue // software rasterizer, or a compute-only device (NPU) with no DXGI record
		}
		sort.Slice(a.Procs, func(i, j int) bool {
			ti := a.Procs[i].Dedicated + a.Procs[i].Shared
			tj := a.Procs[j].Dedicated + a.Procs[j].Shared
			if ti != tj {
				return ti > tj
			}
			return a.Procs[i].Pct > a.Procs[j].Pct
		})
		out = append(out, *a)
	}
	return out
}

// gpuReader owns the slow-changing parts of the GPU picture.
type gpuReader struct {
	static        []gpuStatic
	nv            *nvmlLib
	nvDevs        []nvmlDev
	adl           *adlLib
	seen          map[string]bool
	last          map[string]GPUAdapter // last vendor sensor values per adapter ID
	lastAt        map[string]time.Time  // when each cached reading was taken
	lastDiscovery time.Time
}

// sensorStaleAfter bounds how long an unpolled vendor reading (a sleeping
// dGPU's last values) is still shown: past this it is dropped rather than
// displayed as if it were live.
const sensorStaleAfter = 2 * time.Minute

// cachedSensors replays the last reading for adapters that are not being
// polled right now; readings older than sensorStaleAfter are dropped instead
// of being shown as if they were live.
func (r *gpuReader) cachedSensors(a *GPUAdapter) {
	if t, ok := r.lastAt[a.ID]; !ok || time.Since(t) > sensorStaleAfter {
		return // no (recent enough) reading: show nothing rather than stale data
	}
	copySensors(a, r.last[a.ID])
}

// close releases the vendor library sessions (the ADL context, the NVML
// session) so a stopped sampler does not leave them alive in the driver.
// Called from the sampler goroutine, the only thread that touches the reader.
func (r *gpuReader) close() {
	if r == nil {
		return
	}
	r.nv.close()
	r.adl.close()
}

func newGPUReader() *gpuReader {
	r := &gpuReader{seen: map[string]bool{}, last: map[string]GPUAdapter{}, lastAt: map[string]time.Time{}}
	r.static = enumerateGPUs()
	r.lastDiscovery = time.Now()
	for _, s := range r.static {
		r.seen[s.id] = true
		r.seen[strings.ToLower(s.id)] = true
	}
	r.nv = openNVML()
	r.nvDevs = r.nv.devices()
	r.adl = openADL()
	return r
}

// refreshGPUDiscovery coalesces a periodic refresh and newly observed PDH
// devices into one inventory query. Unknown compute devices are remembered
// until the next periodic refresh, without copying large counter arrays.
func (r *gpuReader) refreshGPUDiscovery(now time.Time, eng, ded []PDHInstanceValue, discover func() []gpuStatic) {
	refresh := now.Sub(r.lastDiscovery) >= 30*time.Second
	for _, values := range [][]PDHInstanceValue{ded, eng} {
		for _, it := range values {
			id := rawLUIDKey(it.Instance)
			if id == "" || r.seen[id] {
				continue
			}
			if r.seen[strings.ToUpper(id)] {
				r.seen[id] = true
			} else {
				refresh = true
			}
		}
	}
	if !refresh {
		return
	}
	r.static, r.lastDiscovery = discover(), now
	r.seen = make(map[string]bool, 2*len(r.static))
	for _, st := range r.static {
		r.seen[st.id], r.seen[strings.ToLower(st.id)] = true, true
	}
	for _, values := range [][]PDHInstanceValue{ded, eng} {
		for _, it := range values {
			if id := rawLUIDKey(it.Instance); id != "" {
				r.seen[id] = true
			}
		}
	}
}

// build combines one PDH sample with static data and vendor sensors.
func (r *gpuReader) build(eng, ded, shr, pded, pshr []PDHInstanceValue) []GPUAdapter {
	if r == nil {
		return nil
	}
	r.refreshGPUDiscovery(time.Now(), eng, ded, enumerateGPUs)
	list := buildGPUs(r.static, eng, ded, shr, pded, pshr)
	busOf := map[string]int{}
	for _, st := range r.static {
		busOf[st.id] = st.bus
	}
	wanted := gpuSensorsWanted.Load()
	var nvIdx []int
	for i := range list {
		switch list[i].Vendor {
		case "NVIDIA":
			nvIdx = append(nvIdx, i)
		case "AMD":
			// An APU's graphics is the CPU's own silicon: reading it wakes
			// nothing, and its table may carry the CPU temperature the CPU
			// card shows all the time. A discrete card waits for a GPU view.
			ad, found := r.adl.find(busOf[list[i].ID], list[i].Name, r.static...)
			if !found {
				continue
			}
			list[i].Integrated = r.adl.isAPU(ad)
			if !wanted && !list[i].Integrated {
				continue
			}
			r.adl.fill(ad, &list[i])
			// ADL only assigns positive, validated memory clocks. Availability
			// is independent of whether its graphics-clock query succeeded.
			list[i].HasMemClock = list[i].MemClockMHz > 0
		}
	}
	var nvLocal []vendorGPUIdentity
	var nvRemote []vendorGPUIdentity
	if wanted && len(nvIdx) > 0 {
		nvLocal = vendorGPUIdentities(r.static, 0x10de)
		for _, d := range r.nvDevs {
			nvRemote = append(nvRemote, vendorGPUIdentity{d.bus, d.name})
		}
	}
	for _, i := range nvIdx {
		if !wanted {
			continue
		}
		if d := matchVendorGPU(busOf[list[i].ID], list[i].Name, nvLocal, nvRemote); d >= 0 && r.nv != nil {
			r.nv.fill(r.nvDevs[d], &list[i])
		}
	}
	for i := range list {
		g := &list[i]
		if wanted {
			fillNativeGPUSensors(g)
		}
		if wanted || g.Integrated {
			// Cache the completed sample once, including native fallback values.
			// A failed fresh read stays unavailable rather than replaying it as live.
			r.last[g.ID], r.lastAt[g.ID] = *g, time.Now()
		} else {
			// This also covers AMD/NVIDIA adapters without their vendor library.
			r.cachedSensors(g)
		}
	}
	return list
}

// ---- PCI address of an adapter, from the kernel graphics subsystem ------------

var (
	gdi32GPU                      = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTOpenAdapterFromLuid = gdi32GPU.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo    = gdi32GPU.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter        = gdi32GPU.NewProc("D3DKMTCloseAdapter")
)

const kmtqaiTypeAdapterAddress = 6 // KMTQAITYPE_ADAPTERADDRESS

// adapterAddress returns the PCI bus/device/function of the adapter with this
// LUID (the same query Task Manager uses for "Physical location").
func adapterAddress(luidLow uint32, luidHigh int32) (bus, dev, fn uint32, ok bool) {
	for _, p := range []*windows.LazyProc{procD3DKMTOpenAdapterFromLuid, procD3DKMTQueryAdapterInfo, procD3DKMTCloseAdapter} {
		if p.Find() != nil {
			return 0, 0, 0, false
		}
	}
	var open struct {
		LuidLow  uint32
		LuidHigh int32
		Adapter  uint32
	}
	open.LuidLow, open.LuidHigh = luidLow, luidHigh
	if r, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); r != 0 {
		return 0, 0, 0, false
	}
	defer func() {
		closeArg := open.Adapter
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&closeArg)))
	}()
	var addr struct{ Bus, Device, Function uint32 }
	q := struct {
		Adapter uint32
		Type    int32
		Data    unsafe.Pointer
		Size    uint32
	}{open.Adapter, kmtqaiTypeAdapterAddress, unsafe.Pointer(&addr), uint32(unsafe.Sizeof(addr))}
	if r, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); r != 0 {
		return 0, 0, 0, false
	}
	return addr.Bus, addr.Device, addr.Function, true
}

// gpuSensorsWanted gates NVML: polling it every second can keep a laptop's
// discrete GPU awake (out of its low-power state), so sensors are read only
// while a GPU view is on screen.
var gpuSensorsWanted atomic.Bool

// SetGPUSensorsWanted tells the sampler whether GPU clock/power/temperature
// are being displayed.
func SetGPUSensorsWanted(on bool) { gpuSensorsWanted.Store(on) }

func copySensors(dst *GPUAdapter, src GPUAdapter) {
	dst.HasMemClock = src.HasMemClock
	dst.HasTemp, dst.HasPower, dst.HasClock, dst.HasFan = src.HasTemp, src.HasPower, src.HasClock, src.HasFan
	dst.TempC, dst.PowerW, dst.PowerLimitW = src.TempC, src.PowerW, src.PowerLimitW
	dst.ClockMHz, dst.MemClockMHz, dst.FanPct, dst.FanRPM = src.ClockMHz, src.MemClockMHz, src.FanPct, src.FanRPM
	dst.HasHotspot, dst.HotspotC = src.HasHotspot, src.HotspotC
	dst.HasSocTemp, dst.SocTempC, dst.PowerSource = src.HasSocTemp, src.SocTempC, src.PowerSource
	dst.Integrated = dst.Integrated || src.Integrated
	dst.HasVoltage, dst.VoltageV = src.HasVoltage, src.VoltageV
	dst.HasDriverPct, dst.DriverPct = src.HasDriverPct, src.DriverPct
	dst.HasCPUTemp, dst.HasCPUPower, dst.HasCPUClock = src.HasCPUTemp, src.HasCPUPower, src.HasCPUClock
	dst.CPUTempC, dst.CPUPowerW, dst.CPUClockMHz = src.CPUTempC, src.CPUPowerW, src.CPUClockMHz
}
