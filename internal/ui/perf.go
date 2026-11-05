//go:build windows

package ui

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"

	"gotm/internal/metrics"
)

// perfPage draws the whole Performance screen into one back buffer: resource
// cards on the left, the selected resource's graph and facts on the right.
type perfPage struct {
	app     *App
	root    *walk.Composite
	v       *canvasView
	sel     int
	perCore bool
	gpuOver bool // GPU detail: one overall graph instead of one per engine
	// hist is the history the graphs show: the mirror's live points, synced
	// incrementally each tick (perf_series.go). Treat it as read-only.
	hist   []metrics.HistPoint
	mirror histMirror
	curves map[seriesKey]*derivedSeries
	paints uint64

	// GPU process list scrolling (design units for the area, rows for the offset)
	gpuScroll int
	gpuList   rect
	gpuRows   int
	gpuProcs  int

	// The detail viewport scrolls only when its facts or core tiles cannot fit.
	detailScroll, detailMax int
	detailArea              rect

	railArea             rect
	railFirst, railCount int

	// hover readout on the main graph: the sample under the mouse (-1 = none)
	// and the plot area it maps to, recorded while painting.
	hoverIdx     int
	hoverGraph   rect
	hoverGraphOn bool
}

// resItem is one card on the left: a resource kind, plus which adapter for GPUs.
type resItem struct {
	kind int
	gpu  int // index into Snapshot.GPUs, -1 for the legacy combined GPU card
}

// items lists the cards: CPU, Memory, one per display adapter, Disk, Network.
func (p *perfPage) items(s *metrics.Snapshot) []resItem {
	out := []resItem{{resCPU, -1}, {resMem, -1}}
	switch {
	case s != nil && len(s.GPUs) > 0:
		for i := range s.GPUs {
			out = append(out, resItem{resGPU, i})
		}
	default:
		out = append(out, resItem{resGPU, -1})
	}
	return append(out, resItem{resDisk, -1}, resItem{resNet, -1})
}

const (
	resCPU = iota
	resMem
	resGPU
	resDisk
	resNet
	resCount
)

// resTitle and resColor describe a resource kind; the colour comes from the
// live palette so a theme change reaches the cards too.
func resTitle(kind int) string {
	return [resCount]string{"CPU", "Memory", "GPU", "Disk", "Network"}[kind]
}

func resColor(kind int) walk.Color {
	switch kind {
	case resCPU:
		return T.CPU
	case resMem:
		return T.Mem
	case resGPU:
		return T.GPU
	case resDisk:
		return T.Disk
	}
	return T.Net
}

func newPerfPage(a *App) *perfPage {
	p := &perfPage{app: a, perCore: true, hoverIdx: -1}
	if v := os.Getenv("GOTM_PERF_SEL"); v != "" { // debugging aid: open on a given card
		fmt.Sscanf(v, "%d", &p.sel)
	}
	p.v = newCanvasView(p.paint)
	p.v.onHide = p.releaseDerived
	p.v.keyboard, p.v.keyName = true, "Performance. Up and Down select a resource, Space or Enter changes its graph view. Page Up and Page Down scroll through details."
	p.v.acc, p.v.accName, p.v.accCtl = true, "Performance", ctlPane
	p.v.onClick = func(id string, _ walk.MouseButton) {
		var n int
		switch {
		case id == "rail:up":
			p.scrollRail(-1)
		case id == "rail:down":
			p.scrollRail(1)
		case id == "cores":
			p.perCore = !p.perCore
		case id == "gpuview:overall":
			p.gpuOver = true
		case id == "gpuview:engines":
			p.gpuOver = false
		default:
			if _, err := fmt.Sscanf(id, "res:%d", &n); err == nil {
				if p.sel != n {
					p.gpuScroll, p.detailScroll = 0, 0
				}
				p.sel = n
			}
		}
		p.v.invalidate()
	}
	return p
}

func (p *perfPage) build() decl.Widget {
	return decl.Composite{
		AssignTo:      &p.root,
		StretchFactor: 1,
		Background:    decl.SolidColorBrush{Color: T.Bg},
		Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			p.app.heads[pgPerformance].declare(0, 76, 0, 76, 0),
			p.v.declare(0, 0, 0, 0, bodyStretch),
			p.app.foots[pgPerformance].declare(0, 32, 0, 32, 0),
		},
	}
}

