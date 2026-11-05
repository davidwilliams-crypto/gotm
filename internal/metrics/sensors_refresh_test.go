//go:build windows

package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSensorPowerRefreshOutsideDetailPage(t *testing.T) {
	h := &SensorHub{slowAt: time.Now().Add(-time.Minute), powerAt: time.Now().Add(-time.Minute), power: PowerStatic{Plan: "old"}}
	var driveReads atomic.Int32
	h.refreshSlowWith(func(bool) []DriveHealth { driveReads.Add(1); return nil },
		func() PowerStatic { return PowerStatic{Plan: "new", OnAC: true} },
		func() ([]uint32, []uint32) { return nil, []uint32{4200} })
	deadline := time.Now().Add(time.Second)
	for h.Power().Plan != "new" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.Power(); got.Plan != "new" || !got.OnAC {
		t.Fatalf("power did not refresh outside detail page: %+v", got)
	}
	if got := driveReads.Load(); got != 0 {
		t.Fatalf("suspended drive polling ran %d times", got)
	}
}

func TestSensorRefreshIntervalsAndDetailGate(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		powerAt, driveAt     time.Time
		drivesFull           bool // the cached drives came from a full health read
		detail               bool
		wantPower, wantDrive int32
		wantFull             bool // the drive read asked for temperatures/SMART
	}{
		{"initial discovery", time.Time{}, time.Time{}, false, false, 1, 1, false},
		{"initial discovery on detail page", time.Time{}, time.Time{}, false, true, 1, 1, true},
		{"recent refresh", time.Now(), time.Now(), true, true, 0, 0, false},
		{"power due with drives recent", time.Now().Add(-time.Minute), time.Now(), true, true, 1, 0, false},
		{"drives due with power recent", time.Now(), time.Now().Add(-time.Minute), true, true, 0, 1, true},
		{"detail refresh", time.Now().Add(-time.Minute), time.Now().Add(-time.Minute), true, true, 1, 1, true},
		{"drives suspended", time.Now().Add(-time.Minute), time.Now().Add(-time.Minute), true, false, 1, 0, false},
		// Start-up read only names: opening the page reads health at once.
		{"names only, page opened", time.Now(), time.Now(), false, true, 0, 1, true},
		{"names only, page hidden", time.Now(), time.Now().Add(-time.Minute), false, false, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SensorHub{powerAt: tc.powerAt, slowAt: tc.driveAt, drivesFull: tc.drivesFull}
			h.SetDetailWanted(tc.detail)
			var powerReads, driveReads, fullReads atomic.Int32
			h.refreshSlowWith(func(full bool) []DriveHealth {
				driveReads.Add(1)
				if full {
					fullReads.Add(1)
				}
				return []DriveHealth{{Model: "test"}}
			},
				func() PowerStatic { powerReads.Add(1); return PowerStatic{Plan: "test"} },
				func() ([]uint32, []uint32) { return nil, []uint32{4200} })
			deadline := time.Now().Add(time.Second)
			for {
				h.mu.Lock()
				busy := h.powering || h.slowing || h.clocking
				h.mu.Unlock()
				if !busy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("refresh did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			if got := powerReads.Load(); got != tc.wantPower {
				t.Errorf("power reads = %d, want %d", got, tc.wantPower)
			}
			if got := driveReads.Load(); got != tc.wantDrive {
				t.Errorf("drive reads = %d, want %d", got, tc.wantDrive)
			}
			if got := fullReads.Load() > 0; got != tc.wantFull {
				t.Errorf("full health read = %v, want %v", got, tc.wantFull)
			}
			if tc.wantPower != 0 && (h.Power().Plan != "test" || len(h.maxMHz) != 1 || h.maxMHz[0] != 4200) {
				t.Fatal("power/clock result not cached")
			}
			if tc.wantDrive != 0 {
				h.mu.Lock()
				full := h.drivesFull
				h.mu.Unlock()
				if len(h.Drives()) != 1 || full != tc.wantFull {
					t.Fatal("drive discovery result not cached")
				}
			}
		})
	}
}

func TestSensorPowerRefreshWhileDriveReadBlocked(t *testing.T) {
	h := &SensorHub{power: PowerStatic{Plan: "old"}}
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.refreshSlowWith(func(bool) []DriveHealth {
		close(started)
		<-release
		return []DriveHealth{{Model: "test drive"}}
	}, func() PowerStatic { return PowerStatic{Plan: "new"} },
		func() ([]uint32, []uint32) { return nil, []uint32{4200} })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial drive discovery did not start")
	}
	deadline := time.Now().Add(time.Second)
	for h.Power().Plan != "new" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.Power().Plan; got != "new" {
		t.Fatalf("power refresh blocked behind drive read: %q", got)
	}
}

// The maximum clock is static: once known, the 15 s power refresh does not
// call CallNtPowerInformation again.
func TestSensorRatedClockReadOnce(t *testing.T) {
	h := &SensorHub{slowAt: time.Now(), maxMHz: []uint32{3000}}
	var clockReads atomic.Int32
	h.refreshSlowWith(func(bool) []DriveHealth { return nil },
		func() PowerStatic { return PowerStatic{} },
		func() ([]uint32, []uint32) { clockReads.Add(1); return nil, []uint32{4200} })
	time.Sleep(20 * time.Millisecond)
	if got := clockReads.Load(); got != 0 {
		t.Fatalf("rated clock re-read %d times", got)
	}
}
