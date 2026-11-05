//go:build windows

package metrics

import (
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// These optional WDDM 2.4 queries use the installed display driver through
// Windows itself. They need neither a vendor SDK nor an additional driver.
// Layouts and enum values follow Microsoft's SDK d3dkmthk.h / d3dkmdt.h:
// https://github.com/microsoft/win32metadata/tree/main/generation/WinSDK/RecompiledIdlHeaders/shared
const (
	nativeGPUNodeMetadata    = 25
	nativeGPUNodePerfData    = 61
	nativeGPUAdapterPerfData = 62
)

type nativeGPUAdapterPerf struct {
	PhysicalAdapterIndex                                      uint32
	_                                                         uint32 // D3DKMT_ALIGN64
	MemoryFrequency, MaxMemoryFrequency, MaxMemoryFrequencyOC uint64
	MemoryBandwidth, PCIEBandwidth                            uint64
	FanRPM, Power, Temperature                                uint32
	PowerStateOverride                                        uint8
	_                                                         [3]byte
}

type nativeGPUNodePerf struct {
	NodeOrdinal, PhysicalAdapterIndex       uint32
	Frequency, MaxFrequency, MaxFrequencyOC uint64
	Voltage, VoltageMax, VoltageMaxOC       uint32
	_                                       uint32
	// Before WDDM 2.5 this same slot was Reserved, not a second field.
	MaxTransitionLatency uint64
}

type nativeGPUNodeMeta struct {
	NodeOrdinalAndAdapterIndex      uint32
	EngineType                      uint32
	FriendlyName                    [32]uint16
	Flags                           uint32
	GpuMmuSupported, IoMmuSupported uint8
	_                               [2]byte
}

type nativeGPUQuery func(kind int32, data unsafe.Pointer, size uint32) bool

func parseNativeGPULUID(id string) (low uint32, high int32, ok bool) {
	if len(id) != 21 || !strings.EqualFold(id[:2], "0x") || id[10] != '_' || !strings.EqualFold(id[11:13], "0x") {
		return
	}
	hi, err := strconv.ParseUint(id[2:10], 16, 32)
	if err != nil {
		return
	}
	lo, err := strconv.ParseUint(id[13:21], 16, 32)
	if err != nil {
		return
	}
	return uint32(lo), int32(hi), true
}

// nativeNodeCache remembers, per adapter LUID, what the driver answered
// before: which nodes are 3D engines (metadata does not change while the
// driver stays loaded) and which queries it does not implement. It also holds
// the adapter handle open between polls, so a GPU view does not open and
// close every adapter on every sample. It is used only on the GPU worker
// goroutine; the worker drops it (closing the handle) whenever the inventory
// changes (driver update, hot-plug), when GPU sensors have not been wanted for
// a while, and when it shuts down.
type nativeNodeCache struct {
	scanned      bool     // node metadata has been enumerated
	nodes3D      []uint32 // ordinals of 3D engine nodes, in order
	adapterFails int      // consecutive failed adapter performance queries
	nodeFails    int      // consecutive failed node performance queries
	scanFails    int      // consecutive node scans that found no 3D engine
	openFails    int      // consecutive failed D3DKMTOpenAdapterFromLuid calls
	adapterRetry time.Time
	nodeRetry    time.Time
	openRetry    time.Time // the LUID could not be opened: not before this

	handle  uint32       // open D3DKMT adapter handle, 0 when closed
	closeFn func(uint32) // closes handle (the ops that opened it)
}

const (
	// nativeGPUGiveUp failures in a row mark a query (or opening the adapter)
	// unsupported until nativeGPURetry has passed: a driver that cannot answer
	// is not asked every sample. Failures before that are retried after
	// nativeGPUBackoff, so a driver that was merely busy for a moment (a TDR,
	// a power transition) is asked again within seconds, not minutes.
	nativeGPUGiveUp  = 3
	nativeGPUBackoff = 5 * time.Second
	nativeGPURetry   = 5 * time.Minute
)

// nativeStrike records one failure in a row of *fails: a short pause before
// the next attempt, and a long one once nativeGPUGiveUp have failed.
func nativeStrike(fails *int, retry *time.Time, now time.Time) {
	if *fails++; *fails >= nativeGPUGiveUp {
		*fails, *retry = 0, now.Add(nativeGPURetry)
	} else {
		*retry = now.Add(nativeGPUBackoff)
	}
}

// closeHandle closes the held adapter handle, if any.
func (c *nativeNodeCache) closeHandle() {
	if c == nil || c.handle == 0 {
		return
	}
	if c.closeFn != nil {
		c.closeFn(c.handle)
	}
	c.handle, c.closeFn = 0, nil
}

// nativeGPUOps are the three D3DKMT calls; tests substitute fakes.
type nativeGPUOps struct {
	available func() bool
	open      func(low uint32, high int32) (uint32, bool)
	query     func(h uint32, kind int32, data unsafe.Pointer, size uint32) bool
	close     func(h uint32)
}

var nativeGPUSys = &nativeGPUOps{
	available: func() bool {
		return procD3DKMTOpenAdapterFromLuid.Find() == nil && procD3DKMTQueryAdapterInfo.Find() == nil && procD3DKMTCloseAdapter.Find() == nil
	},
	open: func(low uint32, high int32) (uint32, bool) {
		open := struct {
			Low     uint32
			High    int32
			Adapter uint32
		}{Low: low, High: high}
		status, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open)))
		return open.Adapter, int32(status) >= 0 && open.Adapter != 0
	},
	query: func(h uint32, kind int32, data unsafe.Pointer, size uint32) bool {
		q := struct {
			Adapter uint32
			Type    int32
			Data    unsafe.Pointer
			Size    uint32
		}{h, kind, data, size}
		status, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q)))
		runtime.KeepAlive(data)
		return int32(status) >= 0
	},
	close: func(h uint32) {
		closeArg := h
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&closeArg)))
	},
}

