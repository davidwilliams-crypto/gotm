//go:build windows

package ui

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// renderPixels renders ser with renderGraph into a fresh w*h image.
func renderPixels(w, h int, ser []series, st graphStyle) []uint32 {
	pix := make([]uint32, w*h)
	renderGraph(pix, w, h, ser, st)
	return pix
}

// pixelDiff reports how many pixels differ and the largest channel difference.
func pixelDiff(a, b []uint32) (n, worst int) {
	for i := range a {
		if a[i] == b[i] {
			continue
		}
		n++
		for shift := 0; shift < 24; shift += 8 {
			d := int(a[i]>>shift&0xFF) - int(b[i]>>shift&0xFF)
			worst = max(worst, d, -d)
		}
	}
	return n, worst
}

// A decimated long series must render like the full series: decimation is
// per supersampled column, the resolution the curve is rasterized at, so a
// noisy history or a spike's edges look the same as with every point drawn.
func TestDecimatedGraphMatchesFullRender(t *testing.T) {
	const w, h = 400, 100 // 1200 supersampled columns: 3600 points are decimated
	r := rand.New(rand.NewSource(42))
	noisy := make([]float64, 3600)
	for i := range noisy {
		noisy[i] = 30 + 25*r.Float64()
	}
	for i := 1200; i < 1210; i++ {
		noisy[i] = math.NaN()
	}
	spike := make([]float64, 3600)
	for i := range spike {
		spike[i] = 10
	}
	spike[1801], spike[2500] = 97, 2
	times := make([]int64, 14400)
	timed := make([]float64, len(times))
	for i := range times {
		times[i] = int64(i)*1000 + int64(r.Intn(400))
		timed[i] = 50 + 40*math.Sin(float64(i)/9) + 8*r.Float64()
	}
	cases := []struct {
		name string
		ser  []series
		st   graphStyle
	}{
		{"noisy filled", []series{{vals: noisy, color: T.CPU, fill: true}}, graphStyle{bg: T.Card, max: 100, lineW: 2, grid: true}},
		{"noisy line", []series{{vals: noisy, color: T.Warn}}, graphStyle{bg: T.Card, max: 100, lineW: 1}},
		{"spike", []series{{vals: spike, color: T.CPU, fill: true}}, graphStyle{bg: T.Card, max: 100, lineW: 2}},
		{"timed", []series{{vals: timed, times: times, color: T.Mem, fill: true}},
			graphStyle{bg: T.Card, max: 100, lineW: 2, timeFrom: times[0], timeTo: times[len(times)-1]}},
	}
	defer func() { graphNoDecimation = false }()
	for _, c := range cases {
		graphNoDecimation = false
		got := renderPixels(w, h, c.ser, c.st)
		graphNoDecimation = true
		want := renderPixels(w, h, c.ser, c.st)
		n, worst := pixelDiff(got, want)
		t.Logf("%s: %d of %d pixels differ, worst channel difference %d", c.name, n, w*h, worst)
		if n > w*h/1000 || worst > 16 {
			t.Errorf("%s: decimated render differs from the full one on %d pixels (worst %d/255)", c.name, n, worst)
		}
	}
	// The decimation really ran (it is what keeps long histories cheap).
	graphNoDecimation = false
	pts, _ := graphVertices(nil, nil, series{vals: timed, times: times}, graphStyle{timeFrom: times[0], timeTo: times[len(times)-1]},
		w*supersample, h*supersample, supersample, w, 100)
	if len(pts) >= len(timed) {
		t.Fatalf("14,400 points were not decimated (%d vertices)", len(pts))
	}
}

// A timed series that runs backwards passes through the same columns twice;
// the stroke must cover both passes and not the space between them.
func TestStrokeHandlesBackwardsTimestamps(t *testing.T) {
	const W, H = 80, 100
	img := make([]uint32, W*H)
	pts := []walk.Point{{X: 10, Y: 20}, {X: 50, Y: 20}, {X: 10, Y: 80}, {X: 50, Y: 80}}
	vals := []float64{1, 1, 1, 1}
	strokeGraph(img, W, H, pts, vals, 1.5, 0xFFFFFF)
	at := func(x, y int) bool { return img[y*W+x] != 0 }
	for _, p := range []walk.Point{{X: 30, Y: 20}, {X: 30, Y: 80}, {X: 30, Y: 50}, {X: 12, Y: 77}} {
		if !at(p.X, p.Y) {
			t.Errorf("stroke missing at %v", p)
		}
	}
	// Column 15 holds the top pass (y 20), the diagonal (y ~72) and the
	// bottom pass (y 80); y 40 is far from all of them.
	for _, y := range []int{30, 40, 60} {
		if at(15, y) {
			t.Errorf("gap between two passes through column 15 filled at y=%d", y)
		}
	}
}

// For data that only advances, splitting into monotonic runs changes nothing.
func TestStrokeMonotonicDataUnchanged(t *testing.T) {
	const W, H = 300, 120
	r := rand.New(rand.NewSource(7))
	var pts []walk.Point
	x := 0
	for x < W {
		pts = append(pts, walk.Point{X: x, Y: 10 + r.Intn(100)})
		x += r.Intn(4) // repeated x (vertical steps) included
	}
	vals := make([]float64, len(pts))
	a, b := make([]uint32, W*H), make([]uint32, W*H)
	strokeGraph(a, W, H, pts, vals, 3, 0xFFFFFF)
	strokeSpan(b, W, H, pts, 3, 3, 0xFFFFFF)
	if n, _ := pixelDiff(a, b); n != 0 {
		t.Fatalf("monotonic stroke changed on %d pixels", n)
	}
}

