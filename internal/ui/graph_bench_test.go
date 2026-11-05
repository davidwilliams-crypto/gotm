//go:build windows

package ui

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/tailscale/walk"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

// benchCanvas runs body on the native UI thread with a w x h bitmap canvas.
func benchCanvas(b *testing.B, w, h int, body func(cv *walk.Canvas)) {
	b.Helper()
	done := make(chan struct{})
	nativeTests <- func() {
		defer close(done)
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
		saved := gs
		gs = 1
		defer func() { gs = saved }()
		body(cv)
	}
	<-done
}

// noisy is a deterministic, CPU-like curve with sharp peaks.
func noisy(n int, seed uint64) []float64 {
	out := make([]float64, n)
	x := seed | 1
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = 35 + 25*math.Sin(float64(i)/23) + float64(x%2000)/100
	}
	return out
}

// Long histories (HistorySeconds up to 3600 at RefreshMs down to 250) used to
// make one 800 px graph cost close to a second.
func BenchmarkPlotSeriesLongHistory(b *testing.B) {
	for _, n := range []int{60, 1800, 3600, 14400} {
		b.Run(fmt.Sprintf("points%d", n), func(b *testing.B) {
			benchCanvas(b, 800, 240, func(cv *walk.Canvas) {
				ser := []series{{vals: noisy(n, 7), color: T.CPU, fill: true}}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					plotSeries(cv, rect{0, 0, 800, 240}, ser, graphStyle{bg: T.Card, max: 100, lineW: 2, grid: true})
				}
				b.StopTimer()
			})
		})
	}
}

func benchPerfApp(cores, points int) (*App, *perfPage) {
	snap := &metrics.Snapshot{T: 1_700_000_000_000, CPU: 40, Cores: make([]float64, cores)}
	for i := range snap.Cores {
		snap.Cores[i] = float64(i * 7 % 100)
	}
	a := &App{cfg: config.Default(), sensors: metrics.NewSensorHubDeferred(),
		host: metrics.HostInfo{CPUName: "Bench processor", NumCPU: cores}, snap: snap}
	p := newPerfPage(a)
	hist := make([]metrics.HistPoint, points)
	cpu := noisy(points, 3)
	for i := range hist {
		hist[i] = metrics.HistPoint{T: snap.T - int64(points-i)*1000, CPU: cpu[i], Mem: 50, Cores: make([]float64, cores)}
		for c := range hist[i].Cores {
			hist[i].Cores[c] = cpu[(i+c*13)%points]
		}
	}
	p.hist = hist
	return a, p
}

// The CPU page: rail sparklines, main graph and one tile per logical CPU.
func BenchmarkPerfPagePaint(b *testing.B) {
	for _, c := range []struct{ cores, points int }{{14, 60}, {56, 60}, {14, 1800}, {56, 1800}} {
		b.Run(fmt.Sprintf("cores%d_points%d", c.cores, c.points), func(b *testing.B) {
			benchCanvas(b, 1140, 720, func(cv *walk.Canvas) {
				_, p := benchPerfApp(c.cores, c.points)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					p.v.hits, p.v.items = p.v.hits[:0], p.v.items[:0]
					p.paint(cv, 1140, 720)
				}
				b.StopTimer()
			})
		})
	}
}

func benchRecorderPage(samples int) *recorderPage {
	const base = int64(1_700_000_000_000)
	r := metrics.NewRecorder(3*time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	cpu := noisy(samples, 11)
	for i := 0; i < samples; i++ {
		r.Add(&metrics.Snapshot{T: base + int64(i)*2000, CPU: cpu[i], Mem: metrics.MemInfo{Percent: 40 + cpu[i]/4},
			GPU: metrics.GPUInfo{Percent: cpu[i] / 2, OK: i%500 != 0}})
	}
	r.SetEnabled(false)
	p := newRecorderPage(&App{rec: r, snap: &metrics.Snapshot{T: base + int64(samples)*2000}})
	p.rangeSecs = 10800
	return p
}

// A 3-hour recording: 5400 samples x 3 series.
func BenchmarkRecorderPaint(b *testing.B) {
	benchCanvas(b, 1140, 720, func(cv *walk.Canvas) {
		p := benchRecorderPage(5400)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p.view.hits, p.view.items = p.view.hits[:0], p.view.items[:0]
			p.paint(cv, 1140, 720)
		}
		b.StopTimer()
	})
}

// benchLivePerf drives the page like the application: a sampler-like ring
// that gains a point (and evicts the oldest) per tick, the incremental
// mirror, and the canvas's graph cache around each paint.
func benchLivePerf(b *testing.B, cores, points int, newData bool) {
	benchCanvas(b, 1140, 720, func(cv *walk.Canvas) {
		_, p := benchPerfApp(cores, points)
		ring := p.hist
		src := &fakeHistory{hist: ring}
		p.hist = nil
		cache := &p.v.graphs
		paint := func() {
			p.mirror.sync(src)
			p.hist = p.mirror.view()
			activeGraphs = cache
			cache.begin()
			p.v.hits, p.v.items = p.v.hits[:0], p.v.items[:0]
			p.paint(cv, 1140, 720)
			cache.end()
			activeGraphs = nil
		}
		paint()
		cpu := noisy(4096, 5)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if newData {
				last := src.hist[len(src.hist)-1]
				next := metrics.HistPoint{T: last.T + 1000, CPU: cpu[i%len(cpu)], Mem: 50, Cores: make([]float64, cores)}
				for c := range next.Cores {
					next.Cores[c] = cpu[(i+c*13)%len(cpu)]
				}
				ring = append(ring, next) // amortized: the benchmark should not copy the ring
				src.hist = ring[len(ring)-points:]
			}
			paint()
		}
		b.StopTimer()
	})
}

// A tick: one new point per curve, every graph redrawn.
func BenchmarkPerfPageTick(b *testing.B) {
	for _, c := range []struct{ cores, points int }{{14, 60}, {56, 60}, {14, 1800}, {56, 1800}, {14, 14400}} {
		b.Run(fmt.Sprintf("cores%d_points%d", c.cores, c.points), func(b *testing.B) {
			benchLivePerf(b, c.cores, c.points, true)
		})
	}
}

// A repaint without new data (hover readout, scrolling, focus).
func BenchmarkPerfPageRepaint(b *testing.B) {
	for _, c := range []struct{ cores, points int }{{14, 60}, {56, 1800}} {
		b.Run(fmt.Sprintf("cores%d_points%d", c.cores, c.points), func(b *testing.B) {
			benchLivePerf(b, c.cores, c.points, false)
		})
	}
}

// Scrubbing a 3-hour recording: the timeline comes from the graph cache.
func BenchmarkRecorderScrub(b *testing.B) {
	benchCanvas(b, 1140, 720, func(cv *walk.Canvas) {
		p := benchRecorderPage(5400)
		from, to := p.window()
		activeGraphs = &p.view.graphs
		defer func() { activeGraphs = nil }()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p.live, p.selT = false, from+int64(i%100)*(to-from)/100
			p.view.graphs.begin()
			p.view.hits, p.view.items = p.view.hits[:0], p.view.items[:0]
			p.paint(cv, 1140, 720)
			p.view.graphs.end()
		}
		b.StopTimer()
	})
}

// Arrow keys on a 3-hour recording.
func BenchmarkRecorderKeyStep(b *testing.B) {
	p := benchRecorderPage(5400)
	p.live = false
	p.selT = 1_700_000_000_000 + 2700*2000
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			p.pageKey(walk.KeyLeft, 0)
		} else {
			p.pageKey(walk.KeyRight, 0)
		}
	}
}
