//go:build windows

package ui

import (
	"gotm/internal/metrics"
	"path/filepath"
	"testing"
	"time"
)

func TestStartupPublishesCPUDetailsBeforeNetworkLookup(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	initialized := false
	out := startStartup(func() metrics.HostInfo { return metrics.HostInfo{CPUName: "Test CPU", NumCPU: 64} }, func() { initialized = true }, func() (string, string) { <-blocked; return "test-ip", "" })
	select {
	case first := <-out:
		if first.host.CPUName != "Test CPU" || !initialized || first.localIP != "" {
			t.Fatalf("incomplete first result: %+v", first)
		}
	case <-time.After(time.Second):
		t.Fatal("network lookup delayed CPU details")
	}
}

func TestSnapshotMailboxKeepsNewestWithoutLosingSensorExtrema(t *testing.T) {
	a := &App{sensors: metrics.NewSensorHubDeferred(), rec: metrics.NewRecorder(time.Hour, time.Second, "")}
	a.rec.SetEnabled(true)
	for i, cpu := range []float64{5, 95, 20} {
		update := a.prepareSnapshot(&metrics.Snapshot{T: int64(i+1) * 2000, CPU: cpu, CPUFreq: []float64{2200}}, metrics.HostInfo{})
		queued := a.updates.offer(update)
		if queued != (i == 0) {
			t.Fatalf("offer %d queued=%t", i, queued)
		}
	}
	newest := a.updates.take()
	if newest == nil || newest.snap.CPU != 20 {
		t.Fatal("UI did not receive newest snapshot")
	}
	found := false
	for _, row := range newest.rows {
		if row.ID == "cpu.load.total" {
			found = true
			if row.Min != "5.0 %" || row.Max != "95.0 %" {
				t.Fatalf("lost skipped sample extrema: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("CPU row missing")
	}
	if a.rec.Len() != 3 {
		t.Fatalf("recording lost coalesced samples: %d", a.rec.Len())
	}
	if !a.updates.offer(&snapshotUpdate{}) {
		t.Fatal("mailbox did not rearm after delivery")
	}
}

var sensorStartupSink *metrics.SensorHub

func BenchmarkSensorStartupMainThread(b *testing.B) {
	b.Run("synchronous", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sensorStartupSink = metrics.NewSensorHub()
		}
	})
	b.Run("deferred", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sensorStartupSink = metrics.NewSensorHubDeferred()
		}
	})
}

func TestBlockedStartupDoesNotGateLiveSamples(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	startup := startStartup(func() metrics.HostInfo { <-blocked; return metrics.HostInfo{CPUName: "full"} }, func() {}, func() (string, string) { return "", "" })
	samples := make(chan *metrics.Snapshot, 1)
	done := make(chan struct{})
	defer close(done)
	received := make(chan metrics.HostInfo, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		pumpSnapshots(samples, startup, done, metrics.HostInfo{NumCPU: 64}, func(startupInfo) {}, func(s *metrics.Snapshot, h metrics.HostInfo) { received <- h })
	}()
	samples <- &metrics.Snapshot{CPU: 35}
	select {
	case host := <-received:
		if host.NumCPU != 64 {
			t.Fatal("initial host copy lost")
		}
	case <-time.After(time.Second):
		t.Fatal("startup blocked live sampling")
	}
	close(samples)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("pump did not shut down")
	}
}

func TestStallLogInitializesPathBeforeStartingOnce(t *testing.T) {
	var c stallLogConfig
	starts := 0
	launch := func(path string) {
		starts++
		if path != c.path || path != filepath.Join("first", "stall.log") {
			t.Fatalf("path not published before launch: %q", path)
		}
	}
	c.initialize("", launch)
	c.initialize("first", launch)
	c.initialize("second", launch)
	if starts != 1 || c.path != filepath.Join("first", "stall.log") {
		t.Fatalf("starts=%d path=%q", starts, c.path)
	}
}
