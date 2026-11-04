//go:build windows

package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestClockFallbackNeverWaitsForBlockedDriver(t *testing.T) {
	h := NewSensorHubDeferred()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	reader := func() ([]uint32, []uint32) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []uint32{2400}, []uint32{3300}
	}
	returned := make(chan struct{})
	go func() { h.clockSnapshot(reader); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("clock request blocked caller")
	}
	<-started
	for i := 0; i < 20; i++ {
		if got := h.clockSnapshot(reader); len(got) != 0 {
			t.Fatal("invented initial clock")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unbounded requests: %d", calls.Load())
	}
	// Cached detail reads must not contend with the blocked driver either.
	_ = h.Topology()
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		done := !h.clocking
		cur, max := h.currentMHz, h.maxMHz
		h.mu.Unlock()
		if done {
			if len(cur) != 1 || cur[0] != 2400 || len(max) != 1 || max[0] != 3300 {
				t.Fatalf("cache=%v/%v", cur, max)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("clock result not published")
		}
		time.Sleep(time.Millisecond)
	}
}