func (p *perfPage) init() {
	p.app.registerRoot(pgPerformance, p.root)
	p.v.onWheel = func(delta int) {
		if p.railArea.px().contains(p.v.mx, p.v.my) {
			p.scrollRail(-delta / 40)
			return
		}
		if p.gpuList.px().contains(p.v.mx, p.v.my) && p.scrollGPUProcesses(-delta/40) {
			return
		}
		if p.detailArea.px().contains(p.v.mx, p.v.my) {
			p.scrollDetail(-delta / 2)
		}
	}
	// Hovering the big graph pins a marker on the sample under the cursor and
	// repaints only when that sample changes, so a sweep costs no more than the
	// regular refresh.
	p.v.onMove = func(x, y int) {
		if idx := p.hoverIndexAt(x, y); idx != p.hoverIdx {
			p.hoverIdx = idx
			p.v.invalidate()
		}
	}
	p.v.attachWheel()
}

// hoverIndexAt maps a mouse position (device pixels) to the history sample it
// sits over, or -1 when the position is off the main graph.
func (p *perfPage) hoverIndexAt(x, y int) int {
	if !p.hoverGraphOn {
		return -1
	}
	r := p.hoverGraph.px()
	if !r.contains(x, y) || (p.detailArea.H > 0 && !p.detailArea.px().contains(x, y)) {
		return -1
	}
	n := len(p.hist)
	if n < 2 || r.W <= 0 {
		return -1
	}
	f := (float64(x) - float64(r.X)) / float64(r.W)
	return clampI(int(math.Round(f*float64(n-1))), 0, n-1)
}

func (p *perfPage) clampGPUScroll() {
	if max := p.gpuProcs - p.gpuRows; p.gpuScroll > max {
		p.gpuScroll = max
	}
	if p.gpuScroll < 0 {
		p.gpuScroll = 0
	}
}

func (p *perfPage) scrollGPUProcesses(rows int) bool {
	if p.gpuRows == 0 || p.gpuProcs <= p.gpuRows {
		return false
	}
	before := p.gpuScroll
	p.gpuScroll += rows
	p.clampGPUScroll()
	if p.gpuScroll == before {
		return false
	}
	p.v.invalidate()
	return true
}

func (p *perfPage) shown()                   { p.v.invalidate() }
func (p *perfPage) focusTarget() walk.Window { return p.v.cw }
func (p *perfPage) reload()                  { p.syncHistory(); p.v.invalidate() }
func (p *perfPage) pageKey(key walk.Key, mods walk.Modifiers) bool {
	if mods != 0 {
		return false
	}
	last := len(p.items(p.app.snap)) - 1
	p.sel = clampI(p.sel, 0, last)
	switch key {
	case walk.KeyLeft, walk.KeyUp:
		p.sel = max(0, p.sel-1)
	case walk.KeyRight, walk.KeyDown:
		p.sel = min(last, p.sel+1)
	case walk.KeyHome:
		p.sel = 0
	case walk.KeyEnd:
		p.sel = last
	case walk.KeyPrior, walk.KeyNext:
		direction := 1
		if key == walk.KeyPrior {
			direction = -1
		}
		// The process list follows the graphs and facts. Page through its
		// rows at the bottom, then back through the facts on the way up.
		if p.detailScroll == p.detailMax && p.scrollGPUProcesses(direction*p.gpuRows) {
			return true
		}
		p.scrollDetail(direction * max(40, p.detailArea.H-32))
		return true
	case walk.KeySpace, walk.KeyReturn:
		it := p.items(p.app.snap)[p.sel]
		if it.kind == resCPU {
			p.perCore = !p.perCore
		} else if it.kind == resGPU && it.gpu >= 0 && len(p.app.snap.GPUs[it.gpu].Engines) > 0 {
			p.gpuOver = !p.gpuOver
		}
	default:
		return false
	}
	p.gpuScroll, p.detailScroll = 0, 0
	p.revealResource()
	p.v.keyHit = fmt.Sprintf("res:%d", p.sel)
	p.v.invalidate()
	return true
}
func (p *perfPage) tick(*metrics.Snapshot) {
	p.syncHistory()
	p.v.invalidate()
}

func (p *perfPage) capacity() int {
	c := p.app.cfg.HistorySeconds * 1000 / p.app.cfg.RefreshMs
	if c < 10 {
		c = 10
	}
	return c
}

func histCPU(h *metrics.HistPoint) float64   { return h.CPU }
func histMem(h *metrics.HistPoint) float64   { return h.Mem }
func histDiskR(h *metrics.HistPoint) float64 { return h.DiskR }
func histDiskW(h *metrics.HistPoint) float64 { return h.DiskW }
func histNetRx(h *metrics.HistPoint) float64 { return h.NetRx }
func histNetTx(h *metrics.HistPoint) float64 { return h.NetTx }
func histGPU(h *metrics.HistPoint) float64 {
	if !h.GPUOK {
		return math.NaN()
	}
	return h.GPU
}

// plotted is one derived curve ready to draw.
func (p *perfPage) plotted(kind int, f func(*metrics.HistPoint) float64, c walk.Color, fill bool) series {
	vals, ver := p.curve(seriesKey{kind: kind}, f)
	return series{vals: vals, ver: ver, color: c, fill: fill}
}

