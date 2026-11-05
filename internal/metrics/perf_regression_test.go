//go:build windows

package metrics

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ---- sensor tree: tracking stays exact while the page is hidden ------------------

// Without page tracking (SetDetailWanted never called) every update formats.
func TestSensorTreeFormattedWithoutPageTracking(t *testing.T) {
	h := NewSensorHubDeferred()
	h.slowAt, h.powerAt = farFuture, farFuture
	h.Update(&Snapshot{CPU: 10}, HostInfo{})
	rows := h.Update(&Snapshot{CPU: 70}, HostInfo{})
	if r, _ := rowByID(rows, "cpu.load.total"); r.Value != "70.0 %" || r.Min != "10.0 %" {
		t.Fatalf("stale tree: %+v", r)
	}
}

func TestSensorMinMaxTrackedWhileHidden(t *testing.T) {
	h := NewSensorHubDeferred()
	h.slowAt, h.powerAt = farFuture, farFuture
	h.SetDetailWanted(false) // another page is on screen
	host := HostInfo{Hostname: "pc", CPUName: "cpu"}
	first := h.Update(&Snapshot{CPU: 10}, host) // nothing built yet: a tree is formatted
	if r, ok := rowByID(first, "cpu.load.total"); !ok || r.Value != "10.0 %" {
		t.Fatalf("first tree: %+v %v", r, ok)
	}
	for _, v := range []float64{90, 50} {
		rows := h.Update(&Snapshot{CPU: v}, host)
		if len(rows) == 0 {
			t.Fatal("hidden update returned no tree; the page would show nothing when opened")
		}
	}
	h.SetDetailWanted(true)
	rows := h.Update(&Snapshot{CPU: 30}, host)
	r, ok := rowByID(rows, "cpu.load.total")
	if !ok || r.Value != "30.0 %" || r.Min != "10.0 %" || r.Max != "90.0 %" {
		t.Fatalf("shown page lost hidden extrema or got a stale tree: %+v", r)
	}
	// Rows already handed out are never rewritten.
	if r0, _ := rowByID(first, "cpu.load.total"); r0.Value != "10.0 %" || r0.Max != "10.0 %" {
		t.Fatalf("published rows mutated: %+v", r0)
	}
}

func TestSensorHiddenUpdateAllocations(t *testing.T) {
	h := NewSensorHubDeferred()
	h.slowAt, h.powerAt = farFuture, farFuture
	h.drives = []DriveHealth{{Index: 0, Model: "disk", Bus: "NVMe"}}
	h.SetDetailWanted(false)
	s := sensorBenchSnapshot()
	host := HostInfo{Hostname: "pc", CPUName: "cpu"}
	h.Update(s, host)
	h.Update(s, host)
	if n := testing.AllocsPerRun(50, func() { s.CPU++; h.Update(s, host) }); n > 1 {
		t.Fatalf("hidden sensor update allocates %.0f times", n)
	}
}

// The UI paints with CPUTemperature; it must not wait for Update's work.
func TestCPUTemperatureDuringUpdate(t *testing.T) {
	h := NewSensorHubDeferred()
	h.updMu.Lock() // an Update in progress
	done := make(chan struct{})
	go func() { h.CPUTemperature(); _ = h.Topology(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("paint accessors waited for the tree builder")
	}
	h.updMu.Unlock()
}

func TestRyzenTemperatureAccessor(t *testing.T) {
	r := newRyzenSDKWithMarker(regMarker{})
	if _, ok := r.Temperature(); ok {
		t.Fatal("temperature before any reading")
	}
	r.mu.Lock()
	r.have, r.last = true, RyzenReading{TempC: 61.5, Cores: make([]RyzenCore, 64)}
	r.mu.Unlock()
	if v, ok := r.Temperature(); !ok || v != 61.5 {
		t.Fatalf("temperature %v %v", v, ok)
	}
	if n := testing.AllocsPerRun(100, func() { r.Temperature() }); n != 0 {
		t.Fatalf("Temperature allocates %.0f times", n)
	}
}

// ---- AMD SDK: Stop never blocks the caller -------------------------------------------

func TestRyzenStopReturnsWhileWorkerBusy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	prev := ryzenRun
	ryzenRun = func(r *RyzenSDK, stop <-chan struct{}) {
		close(entered)
		<-release // a hung signature check or driver call
		r.failFor(stop, "late failure from a stopped session")
	}
	t.Cleanup(func() { ryzenRun = prev })
	r := newRyzenSDKWithMarker(regMarker{})
	r.Start()
	<-entered
	start := time.Now()
	r.Stop()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Stop blocked %v", d)
	}
	if st, _ := r.State(); st != RyzenOff {
		t.Fatalf("state after Stop = %v", st)
	}
	if r.Wait(50 * time.Millisecond) {
		t.Fatal("Wait reported a still-running worker as finished")
	}
	close(release)
	if !r.Wait(2 * time.Second) {
		t.Fatal("worker did not finish")
	}
	if st, _ := r.State(); st != RyzenOff {
		t.Fatalf("stopped session published state %v", st)
	}
	r.Stop() // repeated Stop is harmless
}

