//go:build windows

package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestBuildGPUsFromSyntheticCounters(t *testing.T) {
	static := []gpuStatic{
		{id: "0X00000000_0X00015516", name: "AMD Radeon(TM) Graphics", vendorID: 0x1002, ded: 512 << 20, shr: 32 << 30, driver: "32.0.21030.31", location: "11:00.0"},
		{id: "0X00000000_0X000175F6", name: "NVIDIA GeForce RTX 5080", vendorID: 0x10DE, ded: 16 << 30, shr: 32 << 30, driver: "32.0.16.1714"},
		{id: "0X00000000_0X00000001", name: "Microsoft Basic Render Driver", vendorID: 0x1414, software: true},
	}
	eng := []PDHInstanceValue{
		{Instance: "pid_100_luid_0x00000000_0x000175F6_phys_0_eng_0_engtype_3D", Value: 30},
		{Instance: "pid_200_luid_0x00000000_0x000175F6_phys_0_eng_0_engtype_3D", Value: 25},
		{Instance: "pid_100_luid_0x00000000_0x000175F6_phys_0_eng_5_engtype_Copy", Value: 3},
		{Instance: "pid_100_luid_0x00000000_0x000175F6_phys_0_eng_6_engtype_Copy", Value: 1},
		{Instance: "pid_300_luid_0x00000000_0x000175F6_phys_0_eng_9_engtype_VideoDecode", Value: 12},
		{Instance: "pid_100_luid_0x00000000_0x00015516_phys_0_eng_0_engtype_3D", Value: 80},
		{Instance: "pid_100_luid_0x00000000_0x00015516_phys_0_eng_1_engtype_Compute_0", Value: 140}, // over 100 is clamped
	}
	ded := []PDHInstanceValue{
		{Instance: "luid_0x00000000_0x000175F6_phys_0", Value: 2 << 30},
		{Instance: "luid_0x00000000_0x00015516_phys_0", Value: 33 << 20},
	}
	shr := []PDHInstanceValue{{Instance: "luid_0x00000000_0x000175F6_phys_0", Value: 700 << 20}}
	pded := []PDHInstanceValue{
		{Instance: "pid_100_luid_0x00000000_0x000175F6_phys_0", Value: 1500 << 20},
		{Instance: "pid_200_luid_0x00000000_0x000175F6_phys_0", Value: 400 << 20},
	}
	pshr := []PDHInstanceValue{{Instance: "pid_200_luid_0x00000000_0x000175F6_phys_0", Value: 90 << 20}}

	gpus := buildGPUs(static, eng, ded, shr, pded, pshr)
	if got := summarizeGPUs(gpus); got.Percent != 100 || got.MemBytes != (2<<30)+(33<<20) || !got.OK {
		t.Errorf("summary must use busiest visible adapter and their total memory: %+v", got)
	}
	if got := summarizeGPUs(nil); got != (GPUInfo{}) {
		t.Errorf("no adapters should have no GPU summary: %+v", got)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d adapters, want 2 (software adapter hidden): %+v", len(gpus), gpus)
	}
	amd, nv := gpus[0], gpus[1]
	if amd.Name != "AMD Radeon(TM) Graphics" || amd.Vendor != "AMD" || amd.Location != "11:00.0" {
		t.Errorf("amd identity: %+v", amd)
	}
	if amd.Pct != 100 || len(amd.Engines) != 2 || amd.Engines[0].Name != "3D" || amd.Engines[1].Name != "Compute 0" || amd.Engines[1].Pct != 100 {
		t.Errorf("amd engines: %+v", amd.Engines)
	}
	if amd.DedicatedUsed != 33<<20 || amd.DedicatedTotal != 512<<20 {
		t.Errorf("amd memory: %d / %d", amd.DedicatedUsed, amd.DedicatedTotal)
	}
	if nv.Driver != "617.14 (32.0.16.1714)" {
		t.Errorf("nvidia driver string %q", nv.Driver)
	}
	var names []string
	for _, e := range nv.Engines {
		names = append(names, e.Name)
	}
	want := []string{"3D", "Copy 0", "Copy 1", "Video Decode"}
	if len(names) != len(want) {
		t.Fatalf("nvidia engines %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("engine %d = %q, want %q", i, names[i], want[i])
		}
	}
	if nv.Engines[0].Pct != 55 || nv.Pct != 55 {
		t.Errorf("3D engine sums across processes: %+v pct=%v", nv.Engines[0], nv.Pct)
	}
	if nv.DedicatedUsed != 2<<30 || nv.SharedUsed != 700<<20 {
		t.Errorf("nvidia memory %d / %d", nv.DedicatedUsed, nv.SharedUsed)
	}
	if len(nv.Procs) < 3 || nv.Procs[0].PID != 100 || nv.Procs[0].Dedicated != 1500<<20 || nv.Procs[1].PID != 200 || nv.Procs[1].Shared != 90<<20 {
		t.Errorf("nvidia processes: %+v", nv.Procs)
	}
	var found300 bool
	for _, p := range nv.Procs {
		if p.PID == 300 && p.Pct == 12 {
			found300 = true
		}
	}
	if !found300 {
		t.Error("engine-only process 300 missing")
	}
}

