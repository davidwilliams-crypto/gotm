//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	"golang.org/x/sys/windows"
	"math"
	"testing"
)

func BenchmarkPlotSeriesFilled(b *testing.B) {
	for _, width := range []int{160, 800} {
		b.Run(fmtWidth(width), func(b *testing.B) {
			done := make(chan struct{})
			nativeTests <- func() {
				defer close(done)
				bmp, err := walk.NewBitmapForDPI(walk.Size{Width: width, Height: 240}, 96)
				if err != nil {
					panic(err)
				}
				defer bmp.Dispose()
				cv, err := walk.NewCanvasFromImage(bmp)
				if err != nil {
					panic(err)
				}
				defer cv.Dispose()
				vals := make([]float64, 120)
				for i := range vals {
					vals[i] = 50 + 30*math.Sin(float64(i)/8)
				}
				ser := []series{{vals: vals, color: T.CPU, fill: true}}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					plotSeries(cv, rect{0, 0, width, 240}, ser, graphStyle{bg: T.Card, max: 100, lineW: 2})
				}
				b.StopTimer()
			}
			<-done
		})
	}
}
func fmtWidth(w int) string {
	if w == 160 {
		return "spark160"
	}
	return "main800"
}

func TestGraphFillClipsAndPreservesGaps(t *testing.T) {
	pts := []walk.Point{{X: -100, Y: 20}, {X: 100, Y: 40}, {X: 200, Y: 10}, {X: 300, Y: 30}, {X: 500, Y: 50}}
	vals := []float64{1, 2, math.NaN(), 3, 4}
	polygons, counts := graphFillPolygons(pts, vals, 400, 100)
	if len(counts) != 2 || len(polygons) != 8 {
		t.Fatalf("gap joined: points=%v counts=%v", polygons, counts)
	}
	for _, p := range polygons {
		if p.X < 0 || p.X > 400 || p.Y < 0 || p.Y > 100 {
			t.Fatalf("unclipped vertex: %+v", p)
		}
	}
	if polygons[1].X != 0 || polygons[1].Y != 30 {
		t.Fatalf("left crossing not interpolated: %v", polygons)
	}
	if polygons[6].X != 400 || polygons[6].Y != 40 {
		t.Fatalf("right crossing not interpolated: %v", polygons)
	}
}

func TestGraphFillHandlesReverseAndVerticalSegments(t *testing.T) {
	points, counts := graphFillPolygons([]walk.Point{{X: 30, Y: 20}, {X: 10, Y: 40}, {X: 10, Y: 25}}, []float64{1, 2, 3}, 100, 80)
	if len(counts) != 2 || points[1].X != 10 || points[1].Y != 40 || points[5].Y != 25 {
		t.Fatalf("reverse/equal timestamps: %v %v", points, counts)
	}
}

func TestGraphFillRenderedGapsAndBounds(t *testing.T) {
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
		bg := walk.RGB(0, 0, 0)
		line := walk.RGB(255, 255, 255)
		outside := walk.RGB(255, 0, 0)
		fillRect(cv, rect{0, 0, 420, 120}, outside)
		plotSeries(cv, rect{10, 10, 400, 100}, []series{{vals: []float64{80, 80, math.NaN(), 80, 80}, color: line, fill: true}}, graphStyle{bg: bg, max: 100, lineW: 1})
		cv.Dispose()
		im, err := bmp.ToImage()
		if err != nil {
			panic(err)
		}
		for _, p := range []struct {
			x, y  int
			color walk.Color
		}{{50, 90, blend(line, bg, 0.22)}, {350, 90, blend(line, bg, 0.22)}, {210, 90, bg}, {5, 90, outside}, {415, 90, outside}} {
			if !hasColorNear(im, p.x, p.y, p.color, 0) {
				t.Errorf("unexpected fill at %d,%d", p.x, p.y)
			}
		}
	})
}

func TestGraphDrawingReleasesNativeResources(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 160, Height: 80}, 96)
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
		ser := []series{{vals: []float64{10, 80, 30, 60}, color: T.CPU, fill: true}}
		paint := func() {
			plotSeries(cv, rect{0, 0, 160, 80}, ser, graphStyle{bg: T.Card, max: 100, lineW: 2, grid: true})
		}
		paint()
		before := count()
		for i := 0; i < 50; i++ {
			paint()
		}
		if after := count(); after > before {
			t.Fatalf("GDI objects grew after 50 paints: %d -> %d", before, after)
		}
	})
}
