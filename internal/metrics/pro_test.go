//go:build windows

package metrics

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConnectionsSeeOurOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	want := ln.Addr().(*net.TCPAddr).Port

	var found *Connection
	for _, c := range ReadConnections() {
		c := c
		if c.Proto == "TCP" && c.LocalPort == want && c.State == "Listen" {
			found = &c
		}
	}
	if found == nil {
		t.Fatalf("listener on port %d not in connection table", want)
	}
	if found.PID != uint32(os.Getpid()) {
		t.Errorf("owner pid = %d, want %d", found.PID, os.Getpid())
	}
	if found.Exposure != "Local only" {
		t.Errorf("exposure = %q, want Local only for 127.0.0.1", found.Exposure)
	}
}

func TestInstalledAppsAndDrivers(t *testing.T) {
	apps := ReadInstalledApps()
	if len(apps) < 3 {
		t.Fatalf("expected several installed apps, got %d", len(apps))
	}
	for _, a := range apps {
		if a.Name == "" {
			t.Fatal("app with empty name")
		}
	}
	drv, err := ReadDrivers()
	if err != nil || len(drv) < 30 {
		t.Fatalf("drivers: n=%d err=%v", len(drv), err)
	}
	withCompany := 0
	for _, d := range drv {
		if d.Company != "" {
			withCompany++
		}
	}
	if withCompany < 10 {
		t.Errorf("version info resolved for only %d drivers", withCompany)
	}
}

func TestTopologyAndClocks(t *testing.T) {
	topo := ReadTopology()
	if topo.Logical < 1 || topo.Cores < 1 || topo.Packages < 1 {
		t.Fatalf("topology: %+v", topo)
	}
	if topo.Cores > topo.Logical {
		t.Errorf("more cores than logical processors: %+v", topo)
	}
	cur, max := ReadCPUClocks()
	if len(cur) != topo.Logical || len(max) != topo.Logical {
		t.Fatalf("clock vectors %d/%d, logical %d", len(cur), len(max), topo.Logical)
	}
	for i := range cur {
		if max[i] == 0 {
			t.Errorf("cpu %d max clock is 0", i)
		}
	}
	if p := ReadPowerStatic(); p.Plan == "" {
		t.Log("power plan name unavailable on this host")
	}
}

func TestDriveHealthDoesNotPanicAndNamesDrives(t *testing.T) {
	ds := ReadDriveHealth()
	if len(ds) == 0 {
		t.Skip("no physical drive could be opened (needs a standard Windows disk)")
	}
	for _, d := range ds {
		if d.Model == "" {
			t.Errorf("drive %d has no model", d.Index)
		}
		t.Logf("drive %d: %s %s temp=%v smart=%v used=%d%% poh=%.0f", d.Index, d.Model, d.Bus, d.TempC, d.HasSMART, d.UsedPct, d.PowerOnHrs)
	}
}

func TestDiskScanSizesAreExact(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.bin", 1000)
	write("sub/b.bin", 2_000_000) // > 1 MiB so it can be a "largest file"
	write("sub/deep/c.bin", 500)
	write("empty/.keep", 0)

	var prog ScanProgress
	res := ScanDir(context.Background(), root, &prog)
	if want := int64(1000 + 2_000_000 + 500); res.Total != want {
		t.Fatalf("total = %d, want %d", res.Total, want)
	}
	if res.Files != 4 {
		t.Errorf("files = %d, want 4", res.Files)
	}
	if res.Entries[0].Name != "sub" || res.Entries[0].Size != 2_000_500 || !res.Entries[0].IsDir {
		t.Errorf("largest child wrong: %+v", res.Entries[0])
	}
	if len(res.TopFiles) != 1 || res.TopFiles[0].Name != "b.bin" {
		t.Errorf("top files wrong: %+v", res.TopFiles)
	}
}

func TestDiskScanCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := ScanDir(ctx, os.Getenv("SystemRoot"), nil)
	if !res.Cancelled {
		t.Error("cancelled scan not reported as cancelled")
	}
}

func TestCancelledScanDoesNotReadRootFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.bin"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := ScanDir(ctx, root, nil)
	if !res.Cancelled || res.Files != 0 || len(res.Entries) != 0 {
		t.Fatalf("cancelled scan still read files: %+v", res)
	}
}

func TestRecorderClearAcceptsFirstNewSample(t *testing.T) {
	r := NewRecorder(time.Hour, 2*time.Second, "")
	r.Add(snap(1_700_000_000_000, 10))
	r.Clear()
	r.Add(snap(1_700_000_001_000, 20))
	if r.Len() != 1 {
		t.Fatal("clear kept the old sample deadline")
	}
}

func TestRecorderReloadSortsAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	body := fmt.Sprintf("{\"t\":%d,\"cpu\":20}\n{\"t\":%d,\"cpu\":10}\n{\"t\":%d,\"cpu\":20}\n", now-1000, now-2000, now-1000)
	if err := os.WriteFile(filepath.Join(dir, "today.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(time.Hour, time.Second, dir)
	r.LoadDisk(time.Hour)
	r.LoadDisk(time.Hour)
	got := r.Samples(0, 0)
	if len(got) != 2 || got[0].CPU != 10 || got[1].CPU != 20 {
		t.Fatalf("bad replay order or duplicates: %+v", got)
	}
}

func TestFutureReplayDoesNotBlockLiveRecording(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	body := fmt.Sprintf("{\"t\":%d,\"cpu\":20}\n", now+300_000)
	if err := os.WriteFile(filepath.Join(dir, "future.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(time.Hour, 2*time.Second, dir)
	r.LoadDisk(time.Hour)
	r.Add(snap(now, 10))
	got := r.Samples(0, 0)
	if len(got) != 1 || got[0].CPU != 10 {
		t.Fatalf("future history blocked current data: %+v", got)
	}
}

func snap(t int64, cpu float64, procs ...Proc) *Snapshot {
	return &Snapshot{T: t, CPU: cpu, Mem: MemInfo{Percent: 50}, Procs: procs}
}

func TestRecorderRingRangeNearestAndMarks(t *testing.T) {
	r := NewRecorder(2*time.Minute, time.Second, "")
	base := int64(1_700_000_000_000)
	for i := 0; i < 200; i++ { // 200 s of 1 Hz samples, ring holds 120
		r.Add(snap(base+int64(i)*1000, float64(i%100),
			Proc{PID: 10, Name: "busy.exe", CPU: float64(i % 100)},
			Proc{PID: 0, Name: "System Idle Process", CPU: 90}))
	}
	if r.Len() != 120 {
		t.Fatalf("ring len = %d, want 120", r.Len())
	}
	got := r.Samples(base+150_000, base+160_000)
	if len(got) != 11 || got[0].T != base+150_000 {
		t.Fatalf("range returned %d samples starting %d", len(got), got[0].T-base)
	}
	s, ok := r.Nearest(base + 150_400)
	if !ok || s.T != base+150_000 {
		t.Errorf("nearest = %d", s.T-base)
	}
	if len(s.Top) != 1 || s.Top[0].Name != "busy.exe" {
		t.Errorf("idle process leaked into top list: %+v", s.Top)
	}
	r.AddMark("spike", base+150_000)
	if m := r.Marks(); len(m) != 1 || m[0].Label != "spike" {
		t.Errorf("marks: %+v", m)
	}

	csv := filepath.Join(t.TempDir(), "rec.csv")
	if err := r.ExportCSV(csv, 0, 0); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(csv)
	if lines := strings.Count(string(b), "\n"); lines != 121 {
		t.Errorf("csv lines = %d, want 121", lines)
	}
}

func TestRecorderRespectsIntervalAndPersists(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(time.Hour, 2*time.Second, dir)
	r.SetPersist(true)
	// Model a completed recording, ending before replay starts. Future-dated
	// samples are deliberately excluded from the historical window.
	now := time.Now().Add(-10 * time.Second).UnixMilli()
	for i := 0; i < 10; i++ { // 1 Hz input, 2 s interval -> every other sample kept
		r.Add(snap(now+int64(i)*1000, 10))
	}
	if r.Len() != 5 {
		t.Fatalf("kept %d samples, want 5", r.Len())
	}
	r.SetPersist(false) // flush/close

	r2 := NewRecorder(time.Hour, 2*time.Second, dir)
	if n := r2.LoadDisk(time.Hour); n != 5 {
		t.Fatalf("reloaded %d samples, want 5", n)
	}
}

func TestSensorTreeShapeAndMinMax(t *testing.T) {
	h := NewSensorHub()
	host := ReadHostInfo()
	s1 := &Snapshot{CPU: 10, Cores: []float64{10, 20}, Mem: MemInfo{Percent: 40, Used: 1 << 30, Available: 1 << 30}}
	s2 := &Snapshot{CPU: 50, Cores: []float64{50, 60}, Mem: MemInfo{Percent: 40, Used: 1 << 30, Available: 1 << 30}}
	h.Update(s1, host)
	rows := h.Update(s2, host)

	var total *SensorRow
	groups := map[string]bool{}
	for i := range rows {
		if rows[i].ID == "cpu.load.total" {
			total = &rows[i]
		}
		if rows[i].Group {
			groups[rows[i].ID] = true
		}
	}
	for _, id := range []string{"pc", "cpu", "cpu.temp", "cpu.load", "cpu.clk", "cpu.pwr", "mem", "power"} {
		if !groups[id] {
			t.Errorf("missing group %q", id)
		}
	}
	if total == nil || total.Value != "50.0 %" || total.Min != "10.0 %" || total.Max != "50.0 %" {
		t.Fatalf("cpu total row: %+v", total)
	}
}
