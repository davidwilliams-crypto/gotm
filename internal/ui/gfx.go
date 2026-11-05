//go:build windows

package ui

import (
	"math"
	"strings"
	"unsafe"

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

// textWidth returns the natural width in design units. Walk also uses this
// routine to count how many characters fit, so request a calculated rectangle
// explicitly. Otherwise DrawTextEx leaves the supplied width unchanged.
func textWidth(cv *walk.Canvas, s string, f *walk.Font) int {
	b, _, err := cv.MeasureTextPixels(s, f, walk.Rectangle{Width: 10000, Height: 200}, walk.TextLeft|walk.TextSingleLine|walk.TextNoPrefix|win.DT_CALCRECT)
	if err != nil {
		return len(s) * 8
	}
	return int(math.Ceil(float64(b.Width) / gs))
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
				if !math.IsInf(v, 0) && v > maxV {
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
	// The graph is opaque and is copied with SRCCOPY. A native temporary
	// bitmap avoids Walk's alpha postprocessing (a full read/scan/write of
	// every supersampled pixel on Canvas.Dispose), which is unused here.
	hdc := win.CreateCompatibleDC(dst.HDC())
	if hdc == 0 {
		return maxV
	}
	defer win.DeleteDC(hdc)
	header := win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})), BiWidth: int32(W), BiHeight: int32(H), BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(hdc, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bitmap == win.ERROR_INVALID_PARAMETER {
		return maxV
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	oldBitmap := win.SelectObject(hdc, win.HGDIOBJ(bitmap))
	if oldBitmap == 0 {
		return maxV
	}
	defer win.SelectObject(hdc, oldBitmap)
	brush := win.CreateBrushIndirect(&win.LOGBRUSH{LbStyle: win.BS_SOLID, LbColor: win.COLORREF(st.bg)})
	if brush == 0 {
		return maxV
	}
	win.FillRect(hdc, &win.RECT{Right: int32(W), Bottom: int32(H)}, brush)
	win.DeleteObject(win.HGDIOBJ(brush))
	if st.grid {
		pen := win.ExtCreatePen(win.PS_COSMETIC|win.PS_SOLID, 1, &win.LOGBRUSH{LbStyle: win.BS_SOLID, LbColor: win.COLORREF(T.Grid)}, 0, nil)
		if pen != 0 {
			oldPen := win.SelectObject(hdc, win.HGDIOBJ(pen))
			for i := 1; i < 4; i++ {
				y := H * i / 4
				win.MoveToEx(hdc, 0, y, nil)
				win.LineTo(hdc, int32(W), int32(y))
			}
			for i := 1; i < 6; i++ {
				x := W * i / 6
				win.MoveToEx(hdc, x, 0, nil)
				win.LineTo(hdc, int32(x), int32(H))
			}
			win.SelectObject(hdc, oldPen)
			win.DeleteObject(win.HGDIOBJ(pen))
		}
	}
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
		// Without timestamps the series is spread across the full width by
		// its own length, not by the ring's capacity: n can exceed capN for a
		// while after the refresh rate is lowered, and a capacity-based step
		// would push x far off the left edge (and make the fill loop below
		// stroke tens of thousands of off-chart columns per frame).
		step := float64(W) / float64(n-1)
		timed := len(s.times) == n && st.timeTo > st.timeFrom
		for i, v := range s.vals {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			y := float64(H) - math.Min(1, math.Max(0, v/maxV))*float64(H-ss)
			x := float64(i) * step
			if timed {
				x = timePosition(s.times[i], st.timeFrom, st.timeTo) * float64(W)
			}
			pts[i] = walk.Point{X: int(x), Y: int(y)}
		}
		if s.fill {
			fillGraphArea(hdc, pts, s.vals, W, H, blend(s.color, st.bg, 0.22))
		}
		pen := win.ExtCreatePen(win.PS_GEOMETRIC|win.PS_SOLID|win.PS_ENDCAP_ROUND|win.PS_JOIN_ROUND, uint32(lw*ss), &win.LOGBRUSH{LbStyle: win.BS_SOLID, LbColor: win.COLORREF(s.color)}, 0, nil)
		if pen != 0 {
			oldPen := win.SelectObject(hdc, win.HGDIOBJ(pen))
			nativePoints := make([]win.POINT, len(pts))
			for i, p := range pts {
				nativePoints[i] = win.POINT{X: int32(p.X), Y: int32(p.Y)}
			}
			for _, span := range finiteSpans(s.vals) {
				if span[1]-span[0] >= 2 {
					win.Polyline(hdc, unsafe.Pointer(&nativePoints[span[0]]), int32(span[1]-span[0]))
				}
			}
			win.SelectObject(hdc, oldPen)
			win.DeleteObject(win.HGDIOBJ(pen))
		}
	}

	// halftone downsample onto the destination
	win.SetStretchBltMode(dst.HDC(), win.HALFTONE)
	win.SetBrushOrgEx(dst.HDC(), 0, 0, nil)
	win.StretchBlt(dst.HDC(), int32(r.X), int32(r.Y), int32(r.W), int32(r.H),
		hdc, 0, 0, int32(W), int32(H), win.SRCCOPY)
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

func finiteValue(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// finiteSpans preserves missing samples as gaps rather than joining across them.
func finiteSpans(values []float64) [][2]int {
	var spans [][2]int
	for i := 0; i < len(values); {
		if !finiteValue(values[i]) {
			i++
			continue
		}
		start := i
		for i < len(values) && finiteValue(values[i]) {
			i++
		}
		spans = append(spans, [2]int{start, i})
	}
	return spans
}

var (
	procGraphPolyPolygon     = gdi32.NewProc("PolyPolygon")
	procGraphSetPolyFillMode = gdi32.NewProc("SetPolyFillMode")
)

// graphFillPolygons describes the same union of clipped vertical areas as the
// old per-column strokes. Separate trapezoids retain missing-data gaps and
// also handle repeated or reversed timestamps without self-intersection.
func graphFillPolygons(pts []walk.Point, vals []float64, width, height int) ([]win.POINT, []int32) {
	if len(pts) < 2 || len(vals) != len(pts) || width <= 0 || height <= 0 {
		return nil, nil
	}
	polygons := make([]win.POINT, 0, 4*(len(pts)-1))
	counts := make([]int32, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		if !finiteValue(vals[i-1]) || !finiteValue(vals[i]) {
			continue
		}
		a, b := pts[i-1], pts[i]
		left, right := max(min(a.X, b.X), 0), min(max(a.X, b.X), width-1)
		if left > right {
			continue
		}
		yAt := func(x int) int32 {
			y := float64(b.Y)
			if b.X != a.X {
				y = float64(a.Y) + (float64(b.Y)-float64(a.Y))*(float64(x)-float64(a.X))/(float64(b.X)-float64(a.X))
			}
			return int32(math.Max(0, math.Min(float64(height), y)))
		}
		polygons = append(polygons, win.POINT{X: int32(left), Y: int32(height)}, win.POINT{X: int32(left), Y: yAt(left)}, win.POINT{X: int32(right + 1), Y: yAt(right + 1)}, win.POINT{X: int32(right + 1), Y: int32(height)})
		counts = append(counts, 4)
	}
	return polygons, counts
}

func fillGraphArea(hdc win.HDC, pts []walk.Point, vals []float64, width, height int, color walk.Color) {
	polygons, counts := graphFillPolygons(pts, vals, width, height)
	if len(counts) == 0 {
		return
	}
	brush := win.CreateBrushIndirect(&win.LOGBRUSH{LbStyle: win.BS_SOLID, LbColor: win.COLORREF(color)})
	if brush == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(brush))
	saved := win.SaveDC(hdc)
	if saved == 0 {
		return
	}
	defer win.RestoreDC(hdc, saved)
	win.SelectObject(hdc, win.HGDIOBJ(brush))
	win.SelectObject(hdc, win.GetStockObject(win.NULL_PEN))
	// Winding fills overlapping trapezoids as a union, not alternating holes.
	procGraphSetPolyFillMode.Call(uintptr(hdc), 2)
	procGraphPolyPolygon.Call(uintptr(hdc), uintptr(unsafe.Pointer(&polygons[0])), uintptr(unsafe.Pointer(&counts[0])), uintptr(len(counts)))
}