func TestParseLocationAndDriverVersion(t *testing.T) {
	if got := parseLocation(`@System32\drivers\pci.sys,#65536;PCI bus %1, device %2, function %3;(17,0,0)`); got != "11:00.0" {
		t.Errorf("location = %q", got)
	}
	if got := nvidiaMarketingVersion("32.0.15.6094"); got != "560.94" {
		t.Errorf("nvidia version = %q", got)
	}
	if got := prettyEngine("VideoEncode"); got != "Video Encode" {
		t.Errorf("pretty = %q", got)
	}
}

func TestLiveGPUsOnThisMachine(t *testing.T) {
	SetGPUSensorsWanted(true) // as when a GPU view is on screen
	defer SetGPUSensorsWanted(false)
	pc := newPerfCounters()
	if pc == nil {
		t.Skip("PDH unavailable")
	}
	defer pc.close()
	time.Sleep(1100 * time.Millisecond)
	ps := pc.read()
	if len(ps.GPUs) == 0 {
		t.Skip("no GPUs reported")
	}
	physical := 0
	for _, g := range ps.GPUs {
		t.Logf("%s [%s] id=%s ded %d/%d shr %d/%d pct %.1f engines=%d procs=%d drv=%q loc=%q temp=%v/%v power=%v clock=%v",
			g.Name, g.Vendor, g.ID, g.DedicatedUsed, g.DedicatedTotal, g.SharedUsed, g.SharedTotal, g.Pct,
			len(g.Engines), len(g.Procs), g.Driver, g.Location, g.HasTemp, g.TempC, g.HasPower, g.HasClock)
		if g.Name == "" {
			t.Errorf("adapter missing identity: %+v", g)
		}
		if virtualDisplayAdapter(g.ID, g.Vendor == "Microsoft") {
			continue // Hyper-V video / remote display: no memory sizes by design
		}
		physical++
		if g.DedicatedTotal+g.SharedTotal == 0 {
			t.Errorf("adapter missing memory: %+v", g)
		}
	}
	if physical == 0 {
		t.Skip("only virtual or remote display adapters on this machine")
	}
}

// virtualDisplayAdapter recognises adapters that are not PCI hardware: VM
// synthetic video (VMBus), remote/indirect displays (SWD, ROOT) and
// Microsoft's own display transports. They have no PCI address or memory size.
func virtualDisplayAdapter(id string, microsoft bool) bool {
	u := strings.ToUpper(id)
	return microsoft || strings.HasPrefix(u, `PNP:VMBUS\`) || strings.HasPrefix(u, `PNP:SWD\`) || strings.HasPrefix(u, `PNP:ROOT\`)
}

// The kernel's PCI address must agree with the registry's for each adapter.
func TestAdapterAddressMatchesRegistry(t *testing.T) {
	physical := 0
	for _, st := range enumerateGPUs() {
		if st.software || virtualDisplayAdapter(st.id, st.vendorID == 0x1414) {
			t.Logf("%s (%s): virtual or remote display adapter, no PCI address", st.name, st.id)
			continue
		}
		physical++
		reg := pciLocation(st.vendorID, st.deviceID)
		t.Logf("%s: kernel %s (bus %d), registry %s", st.name, st.location, st.bus, reg)
		if st.bus < 0 {
			t.Errorf("%s: no PCI address from D3DKMT", st.name)
		} else if reg != "" && reg != st.location {
			t.Errorf("%s: kernel says %s, registry %s", st.name, st.location, reg)
		}
	}
	if physical == 0 {
		t.Skip("only virtual or remote display adapters on this machine")
	}
}

func TestVirtualDisplayAdapterClassification(t *testing.T) {
	for _, tc := range []struct {
		id        string
		microsoft bool
		want      bool
	}{
		{`PNP:VMBUS\{DA0A7802-E377-4AAC-8E77-0558EB1073F8}\{5620E0C7}`, false, true},
		{`PNP:SWD\REMOTEDISPLAYENUM\RDPIDD_INDIRECTDISPLAY&SESSIONID_0002`, false, true},
		{"0X00000000_0X0000D1E2", true, true},
		{"0X00000000_0X0000D1E2", false, false},
		{`PNP:PCI\VEN_102B&DEV_0534\4&1`, false, false}, // a real PCI device without a LUID is still checked
	} {
		if got := virtualDisplayAdapter(tc.id, tc.microsoft); got != tc.want {
			t.Errorf("%s microsoft=%v: %v, want %v", tc.id, tc.microsoft, got, tc.want)
		}
	}
}