// seriesFor extracts the curves of one card from the history.
func (p *perfPage) seriesFor(it resItem) []series {
	col := resColor(it.kind)
	switch it.kind {
	case resCPU:
		return []series{p.plotted(serCPU, histCPU, col, true)}
	case resMem:
		return []series{p.plotted(serMem, histMem, col, true)}
	case resGPU:
		if it.gpu >= 0 && p.app.snap != nil && it.gpu < len(p.app.snap.GPUs) {
			vals, ver := p.gpuCurve(serGPUAdp, p.app.snap.GPUs[it.gpu].ID, "", gpuPointUtil)
			return []series{{vals: vals, ver: ver, color: col, fill: true}}
		}
		return []series{p.plotted(serGPU, histGPU, col, true)}
	case resDisk:
		return []series{p.plotted(serDiskR, histDiskR, T.Disk, true), p.plotted(serDiskW, histDiskW, T.Warn, false)}
	}
	return []series{p.plotted(serNetRx, histNetRx, T.Net, true), p.plotted(serNetTx, histNetTx, T.CPU, false)}
}

func (p *perfPage) fixedMax(r int) float64 {
	if r == resDisk || r == resNet {
		return 0
	}
	return 100
}

func (p *perfPage) headline(it resItem, s *metrics.Snapshot) (value, sub string, pct float64) {
	switch it.kind {
	case resCPU:
		speed := ""
		if len(s.CPUFreq) > 0 {
			var sum float64
			for _, f := range s.CPUFreq {
				sum += f
			}
			speed = fmt.Sprintf(" · %.2f GHz", sum/float64(len(s.CPUFreq))/1000)
		}
		if w, ok := metrics.EnergyPackage(s); ok {
			speed += fmt.Sprintf(" · %.0f W", w)
		}
		return fmt.Sprintf("%.0f%%", s.CPU), fmt.Sprintf("%d logical processors%s", p.app.host.NumCPU, speed), s.CPU
	case resMem:
		return fmt.Sprintf("%.0f%%", s.Mem.Percent), fmt.Sprintf("%s of %s", fmtBytes(s.Mem.Used), fmtBytes(s.Mem.Total)), s.Mem.Percent
	case resGPU:
		if it.gpu >= 0 && it.gpu < len(s.GPUs) {
			g := &s.GPUs[it.gpu]
			if !g.UtilizationAvailable() {
				return "n/a", shortGPUName(g.Name), 0
			}
			return fmt.Sprintf("%.0f%%", g.Pct), shortGPUName(g.Name), g.Pct
		}
		if !s.GPU.OK {
			return "n/a", "GPU counters unavailable", 0
		}
		return fmt.Sprintf("%.0f%%", s.GPU.Percent), fmtBytes(s.GPU.MemBytes) + " video memory", s.GPU.Percent
	case resDisk:
		return fmt.Sprintf("%.0f%%", s.Disk.Percent), "R " + fmtRate(s.Disk.ReadBps) + "  W " + fmtRate(s.Disk.WriteBps), s.Disk.Percent
	}
	return fmtRate(s.Net.RxBps + s.Net.TxBps), "↓ " + fmtRate(s.Net.RxBps) + "   ↑ " + fmtRate(s.Net.TxBps), 0
}

