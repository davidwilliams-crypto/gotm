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
// driver stays loaded) and which queries it does not implement. The worker
// drops it whenever the inventory changes (driver update, hot-plug).
type nativeNodeCache struct {
	scanned       bool     // node metadata has been enumerated
	nodes3D       []uint32 // ordinals of 3D engine nodes, in order
	adapterFails  int      // consecutive failed adapter performance queries
	nodeFails     int      // consecutive failed node performance queries
	adapterRetry  time.Time
	nodeRetry     time.Time
	unsupportedAt time.Time // the LUID could not be opened
}

const (
	// nativeGPUGiveUp failures in a row mark a query unsupported until
	// nativeGPURetry has passed: a driver that cannot answer is not asked every
	// sample, and one that was merely busy is asked again later.
	nativeGPUGiveUp = 3
	nativeGPURetry  = 5 * time.Minute
)

// fillNativeGPUSensors fills missing readings for this exact DXGI/PDH LUID.
// The caller gates sampling on the GPU detail view to avoid waking idle GPUs.
func fillNativeGPUSensors(g *GPUAdapter) { fillNativeGPUSensorsCached(g, nil) }

// fillNativeGPUSensorsCached is fillNativeGPUSensors using (and updating) a
// per-adapter cache; nil scans and queries everything, as on first use.
func fillNativeGPUSensorsCached(g *GPUAdapter, c *nativeNodeCache) {
	if g == nil || g.Software || (g.HasTemp && g.HasFan && g.HasClock && (g.HasMemClock || g.MemClockMHz > 0)) {
		return
	}
	now := time.Now()
	if c != nil {
		if !c.unsupportedAt.IsZero() && now.Sub(c.unsupportedAt) < nativeGPURetry {
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
	if procD3DKMTOpenAdapterFromLuid.Find() != nil || procD3DKMTQueryAdapterInfo.Find() != nil || procD3DKMTCloseAdapter.Find() != nil {
		return
	}

	open := struct {
		Low     uint32
		High    int32
		Adapter uint32
	}{Low: low, High: high}
	if status, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); int32(status) < 0 || open.Adapter == 0 {
		if c != nil {
			c.unsupportedAt = now
		}
		return
	}
	defer func() {
		closeArg := open.Adapter
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&closeArg)))
	}()
	fillNativeGPUFromQueryCached(g, func(kind int32, data unsafe.Pointer, size uint32) bool {
		q := struct {
			Adapter uint32
			Type    int32
			Data    unsafe.Pointer
			Size    uint32
		}{open.Adapter, kind, data, size}
		status, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q)))
		runtime.KeepAlive(data)
		return int32(status) >= 0
	}, c, now)
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
			if c.adapterFails++; c.adapterFails >= nativeGPUGiveUp {
				c.adapterFails, c.adapterRetry = 0, now.Add(nativeGPURetry)
			}
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
		} else if c.nodeFails++; c.nodeFails >= nativeGPUGiveUp {
			c.nodeFails, c.nodeRetry = 0, now.Add(nativeGPURetry)
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
		// No 3D node (or no metadata support): look again only after a while.
		c.nodeRetry = now.Add(nativeGPURetry)
		return
	}
	c.scanned, c.nodes3D = true, nodes
}
