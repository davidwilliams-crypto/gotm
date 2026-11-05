//go:build windows

package ui

import (
	"image"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"golang.org/x/sys/windows"

	"gotm/internal/metrics"
)

// fakeHistory follows the sampler's ring semantics for histMirror tests.
type fakeHistory struct {
	hist  []metrics.HistPoint
	fulls int
}

func (f *fakeHistory) AppendHistoryAfter(dst []metrics.HistPoint, after int64) ([]metrics.HistPoint, metrics.HistoryInfo) {
	info := metrics.HistoryInfo{Len: len(f.hist)}
	if n := len(f.hist); n > 0 {
		info.Oldest, info.Newest = f.hist[0].T, f.hist[n-1].T
	}
	i := sort.Search(len(f.hist), func(i int) bool { return f.hist[i].T > after })
	if i > 0 {
		info.Prev = f.hist[i-1].T
	}
	return append(dst, f.hist[i:]...), info
}

func (f *fakeHistory) History() []metrics.HistPoint {
	f.fulls++
	return append([]metrics.HistPoint(nil), f.hist...)
}

func (f *fakeHistory) add(t int64, cpu float64) {
	f.hist = append(f.hist, metrics.HistPoint{T: t, CPU: cpu, Cores: []float64{cpu / 2}})
}

func (f *fakeHistory) evict(k int) { f.hist = append([]metrics.HistPoint(nil), f.hist[k:]...) }

// The incremental mirror must always equal a full copy of the history,
// through appends, evictions, a shrunken and regrown window and clock
// rollbacks, and derived curves must equal a full recomputation.
func TestHistoryMirrorTracksSamplerRing(t *testing.T) {
	src := &fakeHistory{}
	for i := int64(1); i <= 100; i++ {
		src.add(i*10, float64(i))
	}
	var m histMirror
	var cpu, core derivedSeries
	coreOf := func(h *metrics.HistPoint) float64 { return h.Cores[0] }
	check := func(step string, wantFulls int) {
		t.Helper()
		m.sync(src)
		got := m.view()
		if len(got) != len(src.hist) {
			t.Fatalf("%s: mirror has %d points, ring %d", step, len(got), len(src.hist))
		}
		for i := range got {
			if got[i].T != src.hist[i].T || got[i].CPU != src.hist[i].CPU {
				t.Fatalf("%s: point %d = %d/%v, ring %d/%v", step, i, got[i].T, got[i].CPU, src.hist[i].T, src.hist[i].CPU)
			}
		}
		if src.fulls != wantFulls {
			t.Fatalf("%s: %d full copies, want %d", step, src.fulls, wantFulls)
		}
		for _, d := range []struct {
			s *derivedSeries
			f func(*metrics.HistPoint) float64
		}{{&cpu, histCPU}, {&core, coreOf}} {
			vals := d.s.derive(&m, d.f)
			if len(vals) != len(got) {
				t.Fatalf("%s: derived %d values for %d points", step, len(vals), len(got))
			}
			for i := range vals {
				if vals[i] != d.f(&got[i]) {
					t.Fatalf("%s: derived value %d = %v, want %v", step, i, vals[i], d.f(&got[i]))
				}
			}
		}
	}
	check("initial", 1)
	ver := cpu.version(seriesKey{kind: serCPU})
	check("unchanged", 1)
	if cpu.version(seriesKey{kind: serCPU}) != ver {
		t.Fatal("version changed without new data")
	}
	for i := int64(101); i <= 105; i++ {
		src.add(i*10, float64(i))
	}
	src.evict(5)
	check("append and evict", 1)
	if cpu.version(seriesKey{kind: serCPU}) == ver {
		t.Fatal("version unchanged after new data")
	}
	src.evict(len(src.hist) - 20) // history window reduced
	check("shrink", 1)
	for i := int64(106); i <= 300; i++ { // window enlarged again
		src.add(i*10, float64(i))
	}
	check("regrow", 1)
	src.hist = nil
	check("emptied", 1)
	for i := int64(301); i <= 400; i++ {
		src.add(i*10, float64(i))
	}
	check("refilled", 1)
	// Clock rollback: the sampler drops the points that now lie in the
	// future and appends the new sample.
	cut := sort.Search(len(src.hist), func(i int) bool { return src.hist[i].T > 3500 })
	src.hist = src.hist[:cut]
	src.add(3501, 7)
	check("rollback", 2)
	// A rollback that time has overtaken again before the page looked.
	cut = sort.Search(len(src.hist), func(i int) bool { return src.hist[i].T > 3300 })
	src.hist = src.hist[:cut]
	for t := int64(3303); t <= 3803; t += 10 {
		src.add(t, float64(t%97))
	}
	check("overtaken rollback", 3)
}