func (p *perfPage) paint(cv *walk.Canvas, w, h int) {
	s := p.app.snap
	p.gpuRows = 0 // set again by the GPU detail when it draws a process list
	p.hoverGraphOn = false
	p.paints++
	if p.paints%64 == 0 {
		p.pruneCurves()
	}
	if s == nil {
		drawText(cv, "Collecting the first sample…", uiFont(12), T.Muted, rect{24, 24, w - 48, 30}, taLeft)
		return
	}
	v := p.v

	// ---- resource cards -------------------------------------------------------
	items := p.items(s)
	p.sel = clampI(p.sel, 0, len(items)-1)
	if v.keyHit == "" {
		v.keyHit = fmt.Sprintf("res:%d", p.sel) // keyboard focus starts on the selected card
	}
	margin := clampI(w/42, 10, 24)
	railW := clampI((w-2*margin)/4, 106, 282)
	p.paintRail(cv, rect{margin, 10, railW, h - 20}, items, s)

	// The detail takes the remaining width even on a small remote desktop.
	x0 := margin + railW + margin
	dw := w - x0 - margin
	cur := items[p.sel]
	kind := cur.kind
	// The same demand as App.gpuSensorsWanted, from the item already chosen
	// above instead of rebuilding the card list during every paint.
	metrics.SetGPUSensorsWanted(p.app.cur == pgPower || (p.app.cur == pgPerformance && kind == resGPU))
	title := resTitle(kind)
	var device string
	switch kind {
	case resCPU:
		device = trimSpace(p.app.host.CPUName)
	case resMem:
		device = fmtBytes(s.Mem.Total) + " installed"
	case resGPU:
		device = "All graphics adapters"
		if cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
			title = fmt.Sprintf("GPU %d", cur.gpu)
			device = s.GPUs[cur.gpu].Name
		}
	case resDisk:
		device = "All physical disks"
	case resNet:
		device = "All network adapters"
	}
	value, sub, pct := p.headline(cur, s)
	if kind == resGPU && cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
		sub = gpuSubtitle(&s.GPUs[cur.gpu])
	}
	span := "History window · " + humanDur(time.Duration(p.app.cfg.HistorySeconds)*time.Second)
	valueFont := uiLight(23)
	valueW := textWidth(cv, value, valueFont) + 4
	drawText(cv, title, uiBold(17), T.Text, rect{x0, 3, dw - valueW - 12, 36}, taLeft)
	drawText(cv, value, valueFont, resColor(kind), rect{x0 + dw - valueW, 1, valueW, 38}, taRight)
	drawText(cv, device, uiFont(10), T.Text, rect{x0, 39, dw, 19}, taLeft)
	spanW := textWidth(cv, span, uiFont(9)) + 4
	drawText(cv, sub, uiFont(9), T.Muted, rect{x0, 59, dw - spanW - 10, 18}, taLeft)
	drawText(cv, span, uiFont(9), T.Muted, rect{x0 + dw - spanW, 59, spanW, 18}, taRight)
	v.addText(rect{x0, 3, dw, 74}, fmt.Sprintf("%s: %s. %s. %s. %s", title, value, device, sub, span))
	if kind != resNet && value != "n/a" {
		bar := rect{x0, 82, dw, 3}
		fillRect(cv, bar, T.Grid)
		fillRect(cv, rect{bar.X, bar.Y, clampI(int(float64(dw)*pct/100), 0, dw), bar.H}, resColor(kind))
	} else {
		hline(cv, x0, x0+dw, 83, T.Stroke)
	}

	const top = 96
	if kind == resGPU && cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
		p.paintGPUBody(cv, cur, x0, dw, top, h)
		return
	}
	stats := p.statsFor(cur, s)
	statCols := clampI(dw/142, 2, 4)
	const factStep = 50
	statH := ((len(stats)+statCols-1)/statCols)*factStep + 12
	const minGraph = 112
	bodyH := max(1, h-top)
	gridH, cols, rows, cellH := 0, 0, 0, 0
	if kind == resCPU && p.perCore && len(s.Cores) > 0 {
		cols = perCoreCols(dw, len(s.Cores), bodyH-minGraph-statH)
		rows = (len(s.Cores) + cols - 1) / cols
		cellH = clampI((bodyH-minGraph-statH-8)/rows-6, 28, 52)
		gridH = rows*(cellH+6) + 8
	}
	gh := max(minGraph, bodyH-gridH-statH-12)
	gy, finish := p.beginDetail(cv, rect{x0, top, dw, bodyH}, gh+gridH+statH+12)
	defer finish()
	gr := rect{x0, gy, dw, gh}
	if gr.Y+gr.H > top {
		p.paintHistory(cv, gr, cur)
	}

	// per-core grid
	y := gr.Y + gr.H + 8
	if gridH > 0 {
		cw := (dw - (cols-1)*8) / cols
		for i, c := range s.Cores {
			cx := x0 + (i%cols)*(cw+8)
			cy := y + (i/cols)*(cellH+6)
			if cy+cellH <= top || cy >= h {
				continue
			}
			cr := rect{cx, cy, cw, cellH}
			fillRound(cv, cr, 8, T.Card)
			strokeRound(cv, cr, 8, T.Stroke)
			vals, ver := p.coreSeries(i)
			ser := []series{{vals: vals, ver: ver, color: heatColor(c), fill: true}}
			plotSeries(cv, rect{cr.X + 4, cr.Y + 4, cr.W - 8, cr.H - 8}, ser,
				graphStyle{bg: T.Card, max: 100, capacity: p.capacity(), lineW: 2})
			label := fmt.Sprintf("CPU %d", i)
			switch {
			case cw < 110: // many narrow tiles on a small window: the index only
				label = fmt.Sprint(i)
			case i < len(s.CPUFreq):
				label = fmt.Sprintf("%d  %.1f GHz", i, s.CPUFreq[i]/1000) // short: tiles are narrow
			}
			if cw < 70 { // too narrow for two labels: the load only
				drawText(cv, fmt.Sprintf("%.0f%%", c), uiBold(8), heatColor(c), rect{cr.X, cr.Y + 3, cr.W, 16}, taCenter)
			} else {
				drawText(cv, label, uiFont(8), T.Muted, rect{cr.X + 8, cr.Y + 3, cr.W - 42, 16}, taLeft)
				drawText(cv, fmt.Sprintf("%.0f%%", c), uiBold(8), heatColor(c), rect{cr.X + cr.W - 46, cr.Y + 3, 38, 16}, taRight)
			}
			coreName := fmt.Sprintf("CPU %d: %.0f%%", i, c)
			if i < len(s.CPUFreq) {
				coreName += fmt.Sprintf(", %.2f GHz", s.CPUFreq[i]/1000)
			}
			v.addText(cr, coreName)
		}
		y += gridH
	}

	// stats grid
	cw := dw / statCols
	for i, st := range stats {
		sx := x0 + (i%statCols)*cw
		sy := y + (i/statCols)*factStep
		if sy+46 <= top || sy >= h {
			continue
		}
		drawText(cv, st[0], uiFont(9), T.Muted, rect{sx, sy, cw - 12, 16}, taLeft)
		drawText(cv, st[1], uiFont(12), T.Text, rect{sx, sy + 20, cw - 12, 28}, taLeft)
		v.addText(rect{sx, sy, cw - 12, 48}, st[0]+": "+st[1])
	}
}

