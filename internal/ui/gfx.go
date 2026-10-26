//go:build windows

package ui

import (
	"math"
	"strings"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func syscallUTF16(s string) (*uint16, error) { return windows.UTF16PtrFromString(s) }

// fontInstalled reports whether a TrueType family appears in the font registry.
func fontInstalled(family string) bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion\Fonts`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.HasPrefix(n, family) {
			return true
		}
	}
	return false
}

// All layout and painting code works in 96-DPI "design units". gs converts to
// device pixels; it is refreshed at the start of every paint from the canvas
// DPI, so the same numbers look right at 100%, 150% or 200% scaling.
var gs = 1.0

func sc(v int) int { return int(math.Round(float64(v) * gs)) }

// rect is a rectangle in design units.
type rect struct{ X, Y, W, H int }

func (r rect) px() rect { return rect{sc(r.X), sc(r.Y), sc(r.W), sc(r.H)} }

// wr converts to a device-pixel walk.Rectangle (scaling from design units).
func (r rect) wr() walk.Rectangle {
	p := r.px()
	return walk.Rectangle{X: p.X, Y: p.Y, Width: p.W, Height: p.H}
}
func (r rect) contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}
func (r rect) inset(d int) rect { return rect{r.X + d, r.Y + d, r.W - 2*d, r.H - 2*d} }

// pen/brush helpers --------------------------------------------------------------

func solid(c walk.Color) *walk.SolidColorBrush {
	b, _ := walk.NewSolidColorBrush(c)
	return b
}

func fillRect(cv *walk.Canvas, r rect, c walk.Color) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	b := solid(c)
	defer b.Dispose()
	_ = cv.FillRectanglePixels(b, r.wr())
}

func fillRound(cv *walk.Canvas, r rect, radius int, c walk.Color) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	b := solid(c)
	defer b.Dispose()
	_ = cv.FillRoundedRectanglePixels(b, r.wr(), walk.Size{Width: sc(radius) * 2, Height: sc(radius) * 2})
}

func strokeRound(cv *walk.Canvas, r rect, radius int, c walk.Color) {
	if r.W <= 1 || r.H <= 1 {
		return
	}
	p, _ := walk.NewCosmeticPen(walk.PenSolid, c)
	defer p.Dispose()
	w := r.wr()
	_ = cv.DrawRoundedRectanglePixels(p, walk.Rectangle{X: w.X, Y: w.Y, Width: w.Width - 1, Height: w.Height - 1},
		walk.Size{Width: sc(radius) * 2, Height: sc(radius) * 2})
}

func hline(cv *walk.Canvas, x1, x2, y int, c walk.Color) {
	p, _ := walk.NewCosmeticPen(walk.PenSolid, c)
	defer p.Dispose()
	_ = cv.DrawLinePixels(p, walk.Point{X: sc(x1), Y: sc(y)}, walk.Point{X: sc(x2), Y: sc(y)})
}

// text alignment flags
const (
	taLeft   = walk.TextLeft | walk.TextVCenter | walk.TextSingleLine | walk.TextEndEllipsis | walk.TextNoPrefix
	taRight  = walk.TextRight | walk.TextVCenter | walk.TextSingleLine | walk.TextEndEllipsis | walk.TextNoPrefix
	taCenter = walk.TextCenter | walk.TextVCenter | walk.TextSingleLine | walk.TextEndEllipsis | walk.TextNoPrefix
)

func drawText(cv *walk.Canvas, s string, f *walk.Font, c walk.Color, r rect, flags walk.DrawTextFormat) {
	if s == "" || r.W <= 0 || r.H <= 0 {
		return
	}
	_ = cv.DrawTextPixels(s, f, c, r.wr(), flags|walk.TextEndEllipsis)
}

// textWidth measures s in pixels.
func textWidth(cv *walk.Canvas, s string, f *walk.Font) int {
	b, _, err := cv.MeasureTextPixels(s, f, walk.Rectangle{Width: 10000, Height: 200}, walk.TextLeft|walk.TextSingleLine|walk.TextNoPrefix)
	if err != nil {
		return len(s) * 8
	}
	return int(float64(b.Width) / gs)
}

// ---- supersampled graphs ------------------------------------------------------

const supersample = 3 // GDI has no anti-aliasing, so graphs are drawn at 3x and downsampled

// series is one plotted curve.
type series struct {
	vals  []float64
	times []int64 // optional timestamps; otherwise use the graph's sample capacity
	color walk.Color
	fill  bool
}

type graphStyle struct {
	bg       walk.Color
	max      float64 // 0 = autoscale
	capacity int     // number of samples the full width represents
	grid     bool
	lineW    int // line width in output pixels
	smooth   bool
	timeFrom int64 // timestamp domain, used with series.times
	timeTo   int64
}

// niceCeil rounds m up to a "round" axis maximum.
func niceCeil(m float64) float64 {
	if m <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(m)))
	for _, s := range []float64{1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10} {
		if m <= s*mag {
			return s * mag
		}
	}
	return 10 * mag
}

// plotSeries draws curves into r on dst using 3x supersampling. It returns the
// axis maximum that was used so callers can label it.
func plotSeries(dst *walk.Canvas, r rect, ser []series, st graphStyle) float64 {
	r = r.px() // from here on everything is in device pixels
	st.lineW = sc(max(st.lineW, 1))
	saved := gs
	gs = 1
	defer func() { gs = saved }()
	if r.W < 4 || r.H < 4 {
		return st.max
	}
	maxV := st.max
	if maxV == 0 {
		for _, s := range ser {
			for _, v := range s.vals {
				if v > maxV {
					maxV = v
				}
			}
		}
		maxV = niceCeil(maxV * 1.1)
	}
	ss := supersample
	for ss > 1 && r.W*ss*r.H*ss > 6_000_000 {
		ss-- // cap the off-screen bitmap at about 24 MB
	}
	W, H := r.W*ss, r.H*ss
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: W, Height: H}, 96)
	if err != nil {
		return maxV
	}
	defer bmp.Dispose()
	cv, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		return maxV
	}
	defer cv.Dispose()

	fillRect(cv, rect{0, 0, W, H}, st.bg)
	if st.grid {
		gp, _ := walk.NewCosmeticPen(walk.PenSolid, T.Grid)
		for i := 1; i < 4; i++ {
			y := H * i / 4
			_ = cv.DrawLinePixels(gp, walk.Point{X: 0, Y: y}, walk.Point{X: W, Y: y})
		}
		for i := 1; i < 6; i++ {
			x := W * i / 6
			_ = cv.DrawLinePixels(gp, walk.Point{X: x, Y: 0}, walk.Point{X: x, Y: H})
		}
		gp.Dispose()
	}

	capN := st.capacity
	if capN < 2 {
		capN = 2
	}
	step := float64(W) / float64(capN-1)
	lw := st.lineW
	if lw < 1 {
		lw = 2
	}

	for _, s := range ser {
		n := len(s.vals)
		if n < 2 {
			continue
		}
		pts := make([]walk.Point, n)
		x0 := float64(W) - float64(n-1)*step
		for i, v := range s.vals {
			y := float64(H) - math.Min(1, math.Max(0, v/maxV))*float64(H-ss)
			x := x0 + float64(i)*step
			if len(s.times) == n && st.timeTo > st.timeFrom {
				x = timePosition(s.times[i], st.timeFrom, st.timeTo) * float64(W)
			}
			pts[i] = walk.Point{X: int(x), Y: int(y)}
		}
		if s.fill {
			// area under the curve, as 1px (supersampled) vertical strokes
			fillC := blend(s.color, st.bg, 0.22)
			fp, _ := walk.NewCosmeticPen(walk.PenSolid, fillC)
			for i := 1; i < n; i++ {
				a, b := pts[i-1], pts[i]
				for x := a.X; x <= b.X; x++ {
					y := b.Y
					if b.X != a.X {
						y = a.Y + (b.Y-a.Y)*(x-a.X)/(b.X-a.X)
					}
					_ = cv.DrawLinePixels(fp, walk.Point{X: x, Y: H}, walk.Point{X: x, Y: y})
				}
			}
			fp.Dispose()
		}
		br := solid(s.color)
		gp, err := walk.NewGeometricPen(walk.PenSolid|walk.PenCapRound|walk.PenJoinRound, lw*ss, br)
		if err == nil {
			_ = cv.DrawPolylinePixels(gp, pts)
			gp.Dispose()
		}
		br.Dispose()
	}

	// halftone downsample onto the destination
	win.SetStretchBltMode(dst.HDC(), win.HALFTONE)
	win.SetBrushOrgEx(dst.HDC(), 0, 0, nil)
	win.StretchBlt(dst.HDC(), int32(r.X), int32(r.Y), int32(r.W), int32(r.H),
		cv.HDC(), 0, 0, int32(W), int32(H), win.SRCCOPY)
	return maxV
}

// timePosition gives samples, markers and the selection cursor the same
// normalized position, including the exact start and end of the chart.
func timePosition(t, from, to int64) float64 {
	if to <= from {
		return 0
	}
	return float64(t-from) / float64(to-from)
}