func TestRyzenStopWhileQueuedBehindPreviousSession(t *testing.T) {
	ryzenSession <- struct{}{} // an earlier session is still releasing the SDK
	defer func() { <-ryzenSession }()
	var ran atomic.Bool
	prev := ryzenRun
	ryzenRun = func(r *RyzenSDK, stop <-chan struct{}) { ran.Store(true) }
	t.Cleanup(func() { ryzenRun = prev })
	r := newRyzenSDKWithMarker(regMarker{})
	r.Start()
	r.Stop()
	if !r.Wait(time.Second) {
		t.Fatal("queued session did not give up when stopped")
	}
	if ran.Load() {
		t.Fatal("a stopped session still started the SDK")
	}
}

// ---- sampler scheduling ------------------------------------------------------------------

func TestSamplerIntervalChangeAppliesPromptly(t *testing.T) {
	s := NewSampler(10000, 60)
	ticks := make(chan time.Time, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, func(publish bool) {
		if publish {
			ticks <- time.Now()
		}
	})
	time.Sleep(100 * time.Millisecond)
	changed := time.Now()
	s.SetInterval(250)
	select {
	case at := <-ticks:
		if d := at.Sub(changed); d > time.Second {
			t.Fatalf("new interval applied after %v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interval change waited for the old 10 s period")
	}
}

func TestSamplerPeriodExcludesTickDuration(t *testing.T) {
	s := NewSampler(250, 60)
	var mu sync.Mutex
	var at []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, func(publish bool) {
		time.Sleep(100 * time.Millisecond) // a slow tick
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
	})
	time.Sleep(1900 * time.Millisecond)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 6 {
		t.Fatalf("only %d ticks in 1.9 s at 250 ms (tick time added to the period?)", len(at))
	}
	period := at[len(at)-1].Sub(at[1]) / time.Duration(len(at)-2)
	if period < 200*time.Millisecond || period > 300*time.Millisecond {
		t.Fatalf("average period %v, want about 250 ms", period)
	}
}

func TestSamplerLateTickSkipsWithoutBurst(t *testing.T) {
	s := NewSampler(250, 60)
	var mu sync.Mutex
	var at []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	go s.loop(ctx, func(publish bool) {
		n++
		if n == 2 {
			time.Sleep(900 * time.Millisecond) // misses three deadlines
		}
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
	})
	time.Sleep(2 * time.Second)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	for i := 2; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 50*time.Millisecond {
			t.Fatalf("burst after a late tick: gap %v", gap)
		}
	}
}

// ---- process parsing: names reused across ticks ------------------------------------

// fakeProcessBuffer lays out SYSTEM_PROCESS_INFORMATION records with their
// UNICODE_STRING image names inside the same buffer, as the kernel does.
func fakeProcessBuffer(procs []rawProc) []byte {
	const rec = procInfoMinLen
	b := make([]byte, len(procs)*rec+len(procs)*128)
	base := uintptr(unsafe.Pointer(&b[0]))
	le := binary.LittleEndian
	nameOff := len(procs) * rec
	for i, p := range procs {
		r := b[i*rec:]
		if i < len(procs)-1 {
			le.PutUint32(r[offNextEntry:], rec)
		}
		le.PutUint64(r[offCreateTime:], uint64(p.CreateTime))
		le.PutUint64(r[offPID:], uint64(p.PID))
		u := utf16.Encode([]rune(p.Name))
		le.PutUint16(r[offImageLen:], uint16(2*len(u)))
		le.PutUint64(r[offImagePtr:], uint64(base)+uint64(nameOff))
		for _, c := range u {
			le.PutUint16(b[nameOff:], c)
			nameOff += 2
		}
		nameOff += 2
	}
	return b
}

