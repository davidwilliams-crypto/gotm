//go:build windows

package ui

import (
	"testing"
	"time"

	"github.com/tailscale/walk"
	"gotm/internal/metrics"
)

func TestRecorderPageStartsOnlyOnManualActivation(t *testing.T) {
	r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
	p := newRecorderPage(&App{rec: r})
	p.shown()
	p.reload()
	p.tick(nil)
	const base = int64(1_700_000_000_000)
	r.Add(&metrics.Snapshot{T: base, CPU: 10})
	if r.Len() != 0 || p.bRec.text != "Start recording" || !p.bRec.enabled {
		t.Fatal("page did not offer manual Start without capturing")
	}
	p.bRec.v.onClick("b", walk.LeftButton)
	r.Add(&metrics.Snapshot{T: base + 2000, CPU: 20})
	if !r.Enabled() || r.Len() != 1 {
		t.Fatal("Start button did not begin capture")
	}
	pageKeyForTest(t, p, walk.KeySpace, 0)
	r.Add(&metrics.Snapshot{T: base + 4000, CPU: 30})
	if r.Enabled() || r.Len() != 1 || p.bRec.text != "Start recording" {
		t.Fatal("keyboard pause did not stop capture and offer Start")
	}
}
