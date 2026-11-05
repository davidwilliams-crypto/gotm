//go:build windows

package metrics

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- native WDDM sensors: transient failures, handle reuse -----------------------

// fakeNativeGPU is a WDDM-only-sensor adapter (temperature from adapter
// performance data, clock from 3D node 0) whose calls can be made to fail.
type fakeNativeGPU struct {
	failOpen, failQuery bool
	opens, closes       int
	queries             int
	open                map[uint32]bool
	next                uint32
}

func (f *fakeNativeGPU) ops() *nativeGPUOps {
	return &nativeGPUOps{
		available: func() bool { return true },
		open: func(low uint32, high int32) (uint32, bool) {
			f.opens++
			if f.failOpen {
				return 0, false
			}
			f.next++
			if f.open == nil {
				f.open = map[uint32]bool{}
			}
			f.open[f.next] = true
			return f.next, true
		},
		query: func(h uint32, kind int32, p unsafe.Pointer, size uint32) bool {
			f.queries++
			if f.failQuery || !f.open[h] {
				return false
			}
			b := unsafe.Slice((*byte)(p), int(size))
			switch kind {
			case nativeGPUAdapterPerfData:
				binary.LittleEndian.PutUint32(b[56:], 450) // 45.0 °C
			case nativeGPUNodeMetadata:
				if binary.LittleEndian.Uint32(b) > 0 {
					return false
				}
				binary.LittleEndian.PutUint32(b[4:], 1) // 3D
			case nativeGPUNodePerfData:
				binary.LittleEndian.PutUint64(b[8:], 1200000000)
			}
			return true
		},
		close: func(h uint32) { f.closes++; delete(f.open, h) },
	}
}

const fakeLUID = "0X00000000_0X0000ABCD"

func TestNativeGPUTransientOpenFailureRetriesWithinSeconds(t *testing.T) {
	f := &fakeNativeGPU{failOpen: true}
	ops := f.ops()
	var c nativeNodeCache
	now := time.Now()
	g := GPUAdapter{ID: fakeLUID}
	fillNativeGPUSensorsWith(&g, &c, ops, now) // a TDR as the Power page opens
	if g.HasTemp || g.HasClock {
		t.Fatalf("reading from a failed open: %+v", g)
	}
	f.failOpen = false
	now = now.Add(nativeGPUBackoff + time.Millisecond)
	g = GPUAdapter{ID: fakeLUID}
	fillNativeGPUSensorsWith(&g, &c, ops, now)
	if !g.HasTemp || g.TempC != 45 || !g.HasClock || g.ClockMHz != 1200 {
		t.Fatalf("sensors still unavailable %v after a transient open failure: %+v", nativeGPUBackoff, g)
	}
}

func TestNativeGPUOpenGivesUpAfterThreeStrikes(t *testing.T) {
	f := &fakeNativeGPU{failOpen: true}
	ops := f.ops()
	var c nativeNodeCache
	now := time.Now()
	for i := 0; i < 30; i++ { // a poll every second for 30 s
		fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, &c, ops, now)
		now = now.Add(time.Second)
	}
	if f.opens != nativeGPUGiveUp {
		t.Fatalf("open tried %d times in 30 s, want %d (short backoff, then a long pause)", f.opens, nativeGPUGiveUp)
	}
	fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, &c, ops, now.Add(nativeGPURetry))
	if f.opens != nativeGPUGiveUp+1 {
		t.Fatal("unsupported adapter never retried")
	}
}