func TestProcessNamesReusedAcrossTicks(t *testing.T) {
	buf := fakeProcessBuffer([]rawProc{
		{PID: 0, Name: ""},
		{PID: 100, CreateTime: 5, Name: "chrome.exe"},
		{PID: 200, CreateTime: 7, Name: "wörd.exe"},
		{PID: 300, CreateTime: 9, Name: ""},
	})
	got := parseProcessesInto(buf, nil, nil)
	want := []string{"System Idle Process", "chrome.exe", "wörd.exe", "pid 300"}
	if len(got) != len(want) {
		t.Fatalf("parsed %d processes", len(got))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("name %d = %q, want %q", i, got[i].Name, want[i])
		}
	}
	// Same instance: the previous tick's string is reused, not decoded again.
	prev := map[uint32]prevProc{100: {create: 5, name: "cached"}, 200: {create: 6, name: "old process"}}
	got = parseProcessesInto(buf, got, prev)
	if got[1].Name != "cached" {
		t.Fatalf("PID 100 decoded again: %q", got[1].Name)
	}
	// PID 200 was reused by a new process (different creation time).
	if got[2].Name != "wörd.exe" {
		t.Fatalf("reused PID kept the old name: %q", got[2].Name)
	}
	reuse := map[uint32]prevProc{}
	for _, p := range got {
		reuse[p.PID] = prevProc{create: p.CreateTime, name: p.Name}
	}
	if n := testing.AllocsPerRun(20, func() { got = parseProcessesInto(buf, got, reuse) }); n != 0 {
		t.Fatalf("steady-state parse allocates %.0f times", n)
	}
}

// ---- PDH interning --------------------------------------------------------------------

func utf16Bytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u)+2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func TestPDHInternReusesAndPrunes(t *testing.T) {
	c := &pdhCounter{names: map[string]*pdhIntern{}}
	b := utf16Bytes("pid_42_luid_0x00000000_0x0000d1e2_phys_0_eng_0_engtype_3D")
	c.calls = 1
	s1 := c.intern(b, 0)
	s2 := c.intern(b, 0)
	if s1 != "pid_42_luid_0x00000000_0x0000d1e2_phys_0_eng_0_engtype_3D" || unsafe.StringData(s1) != unsafe.StringData(s2) {
		t.Fatalf("not interned: %q", s1)
	}
	if n := testing.AllocsPerRun(100, func() { c.intern(b, 0) }); n != 0 {
		t.Fatalf("interned lookup allocates %.0f times", n)
	}
	other := utf16Bytes("温度")
	if got := c.intern(other, 0); got != "温度" {
		t.Fatalf("unicode name %q", got)
	}
	// A sweep keeps names seen in the latest collection and drops the rest.
	c.calls = pdhInternSweepEvery
	c.intern(b, 0)
	c.sweep()
	if len(c.names) != 1 {
		t.Fatalf("%d names after sweep, want 1", len(c.names))
	}
}

func TestGPUParseCachePrunesExitedInstances(t *testing.T) {
	var c gpuParseCache
	c.next()
	live := "pid_1_luid_0x00000000_0x00000001_phys_0_eng_0_engtype_3D"
	gone := "pid_2_luid_0x00000000_0x00000001_phys_0_eng_0_engtype_VideoDecode"
	e := c.lookup(gone)
	if !e.ok || e.luid != "0X00000000_0X00000001" || e.pid != 2 || e.pretty.name != "Video Decode" || !e.hasPID {
		t.Fatalf("parsed %+v", e)
	}
	for i := 0; i < 2*gpuParseCacheSweepEvery; i++ {
		c.next()
		c.lookup(live)
		c.sweep()
	}
	if _, ok := c.inst[gone]; ok {
		t.Fatal("instance of an exited process never pruned")
	}
	if _, ok := c.inst[live]; !ok {
		t.Fatal("live instance pruned")
	}
	if c.canonical("0x00000000_0x0000d1e2") != "0X00000000_0X0000D1E2" {
		t.Fatal("canonical LUID")
	}
}

