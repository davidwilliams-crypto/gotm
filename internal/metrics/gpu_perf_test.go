//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"
	"unicode/utf16"
)

func gpuPerfFixture() (static []gpuStatic, eng, ded, shr, pded, pshr []PDHInstanceValue) {
	for adapter := 1; adapter <= 2; adapter++ {
		id := fmt.Sprintf("0X00000000_0X%08X", adapter)
		static = append(static, gpuStatic{id: id, name: "Benchmark GPU", vendorID: 0x8086, bus: adapter})
		inst := fmt.Sprintf("luid_0x00000000_0x%08x_phys_0", adapter)
		ded = append(ded, PDHInstanceValue{Instance: inst, Value: 1 << 30})
		shr = append(shr, PDHInstanceValue{Instance: inst, Value: 1 << 28})
		for pid := 1; pid <= 64; pid++ {
			base := fmt.Sprintf("pid_%d_%s", pid, inst)
			pded = append(pded, PDHInstanceValue{Instance: base, Value: 16 << 20})
			pshr = append(pshr, PDHInstanceValue{Instance: base, Value: 4 << 20})
			for engine, typ := range []string{"3D", "Copy", "VideoDecode", "VideoEncode", "Compute_0", "Compute_1"} {
				eng = append(eng, PDHInstanceValue{Instance: fmt.Sprintf("%s_eng_%d_engtype_%s", base, engine, typ), Value: 0.1})
			}
		}
	}
	return
}

func BenchmarkGPUAssembly(b *testing.B) {
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := buildGPUs(static, eng, ded, shr, pded, pshr); len(got) != 2 || len(got[0].Procs) != 64 {
			b.Fatal("invalid fixture")
		}
	}
}

func BenchmarkGPUReaderHidden(b *testing.B) {
	previous := gpuSensorsWanted.Swap(false)
	b.Cleanup(func() { gpuSensorsWanted.Store(previous) })
	static, eng, ded, shr, pded, pshr := gpuPerfFixture()
	r := &gpuReader{static: static, seen: map[string]bool{static[0].id: true, static[1].id: true}, lastDiscovery: time.Now()}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := r.build(eng, ded, shr, pded, pshr); len(got) != 2 {
			b.Fatal("invalid fixture")
		}
	}
}

func BenchmarkPDHInstanceDecode(b *testing.B) {
	u := utf16.Encode([]rune("pid_12345_luid_0x00000000_0x00001234_phys_0_eng_0_engtype_3D"))
	buf := make([]byte, (len(u)+1)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], c)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if utf16At(buf, 0) == "" {
			b.Fatal("empty name")
		}
	}
}

func TestGPUDiscoveryCoalescesUnknownDevices(t *testing.T) {
	static, eng, ded, _, _, _ := gpuPerfFixture()
	eng = append(eng, PDHInstanceValue{Instance: "pid_9_luid_0x00000000_0x00000099_phys_0_eng_0_engtype_Compute"})
	now := time.Now()
	r := &gpuReader{static: static, seen: map[string]bool{}}
	calls := 0
	discover := func() []gpuStatic { calls++; return static }
	r.refreshGPUDiscovery(now, eng, ded, discover)
	if calls != 1 {
		t.Fatalf("periodic and unknown-device discovery ran %d times", calls)
	}
	r.refreshGPUDiscovery(now.Add(time.Second), eng, ded, discover)
	if calls != 1 {
		t.Fatal("unchanged excluded device caused another rescan")
	}
	eng = append(eng, PDHInstanceValue{Instance: "pid_9_luid_0x00000000_0x00000098_phys_0_eng_0_engtype_3D"})
	r.refreshGPUDiscovery(now.Add(2*time.Second), eng, ded, discover)
	if calls != 2 {
		t.Fatal("new device did not trigger discovery")
	}
	if r.lastDiscovery != now.Add(2*time.Second) {
		t.Fatal("on-demand discovery did not update freshness")
	}
	r.refreshGPUDiscovery(now.Add(33*time.Second), eng, ded, discover)
	if calls != 3 {
		t.Fatal("periodic removal detection stopped")
	}
}

func TestPDHDecodePreservesUnicodeAndBounds(t *testing.T) {
	for _, want := range []string{"", "GPU Engine", "温度 🌡️", "a\x7fb"} {
		u := utf16.Encode([]rune(want))
		buf := make([]byte, 1+2*(len(u)+1))
		for i, c := range u {
			binary.LittleEndian.PutUint16(buf[1+i*2:], c)
		}
		if got := utf16At(buf, 1); got != want {
			t.Fatalf("decode %q, want %q", got, want)
		}
	}
	if got := utf16At([]byte{'a', 0, 'b'}, 0); got != "a" {
		t.Fatalf("truncated word: %q", got)
	}
}
