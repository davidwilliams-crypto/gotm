//go:build windows

package ui

import (
	"image"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"gotm/internal/metrics"
)

// Irregular sampling must not move a spike away from its time on the axis.
// Exercise the actual page renderer, so feeding index-based data to the shared
// graph renderer is caught even if the mouse/marker math remains correct.
func TestRecorderPlotUsesSampleTimes(t *testing.T) {
	runNativeTest(t, func() {
		oldScale := gs
		gs = 1
		defer func() { gs = oldScale }()
		const base = int64(1_700_000_000_000)
		r := metrics.NewRecorder(time.Hour, 2*time.Second, "")
		r.SetEnabled(true)
		for _, s := range []struct {
			seconds int64
			cpu     float64
		}{{0, 80}, {5, 20}, {10, 80}, {30, 20}, {40, 80}, {60, 20}} {
			r.Add(&metrics.Snapshot{T: base + s.seconds*1000, CPU: s.cpu})
		}
		r.AddMark("marker", base+15_000)
		p := newRecorderPage(&App{rec: r, snap: &metrics.Snapshot{T: base + 60_000}})
		p.live, p.selT = false, base+45_000
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 648, Height: 700}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		p.paint(cv, 648, 700) // chart: x=24..624, y=56..366; one second = 10 px
		cv.Dispose()
		im, err := bmp.ToImage()
		if err != nil {
			panic(err)
		}
		for _, point := range []struct{ x, y int }{{25, 119}, {74, 304}, {124, 119}, {324, 304}, {424, 119}, {623, 304}} {
			if !hasColorNear(im, point.x, point.y, T.CPU, 3) {
				t.Errorf("CPU curve missing at timestamp position (%d,%d)", point.x, point.y)
			}
		}
		if !hasColorNear(im, 174, 200, T.Warn, 0) {
			t.Error("15-second marker is not at x=174")
		}
		if !hasColorNear(im, 474, 200, T.Text, 0) {
			t.Error("45-second selection cursor is not at x=474")
		}
		p.view.w, p.view.h = 648, 700
		for _, pick := range []struct {
			x    int
			want int64
		}{{0, base}, {24, base}, {74, base + 5000}, {124, base + 10_000}, {324, base + 30_000}, {624, base + 60_000}, {648, base + 60_000}} {
			p.pick(pick.x, 200)
			if p.selT != pick.want || p.live {
				t.Errorf("scrub x=%d: time=%d live=%v, want %d", pick.x, p.selT, p.live, pick.want)
			}
		}
	})
}

func hasColorNear(im image.Image, x, y int, color walk.Color, radius int) bool {
	for yy := y - radius; yy <= y+radius; yy++ {
		for xx := x - radius; xx <= x+radius; xx++ {
			r, g, b, _ := im.At(xx, yy).RGBA()
			// HALFTONE downsampling can adjust individual channel intensities.
			// Tolerate that rounding while excluding the much darker area fill.
			if abs(int(r>>8)-int(color.R())) <= 16 && abs(int(g>>8)-int(color.G())) <= 16 && abs(int(b>>8)-int(color.B())) <= 16 {
				return true
			}
		}
	}
	return false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
