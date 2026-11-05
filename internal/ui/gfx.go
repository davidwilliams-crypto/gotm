//go:build windows

package ui

import (
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
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

var (
	procSetDCBrushColor   = gdi32.NewProc("SetDCBrushColor")
	procSetDCPenColor     = gdi32.NewProc("SetDCPenColor")
	procSetDIBitsToDevice = gdi32.NewProc("SetDIBitsToDevice")
)

// The primitives below paint with the DC's stock DC_BRUSH and DC_PEN, whose
// colour is a property of the device context. A paint issues hundreds of
// them; creating and deleting a GDI brush or pen for each one was measurable,
// and there is nothing to release (or leak) this way. The DC_PEN is a solid
// one-pixel cosmetic pen, the same pen walk.NewCosmeticPen created.
func dcBrush(hdc win.HDC, c walk.Color) win.HGDIOBJ {
	procSetDCBrushColor.Call(uintptr(hdc), uintptr(c))
	return win.GetStockObject(win.DC_BRUSH)
}

func dcPen(hdc win.HDC, c walk.Color) win.HGDIOBJ {
	procSetDCPenColor.Call(uintptr(hdc), uintptr(c))
	return win.GetStockObject(win.DC_PEN)
}

func fillRect(cv *walk.Canvas, r rect, c walk.Color) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	fillRectPixels(cv.HDC(), r.wr(), c)
}

func fillRectPixels(hdc win.HDC, w walk.Rectangle, c walk.Color) {
	if w.Width <= 0 || w.Height <= 0 {
		return
	}
	rc := win.RECT{Left: int32(w.X), Top: int32(w.Y), Right: int32(w.X + w.Width), Bottom: int32(w.Y + w.Height)}
	win.FillRect(hdc, &rc, win.HBRUSH(dcBrush(hdc, c)))
}

func fillRound(cv *walk.Canvas, r rect, radius int, c walk.Color) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	hdc, w, e := cv.HDC(), r.wr(), int32(sc(radius)*2)
	oldBrush := win.SelectObject(hdc, dcBrush(hdc, c))
	oldPen := win.SelectObject(hdc, win.GetStockObject(win.NULL_PEN))
	// A null pen leaves the right and bottom edge unpainted: extend by one
	// pixel, exactly as walk's FillRoundedRectanglePixels did.
	win.RoundRect(hdc, int32(w.X), int32(w.Y), int32(w.X+w.Width+1), int32(w.Y+w.Height+1), e, e)
	win.SelectObject(hdc, oldPen)
	win.SelectObject(hdc, oldBrush)
}

func strokeRound(cv *walk.Canvas, r rect, radius int, c walk.Color) {
	if r.W <= 1 || r.H <= 1 {
		return
	}
	hdc, w, e := cv.HDC(), r.wr(), int32(sc(radius)*2)
	oldBrush := win.SelectObject(hdc, win.GetStockObject(win.NULL_BRUSH))
	oldPen := win.SelectObject(hdc, dcPen(hdc, c))
	win.RoundRect(hdc, int32(w.X), int32(w.Y), int32(w.X+w.Width-1), int32(w.Y+w.Height-1), e, e)
	win.SelectObject(hdc, oldPen)
	win.SelectObject(hdc, oldBrush)
}

func hline(cv *walk.Canvas, x1, x2, y int, c walk.Color) {
	linePixels(cv.HDC(), sc(x1), sc(y), sc(x2), sc(y), c)
}