// textWidth runs inside walk's buffered paint, whose canvas is never
// disposed: it must not create GDI objects owned by the canvas. (walk's
// MeasureTextPixels created a metafile DC per canvas, which leaked one GDI
// object per Performance repaint in the live window.)
func TestTextWidthCreatesNoGDIObjects(t *testing.T) {
	runNativeTest(t, func() {
		resources := windows.NewLazySystemDLL("user32.dll").NewProc("GetGuiResources")
		count := func() uintptr { n, _, _ := resources.Call(^uintptr(0), 0); return n }
		newCanvas := func() (*walk.Canvas, func()) {
			bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 64, Height: 32}, 96)
			if err != nil {
				panic(err)
			}
			cv, err := walk.NewCanvasFromImage(bmp)
			if err != nil {
				panic(err)
			}
			return cv, func() { cv.Dispose(); bmp.Dispose() }
		}
		saved := gs
		gs = 1
		defer func() { gs = saved }()
		warm, done := newCanvas()
		_ = textWidth(warm, "warm-up", uiLight(23)) // the font's handles for this DPI
		done()
		for i := 0; i < 20; i++ {
			cv, done := newCanvas()
			before := count()
			for k := 0; k < 5; k++ {
				_ = textWidth(cv, fmt.Sprintf("%d%% %d", i, k), uiLight(23)) // never cached
			}
			after := count()
			done()
			if after != before {
				t.Fatalf("measuring on a canvas created %d GDI objects", after-before)
			}
		}
		cv, done := newCanvas()
		defer done()
		// Same widths as walk's measurement (DT_CALCRECT on a single line).
		for _, s := range []string{"CPU", "100%", "Recording  1h 02m", "↓ 12.4 MB/s"} {
			got := textWidth(cv, s+" ", uiFont(9))
			b, _, err := cv.MeasureTextPixels(s+" ", uiFont(9), walk.Rectangle{Width: 10000, Height: 200},
				walk.TextLeft|walk.TextSingleLine|walk.TextNoPrefix|win.DT_CALCRECT)
			if err != nil || got != b.Width {
				t.Errorf("width of %q = %d, walk measures %d (%v)", s, got, b.Width, err)
			}
		}
	})
}

// Graph caches belong to canvases on the current page of a visible window. A
// layout suspend (which clears WS_VISIBLE through WM_SETREDRAW) must keep
// them; leaving the page or hiding the window must release them, the latter
// at once (with the shared surfaces), since nothing paints while hidden.
func TestGraphCachesFollowPageAndWindowNotRedrawSuspend(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		_ = mw.SetLayout(walk.NewVBoxLayout())
		a := navigationTestApp(mw)
		_ = a.right.SetLayout(walk.NewVBoxLayout())
		for _, k := range []int{pgPerformance, pgRecorder} {
			w, err := walk.NewComposite(a.right)
			if err != nil {
				panic(err)
			}
			_ = w.SetLayout(walk.NewVBoxLayout())
			a.wraps[k] = w
		}
		a.cur = pgPerformance
		cw, err := walk.NewCustomWidgetPixels(a.wraps[pgPerformance], 0, func(*walk.Canvas, walk.Rectangle) error { return nil })
		if err != nil {
			panic(err)
		}
		v := newCanvasView(nil)
		v.cw = cw
		released := 0
		v.onHide = func() { released++ }
		fill := func() {
			v.graphs.begin()
			v.graphs.put(1, &graphImage{w: 1, h: 1, pix: make([]uint32, 1)})
			registerGraphOwner(v)
		}
		sweep := func() {
			graphSweptAt = time.Time{}
			sweepGraphCaches(time.Now())
		}
		savedShown := graphOwnerShown
		defer func() { graphOwnerShown = savedShown; releaseGraphMemory() }()
		win.ShowWindow(win.HWND(mw.Handle()), win.SW_SHOWNOACTIVATE)

		for _, shown := range []struct {
			name string
			f    func(*canvasView) bool
		}{{"app state", a.canvasShown}, {"window styles", canvasWindowShown}} {
			graphOwnerShown = shown.f
			fill()
			mw.SetSuspended(true)
			if win.IsWindowVisible(cw.Handle()) {
				t.Logf("%s: layout suspend left the canvas visible", shown.name)
			}
			sweep()
			mw.SetSuspended(false)
			if v.graphs.len() != 1 || released != 0 {
				t.Fatalf("%s: a layout suspend released the graph caches", shown.name)
			}
			sweep()
			if v.graphs.len() != 1 {
				t.Fatalf("%s: caches of the shown page released", shown.name)
			}
		}

		graphOwnerShown = a.canvasShown
		a.cur = pgRecorder // another page shown
		sweep()
		if v.graphs.len() != 0 || released != 1 {
			t.Fatalf("leaving the page kept its caches (%d, released %d)", v.graphs.len(), released)
		}

		a.cur = pgPerformance
		fill()
		graphSS, graphOut = make([]uint32, 1<<16), make([]uint32, 16)
		graphSSUsed = time.Now() // recently used: the idle release would keep it
		a.setHidden(hideMinimized, hideMinimized)
		if v.graphs.len() != 0 || released != 2 || graphSS != nil || graphOut != nil || len(graphOwners) != 0 {
			t.Fatalf("minimizing kept graph memory: %d cached, released %d, scratch %d", v.graphs.len(), released, len(graphSS))
		}
		a.setHidden(hideMinimized, 0)
	})
}