// Long series are reduced per supersampled column, but the first and last
// points, every peak and every gap survive, at their own positions.
func TestGraphDecimationKeepsPeaksGapsAndEnds(t *testing.T) {
	const n, W, H, ss, outW = 14400, 1200, 300, 3, 400
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = 20 + float64(i%3)
	}
	vals[7000], vals[9000] = 95, 1
	for i := 3000; i < 3100; i++ {
		vals[i] = math.NaN()
	}
	pts, out := graphVertices(nil, nil, series{vals: vals}, graphStyle{}, W, H, ss, outW, 100)
	if len(pts) > 4*(W+2)+2 {
		t.Fatalf("%d vertices for a %d px (supersampled) graph", len(pts), W)
	}
	step := float64(W) / float64(n-1)
	if pts[0].X != 0 || out[0] != vals[0] || pts[len(pts)-1].X < W-1 || out[len(out)-1] != vals[n-1] {
		t.Fatalf("end points lost: first %v=%v last %v=%v", pts[0], out[0], pts[len(pts)-1], out[len(out)-1])
	}
	find := func(v float64) (walk.Point, bool) {
		for i := range out {
			if out[i] == v {
				return pts[i], true
			}
		}
		return walk.Point{}, false
	}
	for _, peak := range []struct {
		v float64
		i int
	}{{95, 7000}, {1, 9000}} {
		p, ok := find(peak.v)
		if !ok || p.X != int(float64(peak.i)*step) {
			t.Errorf("peak %v at index %d lost or moved: %v %v", peak.v, peak.i, p, ok)
		}
	}
	gaps := 0
	for i := range out {
		if math.IsNaN(out[i]) {
			gaps++
			if pts[i-1].X > int(2999*step) || pts[i+1].X < int(3100*step) {
				t.Errorf("gap at the wrong place: %v .. %v", pts[i-1], pts[i+1])
			}
		}
		if i > 0 && !math.IsNaN(out[i]) && !math.IsNaN(out[i-1]) && pts[i].X < pts[i-1].X {
			t.Fatalf("vertices out of order at %d", i)
		}
	}
	if gaps != 1 {
		t.Fatalf("%d gaps, want 1", gaps)
	}

	// Timed series: irregular sampling keeps a peak at its moment.
	times := make([]int64, n)
	for i := range times {
		times[i] = int64(i)*1000 + int64(i%7)*300
	}
	st := graphStyle{timeFrom: times[0], timeTo: times[n-1]}
	pts, out = graphVertices(nil, nil, series{vals: vals, times: times}, st, W, H, ss, outW, 100)
	p, ok := find(95)
	if want := int(timePosition(times[7000], st.timeFrom, st.timeTo) * W); !ok || p.X != want {
		t.Fatalf("timed peak at %v (%v), want x=%d", p, ok, want)
	}
}

// The rendered long history still shows a one-sample spike and a gap.
func TestGraphRenderedLongHistoryShowsSpikeAndGap(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 420, Height: 120}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		saved := gs
		gs = 1
		defer func() { gs = saved }()
		const n = 14400
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = 20
		}
		vals[7200] = 95
		for i := 3000; i < 3600; i++ {
			vals[i] = math.NaN()
		}
		bg, line := walk.RGB(0, 0, 0), walk.RGB(255, 255, 255)
		plotSeries(cv, rect{10, 10, 400, 100}, []series{{vals: vals, color: line, fill: true}}, graphStyle{bg: bg, max: 100, lineW: 2})
		cv.Dispose()
		im, err := bmp.ToImage()
		if err != nil {
			panic(err)
		}
		x := func(i int) int { return 10 + int(float64(i)*400/float64(n-1)) }
		if !hasColorNear(im, x(7200), 16, line, 2) {
			t.Error("one-sample spike missing from a 14,400-point history")
		}
		if !hasColorNear(im, x(3300), 100, bg, 0) || hasColorNear(im, x(3300), 100, blend(line, bg, 0.22), 0) {
			t.Error("missing samples were filled")
		}
		if !hasColorNear(im, x(1000), 100, blend(line, bg, 0.22), 0) || !hasColorNear(im, x(12000), 100, blend(line, bg, 0.22), 0) {
			t.Error("fill missing around the gap")
		}
	})
}