func TestNativeGPUTransientEmptyScanRetriesWithinSeconds(t *testing.T) {
	f := &fakeNativeGPU{}
	ops := f.ops()
	var c nativeNodeCache
	now := time.Now()
	// The adapter answers its performance query but node metadata fails at
	// ordinal 0 for a moment (a power transition).
	inner := ops.query
	busy := true
	ops.query = func(h uint32, kind int32, p unsafe.Pointer, size uint32) bool {
		if busy && kind == nativeGPUNodeMetadata {
			return false
		}
		return inner(h, kind, p, size)
	}
	g := GPUAdapter{ID: fakeLUID}
	fillNativeGPUSensorsWith(&g, &c, ops, now)
	if g.HasClock || !g.HasTemp {
		t.Fatalf("first poll: %+v", g)
	}
	busy = false
	now = now.Add(nativeGPUBackoff + time.Millisecond)
	g = GPUAdapter{ID: fakeLUID}
	fillNativeGPUSensorsWith(&g, &c, ops, now)
	if !g.HasClock || g.ClockMHz != 1200 {
		t.Fatalf("clock unavailable after a transient empty scan: %+v", g)
	}
	// A driver without node metadata stops being scanned after three empty scans.
	var c2 nativeNodeCache
	busy = true
	scans := 0
	ops.query = func(h uint32, kind int32, p unsafe.Pointer, size uint32) bool {
		if kind == nativeGPUNodeMetadata {
			scans++
			return false
		}
		return inner(h, kind, p, size)
	}
	for i := 0; i < 20; i++ {
		fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, &c2, ops, now)
		now = now.Add(time.Second)
	}
	if scans != nativeGPUGiveUp {
		t.Fatalf("metadata scanned %d times, want %d", scans, nativeGPUGiveUp)
	}
}

func TestNativeGPUHandleReusedAcrossPolls(t *testing.T) {
	f := &fakeNativeGPU{}
	ops := f.ops()
	var c nativeNodeCache
	now := time.Now()
	for i := 0; i < 5; i++ {
		g := GPUAdapter{ID: fakeLUID}
		fillNativeGPUSensorsWith(&g, &c, ops, now)
		if !g.HasTemp || !g.HasClock {
			t.Fatalf("poll %d: %+v", i, g)
		}
		now = now.Add(time.Second)
	}
	if f.opens != 1 || f.closes != 0 {
		t.Fatalf("adapter opened %d / closed %d times over 5 polls; want one open, kept", f.opens, f.closes)
	}
	// Every query failing (a reset invalidated the handle): it is closed and a
	// fresh one opened on the next poll.
	f.failQuery = true
	fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, &c, ops, now)
	if f.closes != 1 || c.handle != 0 {
		t.Fatalf("stale handle kept (closes %d, handle %d)", f.closes, c.handle)
	}
	f.failQuery = false
	now = now.Add(nativeGPUBackoff + time.Millisecond)
	g := GPUAdapter{ID: fakeLUID}
	fillNativeGPUSensorsWith(&g, &c, ops, now)
	if f.opens != 2 || !g.HasTemp {
		t.Fatalf("no reopen after a failed poll (opens %d): %+v", f.opens, g)
	}
	// Without a cache the handle is closed at once.
	f2 := &fakeNativeGPU{}
	fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, nil, f2.ops(), now)
	if f2.opens != 1 || f2.closes != 1 {
		t.Fatalf("uncached read leaked a handle (%d/%d)", f2.opens, f2.closes)
	}
}

func TestVendorPollerClosesNativeHandles(t *testing.T) {
	f := &fakeNativeGPU{}
	ops := f.ops()
	p := &vendorGPUPoller{}
	now := time.Now()
	for _, id := range []string{"0X00000000_0X00000001", "0X00000000_0X00000002"} {
		fillNativeGPUSensorsWith(&GPUAdapter{ID: id}, p.nativeFor(id), ops, now)
	}
	if len(f.open) != 2 {
		t.Fatalf("%d handles open", len(f.open))
	}
	// Sensors no longer wanted: kept for a while, then closed.
	p.idleNative(false, now)
	p.idleNative(false, now.Add(nativeHandleIdle/2))
	if len(f.open) != 2 {
		t.Fatal("handles closed as soon as the GPU view closed")
	}
	p.idleNative(false, now.Add(nativeHandleIdle))
	if len(f.open) != 0 {
		t.Fatalf("%d handles still open after the idle period", len(f.open))
	}
	// Inventory change and shutdown close them as well.
	fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, p.nativeFor(fakeLUID), ops, now)
	p.reset(nil)
	if len(f.open) != 0 || p.native != nil {
		t.Fatal("inventory change kept a native handle")
	}
	fillNativeGPUSensorsWith(&GPUAdapter{ID: fakeLUID}, p.nativeFor(fakeLUID), ops, now)
	p.close()
	if len(f.open) != 0 {
		t.Fatal("shutdown kept a native handle")
	}
}

