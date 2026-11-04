//go:build windows

package metrics

import (
	"encoding/binary"
	"os"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

func TestNativeGPUCacheWithoutVendorLibrary(t *testing.T) {
	previous := gpuSensorsWanted.Swap(false)
	t.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	const id = "0X00000000_0X00000001"
	for _, vendor := range []uint32{0x1002, 0x10de, 0x8086} {
		r := &gpuReader{
			static:        []gpuStatic{{id: id, name: "test GPU", vendorID: vendor}},
			lastDiscovery: time.Now(),
			last:          map[string]GPUAdapter{id: {HasMemClock: true, MemClockMHz: 1600}},
			lastAt:        map[string]time.Time{id: time.Now()},
		}
		list := r.build(nil, nil, nil, nil, nil)
		if len(list) != 1 || !list[0].HasMemClock || list[0].MemClockMHz != 1600 {
			t.Fatalf("vendor %x lost native cache: %+v", vendor, list)
		}
		r.lastAt[id] = time.Now().Add(-sensorStaleAfter - time.Second)
		list = r.build(nil, nil, nil, nil, nil)
		if list[0].HasMemClock || list[0].MemClockMHz != 0 {
			t.Fatalf("vendor %x retained stale cache", vendor)
		}
	}
}

func TestNativeGPUNilReaderBuild(t *testing.T) {
	var r *gpuReader
	if got := r.build(nil, nil, nil, nil, nil); got != nil {
		t.Fatalf("nil reader returned %+v", got)
	}
}

// SDK wire fixtures catch field-offset, padding, unit, and query-kind mistakes.
func TestNativeGPUWireReadings(t *testing.T) {
	var g GPUAdapter
	query := func(kind int32, p unsafe.Pointer, size uint32) bool {
		b := unsafe.Slice((*byte)(p), int(size))
		switch kind {
		case 62:
			if size != 64 {
				t.Fatalf("adapter size %d, want 64", size)
			}
			binary.LittleEndian.PutUint64(b[8:], 1750000000)
			binary.LittleEndian.PutUint32(b[48:], 1450)
			binary.LittleEndian.PutUint32(b[52:], 850) // 85 percent, never watts
			binary.LittleEndian.PutUint32(b[56:], 635)
		case 25:
			if size != 80 {
				t.Fatalf("metadata size %d, want 80", size)
			}
			ordinal := binary.LittleEndian.Uint32(b)
			if ordinal == 0 {
				binary.LittleEndian.PutUint32(b[4:], 3)
			} else {
				binary.LittleEndian.PutUint32(b[4:], 1)
			}
		case 61:
			if size != 56 {
				t.Fatalf("node size %d, want 56", size)
			}
			if binary.LittleEndian.Uint32(b) != 1 {
				t.Fatal("clock must use identified 3D node")
			}
			binary.LittleEndian.PutUint64(b[8:], 1250500000)
		default:
			t.Fatalf("unexpected query %d", kind)
		}
		return true
	}
	fillNativeGPUFromQuery(&g, query)
	if !g.HasTemp || g.TempC != 63.5 || !g.HasFan || g.FanRPM != 1450 || !g.HasClock || g.ClockMHz != 1250.5 || g.MemClockMHz != 1750 {
		t.Fatalf("decoded sensors: %+v", g)
	}
	if g.HasPower || g.PowerW != 0 {
		t.Fatal("native percent must not become power watts")
	}
}

func TestNativeGPUUnsupportedAndInvalid(t *testing.T) {
	for _, failed := range []bool{false, true} {
		var g GPUAdapter
		fillNativeGPUFromQuery(&g, func(kind int32, p unsafe.Pointer, size uint32) bool {
			if failed { // Even populated buffers must be ignored after a failed query.
				b := unsafe.Slice((*byte)(p), int(size))
				for i := range b {
					b[i] = 0xff
				}
			}
			return !failed
		})
		if !reflect.DeepEqual(g, GPUAdapter{}) {
			t.Fatalf("unsupported sensors leaked: %+v", g)
		}
	}
	var g GPUAdapter
	fillNativeGPUFromQuery(&g, func(kind int32, p unsafe.Pointer, size uint32) bool {
		b := unsafe.Slice((*byte)(p), int(size))
		if kind == 25 {
			binary.LittleEndian.PutUint32(b[4:], 1)
			return true
		}
		for i := range b {
			b[i] = 0xff
		}
		return true
	})
	if !reflect.DeepEqual(g, GPUAdapter{}) {
		t.Fatalf("sentinel readings leaked: %+v", g)
	}
}

func TestNativeGPUPreservesVendorAndSeparatesClocks(t *testing.T) {
	vendor := GPUAdapter{HasTemp: true, TempC: 55, HasFan: true, FanPct: 0, HasClock: true, ClockMHz: 900, MemClockMHz: 800, HasPower: true, PowerW: 42}
	got := vendor
	fillNativeGPUFromQuery(&got, func(kind int32, p unsafe.Pointer, size uint32) bool {
		b := unsafe.Slice((*byte)(p), int(size))
		for i := range b {
			b[i] = 1
		}
		return true
	})
	if !reflect.DeepEqual(got, vendor) {
		t.Fatalf("vendor readings overwritten: %+v", got)
	}
	var g GPUAdapter
	fillNativeGPUFromQuery(&g, func(kind int32, p unsafe.Pointer, size uint32) bool {
		if kind != 62 {
			return false
		}
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(p), int(size))[8:], 1600000000)
		return true
	})
	if g.HasClock || g.ClockMHz != 0 || !g.HasMemClock || g.MemClockMHz != 1600 {
		t.Fatalf("memory became core clock: %+v", g)
	}
}