func TestGPUReaderSteadyStateAllocations(t *testing.T) {
	previous := gpuSensorsWanted.Swap(false)
	t.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	r := &gpuReader{static: static, seen: map[string]bool{static[0].id: true, static[1].id: true}, lastDiscovery: time.Now()}
	byPID := map[uint32]float64{}
	r.buildInto(eng, ded, shr, pded, pshr, byPID)
	if len(byPID) != 64 {
		t.Fatalf("GPUByPID has %d processes, want 64", len(byPID))
	}
	if n := testing.AllocsPerRun(20, func() { r.buildInto(eng, ded, shr, pded, pshr, byPID) }); n > 20 {
		t.Fatalf("GPU build allocates %.0f times per sample", n)
	}
}

// GPUByPID must equal the old separate pass: per process, the busiest engine
// type summed over displayed adapters only.
func TestGPUByPIDSinglePass(t *testing.T) {
	static := []gpuStatic{{id: "0X00000000_0X00000001", name: "a"}, {id: "0X00000000_0X00000002", name: "b"},
		{id: "0X00000000_0X00000003", name: "soft", software: true}}
	eng := []PDHInstanceValue{
		{Instance: "pid_7_luid_0x00000000_0x00000001_phys_0_eng_0_engtype_3D", Value: 30},
		{Instance: "pid_7_luid_0x00000000_0x00000002_phys_0_eng_0_engtype_3D", Value: 20},
		{Instance: "pid_7_luid_0x00000000_0x00000001_phys_0_eng_1_engtype_Copy", Value: 40},
		{Instance: "pid_7_luid_0x00000000_0x00000003_phys_0_eng_0_engtype_3D", Value: 99}, // software: not shown
		{Instance: "pid_8_luid_0x00000000_0x00000099_phys_0_eng_0_engtype_3D", Value: 99}, // unknown device
		{Instance: "pid_9_luid_0x00000000_0x00000001_phys_0_eng_0_engtype_3D", Value: 80},
		{Instance: "pid_9_luid_0x00000000_0x00000002_phys_0_eng_0_engtype_3D", Value: 80},
	}
	var sc gpuScratch
	byPID := map[uint32]float64{}
	sc.build(static, eng, nil, nil, nil, nil, byPID)
	if byPID[7] != 50 || byPID[9] != 100 || len(byPID) != 2 {
		t.Fatalf("GPUByPID = %v", byPID)
	}
	if _, ok := sc.unknown["0X00000000_0X00000099"]; !ok {
		t.Fatal("unknown device not reported for discovery")
	}
}

// ---- GPU worker isolation ------------------------------------------------------------

type blockingPoller struct {
	release chan struct{}
	polls   atomic.Int32
	resets  atomic.Int32
	reading GPUAdapter
}

func (p *blockingPoller) reset([]gpuStatic) { p.resets.Add(1) }
func (p *blockingPoller) close()            {}
func (p *blockingPoller) poll(inv []gpuStatic, wanted bool, emit func(string, GPUAdapter)) {
	p.polls.Add(1)
	<-p.release
	for _, st := range inv {
		a := p.reading
		a.ID = st.id
		emit(st.id, a)
	}
}

