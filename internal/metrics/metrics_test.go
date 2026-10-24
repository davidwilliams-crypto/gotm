//go:build windows

package metrics

import (
	"os"
	"testing"
	"time"
)

func TestSamplerProducesPlausibleSnapshot(t *testing.T) {
	s := NewSampler(500, 60)
	s.perf = newPerfCounters()
	defer s.perf.close()
	s.tick(false)
	time.Sleep(700 * time.Millisecond)
	s.tick(true)

	snap := s.Latest()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if len(snap.Procs) < 10 {
		t.Fatalf("expected many processes, got %d", len(snap.Procs))
	}
	if len(snap.Cores) != s.NumCPU() {
		t.Errorf("cores=%d want %d", len(snap.Cores), s.NumCPU())
	}
	if snap.Mem.Total == 0 || snap.Mem.Used == 0 || snap.Mem.Percent <= 0 || snap.Mem.Percent > 100 {
		t.Errorf("bad memory: %+v", snap.Mem)
	}
	if snap.CPU < 0 || snap.CPU > 100 {
		t.Errorf("cpu out of range: %v", snap.CPU)
	}

	self := uint32(os.Getpid())
	var found bool
	for _, p := range snap.Procs {
		if p.PID == self {
			found = true
			if p.Name == "" || p.Mem == 0 || p.Path == "" {
				t.Errorf("self row incomplete: %+v", p)
			}
		}
		if p.CPU < 0 || p.CPU > 100.5 {
			t.Errorf("pid %d cpu out of range: %v", p.PID, p.CPU)
		}
	}
	if !found {
		t.Error("own process missing from snapshot")
	}
	t.Logf("cpu=%.1f%% mem=%.1f%% procs=%d disk=%+v net=%+v gpu=%+v",
		snap.CPU, snap.Mem.Percent, len(snap.Procs), snap.Disk, snap.Net, snap.GPU)
}

func TestHostServicesStartupDrives(t *testing.T) {
	h := ReadHostInfo()
	if h.Hostname == "" || h.NumCPU < 1 || h.CPUName == "" {
		t.Errorf("host info incomplete: %+v", h)
	}
	svcs, err := ListServices()
	if err != nil || len(svcs) < 20 {
		t.Fatalf("services: n=%d err=%v", len(svcs), err)
	}
	if len(ReadDrives()) == 0 {
		t.Error("no drives")
	}
	t.Logf("host=%+v services=%d startup=%d", h, len(svcs), len(ListStartup()))
}

func TestProtectedProcessesRefused(t *testing.T) {
	for _, pid := range []uint32{0, 4, uint32(os.Getpid())} {
		if err := KillProcess(Target{PID: pid}); err == nil {
			t.Errorf("KillProcess(%d) should be refused", pid)
		}
	}
}
