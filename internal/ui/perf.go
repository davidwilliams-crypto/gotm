//go:build windows

package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

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
	hist    []metrics.HistPoint

	// GPU process list scrolling (design units for the area, rows for the offset)
	gpuScroll int
	gpuList   rect
	gpuRows   int
	gpuProcs  int
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
	p := &perfPage{app: a, perCore: true}
	if v := os.Getenv("GOTM_PERF_SEL"); v != "" { // debugging aid: open on a given card
		fmt.Sscanf(v, "%d", &p.sel)
	}
	p.v = newCanvasView(p.paint)
	p.v.keyboard, p.v.keyName = true, "Performance. Up and Down select a resource, Space or Enter changes its graph view."
	p.v.acc, p.v.accName, p.v.accCtl = true, "Performance", ctlPane
	p.v.onClick = func(id string, _ walk.MouseButton) {
		var n int
		switch {
		case id == "cores":
			p.perCore = !p.perCore
		case id == "gpuview:overall":
			p.gpuOver = true
		case id == "gpuview:engines":
			p.gpuOver = false
		default:
			if _, err := fmt.Sscanf(id, "res:%d", &n); err == nil {
				if p.sel != n {
					p.gpuScroll = 0
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
		if p.gpuRows == 0 || !p.gpuList.px().contains(p.v.mx, p.v.my) {
			return
		}
		p.gpuScroll -= delta / 40 // three rows per notch
		p.clampGPUScroll()
		p.v.invalidate()
	}
	p.v.attachWheel()
}

func (p *perfPage) clampGPUScroll() {
	if max := p.gpuProcs - p.gpuRows; p.gpuScroll > max {
		p.gpuScroll = max
	}
	if p.gpuScroll < 0 {
		p.gpuScroll = 0
	}
}
func (p *perfPage) shown()                   { p.v.invalidate() }
func (p *perfPage) focusTarget() walk.Window { return p.v.cw }
func (p *perfPage) reload()                  { p.hist = p.app.sampler.History(); p.v.invalidate() }
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
	case walk.KeySpace, walk.KeyReturn:
		it := p.items(p.app.snap)[p.sel]
		if it.kind == resCPU {
			p.perCore = !p.perCore
		} else if it.kind == resGPU {
			p.gpuOver = !p.gpuOver
		}
	default:
		return false
	}
	p.gpuScroll = 0
	p.v.keyHit = fmt.Sprintf("res:%d", p.sel)
	p.v.invalidate()
	return true
}
func (p *perfPage) tick(*metrics.Snapshot) {
	p.hist = p.app.sampler.History()
	p.v.invalidate()
}

func (p *perfPage) capacity() int {
	c := p.app.cfg.HistorySeconds * 1000 / p.app.cfg.RefreshMs
	if c < 10 {
		c = 10
	}
	return c
}

