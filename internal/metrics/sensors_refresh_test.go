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
	h.refreshSlowWith(func() []DriveHealth { driveReads.Add(1); return nil },
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
		detail               bool
		wantPower, wantDrive int32
	}{
		{"initial discovery", time.Time{}, time.Time{}, false, 1, 1},
		{"recent refresh", time.Now(), time.Now(), true, 0, 0},
		{"power due with drives recent", time.Now().Add(-time.Minute), time.Now(), true, 1, 0},
		{"drives due with power recent", time.Now(), time.Now().Add(-time.Minute), true, 0, 1},
		{"detail refresh", time.Now().Add(-time.Minute), time.Now().Add(-time.Minute), true, 1, 1},
		{"drives suspended", time.Now().Add(-time.Minute), time.Now().Add(-time.Minute), false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SensorHub{powerAt: tc.powerAt, slowAt: tc.driveAt}
			h.SetDetailWanted(tc.detail)
			var powerReads, driveReads atomic.Int32
			h.refreshSlowWith(func() []DriveHealth { driveReads.Add(1); return []DriveHealth{{Model: "test"}} },
				func() PowerStatic { powerReads.Add(1); return PowerStatic{Plan: "test"} },
				func() ([]uint32, []uint32) { return nil, []uint32{4200} })
			deadline := time.Now().Add(time.Second)
			for {
				h.mu.Lock()
				busy := h.powering || h.slowing
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
			if tc.wantPower != 0 && (h.Power().Plan != "test" || len(h.maxMHz) != 1 || h.maxMHz[0] != 4200) {
				t.Fatal("power/clock result not cached")
			}
			if tc.wantDrive != 0 && len(h.Drives()) != 1 {
				t.Fatal("drive discovery result not cached")
			}
		})
	}
}

func TestSensorPowerRefreshWhileDriveReadBlocked(t *testing.T) {
	h := &SensorHub{power: PowerStatic{Plan: "old"}}
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.refreshSlowWith(func() []DriveHealth {
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