func (p *perfPage) scrollRail(by int) {
	last := max(0, len(p.items(p.app.snap))-p.railCount)
	if next := clampI(p.railFirst+by, 0, last); next != p.railFirst {
		p.railFirst = next
		p.v.invalidate()
	}
}

func (p *perfPage) revealResource() {
	if p.railCount > 0 {
		p.railFirst = clampI(p.railFirst, max(0, p.sel-p.railCount+1), p.sel)
	}
}

// A typical machine shows every card. With many adapters, the rail scrolls
// independently so the last one is reachable without shrinking its reading.
func (p *perfPage) paintRail(cv *walk.Canvas, r rect, items []resItem, s *metrics.Snapshot) {
	const gap, minHeight = 6, 40
	p.railArea = r
	count := len(items)
	cardRoom := r.H
	if (r.H+gap)/(minHeight+gap) < count {
		cardRoom -= 32
		count = max(1, (cardRoom+gap)/(minHeight+gap))
	}
	if count != p.railCount {
		p.railCount = count
		p.revealResource()
	}
	p.railFirst = clampI(p.railFirst, 0, len(items)-count)
	cardH := min(92, (cardRoom+gap)/count-gap)
	for row, it := range items[p.railFirst : p.railFirst+count] {
		p.paintCard(cv, rect{r.X, r.Y + row*(cardH+gap), r.W, cardH}, it, p.railFirst+row, s)
	}
	if count == len(items) {
		return
	}
	y := r.Y + r.H - 26
	for _, b := range []struct {
		x               int
		id, glyph, name string
		enabled         bool
	}{
		{r.X, "rail:up", "↑", "Previous resources", p.railFirst > 0},
		{r.X + r.W - 26, "rail:down", "↓", "More resources", p.railFirst+count < len(items)},
	} {
		button := rect{b.x, y, 26, 26}
		ink := T.Faint
		if b.enabled {
			ink = T.Text
			fillRound(cv, button, 6, T.CardHi)
			p.v.addItem(button, b.id, b.name, ctlButton, false)
		}
		drawText(cv, b.glyph, uiFont(10), ink, button, taCenter)
	}
	drawText(cv, fmt.Sprintf("%d–%d/%d", p.railFirst+1, p.railFirst+count, len(items)), uiFont(8), T.Muted, rect{r.X + 26, y, r.W - 52, 26}, taCenter)
}

