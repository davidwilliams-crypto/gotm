//go:build windows

package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorderWaitsForManualStart(t *testing.T) {
	for _, persist := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "disk"}[persist], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "recordings")
			r := NewRecorder(time.Hour, 2*time.Second, dir)
			t.Cleanup(func() { r.SetPersist(false) })
			r.SetPersist(persist)
			const base = int64(1_700_000_000_000)
			r.Add(snap(base, 10))
			if r.Enabled() || r.Len() != 0 {
				t.Fatal("recorder captured a sample before manual Start")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("recorder touched disk before manual Start: %v", err)
			}
			r.SetEnabled(true)
			r.Add(snap(base+2000, 20))
			r.SetEnabled(false)
			r.Add(snap(base+4000, 30))
			got := r.Samples(0, 0)
			if len(got) != 1 || got[0].CPU != 20 {
				t.Fatalf("manual Start/Pause did not bound capture: %+v", got)
			}
		})
	}
}

func TestRecorderRelaunchWithSavedHistoryStillNeedsManualStart(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Second).UnixMilli()
	r := NewRecorder(time.Hour, 2*time.Second, dir)
	r.SetPersist(true)
	r.SetEnabled(true)
	r.Add(snap(base, 20))
	r.SetPersist(false)
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("manual recording was not saved: %v, %v", files, err)
	}
	before, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	relaunched := NewRecorder(time.Hour, 2*time.Second, dir)
	t.Cleanup(func() { relaunched.SetPersist(false) })
	relaunched.SetPersist(true)
	if n := relaunched.LoadDisk(time.Hour); n != 1 {
		t.Fatalf("saved history unavailable before Start: %d samples", n)
	}
	relaunched.SetPersist(false)
	relaunched.SetPersist(true)
	relaunched.Add(snap(base+4000, 99))
	after, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if relaunched.Enabled() || relaunched.Len() != 1 || !bytes.Equal(before, after) {
		t.Fatal("relaunch, replay or disk setting restarted recording")
	}
	relaunched.SetEnabled(true)
	relaunched.Add(snap(base+6000, 30))
	got := relaunched.Samples(0, 0)
	if len(got) != 2 || got[1].CPU != 30 {
		t.Fatalf("manual Start after replay did not capture: %+v", got)
	}
}