// linePixels draws a one-pixel cosmetic line in device pixels.
func linePixels(hdc win.HDC, x1, y1, x2, y2 int, c walk.Color) {
	oldPen := win.SelectObject(hdc, dcPen(hdc, c))
	win.MoveToEx(hdc, x1, y1, nil)
	win.LineTo(hdc, int32(x2), int32(y2))
	win.SelectObject(hdc, oldPen)
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

// textWidthKey identifies one measurement. Fonts are interned by fontOf, so
// the pointer identifies the face; the DPI and scale select its pixel size.
type textWidthKey struct {
	s   string
	f   *walk.Font
	dpi int
	gs  float64
}

// textWidths remembers measured labels. A paint measures the same few strings
// again and again; values and units change rarely. The map is bounded: it is
// simply emptied when full.
var (
	textWidthMu sync.Mutex
	textWidths  = map[textWidthKey]int{}
)

const textWidthCacheMax = 512

// textWidth returns the natural width in design units. Walk also uses this
// routine to count how many characters fit, so request a calculated rectangle
// explicitly. Otherwise DrawTextEx leaves the supplied width unchanged.
func textWidth(cv *walk.Canvas, s string, f *walk.Font) int {
	key := textWidthKey{s, f, cv.DPI(), gs}
	textWidthMu.Lock()
	w, ok := textWidths[key]
	textWidthMu.Unlock()
	if ok {
		return w
	}
	b, _, err := cv.MeasureTextPixels(s, f, walk.Rectangle{Width: 10000, Height: 200}, walk.TextLeft|walk.TextSingleLine|walk.TextNoPrefix|win.DT_CALCRECT)
	if err != nil {
		return len(s) * 8
	}
	w = int(math.Ceil(float64(b.Width) / gs))
	textWidthMu.Lock()
	if len(textWidths) >= textWidthCacheMax {
		clear(textWidths)
	}
	textWidths[key] = w
	textWidthMu.Unlock()
	return w
}

// ---- supersampled graphs ------------------------------------------------------

const supersample = 3 // graphs are rasterized at 3x and box-filtered down, for anti-aliasing

// series is one plotted curve.
type series struct {
	vals  []float64
	times []int64 // optional timestamps; otherwise use the graph's sample capacity
	color walk.Color
	fill  bool
	// ver, when non-zero, identifies the content of vals and times (a data
	// version the caller maintains). The graph cache then trusts it instead
	// of hashing every value. Two series with different content must never
	// share a non-zero ver.
	ver uint64
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
//
// The cost is bounded by the graph's size, not by the number of points: a long
// series is reduced to the first, lowest, highest and last point of each
// output pixel column (see graphVertices). While a canvasView paints, the
// finished image is also cached, so a repaint without new data (hover, mouse,
// scrolling, a paused recording) only copies pixels.
func plotSeries(dst *walk.Canvas, r rect, ser []series, st graphStyle) float64 {
	r = r.px() // from here on everything is in device pixels
	st.lineW = sc(max(st.lineW, 1))
	if r.W < 4 || r.H < 4 {
		return st.max
	}
	hdc := dst.HDC()
	cache := activeGraphs
	var key uint64
	if cache != nil {
		key = graphKey(r.W, r.H, ser, st)
		if img := cache.get(key, r.W, r.H); img != nil {
			blitGraph(hdc, r, img.pix)
			return img.maxV
		}
	}
	var pix []uint32
	if cache != nil {
		pix = cache.alloc(r.W * r.H)
	} else {
		pix = growPixels(&graphOut, r.W*r.H)
	}
	maxV, ok := renderGraph(pix, r.W, r.H, ser, st)
	if !ok {
		return maxV
	}
	if cache != nil {
		cache.put(key, &graphImage{w: r.W, h: r.H, pix: pix, maxV: maxV})
	}
	blitGraph(hdc, r, pix)
	return maxV
}

// graphMaxV is the axis maximum: fixed, or the series' peak rounded up.
func graphMaxV(ser []series, st graphStyle) float64 {
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
	return maxV
}

// Scratch state for rendering. Graphs are painted on the UI thread only.
var (
	graphOut   []uint32     // the 1x image when no cache is active
	graphPts   []walk.Point // supersampled vertices of one series
	graphVals  []float64    // their values (NaN marks a gap)
	graphPoly  []win.POINT  // fill polygons
	graphCount []int32
	graphTops  []int32   // per-column top of a fill
	graphLo    []float64 // per-column extent of a curve
	graphHi    []float64
	// graphSS is the supersampled image; released after a few idle seconds.
	graphSS     []uint32
	graphSSUsed time.Time

	graphRenders int // graphs rendered rather than served from a cache (tests)
)

// dibColor converts a COLORREF (0x00BBGGRR) to a 32-bit DIB pixel (0x00RRGGBB).
func dibColor(c walk.Color) uint32 {
	return uint32(c.R())<<16 | uint32(c.G())<<8 | uint32(c.B())
}

func growPixels(buf *[]uint32, n int) []uint32 {
	if cap(*buf) < n {
		*buf = make([]uint32, n)
	}
	return (*buf)[:n]
}

// renderGraph draws the graph at the supersampled size into a scratch image
// and box-filters it into pix (w*h, top-down 0x00RRGGBB). Everything is
// rasterized here in Go, column by column: GDI scan-converted a long noisy
// curve (as a wide-pen outline, and as a fill polygon) in time that grew with
// the square of the point count, which froze the window for long histories.
func renderGraph(pix []uint32, w, h int, ser []series, st graphStyle) (float64, bool) {
	graphRenders++
	maxV := graphMaxV(ser, st)
	ss := supersample
	for ss > 1 && w*ss*h*ss > 6_000_000 {
		ss-- // cap the off-screen image at about 24 MB
	}
	W, H := w*ss, h*ss
	img := growPixels(&graphSS, W*H)
	graphSSUsed = time.Now()
	fillPixels(img, dibColor(st.bg))
	if st.grid {
		grid := dibColor(T.Grid)
		for i := 1; i < 4; i++ {
			fillPixels(img[(H*i/4)*W:][:W], grid)
		}
		for i := 1; i < 6; i++ {
			for x, y := W*i/6, 0; y < H; y++ {
				img[y*W+x] = grid
			}
		}
	}
	lw := st.lineW
	if lw < 1 {
		lw = 2
	}
	for _, s := range ser {
		if len(s.vals) < 2 {
			continue
		}
		graphPts, graphVals = graphVertices(graphPts[:0], graphVals[:0], s, st, W, H, ss, w, maxV)
		pts, vals := graphPts, graphVals
		if s.fill {
			graphPoly, graphCount = appendGraphFillPolygons(graphPoly[:0], graphCount[:0], pts, vals, W, H)
			graphTops = fillGraphColumns(img, W, W, H, graphPoly, graphCount, dibColor(blend(s.color, st.bg, 0.22)), graphTops)
		}
		strokeGraph(img, W, H, pts, vals, float64(lw*ss)/2, dibColor(s.color))
	}
	downsample(pix, w, h, img, W, ss)
	return maxV, true
}

// strokeGraph paints each finite span of the curve (two points or more) as a
// line of the given half width with round caps and joins, as GDI's geometric
// round pen did: the set of pixels whose centre lies within radius of the
// curve. For a curve that advances left to right that set is, in every
// column, a single span, so it is computed from the curve's vertical extent
// per column (lo..hi) dilated by a disk: O(width x line width) per span plus
// the pixels painted, whatever the number of points.
func strokeGraph(img []uint32, W, H int, pts []walk.Point, vals []float64, radius float64, color uint32) {
	R := int(math.Ceil(radius))
	for i := 0; i < len(vals); {
		if !finiteValue(vals[i]) {
			i++
			continue
		}
		start := i
		for i < len(vals) && finiteValue(vals[i]) {
			i++
		}
		if i-start >= 2 {
			strokeSpan(img, W, H, pts[start:i], radius, R, color)
		}
	}
}

func strokeSpan(img []uint32, W, H int, pts []walk.Point, radius float64, R int, color uint32) {
	// Columns [c0, c1] can hold curve that reaches the image.
	c0, c1 := math.MaxInt, math.MinInt
	for _, p := range pts {
		c0, c1 = min(c0, p.X), max(c1, p.X)
	}
	c0, c1 = max(c0, -R), min(c1, W-1+R)
	if c0 > c1 {
		return
	}
	n := c1 - c0 + 1
	graphLo, graphHi = growFloats(graphLo, n), growFloats(graphHi, n)
	lo, hi := graphLo, graphHi
	for k := range lo {
		lo[k], hi[k] = math.Inf(1), math.Inf(-1)
	}
	extend := func(c int, y float64) {
		if c < c0 || c > c1 {
			return
		}
		lo[c-c0], hi[c-c0] = math.Min(lo[c-c0], y), math.Max(hi[c-c0], y)
	}
	for k := 1; k < len(pts); k++ {
		a, b := pts[k-1], pts[k]
		if a.X > b.X {
			a, b = b, a
		}
		if a.X == b.X {
			extend(a.X, float64(a.Y))
			extend(a.X, float64(b.Y))
			continue
		}
		slope := float64(b.Y-a.Y) / float64(b.X-a.X)
		for c := max(a.X, c0); c <= min(b.X, c1); c++ {
			// The segment's extent within this column: [c, c+1) clipped to [a.X, b.X].
			extend(c, float64(a.Y)+slope*float64(c-a.X))
			if c < b.X {
				extend(c, float64(a.Y)+slope*float64(min(c+1, b.X)-a.X))
			}
		}
	}
	// Dilate by the disk: column x is covered within half(d) of the curve in
	// the column d away, for pixel centres up to radius from it.
	var half [16]float64
	R = min(R, len(half)-1)
	for d := 0; d <= R; d++ {
		half[d] = math.Sqrt(math.Max(0, radius*radius-float64(d*d)))
	}
	for x := max(c0-R, 0); x <= min(c1+R, W-1); x++ {
		top, bottom := math.Inf(1), math.Inf(-1)
		for d := -R; d <= R; d++ {
			c := x + d - c0
			if c < 0 || c >= n || lo[c] > hi[c] {
				continue
			}
			hd := half[max(d, -d)]
			if hd <= 0 {
				continue
			}
			top, bottom = math.Min(top, lo[c]-hd), math.Max(bottom, hi[c]+hd)
		}
		if top > bottom {
			continue
		}
		y0 := max(int(math.Round(top)), 0)
		y1 := min(int(math.Round(bottom)), H)
		for y := y0; y < y1; y++ {
			img[y*W+x] = color
		}
	}
}

// fillPixels sets every pixel of s to c at memory-copy speed.
func fillPixels(s []uint32, c uint32) {
	if len(s) == 0 {
		return
	}
	s[0] = c
	for n := 1; n < len(s); n *= 2 {
		copy(s[n:], s[:n])
	}
}

func growFloats(s []float64, n int) []float64 {
	if cap(s) < n {
		return make([]float64, n)
	}
	return s[:n]
}

// graphVertices converts one series to supersampled vertices, appending to
// pts and vals (vals[i] is NaN where the curve has a gap). Up to two points per
// output column are kept as drawn. A longer series is decimated per output
// pixel column: of each finite run of points in a column only the first, the
// lowest, the highest and the last remain, in their original order and at
// their own positions, so peaks, end points and gaps survive while the work
// stays proportional to the graph width. Timed series are bucketed by their
// time position, so irregular sampling keeps every peak at its moment.
func graphVertices(pts []walk.Point, vals []float64, s series, st graphStyle, W, H, ss, outW int, maxV float64) ([]walk.Point, []float64) {
	n := len(s.vals)
	step := 0.0
	if n > 1 {
		// Without timestamps the series is spread across the full width by
		// its own length, not by the ring's capacity: n can exceed capN for a
		// while after the refresh rate is lowered, and a capacity-based step
		// would push x far off the left edge.
		step = float64(W) / float64(n-1)
	}
	timed := len(s.times) == n && st.timeTo > st.timeFrom
	xAt := func(i int) int {
		if timed {
			return int(timePosition(s.times[i], st.timeFrom, st.timeTo) * float64(W))
		}
		return int(float64(i) * step)
	}
	hf, span := float64(H), float64(H-ss)
	at := func(i int) walk.Point {
		f := s.vals[i] / maxV
		if f < 0 {
			f = 0
		} else if f > 1 {
			f = 1
		}
		return walk.Point{X: xAt(i), Y: int(hf - f*span)}
	}
	if n <= 2*outW {
		for i, v := range s.vals {
			if !finiteValue(v) {
				pts, vals = append(pts, walk.Point{}), append(vals, math.NaN())
				continue
			}
			pts, vals = append(pts, at(i)), append(vals, v)
		}
		return pts, vals
	}
	// Bucket by output column; everything left of the chart shares bucket -1
	// and everything right of it bucket outW. Only x is needed per point.
	var (
		active              bool
		col                 int
		first, lo, hi, last int // indices: first, lowest value, highest value, last
	)
	emit := func(i int) {
		pts, vals = append(pts, at(i)), append(vals, s.vals[i])
	}
	flush := func() {
		if !active {
			return
		}
		active = false
		emit(first)
		a, b := min(lo, hi), max(lo, hi)
		if a != first && a != last {
			emit(a)
		}
		if b != first && b != last && b != a {
			emit(b)
		}
		if last != first {
			emit(last)
		}
	}
	gap := false
	for i, v := range s.vals {
		if v != v || math.IsInf(v, 0) {
			flush()
			if !gap && len(vals) > 0 {
				pts, vals = append(pts, walk.Point{}), append(vals, math.NaN())
			}
			gap = true
			continue
		}
		gap = false
		c := xAt(i)
		switch {
		case c < 0:
			c = -1
		case c >= W:
			c = outW
		default:
			c /= ss
		}
		if !active || c != col {
			flush()
			active, col = true, c
			first, lo, hi, last = i, i, i, i
			continue
		}
		if v < s.vals[lo] {
			lo = i
		}
		if v > s.vals[hi] {
			hi = i
		}
		last = i
	}
	flush()
	return pts, vals
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

// graphFillPolygons describes the area under the curve, clipped to the
// bitmap: a vertical span from the curve down to the bottom for every column
// it covers. Missing samples remain gaps.
func graphFillPolygons(pts []walk.Point, vals []float64, width, height int) ([]win.POINT, []int32) {
	return appendGraphFillPolygons(nil, nil, pts, vals, width, height)
}

// appendGraphFillPolygons emits one polygon per run of consecutive finite
// segments whose x never decreases: the clipped curve closed along the
// bottom. A single polygon keeps the scan conversion proportional to the
// width (one trapezoid per segment, each reaching the bottom, made GDI's cost
// grow with the square of the point count). A segment that runs backwards
// (repeated or reversed timestamps) gets a trapezoid of its own, and the
// winding fill mode turns overlaps into a union, so nothing self-intersects
// into a hole. Polygons end one pixel past their last column because GDI
// leaves a filled polygon's right edge unpainted.
func appendGraphFillPolygons(polygons []win.POINT, counts []int32, pts []walk.Point, vals []float64, width, height int) ([]win.POINT, []int32) {
	if len(pts) < 2 || len(vals) != len(pts) || width <= 0 || height <= 0 {
		return polygons, counts
	}
	yAt := func(a, b walk.Point, x int) int32 {
		y := float64(b.Y)
		if b.X != a.X {
			y = float64(a.Y) + (float64(b.Y)-float64(a.Y))*(float64(x)-float64(a.X))/(float64(b.X)-float64(a.X))
		}
		return int32(math.Max(0, math.Min(float64(height), y)))
	}
	start := -1 // index in polygons where the open polygon begins
	var lastA, lastB walk.Point
	var lastRight int
	closePolygon := func() {
		if start < 0 {
			return
		}
		x := lastRight + 1
		polygons = append(polygons, win.POINT{X: int32(x), Y: yAt(lastA, lastB, x)}, win.POINT{X: int32(x), Y: int32(height)})
		counts = append(counts, int32(len(polygons)-start))
		start = -1
	}
	for i := 1; i < len(pts); i++ {
		if !finiteValue(vals[i-1]) || !finiteValue(vals[i]) {
			closePolygon()
			continue
		}
		a, b := pts[i-1], pts[i]
		forward := b.X >= a.X
		left, right := max(min(a.X, b.X), 0), min(max(a.X, b.X), width-1)
		if left > right {
			if !forward {
				closePolygon()
			}
			continue // entirely outside the bitmap
		}
		if !forward {
			closePolygon()
		}
		if start < 0 {
			start = len(polygons)
			polygons = append(polygons, win.POINT{X: int32(left), Y: int32(height)}, win.POINT{X: int32(left), Y: yAt(a, b, left)})
		} else {
			// The previous segment's end is an interior vertex.
			polygons = append(polygons, win.POINT{X: int32(lastRight), Y: yAt(lastA, lastB, lastRight)})
		}
		lastA, lastB, lastRight = a, b, right
		if !forward {
			closePolygon()
		}
	}
	closePolygon()
	return polygons, counts
}

// fillGraphColumns paints the union of the polygons from
// appendGraphFillPolygons into img (top-down, row stride in pixels). Each of
// them is closed along the bottom and its top boundary runs left to right, so
// the union is, for every column, one span from the highest top edge down to
// the bottom. Painting those spans costs O(width*height) whatever the number
// of vertices; GDI's polygon scan conversion of a long, noisy curve grew with
// the square of its vertex count (about 250 ms for 1800 points). Like GDI,
// a polygon covers the columns from its left edge up to, not including, its
// right edge. tops is scratch space, returned for reuse.
func fillGraphColumns(img []uint32, stride, w, h int, polygons []win.POINT, counts []int32, color uint32, tops []int32) []int32 {
	if len(counts) == 0 || w <= 0 || h <= 0 {
		return tops
	}
	if cap(tops) < w {
		tops = make([]int32, w)
	}
	tops = tops[:w]
	for i := range tops {
		tops[i] = int32(h)
	}
	raise := func(x int, y float64) {
		if x < 0 || x >= w {
			return
		}
		t := int32(math.Max(0, math.Min(float64(h), math.Round(y))))
		if t < tops[x] {
			tops[x] = t
		}
	}
	start := 0
	for _, count := range counts {
		poly := polygons[start : start+int(count)]
		start += int(count)
		if len(poly) < 4 {
			continue
		}
		top := poly[1 : len(poly)-1] // drop the two bottom corners
		for i := 1; i < len(top); i++ {
			a, b := top[i-1], top[i]
			if a.X > b.X {
				a, b = b, a
			}
			if a.X == b.X {
				// A vertical step (a decimated column's lowest and highest
				// point) still lifts its column to the higher end.
				if b.X < top[len(top)-1].X {
					raise(int(a.X), float64(min(a.Y, b.Y)))
				}
				continue
			}
			raise(int(a.X), float64(a.Y)) // keep a peak at a vertex
			dx := float64(b.X - a.X)
			for x := max(int(a.X), 0); x < min(int(b.X), w); x++ {
				raise(x, float64(a.Y)+float64(b.Y-a.Y)*(float64(x)+0.5-float64(a.X))/dx)
			}
		}
	}
	// Paint row by row (sequential memory) from the highest top down.
	first, lead, last := h, -1, -1
	for x, t := range tops {
		if int(t) < h {
			if lead < 0 {
				lead = x
			}
			first, last = min(first, int(t)), x
		}
	}
	if last < 0 {
		return tops
	}
	span := tops[lead : last+1]
	for y := first; y < h; y++ {
		row := img[y*stride+lead : y*stride+last+1]
		for x, t := range span {
			if t <= int32(y) {
				row[x] = color
			}
		}
	}
	return tops
}

// ---- off-screen surface and downsampling -------------------------------------
// ---- downsampling and output -----------------------------------------------------

// The supersampled image is released once no graph has been drawn for longer
// than the slowest refresh interval (10 s), e.g. after leaving the page.
const graphScratchIdle = 12 * time.Second

// downsample box-filters an ss*ss supersampled image (row stride in pixels)
// into dst (w*h). Each output pixel is the rounded mean of its ss*ss source
// pixels, which is what the HALFTONE StretchBlt previously used here computed,
// at a fraction of its cost. Large graphs are split across CPUs by rows.
func downsample(dst []uint32, w, h int, src []uint32, stride, ss int) {
	rows := func(y0, y1 int) {
		n := uint32(ss * ss)
		half := n / 2
		for oy := y0; oy < y1; oy++ {
			out := dst[oy*w : oy*w+w]
			base := oy * ss * stride
			if ss == 3 {
				r0 := src[base : base+3*w]
				r1 := src[base+stride : base+stride+3*w]
				r2 := src[base+2*stride : base+2*stride+3*w]
				for ox := range out {
					k := 3 * ox
					a, b, c := r0[k:k+3:k+3], r1[k:k+3:k+3], r2[k:k+3:k+3]
					rb := a[0]&0xFF00FF + a[1]&0xFF00FF + a[2]&0xFF00FF +
						b[0]&0xFF00FF + b[1]&0xFF00FF + b[2]&0xFF00FF +
						c[0]&0xFF00FF + c[1]&0xFF00FF + c[2]&0xFF00FF
					g := a[0]&0xFF00 + a[1]&0xFF00 + a[2]&0xFF00 +
						b[0]&0xFF00 + b[1]&0xFF00 + b[2]&0xFF00 +
						c[0]&0xFF00 + c[1]&0xFF00 + c[2]&0xFF00
					out[ox] = ((rb>>16+4)/9)<<16 | ((g>>8+4)/9)<<8 | ((rb&0xFFFF + 4) / 9)
				}
				continue
			}
			for ox := range out {
				var rb, g uint32
				p := base + ox*ss
				for dy := 0; dy < ss; dy++ {
					for _, c := range src[p+dy*stride : p+dy*stride+ss] {
						rb += c & 0xFF00FF
						g += c & 0xFF00
					}
				}
				out[ox] = ((rb>>16+half)/n)<<16 | ((g>>8+half)/n)<<8 | ((rb&0xFFFF + half) / n)
			}
		}
	}
	workers := min(runtime.GOMAXPROCS(0), h/24, 8)
	if workers < 2 || w*h < 40_000 {
		rows(0, h)
		return
	}
	var wg sync.WaitGroup
	per := (h + workers - 1) / workers
	for y := 0; y < h; y += per {
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			rows(y0, y1)
		}(y, min(h, y+per))
	}
	wg.Wait()
}

// blitGraph copies a finished w*h image to r (device pixels) on hdc, within
// the DC's clip region.
func blitGraph(hdc win.HDC, r rect, pix []uint32) {
	if len(pix) < r.W*r.H || r.W <= 0 || r.H <= 0 {
		return
	}
	bmi := win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})), BiWidth: int32(r.W), BiHeight: -int32(r.H), BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	procSetDIBitsToDevice.Call(uintptr(hdc), uintptr(r.X), uintptr(r.Y), uintptr(r.W), uintptr(r.H),
		0, 0, 0, uintptr(r.H), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&bmi)), win.DIB_RGB_COLORS)
}

// ---- rendered graph cache ---------------------------------------------------------

// graphImage is one finished graph at its output size.
type graphImage struct {
	w, h int
	pix  []uint32
	maxV float64
	used uint64 // paint generation that last drew it
}

// graphCache keeps each canvas's finished graphs between paints. Entries are
// keyed by everything that affects the image: size, style, theme colours and
// the data (a version or a hash of the values), so a retheme, a DPI change, a
// resize or new data simply misses. Graphs not drawn by a paint are dropped at
// its end, and a canvas that is no longer visible releases its cache.
type graphCache struct {
	entries map[uint64]*graphImage
	gen     uint64
	spare   [][]uint32 // pixels of dropped images, reused by the next paint
}

// activeGraphs is the cache of the canvas being painted (UI thread only).
var activeGraphs *graphCache

const graphCacheMax = 256 // more graphs than any page draws

func (c *graphCache) begin() { c.gen++ }

func (c *graphCache) get(key uint64, w, h int) *graphImage {
	img := c.entries[key]
	if img == nil || img.w != w || img.h != h {
		return nil
	}
	img.used = c.gen
	return img
}

func (c *graphCache) put(key uint64, img *graphImage) {
	if c.entries == nil {
		c.entries = map[uint64]*graphImage{}
	}
	if len(c.entries) >= graphCacheMax {
		c.end()
		if len(c.entries) >= graphCacheMax {
			clear(c.entries)
		}
	}
	img.used = c.gen
	c.entries[key] = img
}

// end drops the graphs the last paint did not draw. Their pixel buffers are
// kept for the next paint, which usually redraws the same graphs with new
// data; buffers that stay unused for a whole paint are released.
func (c *graphCache) end() {
	c.spare = c.spare[:0]
	for k, img := range c.entries {
		if img.used != c.gen {
			delete(c.entries, k)
			if len(c.spare) < graphCacheMax {
				c.spare = append(c.spare, img.pix)
			}
		}
	}
}

// alloc returns a buffer of n pixels, recycled when possible.
func (c *graphCache) alloc(n int) []uint32 {
	for i, b := range c.spare {
		if cap(b) >= n && cap(b) <= 2*n {
			last := len(c.spare) - 1
			c.spare[i] = c.spare[last]
			c.spare[last] = nil
			c.spare = c.spare[:last]
			return b[:n]
		}
	}
	return make([]uint32, n)
}

func (c *graphCache) reset() { c.entries, c.spare = nil, nil }

func (c *graphCache) len() int { return len(c.entries) }

// graphKey hashes everything that determines a graph's pixels.
func graphKey(w, h int, ser []series, st graphStyle) uint64 {
	k := keyHash(0x9e3779b97f4a7c15)
	k.add(uint64(w), uint64(h), uint64(st.bg), math.Float64bits(st.max), uint64(st.lineW),
		uint64(st.timeFrom), uint64(st.timeTo), uint64(len(ser)))
	if st.grid {
		k.add(1, uint64(T.Grid))
	}
	for _, s := range ser {
		fill := uint64(0)
		if s.fill {
			fill = 1
		}
		k.add(uint64(s.color), fill, uint64(len(s.vals)), uint64(len(s.times)))
		if s.ver != 0 {
			k.add(s.ver)
			continue
		}
		for _, v := range s.vals {
			k.add(math.Float64bits(v))
		}
		for _, t := range s.times {
			k.add(uint64(t))
		}
	}
	return uint64(k)
}

// keyHash is a small multiply-xorshift hash over 64-bit words.
type keyHash uint64

func (k *keyHash) add(words ...uint64) {
	h := uint64(*k)
	for _, w := range words {
		h ^= w
		h *= 0xff51afd7ed558ccd
		h ^= h >> 32
	}
	*k = keyHash(h)
}

func hashWords(words ...uint64) uint64 {
	k := keyHash(0xcbf29ce484222325)
	k.add(words...)
	return uint64(k)
}

// Canvases with cached graphs, so the caches (and the shared surface) can be
// released once their page is hidden. Checked at most twice a second, from
// the end of any canvas paint (the footer repaints every tick).
var (
	graphOwners    []*canvasView
	graphSweptAt   time.Time
	graphSweepRate = 500 * time.Millisecond
)

func registerGraphOwner(v *canvasView) {
	for _, o := range graphOwners {
		if o == v {
			return
		}
	}
	graphOwners = append(graphOwners, v)
}

func sweepGraphCaches(now time.Time) {
	if now.Sub(graphSweptAt) < graphSweepRate {
		return
	}
	graphSweptAt = now
	kept := graphOwners[:0]
	for _, v := range graphOwners {
		if v.cw != nil && !v.cw.IsDisposed() && win.IsWindowVisible(v.cw.Handle()) {
			kept = append(kept, v)
			continue
		}
		v.releaseGraphs()
	}
	clear(graphOwners[len(kept):])
	graphOwners = kept
	if graphSS != nil && now.Sub(graphSSUsed) > graphScratchIdle {
		graphSS, graphOut = nil, nil
	}
}
