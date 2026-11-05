//go:build windows

package metrics

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// gpuReader owns the slow-changing parts of the GPU picture. It is used only
// by the sampler goroutine. Inventory discovery and every vendor/native sensor
// read run on a dedicated worker (gpuWorker), so a slow or stalled display
// driver can delay only those readings, never a snapshot.
type gpuReader struct {
	static        []gpuStatic
	seen          map[string]bool       // canonical LUIDs already accounted for by discovery
	last          map[string]GPUAdapter // last vendor sensor values per adapter ID
	lastAt        map[string]time.Time  // when each cached reading was taken
	lastDiscovery time.Time

	scratch   gpuScratch
	worker    *gpuWorker
	invGen    uint64            // last inventory generation applied
	readSeq   uint64            // last worker reading sequence merged
	pending   map[string]uint64 // unseen LUID -> discovery request that covers it
	reqSeq    uint64
	waited    bool
	lastBuild time.Time
	interval  time.Duration // observed sampling interval
}

// sensorStaleAfter bounds how long an unpolled vendor reading (a sleeping
// dGPU's last values) is still shown: past this it is dropped rather than
// displayed as if it were live.
const sensorStaleAfter = 2 * time.Minute

const (
	// A reading shown as live must be from one of the last few samples; a
	// stalled driver call therefore turns into "unavailable", not frozen values.
	gpuFreshMin, gpuFreshMax = 3 * time.Second, 45 * time.Second
	// The first sample waits this long, at most, for the initial inventory.
	gpuInventoryWait = 1500 * time.Millisecond
	// gpuDiscoveryEvery is the synchronous (worker-less) rediscovery period.
	gpuDiscoveryEvery = 30 * time.Second
)

// cachedSensors replays the last reading for adapters that are not being
// polled right now; readings older than sensorStaleAfter are dropped instead
// of being shown as if they were live.
func (r *gpuReader) cachedSensors(a *GPUAdapter) {
	if t, ok := r.lastAt[a.ID]; !ok || time.Since(t) > sensorStaleAfter {
		return // no (recent enough) reading: show nothing rather than stale data
	}
	copySensors(a, r.last[a.ID])
}

// close stops the worker, which releases the vendor library sessions (the
// ADL context, the NVML session) on its own thread.
func (r *gpuReader) close() {
	if r == nil {
		return
	}
	r.worker.close()
}

func newGPUReader() *gpuReader {
	r := &gpuReader{seen: map[string]bool{}, last: map[string]GPUAdapter{}, lastAt: map[string]time.Time{}}
	r.worker = startGPUWorker(enumerateGPUs, &vendorGPUPoller{}, true)
	return r
}

// refreshGPUDiscovery coalesces a periodic refresh and newly observed PDH
// devices into one synchronous inventory query. It is the worker-less path
// (tests, fallback); with a worker the same triggers run asynchronously.
// Unknown compute devices are remembered until the next periodic refresh,
// without copying large counter arrays.
func (r *gpuReader) refreshGPUDiscovery(now time.Time, eng, ded []PDHInstanceValue, discover func() []gpuStatic) {
	refresh := now.Sub(r.lastDiscovery) >= gpuDiscoveryEvery
	if !refresh {
		refresh = r.anyUnseen(eng) || r.anyUnseen(ded)
	}
	if !refresh {
		return
	}
	r.applyInventory(discover(), now)
	for _, values := range [2][]PDHInstanceValue{ded, eng} {
		for _, it := range values {
			if raw := rawLUIDKey(it.Instance); raw != "" {
				r.seen[r.scratch.parse.canonical(raw)] = true
			}
		}
	}
}

func (r *gpuReader) anyUnseen(values []PDHInstanceValue) bool {
	for _, it := range values {
		if raw := rawLUIDKey(it.Instance); raw != "" && !r.seen[r.scratch.parse.canonical(raw)] {
			return true
		}
	}
	return false
}

// applyInventory installs a discovery result. Published inventories are
// never modified afterwards, so the slice is shared with the worker.
func (r *gpuReader) applyInventory(static []gpuStatic, now time.Time) {
	r.static, r.lastDiscovery = static, now
	if r.seen == nil {
		r.seen = make(map[string]bool, len(static))
	}
	clear(r.seen)
	for _, st := range static {
		r.seen[st.id] = true
	}
}

// build combines one PDH sample with static data and vendor sensors.
func (r *gpuReader) build(eng, ded, shr, pded, pshr []PDHInstanceValue) []GPUAdapter {
	return r.buildInto(eng, ded, shr, pded, pshr, nil)
}