// fillNativeGPUSensors fills missing readings for this exact DXGI/PDH LUID.
// The caller gates sampling on the GPU detail view to avoid waking idle GPUs.
func fillNativeGPUSensors(g *GPUAdapter) { fillNativeGPUSensorsCached(g, nil) }

// fillNativeGPUSensorsCached is fillNativeGPUSensors using (and updating) a
// per-adapter cache; nil scans and queries everything, as on first use.
func fillNativeGPUSensorsCached(g *GPUAdapter, c *nativeNodeCache) {
	fillNativeGPUSensorsWith(g, c, nativeGPUSys, time.Now())
}

// fillNativeGPUSensorsWith does the work with explicit D3DKMT calls. Without
// a cache the adapter is opened and closed here; with one, its handle stays
// open in the cache until a poll in which no query was answered (a stale
// handle after a reset or removal), or until the worker drops it.
func fillNativeGPUSensorsWith(g *GPUAdapter, c *nativeNodeCache, ops *nativeGPUOps, now time.Time) {
	if g == nil || g.Software || (g.HasTemp && g.HasFan && g.HasClock && (g.HasMemClock || g.MemClockMHz > 0)) {
		return
	}
	if c != nil {
		if c.openRetry.After(now) {
			return
		}
		needAdapter := (!g.HasTemp || !g.HasFan || (!g.HasMemClock && g.MemClockMHz <= 0)) && !c.adapterRetry.After(now)
		needNode := !g.HasClock && !c.nodeRetry.After(now)
		if !needAdapter && !needNode {
			return // nothing this driver can add: do not even open the adapter
		}
	}
	low, high, ok := parseNativeGPULUID(g.ID)
	if !ok {
		return
	}
	if !ops.available() {
		return
	}
	var h uint32
	if c != nil {
		h = c.handle
	}
	if h == 0 {
		var opened bool
		if h, opened = ops.open(low, high); !opened {
			if c != nil {
				nativeStrike(&c.openFails, &c.openRetry, now)
			}
			return
		}
		if c == nil {
			defer ops.close(h)
		} else {
			c.handle, c.closeFn, c.openFails = h, ops.close, 0
		}
	}
	asked, answered := false, false
	fillNativeGPUFromQueryCached(g, func(kind int32, data unsafe.Pointer, size uint32) bool {
		ok := ops.query(h, kind, data, size)
		asked, answered = true, answered || ok
		return ok
	}, c, now)
	if c != nil && asked && !answered {
		// Nothing answered: the handle may be stale (adapter reset or
		// removed). Open a fresh one next time.
		c.closeHandle()
	}
}