// seriesFor extracts the curves of one card from the history.
func (p *perfPage) seriesFor(it resItem) []series {
	r := it.kind
	col := resColor(r)
	pick := func(f func(h *metrics.HistPoint) float64) []float64 {
		out := make([]float64, len(p.hist))
		for i := range p.hist {
			out[i] = f(&p.hist[i])
		}
		return out
	}
	switch r {
	case resCPU:
		return []series{{vals: pick(func(h *metrics.HistPoint) float64 { return h.CPU }), color: col, fill: true}}
	case resMem:
		return []series{{vals: pick(func(h *metrics.HistPoint) float64 { return h.Mem }), color: col, fill: true}}
	case resGPU:
		if it.gpu >= 0 && p.app.snap != nil && it.gpu < len(p.app.snap.GPUs) {
			return []series{{vals: p.gpuSeries(p.app.snap.GPUs[it.gpu].ID, func(g *metrics.GPUHist) float64 { return g.Pct }), color: col, fill: true}}
		}
		return []series{{vals: pick(func(h *metrics.HistPoint) float64 { return h.GPU }), color: col, fill: true}}
	case resDisk:
		return []series{
			{vals: pick(func(h *metrics.HistPoint) float64 { return h.DiskR }), color: T.Disk, fill: true},
			{vals: pick(func(h *metrics.HistPoint) float64 { return h.DiskW }), color: T.Warn},
		}
	}
	return []series{
		{vals: pick(func(h *metrics.HistPoint) float64 { return h.NetRx }), color: T.Net, fill: true},
		{vals: pick(func(h *metrics.HistPoint) float64 { return h.NetTx }), color: T.CPU},
	}
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
	// Cards shrink to fit (several GPUs, short windows); below 88 units they
	// switch to a compact layout so the caption never runs into the value.
	cardH, gap := 96, 10
	if n := len(items); n > 0 && 12+n*(cardH+gap) > h {
		gap = 6
		cardH = max(44, (h-12)/n-gap)
	}
	for i, it := range items {
		r := rect{20, 12 + i*(cardH+gap), 300, cardH}
		id := fmt.Sprintf("res:%d", i)
		fill := T.Card
		switch {
		case i == p.sel:
			fill = T.CardSel
		case v.hover == id:
			fill = T.CardHi
		}
		fillRound(cv, r, 10, fill)
		strokeRound(cv, r, 10, T.Stroke)
		fillRect(cv, rect{r.X, r.Y + 14, 4, r.H - 28}, resColor(it.kind))
		value, sub, _ := p.headline(it, s)
		if it.kind == resCPU {
			sub = p.cpuCardSub(s) // the full line does not fit a card
		}
		title := resTitle(it.kind)
		if it.kind == resGPU && it.gpu >= 0 {
			title = fmt.Sprintf("GPU %d", it.gpu)
		}
		switch {
		case cardH >= 88:
			drawText(cv, title, uiBold(10), T.Text, rect{r.X + 18, r.Y + 8, 120, 22}, taLeft)
			drawText(cv, value, uiLight(20), resColor(it.kind), rect{r.X + 18, r.Y + 28, 140, 36}, taLeft)
			drawText(cv, sub, uiFont(8), T.Muted, rect{r.X + 18, r.Y + cardH - 30, 150, 20}, taLeft)
		case cardH >= 64:
			drawText(cv, title, uiBold(10), T.Text, rect{r.X + 18, r.Y + 6, 70, 22}, taLeft)
			drawText(cv, value, uiLight(14), resColor(it.kind), rect{r.X + 88, r.Y + 4, 74, 26}, taLeft)
			drawText(cv, sub, uiFont(8), T.Muted, rect{r.X + 18, r.Y + cardH - 26, 146, 20}, taLeft)
		default: // one line: title and value
			drawText(cv, title, uiBold(10), T.Text, rect{r.X + 18, r.Y, 70, cardH}, taLeft)
			drawText(cv, value, uiLight(14), resColor(it.kind), rect{r.X + 88, r.Y, 74, cardH}, taLeft)
		}
		spark := rect{r.X + 166, r.Y + min(14, cardH/5), 120, r.H - 2*min(14, cardH/5)}
		plotSeries(cv, spark, p.seriesFor(it), graphStyle{bg: fill, max: p.fixedMax(it.kind), capacity: p.capacity(), lineW: 2})
		v.addItem(r, id, fmt.Sprintf("%s: %s, %s", title, value, sub), ctlListItem, i == p.sel)
	}

	// ---- detail pane ----------------------------------------------------------
	x0 := 344
	dw := w - x0 - 24
	if dw < 300 {
		return
	}
	cur := items[p.sel]
	kind := cur.kind
	metrics.SetGPUSensorsWanted(kind == resGPU && p.app.cur == pgPerformance)
	infoColor := resColor(kind)
	title := resTitle(kind)
	switch kind {
	case resCPU:
		title = trimSpace(p.app.host.CPUName)
	case resMem:
		title = "Memory  " + fmtBytes(s.Mem.Total)
	case resGPU:
		title = "Graphics (all adapters)"
		if cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
			title = s.GPUs[cur.gpu].Name
		}
	case resDisk:
		title = "Disks"
	case resNet:
		title = "Network (all adapters)"
	}
	value, sub, pct := p.headline(cur, s)
	if kind == resGPU && cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
		sub = gpuSubtitle(&s.GPUs[cur.gpu], cur.gpu)
	}
	drawText(cv, title, uiLight(20), T.Text, rect{x0, 8, dw - 180, 40}, taLeft)
	drawText(cv, sub, uiFont(10), T.Muted, rect{x0 + 2, 46, dw - 180, 22}, taLeft)
	drawText(cv, value, uiLight(26), infoColor, rect{x0 + dw - 200, 6, 200, 44}, taRight)
	v.addText(rect{x0, 8, dw, 60}, fmt.Sprintf("%s: %s. %s", title, value, sub))

	// utilisation bar
	by := 76
	bar := rect{x0, by, dw, 8}
	fillRound(cv, bar, 4, T.Grid)
	if kind != resNet {
		fillRound(cv, rect{bar.X, bar.Y, int(float64(bar.W) * pct / 100), bar.H}, 4, infoColor)
	}
	if kind == resGPU && cur.gpu >= 0 && cur.gpu < len(s.GPUs) {
		p.paintGPUBody(cv, s, &s.GPUs[cur.gpu], x0, dw, 96, h)
		return
	}

	// main graph
	stats := p.statsFor(cur, s)
	gy := 96
	// Never let the fact grid push past the footer: show as many rows as fit
	// once a minimum graph (and the per-core grid) has been reserved.
	reserve := 150
	coreRoom := h - gy - 120 - 76 // what the tiles may use: a 120-unit graph and one stats row stay
	if kind == resCPU && p.perCore && len(s.Cores) > 0 {
		c := perCoreCols(dw, len(s.Cores), coreRoom) // the same column count the grid will use
		reserve = 150 + ((len(s.Cores)+c-1)/c)*(26+8) + 8
	}
	maxRows := (h - gy - reserve - 12 - 16) / 60
	if maxRows < 1 {
		maxRows = 1
	}
	if len(stats) > maxRows*4 {
		stats = stats[:maxRows*4]
	}
	statRows := (len(stats) + 3) / 4
	statH := statRows*60 + 16
	avail := h - gy - statH - 12
	gh := avail
	gridH := 0
	var cols, rows int
	if kind == resCPU && p.perCore && len(s.Cores) > 0 {
		cols = perCoreCols(dw, len(s.Cores), coreRoom)
		rows = (len(s.Cores) + cols - 1) / cols
		cellH := 56
		gridH = rows*(cellH+8) + 8
		gh = avail - gridH
		if gh < 120 {
			gh = 120
			cellH = (avail-gh-8)/rows - 8
			if cellH < 26 {
				cellH = 26
			}
			gridH = rows*(cellH+8) + 8
		}
	}
	if gh < 100 {
		gh = 100
	}
	gr := rect{x0, gy, dw, gh}
	fillRound(cv, rect{gr.X - 1, gr.Y - 1, gr.W + 2, gr.H + 2}, 10, T.Card)
	inner := gr.inset(8)
	maxV := plotSeries(cv, inner, p.seriesFor(cur),
		graphStyle{bg: T.Card, max: p.fixedMax(kind), capacity: p.capacity(), grid: true, lineW: 2})
	strokeRound(cv, gr, 10, T.Stroke)
	axis := "100%"
	if kind == resDisk || kind == resNet {
		axis = fmtRate(maxV)
	}
	drawText(cv, axis, uiFont(8), T.Faint, rect{inner.X + 6, inner.Y + 2, 120, 14}, taLeft)
	drawText(cv, fmt.Sprintf("%s ago", time.Duration(p.app.cfg.HistorySeconds)*time.Second), uiFont(8), T.Faint,
		rect{inner.X + 6, inner.Y + inner.H - 16, 120, 14}, taLeft)
	drawText(cv, "now", uiFont(8), T.Faint, rect{inner.X + inner.W - 60, inner.Y + inner.H - 16, 54, 14}, taRight)

	// per-core toggle chip (CPU only)
	if kind == resCPU {
		cr := rect{gr.X + gr.W - 150, gr.Y + 8, 140, 26}
		chip := T.CardHi
		if p.perCore {
			chip = blend(T.Accent, T.Card, 0.35)
		}
		fillRound(cv, cr, 13, chip)
		drawText(cv, "Per-core view", uiFont(9), T.Text, cr, taCenter)
		v.addItem(cr, "cores", "Per-core view", ctlCheckBox, p.perCore)
	}

	// per-core grid
	y := gr.Y + gr.H + 8
	if gridH > 0 {
		cw := (dw - (cols-1)*8) / cols
		cellH := (gridH-8)/rows - 8
		for i, c := range s.Cores {
			cx := x0 + (i%cols)*(cw+8)
			cy := y + (i/cols)*(cellH+8)
			cr := rect{cx, cy, cw, cellH}
			fillRound(cv, cr, 8, T.Card)
			strokeRound(cv, cr, 8, T.Stroke)
			ser := []series{{vals: p.coreSeries(i), color: heatColor(c), fill: true}}
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
	cw := dw / 4
	for i, st := range stats {
		sx := x0 + (i%4)*cw
		sy := y + 8 + (i/4)*60
		drawText(cv, st[0], uiFont(9), T.Muted, rect{sx, sy, cw - 12, 16}, taLeft)
		drawText(cv, st[1], uiFont(12), T.Text, rect{sx, sy + 20, cw - 12, 28}, taLeft)
		v.addText(rect{sx, sy, cw - 12, 48}, st[0]+": "+st[1])
	}
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

func (p *perfPage) coreSeries(i int) []float64 {
	out := make([]float64, 0, len(p.hist))
	for k := range p.hist {
		if i < len(p.hist[k].Cores) {
			out = append(out, p.hist[k].Cores[i])
		}
	}
	return out
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
		add("Utilization", fmtPct(s.GPU.Percent))
		add("Dedicated memory in use", fmtBytes(s.GPU.MemBytes))
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
	return cols
}