// paintCard gives the reading its measured space before offering any room to a sparkline.
func (p *perfPage) paintCard(cv *walk.Canvas, r rect, it resItem, index int, s *metrics.Snapshot) {
	id := fmt.Sprintf("res:%d", index)
	fill, border := T.Card, T.Stroke
	if index == p.sel {
		fill, border = T.CardSel, blend(T.Accent, T.Card, 0.7)
	} else if p.v.hover == id {
		fill = T.CardHi
	}
	fillRound(cv, r, 8, fill)
	strokeRound(cv, r, 8, border)
	color := resColor(it.kind)
	value, sub, _ := p.headline(it, s)
	if it.kind == resCPU {
		sub = p.cpuCardSub(s)
	}
	title := resTitle(it.kind)
	if it.kind == resGPU && it.gpu >= 0 {
		title = fmt.Sprintf("GPU %d", it.gpu)
	}
	p.v.addItem(r, id, fmt.Sprintf("%s: %s, %s", title, value, sub), ctlListItem, index == p.sel)

	x, lineW := r.X+12, r.W-22
	headerH, valueH, valuePt := 19, 25, 15
	if r.H >= 76 {
		valueH, valuePt = 31, 19
	} else if r.H < 49 {
		headerH, valueH, valuePt = r.H/2, r.H-r.H/2, 11
	}
	font := uiLight(valuePt)
	valueW := textWidth(cv, value, font)
	if valueW > lineW {
		font = uiFont(10)
		valueW = textWidth(cv, value, font)
	}
	labelY := r.Y + max(0, min(7, (r.H-headerH-valueH)/2))
	valueY := labelY + headerH
	fillRect(cv, rect{r.X + 1, labelY + 3, 3, headerH - 6}, color)
	drawText(cv, title, uiBold(10), T.Text, rect{x, labelY, lineW, headerH}, taLeft)
	drawText(cv, value, font, color, rect{x, valueY, lineW, valueH}, taLeft)
	if r.Y+r.H-valueY-valueH >= 17 {
		drawText(cv, sub, uiFont(8), T.Muted, rect{x, valueY + valueH, lineW, 17}, taLeft)
	}
	if r.W >= 190 && r.H >= 42 {
		readingW := max(textWidth(cv, title, uiBold(10)), valueW) + 10
		if sparkW := min(90, lineW-readingW); sparkW >= 56 {
			spark := rect{x + lineW - sparkW, labelY + 3, sparkW, headerH + valueH - 7}
			plotSeries(cv, spark, p.seriesFor(it), graphStyle{bg: fill, max: p.fixedMax(it.kind), capacity: p.capacity(), lineW: 2})
		}
	}
}

func (p *perfPage) scrollDetail(delta int) {
	if next := clampI(p.detailScroll+delta, 0, p.detailMax); next != p.detailScroll {
		p.detailScroll = next
		p.hoverIdx = -1
		p.v.invalidate()
	}
}

// beginDetail keeps the heading and rail fixed. Clip both GDI output and hit
// rectangles so scrolled controls cannot cover the header or capture its mouse.
func (p *perfPage) beginDetail(cv *walk.Canvas, area rect, contentH int) (int, func()) {
	p.detailArea = area
	p.detailMax = max(0, contentH-area.H)
	p.detailScroll = min(p.detailScroll, p.detailMax)
	if p.detailMax == 0 {
		return area.Y, func() {}
	}
	thumb := max(18, area.H*area.H/contentH)
	thumbY := area.Y + (area.H-thumb)*p.detailScroll/p.detailMax
	fillRound(cv, rect{area.X + area.W + 3, thumbY, 3, thumb}, 2, T.Muted)
	view := area.px()
	saved := win.SaveDC(cv.HDC())
	if saved != 0 {
		win.IntersectClipRect(cv.HDC(), int32(view.X), int32(view.Y), int32(view.X+view.W), int32(view.Y+view.H))
	}
	itemStart, hitStart := len(p.v.items), len(p.v.hits)
	clip := func(r rect) rect {
		y := max(r.Y, view.Y)
		return rect{r.X, y, r.W, min(r.Y+r.H, view.Y+view.H) - y}
	}
	return area.Y - p.detailScroll, func() {
		if saved != 0 {
			win.RestoreDC(cv.HDC(), saved)
		}
		hits := p.v.hits[:hitStart]
		for _, hit := range p.v.hits[hitStart:] {
			hit.r = clip(hit.r)
			if hit.r.H > 0 {
				hits = append(hits, hit)
			}
		}
		p.v.hits = hits
		items := p.v.items[:itemStart]
		for _, item := range p.v.items[itemStart:] {
			item.r = clip(item.r)
			if item.r.H > 0 {
				items = append(items, item)
			}
		}
		p.v.items = items
	}
}