func nativeGPUClockMHz(hz uint64) (float64, bool) {
	// Zero is ambiguous (unsupported or powered down); all-ones is a common
	// unsupported sentinel. Keep either unavailable instead of inventing data.
	if hz < 1000000 || hz > 20000000000 {
		return 0, false
	}
	return float64(hz) / 1e6, true
}

func fillNativeGPUFromQuery(g *GPUAdapter, query nativeGPUQuery) {
	fillNativeGPUFromQueryCached(g, query, nil, time.Now())
}

// fillNativeGPUFromQueryCached reads what the driver offers. With a cache,
// node metadata is enumerated once and queries the driver repeatedly fails
// are paused (see nativeGPUGiveUp).
func fillNativeGPUFromQueryCached(g *GPUAdapter, query nativeGPUQuery, c *nativeNodeCache, now time.Time) {
	if (!g.HasTemp || !g.HasFan || (!g.HasMemClock && g.MemClockMHz <= 0)) && (c == nil || !c.adapterRetry.After(now)) {
		var a nativeGPUAdapterPerf
		if query(nativeGPUAdapterPerfData, unsafe.Pointer(&a), uint32(unsafe.Sizeof(a))) {
			if c != nil {
				c.adapterFails = 0
			}
			if !g.HasTemp && a.Temperature > 0 && a.Temperature <= 1500 {
				g.TempC, g.HasTemp = float64(a.Temperature)/10, true
			}
			// Zero RPM can mean fan stop or an unimplemented field; without a
			// capability bit it is safer to leave that native reading unavailable.
			if !g.HasFan && a.FanRPM > 0 && a.FanRPM <= 50000 {
				g.FanRPM, g.HasFan = float64(a.FanRPM), true
			}
			if !g.HasMemClock && g.MemClockMHz <= 0 {
				if mhz, ok := nativeGPUClockMHz(a.MemoryFrequency); ok {
					g.MemClockMHz, g.HasMemClock = mhz, true
				}
			}
			// Power is tenths of a percent, NOT watts. Never populate PowerW here.
		} else if c != nil {
			nativeStrike(&c.adapterFails, &c.adapterRetry, now)
		}
	}
	if g.HasClock || (c != nil && c.nodeRetry.After(now)) {
		return
	}
	// A video/copy engine's frequency is not the graphics clock. Identify a
	// 3D engine before reading its current frequency. Index 0 is the primary
	// physical adapter within a linked-adapter LUID; no cross-LUID fallback.
	readNode := func(ordinal uint32) (ok, answered bool) {
		node := nativeGPUNodePerf{NodeOrdinal: ordinal}
		if !query(nativeGPUNodePerfData, unsafe.Pointer(&node), uint32(unsafe.Sizeof(node))) {
			return false, false
		}
		if mhz, ok := nativeGPUClockMHz(node.Frequency); ok {
			g.ClockMHz, g.HasClock = mhz, true
			return true, true
		}
		return false, true
	}
	if c != nil && c.scanned {
		answered := false
		for _, ordinal := range c.nodes3D {
			ok, ans := readNode(ordinal)
			answered = answered || ans
			if ok {
				break
			}
		}
		if answered {
			c.nodeFails = 0
		} else {
			nativeStrike(&c.nodeFails, &c.nodeRetry, now)
		}
		return
	}
	var nodes []uint32
	done := false
	for ordinal := uint32(0); ordinal < 64; ordinal++ {
		meta := nativeGPUNodeMeta{NodeOrdinalAndAdapterIndex: ordinal}
		if !query(nativeGPUNodeMetadata, unsafe.Pointer(&meta), uint32(unsafe.Sizeof(meta))) {
			break
		}
		if meta.EngineType != 1 {
			continue
		} // DXGK_ENGINE_TYPE_3D
		nodes = append(nodes, ordinal)
		if done {
			continue // keep enumerating 3D nodes for the cache, without reading them
		}
		if ok, _ := readNode(ordinal); ok {
			done = true
			if c == nil {
				return
			}
		}
	}
	if c == nil {
		return
	}
	if len(nodes) == 0 {
		// No 3D node, no metadata support, or a driver busy at this moment
		// (metadata failing at ordinal 0): look again soon, and only after
		// nativeGPUGiveUp empty scans in a row wait nativeGPURetry.
		nativeStrike(&c.scanFails, &c.nodeRetry, now)
		return
	}
	c.scanned, c.nodes3D, c.scanFails = true, nodes, 0
}
