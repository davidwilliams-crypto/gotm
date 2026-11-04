//go:build windows

package metrics

import "testing"

func TestGPUDiscoveryMerge(t *testing.T) {
	dx := []gpuIdentity{{gpuStatic: gpuStatic{id: "L1", name: "card", location: "01:00.0", bus: 1}}, {gpuStatic: gpuStatic{id: "L2", name: "card", location: "02:00.0", bus: 2}}}
	pnp := []gpuIdentity{{gpuStatic: gpuStatic{name: "card", driver: "one", location: "01:00.0"}, pnp: "PCI\\one"}, {gpuStatic: gpuStatic{name: "card", driver: "two", location: "02:00.0"}, pnp: "PCI\\two"}}
	got := mergeGPUDiscovery(dx, nil, pnp)
	if len(got) != 2 || got[0].driver != "one" || got[1].driver != "two" {
		t.Fatalf("exact location merge: %+v", got)
	}
	// Identical names or PCI models without exact location must never guess.
	dx[0].location = ""
	dx[1].location = ""
	if got = mergeGPUDiscovery(dx, nil, pnp); len(got) != 4 {
		t.Fatalf("ambiguous identity merged: %+v", got)
	}
}

func TestGPUDiscoveryFallbackAndFiltering(t *testing.T) {
	dx := []gpuIdentity{{gpuStatic: gpuStatic{id: "warp", name: "Microsoft Basic Render Driver", vendorID: 0x1414, deviceID: 0x8c}}}
	kernel := []gpuIdentity{{gpuStatic: gpuStatic{id: "matrox", name: "Matrox", location: "03:00.0"}}, {gpuStatic: gpuStatic{id: "npu"}, excluded: true}}
	pnp := []gpuIdentity{{gpuStatic: gpuStatic{name: "Matrox G200eR", location: "03:00.0", driver: "4.3.1.4", ded: 32 << 20}, pnp: "PCI\\MATROX"}, {gpuStatic: gpuStatic{name: "Remote display"}, pnp: "ROOT\\REMOTE"}}
	got := mergeGPUDiscovery(dx, kernel, pnp)
	if len(got) != 2 || got[0].id != "matrox" || got[0].name != "Matrox G200eR" || got[0].driver != "4.3.1.4" || got[1].id != "PNP:ROOT\\REMOTE" {
		t.Fatalf("fallback: %+v", got)
	}
	// A fresh inventory must not replay a removed SetupAPI device.
	if got = mergeGPUDiscovery(nil, nil, nil); len(got) != 0 {
		t.Fatalf("stale inventory: %+v", got)
	}
}

func TestGPUDiscoveryLUIDOverridesAmbiguousLocations(t *testing.T) {
	dx := []gpuIdentity{{gpuStatic: gpuStatic{id: "L1", name: "DXGI"}}}
	kernel := []gpuIdentity{{gpuStatic: gpuStatic{id: "L1", name: "Kernel"}}}
	pnp := []gpuIdentity{{gpuStatic: gpuStatic{id: "L1", name: "Exact device"}, pnp: "PCI\\ONE"}}
	got := mergeGPUDiscovery(dx, kernel, pnp)
	if len(got) != 1 || got[0].name != "Exact device" {
		t.Fatalf("LUID merge: %+v", got)
	}
}

func TestGPUDiscoveryLive(t *testing.T) {
	for _, g := range enumerateGPUs() {
		if g.id == "" || g.name == "" {
			t.Fatalf("incomplete identity: %+v", g)
		}
		t.Logf("GPU: %+v", g)
	}
}

func TestGPUDiscoveryDoesNotGuessDuplicateLocations(t *testing.T) {
	dx := []gpuIdentity{{gpuStatic: gpuStatic{id: "A", location: "01:00.0"}}, {gpuStatic: gpuStatic{id: "B", location: "01:00.0"}}}
	present := []gpuIdentity{{gpuStatic: gpuStatic{name: "unresolved", location: "01:00.0"}, pnp: "PCI\\DISPLAY"}}
	got := mergeGPUDiscovery(dx, nil, present)
	if len(got) != 3 {
		t.Fatalf("guessed a duplicate PCI location: %+v", got)
	}
}

func TestGPUDiscoveryClassification(t *testing.T) {
	for _, tt := range []struct {
		name               string
		flags              uint32
		software, excluded bool
	}{
		{"hardware", 3, false, false}, {"warp", 5, true, false},
		{"indirect", 2 | 4 | 64, false, false}, {"virtual", 1 | 128, false, false},
		{"compute", 1 | 2048, false, true},
	} {
		sw, excluded := classifyKernelGPU(tt.flags)
		if sw != tt.software || excluded != tt.excluded {
			t.Errorf("%s: software=%t excluded=%t", tt.name, sw, excluded)
		}
	}
}