func (p *perfPage) paintHistory(cv *walk.Canvas, gr rect, cur resItem) {
	kind := cur.kind
	fillRound(cv, rect{gr.X - 1, gr.Y - 1, gr.W + 2, gr.H + 2}, 10, T.Card)
	inner := gr.inset(8)
	maxV := plotSeries(cv, inner, p.seriesFor(cur),
		graphStyle{bg: T.Card, max: p.fixedMax(kind), capacity: p.capacity(), grid: true, lineW: 2})
	strokeRound(cv, gr, 10, T.Stroke)
	p.hoverGraph, p.hoverGraphOn = inner, true
	axis := "100%"
	if kind == resDisk || kind == resNet {
		axis = fmtRate(maxV)
	}
	drawText(cv, axis, uiFont(8), T.Faint, rect{inner.X + 6, inner.Y + 2, 120, 14}, taLeft)
	drawText(cv, p.oldestSampleLabel(), uiFont(8), T.Faint,
		rect{inner.X + 6, inner.Y + inner.H - 16, 120, 14}, taLeft)
	drawText(cv, "now", uiFont(8), T.Faint, rect{inner.X + inner.W - 60, inner.Y + inner.H - 16, 54, 14}, taRight)

	// hover readout: a hairline at the sample under the mouse and a small
	// caption with its value and age, the way the classic Task Manager does.
	if i := p.hoverIdx; i >= 0 && i < len(p.hist) && len(p.hist) >= 2 {
		hp := &p.hist[i]
		x := inner.X + int(float64(inner.W)*float64(i)/float64(len(p.hist)-1))
		linePixels(cv.HDC(), sc(x), sc(inner.Y), sc(x), sc(inner.Y+inner.H), blend(T.Text, T.Card, 0.45))
		label := p.hoverLabel(cur, hp)
		if age := time.Now().UnixMilli() - hp.T; age > 750 {
			label += "  ·  " + humanDur(time.Duration(age)*time.Millisecond) + " ago"
		}
		lw := min(inner.W-8, textWidth(cv, label, uiFont(9))+20)
		ly := inner.Y + 6
		if kind == resCPU {
			ly = inner.Y + 36 // below the per-core chip, which owns the top-right corner
		}
		br := rect{clampI(x-lw/2, inner.X+4, inner.X+inner.W-lw-4), ly, lw, 20}
		fillRound(cv, br, 6, blend(T.Text, T.Card, 0.15))
		strokeRound(cv, br, 6, T.Stroke)
		drawText(cv, label, uiFont(9), T.Text, br, taCenter)
	}

	// per-core toggle chip (CPU only)
	if kind == resCPU {
		cr := rect{gr.X + gr.W - 150, gr.Y + 8, 140, 26}
		chip := T.CardHi
		if p.perCore {
			chip = blend(T.Accent, T.Card, 0.35)
		}
		fillRound(cv, cr, 13, chip)
		drawText(cv, "Per-core view", uiFont(9), T.Text, cr, taCenter)
		p.v.addItem(cr, "cores", "Per-core view", ctlCheckBox, p.perCore)
	}
}

func (p *perfPage) oldestSampleLabel() string {
	if n := len(p.hist); n > 1 {
		span := time.Duration(p.hist[n-1].T-p.hist[0].T) * time.Millisecond
		return humanDur(span.Round(time.Second)) + " ago"
	}
	return ""
}

func heatColor(pct float64) walk.Color {
	switch {
	case pct >= 85:
		return T.Bad
	case pct >= 60:
		return T.Warn
	}
	return T.CPU
}

// hoverLabel is the value caption shown for one history sample under the mouse.
func (p *perfPage) hoverLabel(cur resItem, h *metrics.HistPoint) string {
	switch cur.kind {
	case resCPU:
		return fmt.Sprintf("%.0f%% CPU", h.CPU)
	case resMem:
		return fmt.Sprintf("%.0f%% memory", h.Mem)
	case resGPU:
		if cur.gpu >= 0 {
			id := p.app.snap.GPUs[cur.gpu].ID
			for i := range h.GPUs {
				if g := &h.GPUs[i]; g.ID == id && finiteValue(gpuPointUtil(g)) {
					return fmt.Sprintf("%.0f%% GPU %d", g.Pct, cur.gpu)
				}
			}
			return fmt.Sprintf("GPU %d n/a", cur.gpu)
		}
		if !h.GPUOK {
			return "GPU n/a"
		}
		return fmt.Sprintf("%.0f%% GPU", h.GPU)
	case resDisk:
		return fmt.Sprintf("R %s · W %s", fmtRate(h.DiskR), fmtRate(h.DiskW))
	}
	return fmt.Sprintf("↓ %s · ↑ %s", fmtRate(h.NetRx), fmtRate(h.NetTx))
}

// coreSeries is one logical processor's curve. A point without that core's
// reading (the CPU query failed that tick) is a gap.
func (p *perfPage) coreSeries(i int) ([]float64, uint64) {
	return p.curve(seriesKey{kind: serCore, idx: i}, func(h *metrics.HistPoint) float64 {
		if i < len(h.Cores) {
			return h.Cores[i]
		}
		return math.NaN()
	})
}

