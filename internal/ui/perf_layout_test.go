//go:build windows

package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tailscale/walk"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

func TestPerformanceDetailSurvivesResize(t *testing.T) {
	runNativeTest(t, func() {
		a := &App{cfg: config.Default(), sensors: metrics.NewSensorHubDeferred(),
			host: metrics.HostInfo{CPUName: "Test processor", NumCPU: 32},
			snap: &metrics.Snapshot{Cores: make([]float64, 32), GPUs: []metrics.GPUAdapter{
				{ID: "idle", Name: "Idle adapter", HasUtilization: true}, {ID: "display", Name: "Display adapter"},
			}}}
		p := newPerfPage(a)
		p.hist = []metrics.HistPoint{{T: 1000}, {T: 2000}}
		for _, size := range []walk.Size{{Width: 1140, Height: 720}, {Width: 630, Height: 390}, {Width: 430, Height: 310}} {
			w, h := size.Width, size.Height
			bmp, err := walk.NewBitmapForDPI(size, 96)
			if err != nil {
				panic(err)
			}
			cv, err := walk.NewCanvasFromImage(bmp)
			if err != nil {
				panic(err)
			}
			paint := func() {
				p.v.hits, p.v.items = p.v.hits[:0], p.v.items[:0]
				p.v.w, p.v.h = w, h
				p.paint(cv, w, h)
			}
			visible := func(name string) bool {
				for _, it := range p.v.items {
					if strings.Contains(it.name, name) && it.r.Y >= 0 && it.r.Y+it.r.H <= h && it.r.W > 0 && it.r.H > 0 {
						return true
					}
				}
				return false
			}
			p.pageKey(walk.KeyHome, 0)
			paint()
			if !p.hoverGraphOn || p.hoverGraph.W < w/2 || !visible("Test processor") || !visible("History") {
				t.Errorf("%dx%d: CPU detail, history span or hover plot missing (plot %+v)", w, h, p.hoverGraph)
			}
			for range 10 {
				if visible("L3 cache") {
					break
				}
				p.pageKey(walk.KeyNext, 0)
				paint()
			}
			if !visible("L3 cache") {
				t.Errorf("%dx%d: last CPU fact is unreachable", w, h)
			}
			p.pageKey(walk.KeyDown, 0)
			p.pageKey(walk.KeyDown, 0)
			paint()
			if !p.hoverGraphOn || p.hoverIndexAt(p.hoverGraph.X+4, p.hoverGraph.Y+4) != 0 {
				t.Errorf("%dx%d: adapter overall graph has no sample hover", w, h)
			}
			p.pageKey(walk.KeyDown, 0)
			paint()
			if !visible("Utilization unavailable") || !visible("History") || !visible("GPU 1: n/a") {
				t.Errorf("%dx%d: missing unavailable GPU detail, card or span", w, h)
			}
			for _, it := range p.v.items {
				if strings.HasPrefix(it.id, "gpuview:") || (it.name == "Utilization unavailable" && it.r.H > 80) {
					t.Errorf("%dx%d: unavailable counter has a graph-sized blank state or an active selector: %+v", w, h, it)
				}
			}
			p.pageKey(walk.KeyEnd, 0)
			paint()
			if !p.hoverGraphOn || !visible("Receive:") || !visible("Network:") {
				t.Errorf("%dx%d: network chart, card or rates missing", w, h)
			}
			cv.Dispose()
			bmp.Dispose()
		}
	})
}

// Resource targets must remain legible, even on servers exposing many adapters.
func TestPerformanceManyAdaptersKeepReachableTargets(t *testing.T) {
	runNativeTest(t, func() {
		a := &App{cfg: config.Default(), sensors: metrics.NewSensorHubDeferred(),
			snap: &metrics.Snapshot{GPUs: make([]metrics.GPUAdapter, 24)}}
		p := newPerfPage(a)
		const w, h = 620, 310
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: w, Height: h}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		for _, key := range []walk.Key{walk.KeyHome, walk.KeyEnd, walk.KeyUp, walk.KeyHome} {
			p.pageKey(key, 0)
			p.v.hits, p.v.items = p.v.hits[:0], p.v.items[:0]
			p.paint(cv, w, h)
			visibleSelected := false
			for _, it := range p.v.items {
				if it.ctl != ctlListItem {
					continue
				}
				if it.r.H < 32 || it.r.Y < 0 || it.r.Y+it.r.H > h {
					t.Errorf("unusable card %q at %+v", it.name, it.r)
					return
				}
				if it.selected {
					visibleSelected = true
				}
			}
			if !visibleSelected {
				t.Errorf("selection %d is outside the visible cards", p.sel)
			}
		}
	})
}

func TestPerformanceGPUHoverReadsSelectedAdapter(t *testing.T) {
	p := newPerfPage(&App{snap: &metrics.Snapshot{GPUs: []metrics.GPUAdapter{{ID: "a"}, {ID: "b"}}}})
	h := metrics.HistPoint{GPUOK: true, GPU: 90, GPUs: []metrics.GPUHist{{ID: "b", Pct: 17, HasUtilization: true}, {ID: "a"}}}
	if actual := p.hoverLabel(resItem{resGPU, 1}, &h); actual != "17% GPU 1" {
		t.Fatalf("wrong adapter's hover reading: %s", actual)
	}
	if actual := p.hoverLabel(resItem{resGPU, 0}, &h); actual != "GPU 0 n/a" {
		t.Fatalf("missing adapter counter acquired a reading: %s", actual)
	}
}

func TestPerformancePageKeysReachEveryGPUProcess(t *testing.T) {
	for _, size := range []walk.Size{{Width: 1140, Height: 720}, {Width: 430, Height: 310}} {
		t.Run(fmt.Sprintf("%dx%d", size.Width, size.Height), func(t *testing.T) {
			runNativeTest(t, func() {
				gpu := metrics.GPUAdapter{Name: "Test adapter", HasUtilization: true}
				for i := 1; i <= 13; i++ {
					gpu.Procs = append(gpu.Procs, metrics.GPUProcMem{PID: uint32(i), Name: fmt.Sprintf("process%02d", i)})
				}
				p := newPerfPage(&App{cfg: config.Default(), sensors: metrics.NewSensorHubDeferred(),
					snap: &metrics.Snapshot{GPUs: []metrics.GPUAdapter{gpu}}})
				p.sel = 2
				bmp, err := walk.NewBitmapForDPI(size, 96)
				if err != nil {
					panic(err)
				}
				defer bmp.Dispose()
				cv, err := walk.NewCanvasFromImage(bmp)
				if err != nil {
					panic(err)
				}
				defer cv.Dispose()
				for _, key := range []walk.Key{walk.KeyNext, walk.KeyPrior} {
					seen := map[string]bool{}
					for range len(gpu.Procs) + 3 {
						p.v.hits, p.v.items = p.v.hits[:0], p.v.items[:0]
						p.paint(cv, size.Width, size.Height)
						for _, item := range p.v.items {
							if name, _, ok := strings.Cut(item.name, ", PID "); ok && item.r.H >= 16 {
								seen[name] = true
							}
						}
						beforeDetail, beforeList := p.detailScroll, p.gpuScroll
						p.pageKey(key, 0)
						if beforeDetail == p.detailScroll && beforeList == p.gpuScroll {
							break
						}
					}
					if len(seen) != len(gpu.Procs) {
						t.Errorf("key %v reached %d of %d processes", key, len(seen), len(gpu.Procs))
					}
				}
				if p.detailScroll != 0 || p.gpuScroll != 0 {
					t.Error("Page Up did not return to the graph and first process")
				}
			})
		})
	}
}