// ---- GPU worker: excluded LUIDs and removed adapters ------------------------------

type countingPoller struct{ reading GPUAdapter }

func (p *countingPoller) reset([]gpuStatic) {}
func (p *countingPoller) close()            {}
func (p *countingPoller) poll(inv []gpuStatic, wanted bool, emit func(string, GPUAdapter)) {
	for _, st := range inv {
		a := p.reading
		a.ID = st.id
		emit(st.id, a)
	}
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func TestGPUExcludedLUIDDoesNotRetriggerDiscovery(t *testing.T) {
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	var calls atomic.Int32
	w := newGPUWorker(func() []gpuStatic { calls.Add(1); return static }, &countingPoller{})
	go w.run(false)
	t.Cleanup(w.close)
	r := &gpuReader{worker: w}
	// A compute-only device (or the Basic Render Driver) that PDH reports but
	// the inventory never lists.
	eng = append(eng[:len(eng):len(eng)], PDHInstanceValue{Instance: "pid_9_luid_0x00000000_0x00000077_phys_0_eng_0_engtype_Compute", Value: 1})
	r.build(eng, ded, shr, pded, pshr)
	waitFor(t, "unknown device did not trigger discovery", func() bool { return calls.Load() >= 2 })
	for i := 0; i < 5; i++ {
		r.build(eng, ded, shr, pded, pshr)
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("excluded device caused %d discoveries, want 2", got)
	}
	// A periodic rediscovery installs a new inventory; the excluded device is
	// still accounted for and must not ask for a second discovery.
	w.requestDiscovery(w.reqSeq.Load()) // stands in for the periodic timer
	waitFor(t, "periodic discovery did not run", func() bool { return calls.Load() >= 3 })
	for i := 0; i < 5; i++ {
		r.build(eng, ded, shr, pded, pshr)
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("periodic refresh plus excluded device gave %d discoveries, want 3", got)
	}
	// A genuinely new device still triggers one.
	eng = append(eng, PDHInstanceValue{Instance: "pid_9_luid_0x00000000_0x00000078_phys_0_eng_0_engtype_3D", Value: 1})
	r.build(eng, ded, shr, pded, pshr)
	waitFor(t, "new device did not trigger discovery", func() bool { return calls.Load() >= 4 })
}

func TestGPURemovedAdapterReadingsPruned(t *testing.T) {
	previous := gpuSensorsWanted.Swap(true)
	t.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	var inv atomic.Pointer[[]gpuStatic]
	inv.Store(&static)
	w := newGPUWorker(func() []gpuStatic { return *inv.Load() }, &countingPoller{reading: GPUAdapter{HasTemp: true, TempC: 50}})
	go w.run(false)
	t.Cleanup(w.close)
	r := &gpuReader{worker: w}
	removed := static[1].id
	waitFor(t, "no reading for the second adapter", func() bool {
		r.build(eng, ded, shr, pded, pshr)
		_, ok := r.lastAt[removed]
		return ok
	})
	one := static[:1:1]
	inv.Store(&one)
	w.requestDiscovery(w.reqSeq.Load())
	waitFor(t, "inventory change not installed", func() bool {
		r.build(eng, ded, shr, pded, pshr)
		return len(r.static) == 1
	})
	for i := 0; i < 3; i++ {
		r.build(eng, ded, shr, pded, pshr)
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := r.lastAt[removed]; ok {
		t.Fatal("removed adapter's reading came back through readingsSince")
	}
	w.mu.Lock()
	_, kept := w.readings[removed]
	w.mu.Unlock()
	if kept {
		t.Fatal("worker kept the removed adapter's reading")
	}
}

func TestGPUWorkerStartupPollForAPU(t *testing.T) {
	previous := gpuSensorsWanted.Swap(false)
	t.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	inv := []gpuStatic{{id: "0X00000000_0X00000001", name: "Radeon Graphics", vendorID: 0x1002}}
	w := newGPUWorker(func() []gpuStatic { return inv }, &countingPoller{reading: GPUAdapter{Integrated: true, HasCPUTemp: true}})
	go w.run(false)
	t.Cleanup(w.close)
	// No sample has asked for a poll: the worker read the APU on its own.
	waitFor(t, "no start-up read for an AMD adapter", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.readSeq > 0
	})
}

// ---- AMD SDK: quick off -> on -> exit ---------------------------------------------

func TestRyzenWaitCoversEarlierSession(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	prev := ryzenRun
	ryzenRun = func(r *RyzenSDK, stop <-chan struct{}) {
		if runs.Add(1) == 1 {
			close(entered)
			<-release // the old session is still in UnInit
		}
	}
	t.Cleanup(func() { ryzenRun = prev })
	old := newRyzenSDKWithMarker(regMarker{})
	old.Start()
	<-entered
	old.Stop() // toggle off
	cur := newRyzenSDKWithMarker(regMarker{})
	cur.Start() // toggle on: queued behind the old session
	waitFor(t, "queued session shows no waiting status", func() bool {
		st, d := cur.State()
		return st == RyzenStarting && d == ryzenWaitingDetail
	})
	cur.Stop() // exit
	if cur.Wait(100 * time.Millisecond) {
		t.Fatal("Wait returned while the earlier session was still releasing the SDK")
	}
	if WaitRyzenSessions(10 * time.Millisecond) {
		t.Fatal("WaitRyzenSessions ignored a live session")
	}
	close(release)
	if !cur.Wait(2 * time.Second) {
		t.Fatal("Wait did not see the earlier session finish")
	}
	if !WaitRyzenSessions(time.Second) {
		t.Fatal("sessions still live")
	}
	if runs.Load() != 1 {
		t.Fatalf("a stopped queued session ran the SDK (%d runs)", runs.Load())
	}
}

func TestRyzenQueuedSessionStartsAfterEarlierOne(t *testing.T) {
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	prev := ryzenRun
	ryzenRun = func(r *RyzenSDK, stop <-chan struct{}) {
		if runs.Add(1) == 1 {
			close(entered)
			<-release
			return
		}
		r.mu.Lock()
		if r.current(stop) {
			r.state, r.detail = RyzenRunning, "running"
		}
		r.mu.Unlock()
		close(second)
		<-stop
	}
	t.Cleanup(func() { ryzenRun = prev })
	old := newRyzenSDKWithMarker(regMarker{})
	old.Start()
	<-entered
	old.Stop()
	cur := newRyzenSDKWithMarker(regMarker{})
	cur.Start()
	waitFor(t, "no waiting status", func() bool { _, d := cur.State(); return d == ryzenWaitingDetail })
	close(release)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("queued session never started")
	}
	if st, _ := cur.State(); st != RyzenRunning {
		t.Fatalf("state %v after the slot was free", st)
	}
	cur.Stop()
	if !cur.Wait(2 * time.Second) {
		t.Fatal("session did not finish")
	}
}

