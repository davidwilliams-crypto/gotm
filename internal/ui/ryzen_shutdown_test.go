//go:build windows

package ui

import (
	"testing"
	"time"
)

// fakeSDK is a worker that finishes when release is closed (nil: never).
type fakeSDK struct {
	release chan struct{}
	waits   int
}

func (f *fakeSDK) Stop() {}

func (f *fakeSDK) Wait(timeout time.Duration) bool {
	f.waits++
	select {
	case <-f.release: // nil: never ready
		return true
	default:
	}
	if f.release == nil {
		time.Sleep(timeout)
		return false
	}
	select {
	case <-f.release:
		return true
	case <-time.After(timeout):
		return false
	}
}

func finished() *fakeSDK {
	f := &fakeSDK{release: make(chan struct{})}
	close(f.release)
	return f
}

// Every stopped instance is awaited, not just the last one: one switched
// off in Settings just before exit may still be releasing its driver.
func TestRyzenShutdownWaitsForEveryStoppedInstance(t *testing.T) {
	var s sdkShutdown
	earlier := &fakeSDK{release: make(chan struct{})}
	current := &fakeSDK{release: make(chan struct{})}
	s.add(earlier)
	s.add(current)
	if len(s.pending) != 2 {
		t.Fatalf("%d pending, want both", len(s.pending))
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(current.release)
		time.Sleep(20 * time.Millisecond)
		close(earlier.release)
	}()
	if !s.wait(time.Second, time.Now()) {
		t.Fatal("wait gave up although both workers finished within the budget")
	}
	if earlier.waits == 0 || current.waits == 0 || len(s.pending) != 0 {
		t.Fatalf("not every instance awaited: earlier %d, current %d, pending %d", earlier.waits, current.waits, len(s.pending))
	}
}

// Finished instances are forgotten as new ones are stopped, so toggling the
// SDK repeatedly does not grow the list.
func TestRyzenShutdownForgetsFinishedInstances(t *testing.T) {
	var s sdkShutdown
	for i := 0; i < 10; i++ {
		s.add(finished())
	}
	if len(s.pending) != 1 {
		t.Fatalf("%d pending after ten finished instances", len(s.pending))
	}
	stuck := &fakeSDK{release: make(chan struct{})}
	s.add(stuck)
	s.add(finished())
	if len(s.pending) != 2 || s.pending[0] != stuck {
		t.Fatalf("a still-running instance was dropped: %d pending", len(s.pending))
	}
}

// All waits share one budget: a stuck driver holds up neither sign-out
// (WM_ENDSESSION) nor the exit after the message loop for more than it, in
// total, however many instances are pending.
func TestRyzenShutdownSharesOneBudget(t *testing.T) {
	var s sdkShutdown
	s.add(&fakeSDK{}) // never finishes
	s.add(&fakeSDK{})
	const budget = 150 * time.Millisecond
	start := time.Now()
	if s.wait(budget, start) {
		t.Fatal("stuck workers reported finished")
	}
	if s.wait(budget, time.Now()) { // the later exit path
		t.Fatal("stuck workers reported finished on the second wait")
	}
	if spent := time.Since(start); spent > budget+100*time.Millisecond {
		t.Fatalf("waited %s for a %s budget", spent, budget)
	}
	var empty sdkShutdown
	if !empty.wait(budget, time.Now()) {
		t.Fatal("nothing pending must not wait or fail")
	}
}

// stopRyzenForExit (run at WM_ENDSESSION and after the message loop) awaits
// the instances already stopped.
func TestStopRyzenForExitAwaitsStoppedSDK(t *testing.T) {
	a, _ := windowlessApp()
	worker := &fakeSDK{release: make(chan struct{})}
	close(worker.release)
	a.ryzenStops.add(worker)
	a.stopRyzenForExit() // a.sensors is nil in a windowless App: only the wait runs
	if worker.waits == 0 || len(a.ryzenStops.pending) != 0 {
		t.Fatal("stopped SDK not awaited at the end of the session")
	}
}