func TestGPUBlockedVendorReadDoesNotDelaySample(t *testing.T) {
	previous := gpuSensorsWanted.Swap(true)
	t.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	p := &blockingPoller{release: make(chan struct{}), reading: GPUAdapter{HasTemp: true, TempC: 55}}
	w := startGPUWorker(func() []gpuStatic { return static }, p, false)
	t.Cleanup(func() { w.close() })
	r := &gpuReader{worker: w}
	for i := 0; i < 5; i++ {
		start := time.Now()
		list := r.build(eng, ded, shr, pded, pshr)
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("sample %d waited %v for a blocked driver", i, d)
		}
		if len(list) != 2 || list[0].HasTemp {
			t.Fatalf("sample %d: %d adapters, temp %v (no reading exists yet)", i, len(list), list[0].HasTemp)
		}
	}
	close(p.release) // the driver answers
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := r.build(eng, ded, shr, pded, pshr)
		if list[0].HasTemp && list[0].TempC == 55 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker reading never reached the snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A reading older than a few intervals is not shown as live.
	r.lastAt[static[0].id] = time.Now().Add(-time.Minute)
	r.readSeq = w.readingsSince(0, map[string]GPUAdapter{}, map[string]time.Time{}) // nothing newer merges
	list := r.scratch.build(r.static, eng, ded, shr, pded, pshr, nil)
	r.interval = 250 * time.Millisecond
	r.mergeSensors(list, time.Now())
	if list[0].HasTemp {
		t.Fatal("stale reading shown as live while sensors are wanted")
	}
}

func TestGPUDiscoveryRunsOnWorker(t *testing.T) {
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	block := make(chan struct{})
	var calls atomic.Int32
	discover := func() []gpuStatic {
		if calls.Add(1) > 1 {
			<-block // a slow inventory query (SetupAPI, D3DKMT)
		}
		return static
	}
	p := &blockingPoller{release: make(chan struct{})}
	close(p.release)
	w := startGPUWorker(discover, p, false)
	t.Cleanup(func() { close(block); w.close() })
	r := &gpuReader{worker: w}
	r.build(eng, ded, shr, pded, pshr)
	// A device that PDH reports but the inventory lacks asks for discovery…
	extra := append(eng[:len(eng):len(eng)], PDHInstanceValue{Instance: "pid_9_luid_0x00000000_0x00000077_phys_0_eng_0_engtype_3D", Value: 1})
	for i := 0; i < 5; i++ {
		start := time.Now()
		r.build(extra, ded, shr, pded, pshr)
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("sample waited %v for discovery", d)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("unknown device did not trigger discovery")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("discovery ran %d times for one new device (requests must coalesce)", got)
	}
}

func TestGPUDeviceChangeTriggersDiscovery(t *testing.T) {
	var calls atomic.Int32
	inv := []gpuStatic{{id: "0X00000000_0X00000001", name: "a"}}
	p := &blockingPoller{release: make(chan struct{})}
	close(p.release)
	w := newGPUWorker(func() []gpuStatic { calls.Add(1); return inv }, p)
	w.settle = 150 * time.Millisecond
	go w.run(false)
	t.Cleanup(w.close)
	w.waitReady(time.Second)
	for i := 0; i < 5; i++ { // a burst of notifications
		select {
		case w.devChange <- struct{}{}:
		default:
		}
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("device change did not trigger discovery")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("a notification burst caused %d discoveries, want one after the first", got)
	}
	if got := p.resets.Load(); got != 1 {
		t.Fatalf("unchanged inventory reopened vendor libraries (%d resets)", got)
	}
}

func TestDisplayAdapterNotificationRegisters(t *testing.T) {
	ch := make(chan struct{}, 1)
	unregister := registerDisplayAdapterNotifications(ch)
	if unregister == nil {
		t.Skip("CM_Register_Notification unavailable; discovery falls back to 30 s polling")
	}
	unregister()
}

// ---- native GPU node cache --------------------------------------------------------------

func TestNativeGPUNodeCache(t *testing.T) {
	var meta, node, adapter int
	query := func(kind int32, p unsafe.Pointer, size uint32) bool {
		b := unsafe.Slice((*byte)(p), int(size))
		switch kind {
		case nativeGPUNodeMetadata:
			meta++
			ordinal := binary.LittleEndian.Uint32(b)
			if ordinal > 3 {
				return false
			}
			if ordinal == 2 {
				binary.LittleEndian.PutUint32(b[4:], 1) // the 3D engine
			}
			return true
		case nativeGPUNodePerfData:
			node++
			binary.LittleEndian.PutUint64(b[8:], 1500000000)
			return true
		case nativeGPUAdapterPerfData:
			adapter++
			return false // this driver lacks adapter performance data
		}
		return false
	}
	var c nativeNodeCache
	now := time.Now()
	for i := 0; i < 5; i++ {
		g := GPUAdapter{}
		fillNativeGPUFromQueryCached(&g, query, &c, now)
		if !g.HasClock || g.ClockMHz != 1500 {
			t.Fatalf("sample %d: clock %v %v", i, g.HasClock, g.ClockMHz)
		}
	}
	if meta != 5 { // ordinals 0..3 plus the failing 4, once
		t.Fatalf("node metadata queried %d times; want one scan", meta)
	}
	if adapter != nativeGPUGiveUp {
		t.Fatalf("unsupported adapter query asked %d times; want %d then paused", adapter, nativeGPUGiveUp)
	}
	g := GPUAdapter{}
	fillNativeGPUFromQueryCached(&g, query, &c, now.Add(nativeGPURetry+time.Second))
	if adapter != nativeGPUGiveUp+1 {
		t.Fatal("paused query never retried")
	}
}