// buildInto is build that also fills byPID (see gpuScratch.build).
func (r *gpuReader) buildInto(eng, ded, shr, pded, pshr []PDHInstanceValue, byPID map[uint32]float64) []GPUAdapter {
	if r == nil {
		return nil
	}
	now := time.Now()
	if !r.lastBuild.IsZero() {
		if d := now.Sub(r.lastBuild); d > 0 {
			r.interval = d
		}
	}
	r.lastBuild = now
	w := r.worker
	if w != nil {
		w.requestPoll() // read sensors in parallel; this sample merges the newest published values
		r.syncInventory(now)
	} else {
		r.refreshGPUDiscovery(now, eng, ded, enumerateGPUs)
	}
	list := r.scratch.build(r.static, eng, ded, shr, pded, pshr, byPID)
	if w != nil {
		r.requestUnknown()
		if r.last == nil {
			r.last, r.lastAt = map[string]GPUAdapter{}, map[string]time.Time{}
		}
		r.readSeq = w.readingsSince(r.readSeq, r.last, r.lastAt)
	}
	r.mergeSensors(list, now)
	return list
}

func (r *gpuReader) syncInventory(now time.Time) {
	w := r.worker
	if !r.waited {
		r.waited = true
		w.waitReady(gpuInventoryWait)
	}
	inv, gen, served, ok := w.inventorySince(r.invGen)
	if !ok {
		return
	}
	r.invGen = gen
	r.applyInventory(inv, now)
	// Devices the inventory still does not list (compute-only, excluded) were
	// covered by this discovery: stop asking until the next periodic refresh.
	for id, req := range r.pending {
		if req <= served {
			r.seen[id] = true
			delete(r.pending, id)
		}
	}
	for id := range r.lastAt {
		if !r.seen[id] {
			delete(r.lastAt, id)
			delete(r.last, id)
		}
	}
}

// requestUnknown asks the worker for one discovery covering LUIDs that PDH
// reports but neither the inventory nor an earlier discovery accounted for.
func (r *gpuReader) requestUnknown() {
	ask := false
	for id := range r.scratch.unknown {
		if r.seen[id] {
			continue
		}
		if _, queued := r.pending[id]; queued {
			continue
		}
		if r.pending == nil {
			r.pending = map[string]uint64{}
		}
		if !ask {
			r.reqSeq++
			ask = true
		}
		r.pending[id] = r.reqSeq
	}
	if ask {
		r.worker.requestDiscovery(r.reqSeq)
	}
}

// mergeSensors applies cached vendor/native readings. While a GPU view is on
// screen (or for an APU, whose sensors are always read) only a reading from
// the last few samples counts as live; otherwise the last reading of a
// sleeping adapter is replayed up to sensorStaleAfter.
func (r *gpuReader) mergeSensors(list []GPUAdapter, now time.Time) {
	if len(r.lastAt) == 0 {
		return
	}
	wanted := gpuSensorsWanted.Load()
	fresh := 3 * r.interval
	fresh = max(gpuFreshMin, min(gpuFreshMax, fresh))
	for i := range list {
		g := &list[i]
		t, ok := r.lastAt[g.ID]
		if !ok {
			continue
		}
		rd := r.last[g.ID]
		age := now.Sub(t)
		if wanted || rd.Integrated {
			if age <= fresh {
				copySensors(g, rd)
			}
			continue
		}
		if age <= sensorStaleAfter {
			copySensors(g, rd)
		}
	}
}

// ---- worker ---------------------------------------------------------------------------

// gpuSensorPoller reads vendor and native sensors. It is used only on the
// worker goroutine (whose OS thread is locked, as ADL contexts expect).
type gpuSensorPoller interface {
	// reset is called when the inventory changed: drop matches and sessions.
	reset(inv []gpuStatic)
	// poll reads the adapters that should be read now and emits each reading.
	poll(inv []gpuStatic, wanted bool, emit func(id string, a GPUAdapter))
	close()
}

type gpuReading struct {
	a  GPUAdapter
	at time.Time
}

// gpuWantedPoke lets SetGPUSensorsWanted(true) start a read at once, so the
// first sample of a GPU view already has fresh readings.
var gpuWantedPoke = make(chan struct{}, 1)

type gpuWorker struct {
	discover func() []gpuStatic
	poller   gpuSensorPoller
	periodic time.Duration // rediscovery without device notifications
	notified time.Duration // rediscovery when device-change notifications work
	settle   time.Duration // wait after a device change before rediscovering

	pollReq, discReq, devChange chan struct{}
	stop, done, ready           chan struct{}
	stopOnce                    sync.Once
	reqSeq                      atomic.Uint64

	mu       sync.Mutex
	inv      []gpuStatic
	invGen   uint64
	served   uint64
	readings map[string]gpuReading
	readSeq  uint64
	notifyOK bool
}

func newGPUWorker(discover func() []gpuStatic, poller gpuSensorPoller) *gpuWorker {
	return &gpuWorker{
		discover: discover, poller: poller,
		periodic: 30 * time.Second, notified: 5 * time.Minute, settle: 1500 * time.Millisecond,
		pollReq: make(chan struct{}, 1), discReq: make(chan struct{}, 1), devChange: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{}),
		readings: map[string]gpuReading{},
	}
}

func startGPUWorker(discover func() []gpuStatic, poller gpuSensorPoller, notify bool) *gpuWorker {
	w := newGPUWorker(discover, poller)
	go w.run(notify)
	return w
}

func (w *gpuWorker) run(notify bool) {
	runtime.LockOSThread() // ADL contexts belong to the thread that created them
	defer runtime.UnlockOSThread()
	defer close(w.done)
	defer w.poller.close()
	periodic := w.periodic
	if notify {
		if unregister := registerDisplayAdapterNotifications(w.devChange); unregister != nil {
			defer unregister()
			periodic = w.notified
			w.mu.Lock()
			w.notifyOK = true
			w.mu.Unlock()
		}
	}
	w.rediscover(true)
	close(w.ready)
	timer := time.NewTimer(periodic)
	defer timer.Stop()
	var settle *time.Timer
	var settled <-chan time.Time
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()
	for {
		select {
		case <-w.stop:
			return
		case <-w.discReq:
			w.rediscover(false)
			timer.Reset(periodic)
		case <-w.devChange:
			// Driver installs and hot-plug arrive as bursts; settle, then look once.
			if settle == nil {
				settle = time.NewTimer(w.settle)
			} else {
				settle.Reset(w.settle)
			}
			settled = settle.C
		case <-settled:
			settled = nil
			w.rediscover(false)
			timer.Reset(periodic)
		case <-timer.C:
			w.rediscover(false)
			timer.Reset(periodic)
		case <-w.pollReq:
			w.poll()
		case <-gpuWantedPoke:
			w.poll()
		}
	}
}

func (w *gpuWorker) rediscover(first bool) {
	served := w.reqSeq.Load()
	inv := w.discover()
	w.mu.Lock()
	changed := first || !sameGPUInventory(w.inv, inv)
	w.inv, w.served = inv, served
	w.invGen++
	w.mu.Unlock()
	if changed {
		w.poller.reset(inv)
	}
}

func sameGPUInventory(a, b []gpuStatic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (w *gpuWorker) poll() {
	w.mu.Lock()
	inv := w.inv
	w.mu.Unlock()
	now := time.Now()
	w.poller.poll(inv, gpuSensorsWanted.Load(), func(id string, a GPUAdapter) {
		w.mu.Lock()
		w.readings[id] = gpuReading{a: a, at: now}
		w.readSeq++
		w.mu.Unlock()
	})
}

// requestPoll asks for one sensor read; requests made while one runs coalesce.
func (w *gpuWorker) requestPoll() {
	select {
	case w.pollReq <- struct{}{}:
	default:
	}
}

// requestDiscovery asks for a rediscovery covering request seq.
func (w *gpuWorker) requestDiscovery(seq uint64) {
	w.reqSeq.Store(seq)
	select {
	case w.discReq <- struct{}{}:
	default:
	}
}

func (w *gpuWorker) waitReady(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-w.ready:
	case <-t.C:
	}
}

// inventorySince returns the newest inventory if it is newer than gen, with
// the discovery request number it covers.
func (w *gpuWorker) inventorySince(gen uint64) (inv []gpuStatic, newGen, served uint64, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.invGen == gen {
		return nil, gen, 0, false
	}
	return w.inv, w.invGen, w.served, true
}

// readingsSince copies readings newer than those already in last/lastAt.
func (w *gpuWorker) readingsSince(seq uint64, last map[string]GPUAdapter, lastAt map[string]time.Time) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readSeq == seq {
		return seq
	}
	for id, rd := range w.readings {
		if rd.at.After(lastAt[id]) {
			last[id], lastAt[id] = rd.a, rd.at
		}
	}
	return w.readSeq
}

// close stops the worker and waits a bounded time for it to release the
// vendor sessions. A driver call that never returns keeps only the worker.
func (w *gpuWorker) close() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stop) })
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-w.done:
	case <-t.C:
	}
}

// ---- vendor and native sensor poller -------------------------------------------------

type adlLookup struct {
	ad adlAdapter
	ok bool
}

// vendorGPUPoller opens NVML only when the inventory has an NVIDIA adapter
// and a GPU view wants its sensors, ADL only when it has an AMD adapter;
// both lazily, and both again after the inventory changes.
type vendorGPUPoller struct {
	nv       *nvmlLib
	nvTried  bool
	nvDevs   []nvmlDev
	nvLocal  []vendorGPUIdentity
	nvIndex  map[string]int
	adl      *adlLib
	adlTried bool
	adlIndex map[string]adlLookup
	native   map[string]*nativeNodeCache
}

func (p *vendorGPUPoller) reset(inv []gpuStatic) {
	p.nv.close()
	p.adl.close()
	*p = vendorGPUPoller{}
}

func (p *vendorGPUPoller) close() {
	p.nv.close()
	p.adl.close()
}

func (p *vendorGPUPoller) amd() *adlLib {
	if !p.adlTried {
		p.adlTried = true
		p.adl = openADL()
	}
	if p.adl == nil || !p.adl.ok {
		return nil
	}
	return p.adl
}

func (p *vendorGPUPoller) nvidia(inv []gpuStatic) *nvmlLib {
	if !p.nvTried {
		p.nvTried = true
		p.nv = openNVML()
		p.nvDevs = p.nv.devices()
		p.nvLocal = vendorGPUIdentities(inv, 0x10de)
	}
	if p.nv == nil || !p.nv.ok {
		return nil
	}
	return p.nv
}

func (p *vendorGPUPoller) adlFor(l *adlLib, inv []gpuStatic, st gpuStatic) (adlAdapter, bool) {
	if m, ok := p.adlIndex[st.id]; ok {
		return m.ad, m.ok
	}
	ad, ok := l.find(st.bus, st.name, inv...)
	if p.adlIndex == nil {
		p.adlIndex = map[string]adlLookup{}
	}
	p.adlIndex[st.id] = adlLookup{ad, ok}
	return ad, ok
}

func (p *vendorGPUPoller) nvFor(st gpuStatic) int {
	if d, ok := p.nvIndex[st.id]; ok {
		return d
	}
	remote := make([]vendorGPUIdentity, 0, len(p.nvDevs))
	for _, d := range p.nvDevs {
		remote = append(remote, vendorGPUIdentity{d.bus, d.name})
	}
	d := matchVendorGPU(st.bus, st.name, p.nvLocal, remote)
	if p.nvIndex == nil {
		p.nvIndex = map[string]int{}
	}
	p.nvIndex[st.id] = d
	return d
}

func (p *vendorGPUPoller) nativeFor(id string) *nativeNodeCache {
	c := p.native[id]
	if c == nil {
		if p.native == nil {
			p.native = map[string]*nativeNodeCache{}
		}
		c = &nativeNodeCache{}
		p.native[id] = c
	}
	return c
}

func (p *vendorGPUPoller) poll(inv []gpuStatic, wanted bool, emit func(string, GPUAdapter)) {
	for _, st := range inv {
		if st.software {
			continue
		}
		a := GPUAdapter{ID: st.id, Name: st.name, Vendor: vendorName(st.vendorID), Software: st.software}
		polled := false
		switch st.vendorID {
		case 0x1002:
			// An APU's graphics is the CPU's own silicon: reading it wakes
			// nothing, and its table may carry the CPU temperature the CPU
			// card shows all the time. A discrete card waits for a GPU view.
			if l := p.amd(); l != nil {
				if ad, ok := p.adlFor(l, inv, st); ok {
					a.Integrated = l.isAPU(ad)
					if wanted || a.Integrated {
						l.fill(ad, &a)
						// ADL only assigns positive, validated memory clocks. Availability
						// is independent of whether its graphics-clock query succeeded.
						a.HasMemClock = a.MemClockMHz > 0
						polled = true
					}
				}
			}
		case 0x10de:
			if wanted {
				if l := p.nvidia(inv); l != nil {
					if d := p.nvFor(st); d >= 0 {
						l.fill(p.nvDevs[d], &a)
					}
				}
			}
		}
		if wanted {
			fillNativeGPUSensorsCached(&a, p.nativeFor(st.id))
			polled = true
		}
		// A completed sample is published even when every read failed: a
		// failed fresh read stays unavailable rather than replaying old values.
		if polled {
			emit(st.id, a)
		}
	}
}
