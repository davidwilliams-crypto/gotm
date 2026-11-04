//go:build windows

package ui

import (
	"gotm/internal/config"
	"gotm/internal/metrics"
	"math"
	"testing"
)

func TestGPUHeadlineDoesNotInventZero(t *testing.T) {
	p := &perfPage{app: &App{}}
	s := &metrics.Snapshot{GPUs: []metrics.GPUAdapter{{Name: "Matrox G200eR"}}}
	v, sub, _ := p.headline(resItem{resGPU, 0}, s)
	if v != "n/a" || sub != "Matrox G200eR" {
		t.Fatalf("unavailable headline=%q %q", v, sub)
	}
	s.GPUs[0].HasUtilization = true
	v, _, _ = p.headline(resItem{resGPU, 0}, s)
	if v != "0%" {
		t.Fatalf("measured idle headline=%q", v)
	}
}

func TestMissingGPUHistoryIsGap(t *testing.T) {
	p := &perfPage{hist: []metrics.HistPoint{{}, {GPUs: []metrics.GPUHist{{ID: "a", Pct: 0, HasUtilization: true}}}}}
	values := p.gpuSeries("a", func(g *metrics.GPUHist) float64 { return g.Pct })
	if !math.IsNaN(values[0]) || values[1] != 0 {
		t.Fatalf("missing and idle history were conflated: %v", values)
	}
}

func TestGPUHistoryDistinguishesMissingCounterFromIdle(t *testing.T) {
	p := &perfPage{hist: []metrics.HistPoint{{GPUs: []metrics.GPUHist{{ID: "a"}}}, {GPUs: []metrics.GPUHist{{ID: "a", HasUtilization: true}}}}}
	values := p.gpuSeries("a", func(g *metrics.GPUHist) float64 {
		if !g.HasUtilization {
			return math.NaN()
		}
		return g.Pct
	})
	if !math.IsNaN(values[0]) || values[1] != 0 {
		t.Fatalf("availability=%v", values)
	}
}
func TestFiniteSpansDoNotBridgeMissingSamples(t *testing.T) {
	values := []float64{math.NaN(), 0, 20, math.NaN(), math.Inf(1), 40, 0}
	spans := finiteSpans(values)
	if len(spans) != 2 || spans[0] != ([2]int{1, 3}) || spans[1] != ([2]int{5, 7}) {
		t.Fatalf("spans=%v", spans)
	}
}
func TestRecorderGPUAvailabilityLabel(t *testing.T) {
	if got := recordedGPUText(metrics.RecSample{GPUUnavailable: true}); got != "n/a" {
		t.Fatal(got)
	}
	if got := recordedGPUText(metrics.RecSample{}); got != "0%" {
		t.Fatal(got)
	}
}

func TestProcessGPUUnavailableDoesNotShowIdleOrHeat(t *testing.T) {
	for _, c := range colDefs() {
		if c.key != "gpu" {
			continue
		}
		r := &metrics.Row{Proc: metrics.Proc{GPU: 90, GPUUnavailable: true}}
		if c.text(r) != "n/a" || c.heat(r, 0) != 0 {
			t.Fatal("missing GPU shown as measured")
		}
		r.GPUUnavailable = false
		r.GPU = 0
		if c.text(r) == "n/a" {
			t.Fatal("measured idle hidden")
		}
		return
	}
	t.Fatal("GPU column missing")
}
func TestMissingGPUResetsAlertDuration(t *testing.T) {
	a := &App{cfg: config.Config{Alerts: []config.Alert{{Metric: "gpu", Above: 50, ForSeconds: 10, Enabled: true}}}, alertSince: map[int]int64{0: 1}}
	a.checkAlerts(&metrics.Snapshot{T: 20000, GPU: metrics.GPUInfo{Percent: 99, OK: false}})
	if _, ok := a.alertSince[0]; ok {
		t.Fatal("missing sample retained alert duration")
	}
	a.checkAlerts(&metrics.Snapshot{T: 21000, GPU: metrics.GPUInfo{Percent: 99, OK: true}})
	if a.alertSince[0] != 21000 {
		t.Fatal("available sample did not restart alert duration")
	}
}

func TestGPUSensorDemandFollowsVisiblePage(t *testing.T) {
	a := &App{pages: make([]*pageDef, pgCount)}
	p := &perfPage{app: a, sel: 0}
	a.pages[pgPerformance] = &pageDef{impl: p}
	for _, tc := range []struct {
		page, selection int
		want            bool
	}{
		{pgPerformance, 0, false}, {pgPerformance, 2, true}, {pgPower, 0, true},
		{pgPower, 2, true}, {pgProcesses, 2, false}, {pgRecorder, 2, false}, {pgPerformance, 0, false},
	} {
		a.cur, p.sel = tc.page, tc.selection
		if got := a.gpuSensorsWanted(); got != tc.want {
			t.Errorf("page=%d selection=%d wanted=%t", tc.page, tc.selection, got)
		}
	}
}