// statsFor returns the label/value pairs shown under the graph.
func (p *perfPage) statsFor(it resItem, s *metrics.Snapshot) [][2]string {
	var out [][2]string
	add := func(k, v string) { out = append(out, [2]string{k, v}) }
	switch it.kind {
	case resCPU:
		topo := p.app.sensors.Topology()
		pw := p.app.sensors.Power()
		add("Utilization", fmtPct(s.CPU))
		if len(s.CPUFreq) > 0 {
			var sum float64
			for _, f := range s.CPUFreq {
				sum += f
			}
			add("Speed", fmt.Sprintf("%.2f GHz", sum/float64(len(s.CPUFreq))/1000))
		} else {
			add("Speed", "n/a")
		}
		if w, ok := metrics.EnergyPackage(s); ok {
			add("Package power", fmt.Sprintf("%.1f W", w))
		}
		if t, _, ok := p.app.sensors.CPUTemperature(); ok {
			add("CPU temperature", fmt.Sprintf("%.0f °C", t))
		}
		add("Processes", fmt.Sprint(s.Mem.Processes))
		add("Threads", fmt.Sprint(s.Mem.Threads))
		add("Handles", commaInt(int64(s.Mem.Handles)))
		add("Up time", fmtDur(s.Uptime))
		if topo.BaseMHz > 0 {
			add("Base speed", fmt.Sprintf("%.2f GHz", float64(topo.BaseMHz)/1000))
		}
		add("Sockets", fmt.Sprint(topo.Packages))
		add("Physical cores", fmt.Sprint(topo.Cores))
		add("Logical processors", fmt.Sprint(p.app.host.NumCPU))
		virt := "Disabled"
		if topo.Virt {
			virt = "Enabled"
		}
		add("Virtualization", virt)
		add("L1 cache", fmtBytes(topo.L1))
		add("L2 cache", fmtBytes(topo.L2))
		add("L3 cache", fmtBytes(topo.L3))
		if pw.Plan != "" {
			add("Power plan", pw.Plan)
		}
	case resMem:
		add("In use", fmtBytes(s.Mem.Used))
		add("Available", fmtBytes(s.Mem.Available))
		add("Committed", fmtBytes(s.Mem.CommitTotal)+" / "+fmtBytes(s.Mem.CommitLimit))
		add("Cached", fmtBytes(s.Mem.Cached))
		add("Paged pool", fmtBytes(s.Mem.KernelPaged))
		add("Non-paged pool", fmtBytes(s.Mem.KernelNonpaged))
		add("Installed", fmtBytes(s.Mem.Total))
		add("Processes", fmt.Sprint(s.Mem.Processes))
	case resGPU:
		if s.GPU.OK {
			add("Utilization", fmtPct(s.GPU.Percent))
		} else {
			add("Utilization", "n/a")
		}
		if s.GPU.OK {
			add("Dedicated memory in use", fmtBytes(s.GPU.MemBytes))
		} else {
			add("Dedicated memory in use", "n/a")
		}
	case resDisk:
		add("Active time", fmtPct(s.Disk.Percent))
		add("Read", fmtRate(s.Disk.ReadBps))
		add("Write", fmtRate(s.Disk.WriteBps))
		models := map[int]string{}
		for _, d := range p.app.sensors.Drives() {
			models[d.Index] = d.Model
		}
		for _, d := range s.PerDisk {
			var n int
			if _, err := fmt.Sscanf(d.Name, "%d", &n); err == nil {
				name := strings.TrimSpace(strings.TrimPrefix(d.Name, fmt.Sprint(n)))
				if m := models[n]; m != "" {
					name = m + " " + name
				}
				add(name, fmt.Sprintf("%.0f%% · R %s · W %s", d.Percent, fmtRate(d.ReadBps), fmtRate(d.WriteBps)))
			}
		}
	case resNet:
		add("Receive", fmtRate(s.Net.RxBps))
		add("Send", fmtRate(s.Net.TxBps))
		add("Total", fmtRate(s.Net.RxBps+s.Net.TxBps))
		add("IP address", p.app.localIP)
	}
	return out
}

// cpuCardSub is the compact CPU caption: average clock, package power, temperature.
func (p *perfPage) cpuCardSub(s *metrics.Snapshot) string {
	var parts []string
	if len(s.CPUFreq) > 0 {
		var sum float64
		for _, f := range s.CPUFreq {
			sum += f
		}
		parts = append(parts, fmt.Sprintf("%.2f GHz", sum/float64(len(s.CPUFreq))/1000))
	}
	if w, ok := metrics.EnergyPackage(s); ok {
		parts = append(parts, fmt.Sprintf("%.0f W", w))
	}
	if t, _, ok := p.app.sensors.CPUTemperature(); ok {
		parts = append(parts, fmt.Sprintf("%.0f °C", t))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d logical processors", p.app.host.NumCPU)
	}
	return strings.Join(parts, " · ")
}

// perCoreCols is how many per-core tiles go across the detail pane: 4–8 by
// width, and more (narrower) when the rows would not fit in room units.
func perCoreCols(dw, cores, room int) int {
	cols := 8
	switch {
	case cores <= 8 || dw < 620:
		cols = 4
	case dw < 820:
		cols = 6
	}
	// tiles may shrink to 26 units tall before more columns are added
	for cols < 12 && cols < cores && ((cores+cols-1)/cols)*(26+8)+8 > room {
		cols += 2
	}
	return min(cols, max(2, dw/88))
}
