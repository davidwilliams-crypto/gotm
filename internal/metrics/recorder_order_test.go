//go:build windows

package metrics

import (
	"testing"
	"time"
)

func TestRecorderClockRollbackKeepsSearchesChronological(t *testing.T) {
	r := NewRecorder(time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	base := int64(1_700_000_000_000)
	r.Add(snap(base, 10))
	r.Add(snap(base+2000, 20))
	r.Add(snap(base-10_000, 30))

	got := r.Samples(base-11_000, base-9000)
	if len(got) != 1 || got[0].CPU != 30 {
		t.Fatalf("rollback sample missing from range: %+v", got)
	}
	nearest, ok := r.Nearest(base - 10_000)
	if !ok || nearest.CPU != 30 {
		t.Fatalf("nearest rollback sample = %+v, %v", nearest, ok)
	}
	if got := r.Samples(0, 0); len(got) != 1 {
		t.Fatalf("future samples survived clock rollback: %+v", got)
	}

	// Resume at the new wall time, preserving the configured interval.
	r.Add(snap(base-9000, 40))
	r.Add(snap(base-8000, 50))
	got = r.Samples(0, 0)
	if len(got) != 2 || got[0].CPU != 30 || got[1].CPU != 50 {
		t.Fatalf("recording after rollback: %+v", got)
	}
}

func TestRecorderRollbackReplacesOverlappingTimestamp(t *testing.T) {
	r := NewRecorder(time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	base := int64(1_700_000_000_000)
	for i := range 4 {
		r.Add(snap(base+int64(i)*2000, float64(i)))
	}
	r.Add(snap(base+2000, 99))
	r.Add(snap(base+2000, 100)) // same tick: retain interval throttling
	got := r.Samples(0, 0)
	if len(got) != 2 || got[0].T != base || got[1].T != base+2000 || got[1].CPU != 99 {
		t.Fatalf("duplicate/future timestamps after rollback: %+v", got)
	}
	nearest, _ := r.Nearest(base + 2000)
	if nearest.CPU != 99 {
		t.Fatalf("nearest retained the superseded sample: %+v", nearest)
	}
}

func TestRecorderPausedClockRollbackDoesNotChangeHistory(t *testing.T) {
	r := NewRecorder(time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	base := int64(1_700_000_000_000)
	r.Add(snap(base, 10))
	r.SetEnabled(false)
	r.Add(snap(base-10_000, 20))
	got := r.Samples(0, 0)
	if len(got) != 1 || got[0].T != base || got[0].CPU != 10 {
		t.Fatalf("paused history changed on clock rollback: %+v", got)
	}
	r.SetEnabled(true)
	r.Add(snap(base-8000, 30))
	got = r.Samples(0, 0)
	if len(got) != 1 || got[0].CPU != 30 {
		t.Fatalf("resumed recorder did not accept new wall time: %+v", got)
	}
}
