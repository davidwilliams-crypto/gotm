//go:build windows

package ui

import (
	"fmt"
	"math"
	"strings"

	"github.com/tailscale/walk"

	"gotm/internal/metrics"
)

// shortGPUName trims vendor boilerplate so card captions fit.
func shortGPUName(n string) string {
	n = strings.ReplaceAll(n, "(TM)", "")
	n = strings.ReplaceAll(n, "(R)", "")
	for _, pre := range []string{"NVIDIA ", "AMD "} {
		n = strings.TrimPrefix(n, pre)
	}
	return strings.Join(strings.Fields(n), " ")
}

func gpuSubtitle(g *metrics.GPUAdapter) string {
	var parts []string
	if g.Vendor != "" {
		parts = append(parts, g.Vendor)
	}
	if g.DedicatedTotal > 0 {
		parts = append(parts, fmtBytes(g.DedicatedTotal)+" dedicated")
	}
	return strings.Join(parts, " · ")
}

func gpuPointUtil(g *metrics.GPUHist) float64 {
	if !g.HasUtilization && len(g.Eng) == 0 {
		return math.NaN()
	}
	return g.Pct
}

// gpuSeries pulls one adapter's curve out of the history; a point without
// that adapter is a gap.
func (p *perfPage) gpuSeries(id string, f func(*metrics.GPUHist) float64) []float64 {
	out := make([]float64, 0, len(p.hist))
	for i := range p.hist {
		out = append(out, gpuValue(&p.hist[i], id, f))
	}
	return out
}

func gpuValue(h *metrics.HistPoint, id string, f func(*metrics.GPUHist) float64) float64 {
	if g := gpuPoint(h, id); g != nil {
		return f(g)
	}
	return math.NaN()
}

// gpuCurve is gpuSeries as a derived curve of the mirrored history: only the
// points new since the last paint are looked up (perf_series.go). name tells
// apart curves of the same kind, such as engines.
func (p *perfPage) gpuCurve(kind int, id, name string, f func(*metrics.GPUHist) float64) ([]float64, uint64) {
	return p.curve(seriesKey{kind: kind, id: id, name: name}, func(h *metrics.HistPoint) float64 {
		return gpuValue(h, id, f)
	})
}

func gpuDedicated(x *metrics.GPUHist) float64 {
	if !x.HasDedicatedUsage {
		return math.NaN()
	}
	return x.Ded
}

func gpuShared(x *metrics.GPUHist) float64 {
	if !x.HasSharedUsage {
		return math.NaN()
	}
	return x.Shr
}

func gpuFacts(g *metrics.GPUAdapter) [][2]string {
	var out [][2]string
	add := func(k, v string) { out = append(out, [2]string{k, v}) }
	// Utilization, vendor and memory are already on screen (headline, subtitle,
	// memory graph captions), so the fact grid keeps to what is not.
	notExposed := "Unavailable"
	if g.Vendor == "Intel" {
		notExposed = "Unavailable on Intel"
	}
	switch {
	case g.HasClock && (g.HasMemClock || g.MemClockMHz > 0):
		add("Clock speed", fmt.Sprintf("%.0f MHz · mem %.0f", g.ClockMHz, g.MemClockMHz))
	case g.HasMemClock && !g.HasClock:
		add("Clock speed", fmt.Sprintf("Memory %.0f MHz", g.MemClockMHz))
	case g.HasClock:
		add("Clock speed", fmt.Sprintf("%.0f MHz", g.ClockMHz))
	default:
		add("Clock speed", notExposed)
	}
	powerLabel := "Power draw"
	if g.Vendor == "AMD" && g.HasPower {
		powerLabel = metrics.GPUPowerLabel(g)
	}
	switch {
	case g.HasPower && g.PowerLimitW > 0:
		add(powerLabel, fmt.Sprintf("%.1f W / %.0f W", g.PowerW, g.PowerLimitW))
	case g.HasPower && g.HasVoltage:
		add(powerLabel, fmt.Sprintf("%.1f W · %.3f V", g.PowerW, g.VoltageV))
	case g.HasPower:
		add(powerLabel, fmt.Sprintf("%.1f W", g.PowerW))
	default:
		add("Power draw", notExposed)
	}
	fan := ""
	switch {
	case g.HasFan && g.FanRPM > 0:
		fan = fmt.Sprintf(" · fan %.0f rpm", g.FanRPM)
	case g.HasFan:
		fan = fmt.Sprintf(" · fan %.0f%%", g.FanPct)
	}
	switch {
	case g.HasTemp && g.HasHotspot:
		add("Temperature", fmt.Sprintf("%.0f °C · hotspot %.0f%s", g.TempC, g.HotspotC, fan))
	case g.HasTemp:
		add("Temperature", fmt.Sprintf("%.0f °C%s", g.TempC, fan))
	default:
		add("Temperature", notExposed)
	}
	if g.Driver != "" {
		add("Driver version", g.Driver)
	}
	if g.DriverDate != "" {
		add("Driver date", g.DriverDate)
	}
	if g.Location != "" {
		add("Physical location", "PCI bus "+g.Location)
	}
	return out
}

func memPair(used, total uint64) string {
	if total == 0 {
		return fmtBytes(used)
	}
	return fmtBytes(used) + " / " + fmtBytes(total)
}

// paintGPUBody draws everything under the header for one adapter: engine
// graphs (or one overall graph), memory history, facts, and which processes
// hold the dedicated and shared memory.
func (p *perfPage) paintGPUBody(cv *walk.Canvas, cur resItem, x0, dw, top, h int) {
	g := &p.app.snap.GPUs[cur.gpu]
	v := p.v
	const gap = 8

	facts := gpuFacts(g)
	factCols := clampI(dw/170, 2, 4)
	factRows := (len(facts) + factCols - 1) / factCols
	factH := factRows*46 + 4
	memH := clampI((h-top)/5, 66, 92)
	cols := clampI(dw/136, 1, 5)
	engineRows := (len(g.Engines) + cols - 1) / cols
	available := g.UtilizationAvailable()
	minEng := 88
	if !available {
		minEng = 44
	} else if !p.gpuOver {
		minEng = max(minEng, 28+engineRows*(32+gap)-gap)
	}
	// Keep one process visible at minimum. Further processes scroll within
	// their list, while on a short window the whole detail can also scroll.
	bodyH := max(1, h-top)
	otherH := memH + factH + 3*gap + 8
	procRows := clampI((bodyH-otherH-minEng-36)/20, 1, max(1, min(len(g.Procs), 7)))
	procH := 36 + procRows*20
	engH := minEng
	if available {
		engH = max(minEng, bodyH-otherH-procH)
	}
	visibleTop := top
	top, finish := p.beginDetail(cv, rect{x0, top, dw, bodyH}, engH+otherH+procH)
	defer finish()

	// ---- engines / overall -----------------------------------------------------
	y := top
	area := rect{x0, y, dw, engH}
	if available {
		drawText(cv, "Utilization", uiBold(9), T.Muted, rect{x0, y, dw - 178, 24}, taLeft)
		if len(g.Engines) > 0 {
			ov := rect{x0 + dw - 170, y, 80, 24}
			en := rect{x0 + dw - 86, y, 86, 24}
			for _, c := range []struct {
				r        rect
				text, id string
				on       bool
			}{{ov, "Overall", "gpuview:overall", p.gpuOver}, {en, "Engines", "gpuview:engines", !p.gpuOver}} {
				fill := T.CardHi
				if c.on {
					fill = blend(T.Accent, T.Card, 0.45)
				}
				fillRound(cv, c.r, 12, fill)
				drawText(cv, c.text, uiFont(9), T.Text, c.r, taCenter)
				v.addItem(c.r, c.id, c.text+" graph", ctlRadioButton, c.on)
			}
		}
		y += 28
		area.Y, area.H = y, engH-28
	}
	switch {
	case area.Y+area.H <= visibleTop:
		// Scrolled out of view.
	case !available:
		fillRound(cv, area, 8, T.Card)
		strokeRound(cv, area, 8, T.Stroke)
		drawText(cv, "Utilization unavailable", uiFont(10), T.Muted, area.inset(10), taLeft)
		v.addText(area, "Utilization unavailable")
	case p.gpuOver || len(g.Engines) == 0:
		p.paintHistory(cv, area, cur)
	default:
		cellH := min(96, (area.H-(engineRows-1)*gap)/engineRows)
		cw := (dw - (cols-1)*gap) / cols
		for i, e := range g.Engines {
			cx := x0 + (i%cols)*(cw+gap)
			cy := y + (i/cols)*(cellH+gap)
			if cy+cellH <= visibleTop || cy >= h {
				continue
			}
			cr := rect{cx, cy, cw, cellH}
			fillRound(cv, cr, 8, T.Card)
			name := e.Name
			vals, ver := p.gpuCurve(serGPUEng, g.ID, name, func(h *metrics.GPUHist) float64 {
				v, ok := h.Eng[name]
				if !ok {
					return math.NaN()
				}
				return v
			})
			plotSeries(cv, rect{cr.X + 4, cr.Y + 4, cr.W - 8, cr.H - 8},
				[]series{{vals: vals, ver: ver, color: heatColorGPU(e.Pct), fill: true}},
				graphStyle{bg: T.Card, max: 100, capacity: p.capacity(), lineW: 2})
			strokeRound(cv, cr, 8, T.Stroke)
			drawText(cv, name, uiFont(8), T.Muted, rect{cr.X + 8, cr.Y + 3, cr.W - 50, 16}, taLeft)
			pt := "0%"
			if e.Pct >= 0.05 {
				pt = fmt.Sprintf("%.1f%%", e.Pct)
			}
			drawText(cv, pt, uiFont(8), T.Muted, rect{cr.X + cr.W - 46, cr.Y + 3, 40, 16}, taRight)
			v.addText(cr, name+" engine: "+pt)
		}
	}
	y = top + engH + gap

	// ---- memory history --------------------------------------------------------
	half := (dw - gap) / 2
	for i, m := range []struct {
		title      string
		used, tot  uint64
		kind       int
		pick       func(*metrics.GPUHist) float64
		colorIsMem bool
		available  bool
	}{
		{"Dedicated GPU memory", g.DedicatedUsed, g.DedicatedTotal, serGPUDed, gpuDedicated, false, g.HasDedicatedUsage},
		{"Shared GPU memory", g.SharedUsed, g.SharedTotal, serGPUShr, gpuShared, true, g.HasSharedUsage},
	} {
		cr := rect{x0 + i*(half+gap), y, half, memH}
		if cr.Y+cr.H <= visibleTop || cr.Y >= h {
			continue
		}
		fillRound(cv, cr, 8, T.Card)
		col := T.GPU
		if m.colorIsMem {
			col = T.Mem
		}
		caption := "n/a"
		if m.available {
			caption = memPair(m.used, m.tot)
		}
		labelW := textWidth(cv, m.title, uiFont(8)) + 4
		valueW := textWidth(cv, caption, uiFont(9)) + 4
		label := rect{cr.X + 8, cr.Y + 2, cr.W - 16, 16}
		valueRect := rect{cr.X + cr.W - valueW - 8, cr.Y + 2, valueW, 16}
		align := taRight
		plotY := cr.Y + 20
		if labelW+valueW+24 > cr.W {
			valueRect = rect{label.X, label.Y + 16, label.W, 16}
			align = taLeft
			plotY += 16
		}
		vals, ver := p.gpuCurve(m.kind, g.ID, "", m.pick)
		plotSeries(cv, rect{cr.X + 4, plotY, cr.W - 8, cr.Y + cr.H - plotY - 4},
			[]series{{vals: vals, ver: ver, color: col, fill: true}},
			graphStyle{bg: T.Card, max: float64(m.tot), capacity: p.capacity(), grid: true, lineW: 2})
		strokeRound(cv, cr, 8, T.Stroke)
		drawText(cv, m.title, uiFont(8), T.Muted, label, taLeft)
		drawText(cv, caption, uiFont(9), T.Text, valueRect, align)
		v.addText(cr, m.title+": "+caption)
	}
	y += memH + gap

	// ---- facts -----------------------------------------------------------------
	cw := dw / factCols
	for i, f := range facts {
		sx := x0 + (i%factCols)*cw
		sy := y + (i/factCols)*46
		drawText(cv, f[0], uiFont(9), T.Muted, rect{sx, sy, cw - 12, 16}, taLeft)
		drawText(cv, f[1], uiFont(11), T.Text, rect{sx, sy + 18, cw - 12, 24}, taLeft)
		v.addText(rect{sx, sy, cw - 12, 42}, f[0]+": "+f[1])
	}
	y += factH + gap

	// ---- who is using the memory -------------------------------------------------
	p.gpuRows, p.gpuProcs = procRows, len(g.Procs)
	p.clampGPUScroll()
	hdr := rect{x0, y, dw, 26}
	fillRound(cv, hdr, 6, T.Card)
	title := "Processes"
	if len(g.Procs) > procRows {
		end := p.gpuScroll + procRows
		if end > len(g.Procs) {
			end = len(g.Procs)
		}
		title += fmt.Sprintf("  %d–%d/%d", p.gpuScroll+1, end, len(g.Procs))
	}
	drawText(cv, title, uiBold(9), T.Text, rect{hdr.X + 8, hdr.Y + 4, dw*36/100 - 12, 18}, taLeft)
	v.addText(hdr, "Memory and engine use by process. "+title)
	colName, colPID, colDed, colShr, colPct := x0+8, x0+dw*36/100, x0+dw*49/100, x0+dw*69/100, x0+dw*87/100
	for _, c := range []struct {
		x, end int
		text   string
	}{{colPID, colDed, "PID"}, {colDed, colShr, "Dedicated"}, {colShr, colPct, "Shared"}, {colPct, x0 + dw, "Engine"}} {
		drawText(cv, c.text, uiFont(8), T.Muted, rect{c.x, hdr.Y + 6, c.end - c.x - 2, 16}, taLeft)
	}
	ry := y + 30
	if len(g.Procs) == 0 {
		drawText(cv, "No per-process GPU activity reported.", uiFont(9), T.Faint, rect{colName, ry + 2, dw - 16, 18}, taLeft)
		v.addText(rect{colName, ry + 2, dw - 16, 18}, "No per-process GPU activity reported.")
		return
	}
	p.gpuList = rect{x0, max(visibleTop, y), dw, h - max(visibleTop, y)}
	var maxMem uint64 = 1
	for _, pr := range g.Procs {
		if t := pr.Dedicated + pr.Shared; t > maxMem {
			maxMem = t
		}
	}
	end := p.gpuScroll + procRows
	if end > len(g.Procs) {
		end = len(g.Procs)
	}
	for _, pr := range g.Procs[p.gpuScroll:end] {
		if t := pr.Dedicated + pr.Shared; t > 0 {
			bw := int(float64(colPID-colName-12) * float64(t) / float64(maxMem))
			fillRound(cv, rect{colName, ry + 17, bw, 2}, 1, blend(T.GPU, T.Bg, 0.45))
		}
		drawText(cv, pr.Name, uiFont(9), T.Text, rect{colName, ry, colPID - colName - 12, 16}, taLeft)
		drawText(cv, fmt.Sprint(pr.PID), uiFont(9), T.Muted, rect{colPID, ry, colDed - colPID - 2, 16}, taLeft)
		drawText(cv, fmtBytes(pr.Dedicated), uiFont(9), T.Text, rect{colDed, ry, colShr - colDed - 2, 16}, taLeft)
		drawText(cv, fmtBytes(pr.Shared), uiFont(9), T.Text, rect{colShr, ry, colPct - colShr - 2, 16}, taLeft)
		engineText := "n/a"
		if g.UtilizationAvailable() {
			engineText = fmt.Sprintf("%.1f%%", pr.Pct)
		}
		drawText(cv, engineText, uiFont(9), heatColorGPU(pr.Pct), rect{colPct, ry, x0 + dw - colPct - 2, 16}, taLeft)
		v.addText(rect{colName, ry, dw - 16, 16}, fmt.Sprintf("%s, PID %d, dedicated %s, shared %s, engine %s",
			pr.Name, pr.PID, fmtBytes(pr.Dedicated), fmtBytes(pr.Shared), engineText))
		ry += 20
	}
}

func heatColorGPU(pct float64) walk.Color {
	switch {
	case pct >= 90:
		return T.Bad
	case pct >= 60:
		return T.Warn
	}
	return T.GPU
}