func TestNativeGPUPreservesVendorZeroMemoryClock(t *testing.T) {
	g := GPUAdapter{HasMemClock: true}
	fillNativeGPUFromQuery(&g, func(kind int32, p unsafe.Pointer, size uint32) bool {
		if kind != 62 {
			return false
		}
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(p), int(size))[8:], 1600000000)
		return true
	})
	if !g.HasMemClock || g.MemClockMHz != 0 {
		t.Fatalf("successful vendor zero overwritten: %+v", g)
	}
}

func TestNativeGPUClockRange(t *testing.T) {
	for _, tc := range []struct {
		hz    uint64
		want  float64
		valid bool
	}{
		{0, 0, false}, {999999, 0, false}, {1000000, 1, true},
		{20000000000, 20000, true}, {20000000001, 0, false}, {^uint64(0), 0, false},
	} {
		got, valid := nativeGPUClockMHz(tc.hz)
		if got != tc.want || valid != tc.valid {
			t.Fatalf("%d Hz: got %g/%v, want %g/%v", tc.hz, got, valid, tc.want, tc.valid)
		}
	}
}

// Opt-in because querying live hardware can wake an otherwise sleeping GPU.
func TestNativeGPULiveSmoke(t *testing.T) {
	if os.Getenv("GOTM_GPU_SMOKE") != "1" {
		t.Skip("set GOTM_GPU_SMOKE=1 for live display-driver queries")
	}
	for _, st := range enumerateGPUs() {
		if st.software {
			continue
		}
		g := GPUAdapter{ID: st.id, Name: st.name}
		fillNativeGPUSensors(&g)
		t.Logf("%s [%s]: temp=%v/%.1f fan=%v/%.0f core=%v/%.1f mem=%v/%.1f", g.Name, g.ID, g.HasTemp, g.TempC, g.HasFan, g.FanRPM, g.HasClock, g.ClockMHz, g.HasMemClock, g.MemClockMHz)
		if g.HasPower {
			t.Fatal("native adapter power percentage cannot supply watts")
		}
	}
}

func TestNativeGPUExactLUID(t *testing.T) {
	lo, hi, ok := parseNativeGPULUID("0X89ABCDEF_0X01234567")
	if !ok || lo != 0x01234567 || uint32(hi) != 0x89abcdef {
		t.Fatalf("wrong LUID: %x %x %v", lo, hi, ok)
	}
	for _, s := range []string{"", "0_1", "0X00000000_0X00000001extra", "0X00000000_0X0000000G"} {
		if _, _, ok := parseNativeGPULUID(s); ok {
			t.Fatalf("accepted malformed LUID %q", s)
		}
	}
}
