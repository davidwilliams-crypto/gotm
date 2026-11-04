//go:build windows

package metrics

import "testing"

func TestVendorGPUMatchingRejectsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bus           int
		local, remote []vendorGPUIdentity
		want          int
	}{
		{"unique buses despite identical names", 2, []vendorGPUIdentity{{1, "GPU"}, {2, "GPU"}}, []vendorGPUIdentity{{2, "GPU"}, {1, "GPU"}}, 0},
		{"duplicate remote bus and name", 2, []vendorGPUIdentity{{2, "GPU"}}, []vendorGPUIdentity{{2, "GPU"}, {2, "GPU"}}, -1},
		{"duplicate local bus and name", 2, []vendorGPUIdentity{{2, "GPU"}, {2, "GPU"}}, []vendorGPUIdentity{{2, "GPU"}}, -1},
		{"duplicate names without buses", -1, []vendorGPUIdentity{{-1, "GPU"}, {-1, "GPU"}}, []vendorGPUIdentity{{-1, "GPU"}, {-1, "GPU"}}, -1},
		{"unique name without bus", -1, []vendorGPUIdentity{{-1, "GPU"}}, []vendorGPUIdentity{{4, "Other"}, {5, "GPU"}}, 1},
		{"known conflicting buses", 1, []vendorGPUIdentity{{1, "GPU"}}, []vendorGPUIdentity{{2, "GPU"}}, -1},
		{"same count is not identity", -1, []vendorGPUIdentity{{-1, "GPU"}}, []vendorGPUIdentity{{-1, "Other"}}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchVendorGPU(tc.bus, "GPU", tc.local, tc.remote); got != tc.want {
				t.Fatalf("matched %d, want %d", got, tc.want)
			}
		})
	}
}

func TestADLMatchingRejectsDuplicateAdapters(t *testing.T) {
	l := &adlLib{ok: true, adapters: []adlAdapter{{index: 0, bus: 2, name: "AMD GPU"}, {index: 1, bus: 2, name: "AMD GPU"}}}
	if _, ok := l.find(2, "AMD GPU"); ok {
		t.Fatal("duplicate ADL cards must not match first")
	}
	l.adapters = l.adapters[:1]
	peers := []gpuStatic{{vendorID: 0x1002, bus: 2, name: "AMD GPU"}, {vendorID: 0x1002, bus: 2, name: "AMD GPU"}}
	if _, ok := l.find(2, "AMD GPU", peers...); ok {
		t.Fatal("duplicate discovered cards must not share one ADL adapter")
	}
}

func TestADLDedupDoesNotHideBusAmbiguity(t *testing.T) {
	a := adlAdapterInfo{Index: 1, Bus: 2, VendorID: adlVendorAMD, Present: 1}
	copy(a.Name[:], "AMD GPU")
	b := a
	b.Index = 2
	for _, knownIDs := range []bool{false, true} {
		if knownIDs {
			copy(a.UDID[:], "card-A")
			copy(b.UDID[:], "card-B")
		}
		cards := adlPickAdapters([]adlAdapterInfo{a, b})
		if len(cards) != 2 {
			t.Fatalf("different adapters sharing bus collapsed: %+v", cards)
		}
		l := &adlLib{ok: true, adapters: cards}
		if _, ok := l.find(2, "AMD GPU"); ok {
			t.Fatal("bus ambiguity lost before matching")
		}
	}
}