// ---- drive descriptor cache: hot-swap ---------------------------------------------

func TestDriveDescriptorRefreshedAfterHotSwap(t *testing.T) {
	driveCache.Lock()
	saved := driveCache.m
	driveCache.m = map[int]*driveCacheEntry{}
	driveCache.Unlock()
	t.Cleanup(func() {
		driveCache.Lock()
		driveCache.m = saved
		driveCache.Unlock()
	})
	type disk struct {
		model string
		bus   uint32
		size  uint64
	}
	current := disk{"OldVendor OldDisk", 11, 500 << 30}
	var descReads atomic.Int32
	read := func(idx int, full bool, desc *DriveHealth) (DriveHealth, bool) {
		d := current
		return readDriveWith(idx, full, desc, func() uint64 { return d.size },
			func(id uint32, extra []byte, size int) ([]byte, error) {
				if id != propStorageDevice {
					return nil, errUnsupportedTest
				}
				descReads.Add(1)
				b := make([]byte, 64)
				binary.LittleEndian.PutUint32(b[16:], 40) // ProductIdOffset
				binary.LittleEndian.PutUint32(b[28:], d.bus)
				copy(b[40:], d.model)
				return b, nil
			}), true
	}
	got := readDrivesWith([]int{1}, false, time.Second, read)
	if len(got) != 1 || got[0].Model != "OldVendor OldDisk" || got[0].Bus != "SATA" {
		t.Fatalf("first read: %+v", got)
	}
	readDrivesWith([]int{1}, false, time.Second, read)
	if descReads.Load() != 1 {
		t.Fatalf("same drive: descriptor read %d times, want cached", descReads.Load())
	}
	// Another drive appears at PhysicalDrive1 within the cache lifetime.
	current = disk{"NewVendor NVMe 2TB", 17, 2000 << 30}
	got = readDrivesWith([]int{1}, false, time.Second, read)
	if len(got) != 1 || got[0].Model != "NewVendor NVMe 2TB" || got[0].Bus != "NVMe" {
		t.Fatalf("hot-swapped drive shows the old descriptor: %+v", got)
	}
	readDrivesWith([]int{1}, false, time.Second, read)
	if descReads.Load() != 2 {
		t.Fatalf("descriptor reads %d, want 2 (new drive cached after one read)", descReads.Load())
	}
}