func imagesEqual(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

// A repaint without new data reuses the finished graph; anything that changes
// its pixels (data, version, colours, size) renders it again.
func TestGraphCacheInvalidatesOnChange(t *testing.T) {
	runNativeTest(t, func() {
		saved := gs
		gs = 1
		defer func() { gs = saved }()
		var c graphCache
		activeGraphs = &c
		defer func() { activeGraphs = nil }()
		snapshot := func(draw func(cv *walk.Canvas)) image.Image {
			bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 200, Height: 80}, 96)
			if err != nil {
				panic(err)
			}
			defer bmp.Dispose()
			cv, err := walk.NewCanvasFromImage(bmp)
			if err != nil {
				panic(err)
			}
			draw(cv)
			cv.Dispose()
			im, err := bmp.ToImage()
			if err != nil {
				panic(err)
			}
			return im
		}
		vals := []float64{10, 80, 30, 60, math.NaN(), 50, 70}
		st := graphStyle{bg: T.Card, max: 100, lineW: 2, grid: true}
		paint := func(r rect, ser []series, st graphStyle) image.Image {
			return snapshot(func(cv *walk.Canvas) {
				c.begin()
				plotSeries(cv, r, ser, st)
				c.end()
			})
		}
		expect := func(step string, renders int) {
			t.Helper()
			if graphRenders != renders {
				t.Fatalf("%s: %d renders, want %d", step, graphRenders, renders)
			}
		}
		r := rect{0, 0, 200, 80}
		base := graphRenders
		first := paint(r, []series{{vals: vals, color: T.CPU, fill: true}}, st)
		expect("first paint", base+1)
		again := paint(r, []series{{vals: vals, color: T.CPU, fill: true}}, st)
		expect("unchanged repaint", base+1)
		if !imagesEqual(first, again) {
			t.Fatal("cached graph differs from the rendered one")
		}
		changed := append([]float64(nil), vals...)
		changed[3] = 61
		paint(r, []series{{vals: changed, color: T.CPU, fill: true}}, st)
		expect("new value", base+2)
		paint(r, []series{{vals: changed, color: T.Mem, fill: true}}, st)
		expect("new colour", base+3)
		st2 := st
		st2.bg = T.CardHi
		paint(r, []series{{vals: changed, color: T.Mem, fill: true}}, st2)
		expect("new background", base+4)
		paint(rect{0, 0, 180, 80}, []series{{vals: changed, color: T.Mem, fill: true}}, st2)
		expect("resized", base+5)
		paint(r, []series{{vals: changed, color: T.Mem, fill: true, ver: 7}}, st2)
		expect("versioned", base+6)
		paint(r, []series{{vals: vals, color: T.Mem, fill: true, ver: 7}}, st2)
		expect("same version", base+6)
		paint(r, []series{{vals: vals, color: T.Mem, fill: true, ver: 8}}, st2)
		expect("new version", base+7)
		if c.len() != 1 {
			t.Fatalf("%d cached graphs after paints that drew one", c.len())
		}
		c.begin()
		c.end()
		if c.len() != 0 {
			t.Fatal("a graph no paint drew stayed cached")
		}
	})
}

// Clicks and releases on empty space must not repaint the canvas.
func TestCanvasRepaintsOnlyWhenPressStateChanges(t *testing.T) {
	v := newCanvasView(nil)
	v.keyboard = true
	v.hits = []hit{{rect{0, 0, 10, 10}, "a"}}
	clicked := ""
	v.onClick = func(id string, _ walk.MouseButton) { clicked = id }
	expect := func(step string, n int) {
		t.Helper()
		if v.invalidations != n {
			t.Fatalf("%s: %d repaints, want %d", step, v.invalidations, n)
		}
	}
	v.mouseDown(50, 50, walk.LeftButton)
	v.mouseUp(50, 50, walk.LeftButton)
	expect("click on empty space", 0)
	v.mouseDown(50, 50, walk.RightButton)
	v.mouseUp(50, 50, walk.RightButton)
	expect("right click on empty space", 0)
	v.mouseDown(5, 5, walk.LeftButton)
	expect("press an item", 1)
	v.mouseUp(5, 5, walk.LeftButton)
	expect("release it", 2)
	if clicked != "a" {
		t.Fatal("click not delivered")
	}
	v.hover = "a"
	v.mouseDown(5, 5, walk.LeftButton)
	expect("press the hovered keyboard item", 3)
	v.mouseUp(5, 5, walk.LeftButton)
	expect("release", 4)
}

