//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	"gotm/internal/metrics"
	"testing"
	"time"
)

func pageKeyForTest(t *testing.T, p any, key walk.Key, mods walk.Modifiers) bool {
	t.Helper()
	k, ok := p.(interface {
		pageKey(walk.Key, walk.Modifiers) bool
	})
	if !ok {
		t.Fatal("page has no keyboard handler")
	}
	return k.pageKey(key, mods)
}

func TestSettingsKeyboardRevealsOffscreenControl(t *testing.T) {
	p := newSettingsTab(&App{})
	p.content, p.viewH = 1000, 400
	p.v.w, p.v.h = 300, 400
	p.v.hits = []hit{{rect{10, 20, 80, 30}, "first"}, {rect{10, 600, 80, 30}, "second"}}
	pageKeyForTest(t, p, walk.KeyTab, 0)
	pageKeyForTest(t, p, walk.KeyTab, 0)
	if p.scroll <= 0 {
		t.Fatal("keyboard focus left the next control off screen")
	}
}

func TestPerformanceKeyboardSelectsResources(t *testing.T) {
	p := newPerfPage(&App{snap: &metrics.Snapshot{GPUs: []metrics.GPUAdapter{{}, {}}}})
	pageKeyForTest(t, p, walk.KeyEnd, 0)
	if p.sel != 5 {
		t.Fatalf("last resource=%d, want network at 5", p.sel)
	}
	pageKeyForTest(t, p, walk.KeyHome, 0)
	pageKeyForTest(t, p, walk.KeySpace, 0)
	if p.sel != 0 || p.perCore {
		t.Fatal("CPU view did not toggle from the keyboard")
	}
}

func TestRecorderKeyboardMovesBetweenActualSamples(t *testing.T) {
	r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
	const base = int64(1_700_000_000_000)
	for i := 0; i < 3; i++ {
		r.Add(&metrics.Snapshot{T: base + int64(i)*5000})
	}
	p := newRecorderPage(&App{rec: r, snap: &metrics.Snapshot{T: base + 10_000}})
	pageKeyForTest(t, p, walk.KeyLeft, 0)
	if p.live || p.selT != base+5000 {
		t.Fatalf("selected time=%d, live=%v", p.selT, p.live)
	}
	pageKeyForTest(t, p, walk.KeyHome, 0)
	if p.selT != base {
		t.Fatalf("oldest selected time=%d", p.selT)
	}
	pageKeyForTest(t, p, walk.KeyEnd, 0)
	if !p.live || p.selT != 0 {
		t.Fatal("End did not return to live")
	}
}

func TestRecorderArrowMovesFromDisplayedNearestSample(t *testing.T) {
	r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
	const base = int64(1_700_000_000_000)
	for i := 0; i < 3; i++ {
		r.Add(&metrics.Snapshot{T: base + int64(i)*5000})
	}
	p := newRecorderPage(&App{rec: r, snap: &metrics.Snapshot{T: base + 10_000}})
	p.live, p.selT = false, base+1000
	pageKeyForTest(t, p, walk.KeyRight, 0)
	if p.selT != base+5000 {
		t.Fatalf("arrow skipped the next displayed sample: %d", p.selT-base)
	}
}