// The size check needs no access rights: it must work on the handle the
// descriptor query uses (opened with access 0).
func TestDiskSizeLive(t *testing.T) {
	indices, err := discoverPhysicalDrives()
	if err != nil || len(indices) == 0 {
		t.Skip("no physical drives")
	}
	sized := 0
	for _, idx := range indices {
		h, err := openPhysical(idx)
		if err != nil {
			continue
		}
		n := diskSize(h)
		windows.CloseHandle(h)
		t.Logf("PhysicalDrive%d: %d bytes", idx, n)
		if n > 0 {
			sized++
		}
	}
	if sized == 0 {
		t.Fatal("no drive reported its size through an access-0 handle")
	}
}

// ---- rated clock: needMax bypasses the fallback throttle --------------------------

func TestSensorMaxClockBypassesThrottle(t *testing.T) {
	h := &SensorHub{slowAt: time.Now()}
	// Update's fallback clock read ran a moment ago and gave no maximum.
	h.clockAt = time.Now()
	var reads atomic.Int32
	h.refreshSlowWith(func(bool) []DriveHealth { return nil },
		func() PowerStatic { return PowerStatic{} },
		func() ([]uint32, []uint32) { reads.Add(1); return []uint32{3000}, []uint32{4200} })
	waitFor(t, "rated clock read was throttled away", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.maxMHz) == 1 && h.maxMHz[0] == 4200
	})
	// The plain fallback path stays throttled.
	h.clockSnapshot(func() ([]uint32, []uint32) { reads.Add(1); return nil, nil })
	time.Sleep(20 * time.Millisecond)
	if got := reads.Load(); got != 1 {
		t.Fatalf("clock read %d times; the throttle no longer applies to fallback reads", got)
	}
}