// A paused recording draws the same image every tick: do not repaint it.
func TestRecorderSyncRepaintsOnlyOnChange(t *testing.T) {
	const base = int64(1_700_000_000_000)
	r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	for i := int64(0); i < 30; i++ {
		r.Add(&metrics.Snapshot{T: base + i*2000, CPU: float64(i)})
	}
	r.SetEnabled(false)
	a := &App{rec: r, snap: &metrics.Snapshot{T: base + 60_000}}
	p := newRecorderPage(a)
	p.sync()
	n := p.view.invalidations
	for i := int64(1); i <= 5; i++ {
		a.snap = &metrics.Snapshot{T: base + 60_000 + i*1000}
		p.tick(a.snap)
	}
	if p.view.invalidations != n {
		t.Fatalf("paused recording repainted %d times", p.view.invalidations-n)
	}
	r.AddMark("m", base+10_000)
	p.tick(a.snap)
	if p.view.invalidations != n+1 {
		t.Fatal("a new marker did not repaint")
	}
	r.SetEnabled(true)
	r.Add(&metrics.Snapshot{T: base + 70_000, CPU: 50})
	a.snap = &metrics.Snapshot{T: base + 70_000}
	p.tick(a.snap)
	if p.view.invalidations != n+2 {
		t.Fatal("a new sample did not repaint")
	}
}

// Arrow keys step through samples exactly as the full-ring search did.
func TestRecorderKeysStepThroughSamples(t *testing.T) {
	const base = int64(1_700_000_000_000)
	r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
	r.SetEnabled(true)
	var times []int64
	for i, gap := range []int64{0, 2000, 5000, 2000, 9000, 2000} {
		ts := base + gap
		if i > 0 {
			ts = times[i-1] + gap
		}
		times = append(times, ts)
		r.Add(&metrics.Snapshot{T: ts})
	}
	p := newRecorderPage(&App{rec: r, snap: &metrics.Snapshot{T: times[len(times)-1]}})
	press := func(k walk.Key, want int64) {
		t.Helper()
		p.pageKey(k, 0)
		if p.live || p.selT != want {
			t.Fatalf("key %v: selected %d live=%v, want %d", k, p.selT-base, p.live, want-base)
		}
	}
	press(walk.KeyLeft, times[4]) // from live: the sample before the newest
	press(walk.KeyLeft, times[3])
	press(walk.KeyRight, times[4])
	press(walk.KeyHome, times[0])
	press(walk.KeyLeft, times[0])
	press(walk.KeyRight, times[1])
	p.selT = times[2] + 500 // a scrubbed time between samples: nearest is times[2]
	press(walk.KeyRight, times[3])
	p.pageKey(walk.KeyEnd, 0)
	press(walk.KeyRight, times[5])
}

// Painting primitives and graphs, cached or not, must not leak GDI objects.
func TestPaintingKeepsGDIObjectCountStable(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 300, Height: 120}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		resources := windows.NewLazySystemDLL("user32.dll").NewProc("GetGuiResources")
		count := func() uintptr { n, _, _ := resources.Call(^uintptr(0), 0); return n }
		var c graphCache
		paint := func(i int) {
			fillRect(cv, rect{0, 0, 300, 120}, T.Bg)
			fillRound(cv, rect{4, 4, 100, 40}, 8, T.Card)
			strokeRound(cv, rect{4, 4, 100, 40}, 8, T.Stroke)
			hline(cv, 0, 300, 60, T.Grid)
			drawText(cv, "CPU", uiFont(9), T.Text, rect{8, 8, 80, 16}, taLeft)
			_ = textWidth(cv, "CPU", uiFont(9))
			ser := []series{{vals: []float64{10, 80, float64(i % 50), 60}, color: T.CPU, fill: true}}
			plotSeries(cv, rect{0, 60, 300, 60}, ser, graphStyle{bg: T.Card, max: 100, lineW: 2, grid: true})
			activeGraphs = &c
			c.begin()
			plotSeries(cv, rect{0, 0, 150, 60}, ser, graphStyle{bg: T.Card, max: 100, lineW: 2})
			c.end()
			activeGraphs = nil
		}
		paint(0)
		before := count()
		for i := 0; i < 100; i++ {
			paint(i)
		}
		if after := count(); after > before {
			t.Fatalf("GDI objects grew after 100 paints: %d -> %d", before, after)
		}
	})
}