// ---- drive inventory -----------------------------------------------------------------

func TestDriveReadTimeoutIsolatesHungDrive(t *testing.T) {
	driveCache.Lock()
	saved := driveCache.m
	driveCache.m = map[int]*driveCacheEntry{}
	driveCache.Unlock()
	hang := make(chan struct{})
	t.Cleanup(func() {
		close(hang) // let the hung read finish before the real cache comes back
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			driveCache.Lock()
			e := driveCache.m[1]
			busy := e != nil && e.busy
			driveCache.Unlock()
			if !busy {
				break
			}
		}
		driveCache.Lock()
		driveCache.m = saved
		driveCache.Unlock()
	})
	var reads atomic.Int32
	read := func(idx int, full bool, desc *DriveHealth) (DriveHealth, bool) {
		reads.Add(1)
		if idx == 1 {
			<-hang
		}
		return DriveHealth{Index: idx, Model: "disk", Bus: "NVMe", HasTemp: full, TempC: 40}, true
	}
	start := time.Now()
	got := readDrivesWith([]int{0, 1, 2}, true, 100*time.Millisecond, read)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("inventory waited %v for a hung drive", d)
	}
	if len(got) != 3 || !got[0].HasTemp || !got[2].HasTemp || got[1].HasTemp || !got[1].stalled {
		t.Fatalf("drives: %+v", got)
	}
	// The hung drive is not queried again while its call is outstanding.
	before := reads.Load()
	got = readDrivesWith([]int{0, 1, 2}, true, 100*time.Millisecond, read)
	if reads.Load() != before+2 || !got[1].stalled {
		t.Fatalf("hung drive queried again (%d -> %d)", before, reads.Load())
	}
	// Descriptors are cached: the next read gets them from the cache.
	var cached atomic.Int32
	readDrivesWith([]int{0}, false, time.Second, func(idx int, full bool, desc *DriveHealth) (DriveHealth, bool) {
		if desc != nil && desc.Model == "disk" {
			cached.Add(1)
		}
		return DriveHealth{Index: idx, Model: "disk", Bus: "NVMe"}, true
	})
	if cached.Load() != 1 {
		t.Fatal("drive descriptor not cached")
	}
}

func TestDriveDescriptorOnlyRead(t *testing.T) {
	var queried []uint32
	d := readDriveHealthWith(3, func(id uint32, extra []byte, size int) ([]byte, error) {
		queried = append(queried, id)
		b := make([]byte, 36)
		binary.LittleEndian.PutUint32(b[28:], 17)
		return b, nil
	}, nil, false)
	if d.Bus != "NVMe" || len(queried) != 1 || queried[0] != propStorageDevice {
		t.Fatalf("descriptor-only read queried %v: %+v", queried, d)
	}
	queried = nil
	d = readDriveHealthWith(3, func(id uint32, extra []byte, size int) ([]byte, error) {
		queried = append(queried, id)
		return nil, errUnsupportedTest
	}, &DriveHealth{Model: "cached", Bus: "SATA"}, true)
	for _, id := range queried {
		if id == propStorageDevice {
			t.Fatal("cached descriptor queried again")
		}
	}
	if d.Model != "cached" || d.Bus != "SATA" {
		t.Fatalf("cached descriptor not used: %+v", d)
	}
}

var errUnsupportedTest = &testError{"unsupported"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }
