//go:build windows

// Package metrics reads live system and per-process statistics from Windows
// using only documented or long-stable APIs (NtQuerySystemInformation, PDH,
// psapi, the Service Control Manager and the registry).
package metrics

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// Proc is one row of the process table.
type Proc struct {
	PID     uint32  `json:"pid"`
	PPID    uint32  `json:"ppid"`
	Name    string  `json:"name"`
	Path    string  `json:"path,omitempty"`
	User    string  `json:"user,omitempty"`
	CPU     float64 `json:"cpu"`     // percent of all logical CPUs
	Mem     uint64  `json:"mem"`     // working set, bytes
	Private uint64  `json:"private"` // private bytes
	Threads uint32  `json:"threads"`
	Handles uint32  `json:"handles"`
	IO      float64 `json:"io"`  // bytes/sec (read+write transfer)
	GPU     float64 `json:"gpu"` // percent, busiest engine type
	Prio    string  `json:"prio"`
	Session uint32  `json:"session"`
	Start   int64   `json:"start"` // unix ms
}

// DiskInfo / NetInfo / GPUInfo are system-wide rates.
type DiskInfo struct {
	ReadBps  float64 `json:"readBps"`
	WriteBps float64 `json:"writeBps"`
	Percent  float64 `json:"percent"`
}

type NetInfo struct {
	RxBps float64 `json:"rxBps"`
	TxBps float64 `json:"txBps"`
}

type GPUInfo struct {
	Percent  float64 `json:"percent"`
	MemBytes uint64  `json:"memBytes"` // dedicated memory in use, all adapters
	OK       bool    `json:"ok"`
}

// Snapshot is everything sampled in one tick.
type Snapshot struct {
	T      int64        `json:"t"` // unix ms
	CPU    float64      `json:"cpu"`
	Cores  []float64    `json:"cores"`
	Mem    MemInfo      `json:"mem"`
	Disk   DiskInfo     `json:"disk"`
	Net    NetInfo      `json:"net"`
	GPU    GPUInfo      `json:"gpu"`
	GPUs   []GPUAdapter `json:"gpus,omitempty"` // one entry per display adapter
	Uptime uint64       `json:"uptime"`
	Procs  []Proc       `json:"procs"`

	CPUFreq    []float64    `json:"cpuFreq,omitempty"`    // MHz per logical processor
	CPUPerfPct float64      `json:"cpuPerfPct,omitempty"` // % Processor Performance
	Thermal    []NamedValue `json:"thermal,omitempty"`    // ACPI thermal zones, deg C
	PowerMeter []NamedValue `json:"power,omitempty"`      // ACPI power meters, W
	Energy     []NamedValue `json:"energy,omitempty"`     // Energy Meter (package + cores), W
	PerDisk    []DiskStat   `json:"perDisk,omitempty"`
	Version    int          `json:"v"`
}

// HistPoint is the compact per-tick record kept for the graphs.
type HistPoint struct {
	T     int64     `json:"t"`
	CPU   float64   `json:"cpu"`
	Mem   float64   `json:"mem"`
	DiskR float64   `json:"dr"`
	DiskW float64   `json:"dw"`
	DiskP float64   `json:"dp"`
	NetRx float64   `json:"rx"`
	NetTx float64   `json:"tx"`
	GPU   float64   `json:"gpu"`
	Cores []float64 `json:"cores"`
	GPUs  []GPUHist `json:"gpus,omitempty"`
}

// GPUHist is one adapter's point on the graphs.
type GPUHist struct {
	ID  string
	Pct float64
	Ded float64 // bytes in use
	Shr float64
	Eng map[string]float64
}

type procKey struct {
	pid    uint32
	create int64
}

type prevProc struct {
	create int64
	cpu    uint64
	io     uint64
}

// Sampler owns the sampling loop, the history ring and the SSE fan-out.
type Sampler struct {
	intervalMs atomic.Int64
	histMax    atomic.Int64
	panics     atomic.Int64 // samples lost to a recovered panic

	mu     sync.RWMutex
	latest *Snapshot
	hist   []HistPoint
	subs   map[chan *Snapshot]struct{}

	ncpu      int
	procBuf   []byte
	prevCores []coreTimes
	prevProcs map[uint32]prevProc
	prevAt    time.Time
	meta      *metaCache
	perf      *perfCounters
}

// NewSampler creates a sampler; Run starts it.
func NewSampler(intervalMs, histSeconds int) *Sampler {
	s := &Sampler{
		subs:      map[chan *Snapshot]struct{}{},
		prevProcs: map[uint32]prevProc{},
		ncpu:      logicalCPUs(),
		meta:      newMetaCache(),
	}
	s.SetInterval(intervalMs)
	s.SetHistorySeconds(histSeconds)
	return s
}

func (s *Sampler) SetInterval(ms int) {
	if ms < 250 {
		ms = 250
	}
	if ms > 10000 {
		ms = 10000
	}
	s.intervalMs.Store(int64(ms))
}

func (s *Sampler) SetHistorySeconds(sec int) {
	if sec < 30 {
		sec = 30
	}
	if sec > 3600 {
		sec = 3600
	}
	s.histMax.Store(int64(sec))
}

func (s *Sampler) NumCPU() int { return s.ncpu }

// Run samples until ctx is cancelled.
func (s *Sampler) Run(ctx context.Context) {
	s.perf = newPerfCounters()
	defer s.perf.close()
	// The first pass only sets baselines (CPU deltas, PDH rates); publishing it
	// would put a false 0 % into every graph and into the sensors' Min column.
	s.safeTick(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(s.intervalMs.Load()) * time.Millisecond):
		}
		s.safeTick(true)
	}
}

// safeTick runs one sample; a panic in a reader (a driver DLL, an odd counter)
// costs that sample, not the whole application.
func (s *Sampler) safeTick(publish bool) {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
		}
	}()
	s.tick(publish)
}

func (s *Sampler) tick(publish bool) {
	now := time.Now()
	snap := &Snapshot{T: now.UnixMilli(), Version: 1}

	// --- CPU (per core + total) ---
	cores, err := readCoreTimes(s.ncpu)
	var totalDelta uint64
	if err == nil {
		snap.Cores = make([]float64, len(cores))
		var busySum, allSum float64
		for i, c := range cores {
			if i < len(s.prevCores) {
				p := s.prevCores[i]
				if c.Kernel+c.User < p.Kernel+p.User || c.Idle < p.Idle {
					continue // counters went backwards (CPU set changed): no delta this tick
				}
				dTotal := (c.Kernel + c.User) - (p.Kernel + p.User)
				dIdle := c.Idle - p.Idle
				totalDelta += dTotal
				if dTotal > 0 {
					busy := float64(dTotal-min64(dIdle, dTotal)) * 100 / float64(dTotal)
					snap.Cores[i] = round1(busy)
					busySum += float64(dTotal - min64(dIdle, dTotal))
					allSum += float64(dTotal)
				}
			}
		}
		if allSum > 0 {
			snap.CPU = round1(busySum * 100 / allSum)
		}
		s.prevCores = cores
	}

	// --- memory ---
	if m, ok := readMemory(); ok {
		snap.Mem = m
	}
	snap.Uptime = uptimeSeconds()

	// --- PDH: disk / network / GPU ---
	var gpuByPID map[uint32]float64
	if s.perf != nil {
		ps := s.perf.read()
		snap.Disk, snap.Net, snap.GPU = ps.Disk, ps.Net, ps.GPU
		snap.CPUFreq, snap.CPUPerfPct = ps.CPUFreq, ps.CPUPerfPct
		snap.Thermal, snap.PowerMeter, snap.PerDisk = ps.Thermal, ps.PowerMeter, ps.PerDisk
		snap.Energy = ps.Energy
		snap.GPUs = ps.GPUs
		gpuByPID = ps.GPUByPID
	}

	// --- processes ---
	raws, perr := readProcesses(&s.procBuf)
	if perr == nil {
		elapsed := now.Sub(s.prevAt).Seconds()
		if s.prevAt.IsZero() || elapsed <= 0 {
			elapsed = 0
		}
		seen := make(map[uint32]prevProc, len(raws))
		procs := make([]Proc, 0, len(raws))
		live := make(map[procKey]struct{}, len(raws))
		for _, r := range raws {
			pp := Proc{
				PID: r.PID, PPID: r.PPID, Name: r.Name,
				Mem: r.WorkingSet, Private: r.Private,
				Threads: r.Threads, Handles: r.Handles,
				Prio: prioName(r.BasePrio), Session: r.Session,
			}
			if r.CreateTime > 0 {
				pp.Start = (r.CreateTime - 116444736000000000) / 10000
			}
			if prev, ok := s.prevProcs[r.PID]; ok && prev.create == r.CreateTime && totalDelta > 0 {
				if r.CPUTime >= prev.cpu {
					pp.CPU = round1(float64(r.CPUTime-prev.cpu) * 100 / float64(totalDelta))
				}
				if elapsed > 0 && r.IOBytes >= prev.io {
					pp.IO = math.Round(float64(r.IOBytes-prev.io) / elapsed)
				}
			}
			seen[r.PID] = prevProc{create: r.CreateTime, cpu: r.CPUTime, io: r.IOBytes}
			if g, ok := gpuByPID[r.PID]; ok {
				pp.GPU = round1(g)
			}
			k := procKey{r.PID, r.CreateTime}
			live[k] = struct{}{}
			if r.PID > 4 {
				pp.Path, pp.User = s.meta.lookup(k)
			} else if r.PID == 4 {
				pp.User = systemAccountName() // localized, to match the other SYSTEM processes
			}
			procs = append(procs, pp)
		}
		s.meta.prune(live)
		s.prevProcs = seen
		snap.Procs = procs
	}
	s.prevAt = now

	// name the processes that use each adapter
	if len(snap.GPUs) > 0 {
		names := make(map[uint32]string, len(snap.Procs))
		for _, p := range snap.Procs {
			names[p.PID] = p.Name
		}
		for i := range snap.GPUs {
			for j := range snap.GPUs[i].Procs {
				pr := &snap.GPUs[i].Procs[j]
				if n := names[pr.PID]; n != "" {
					pr.Name = n
				} else {
					pr.Name = "PID " + itoa(pr.PID)
				}
			}
		}
	}

	hp := HistPoint{
		T: snap.T, CPU: snap.CPU, Mem: round1(snap.Mem.Percent),
		DiskR: snap.Disk.ReadBps, DiskW: snap.Disk.WriteBps, DiskP: snap.Disk.Percent,
		NetRx: snap.Net.RxBps, NetTx: snap.Net.TxBps, GPU: snap.GPU.Percent,
		Cores: snap.Cores,
	}
	for _, g := range snap.GPUs {
		gh := GPUHist{ID: g.ID, Pct: g.Pct, Ded: float64(g.DedicatedUsed), Shr: float64(g.SharedUsed),
			Eng: make(map[string]float64, len(g.Engines))}
		for _, e := range g.Engines {
			gh.Eng[e.Name] = e.Pct
		}
		hp.GPUs = append(hp.GPUs, gh)
	}

	if !publish {
		return // baseline pass: deltas are primed, nothing is shown yet
	}
	s.mu.Lock()
	s.latest = snap
	// If the clock stepped backwards (NTP, manual change), drop points that now
	// lie in the future, or they would stay in the graphs until time caught up.
	for len(s.hist) > 0 && s.hist[len(s.hist)-1].T > snap.T {
		s.hist = s.hist[:len(s.hist)-1]
	}
	s.hist = append(s.hist, hp)
	// history length is measured in seconds of wall time, whatever the interval
	cut := snap.T - s.histMax.Load()*1000
	i := 0
	for i < len(s.hist) && s.hist[i].T < cut {
		i++
	}
	if i > 0 {
		s.hist = append([]HistPoint(nil), s.hist[i:]...)
	}
	for ch := range s.subs {
		select {
		case ch <- snap:
		default: // slow consumer: drop this frame
		}
	}
	s.mu.Unlock()
}

// Latest returns the most recent snapshot (nil before the first tick).
func (s *Sampler) Latest() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// History returns a copy of the history ring.
func (s *Sampler) History() []HistPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]HistPoint(nil), s.hist...)
}

// Subscribe returns a channel of snapshots (one per tick) and a cancel function.
// Snapshots are shared and must be treated as read-only.
func (s *Sampler) Subscribe() (<-chan *Snapshot, func()) {
	ch := make(chan *Snapshot, 2)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Subscribers reports the number of live stream consumers.
func (s *Sampler) Subscribers() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// ---- helpers ---------------------------------------------------------------

func prioName(base int32) string {
	switch {
	case base <= 4:
		return "Idle"
	case base <= 6:
		return "BelowNormal"
	case base <= 8:
		return "Normal"
	case base <= 10:
		return "AboveNormal"
	case base <= 13:
		return "High"
	default:
		return "Realtime"
	}
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

var (
	sysNameOnce sync.Once
	sysName     = `NT AUTHORITY\SYSTEM`
)

// systemAccountName is the localized DOMAIN\name of LocalSystem (S-1-5-18),
// e.g. NT-AUTORITÄT\SYSTEM on German Windows.
func systemAccountName() string {
	sysNameOnce.Do(func() {
		sid, err := windows.StringToSid("S-1-5-18")
		if err != nil {
			return
		}
		if acct, dom, _, err := sid.LookupAccount(""); err == nil && acct != "" {
			sysName = dom + `\` + acct
		}
	})
	return sysName
}
