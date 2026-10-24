//go:build windows

package ui

import (
	"fmt"
	"strings"
	"time"

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

func gpuSubtitle(g *metrics.GPUAdapter, idx int) string {
	parts := []string{fmt.Sprintf("GPU %d", idx)}
	if g.Vendor != "" {
		parts = append(parts, g.Vendor)
	}
	if g.DedicatedTotal > 0 {
		parts = append(parts, fmtBytes(g.DedicatedTotal)+" dedicated")
	}
	return strings.Join(parts, " · ")
}

// gpuSeries pulls one adapter's curve out of the history.
func (p *perfPage) gpuSeries(id string, f func(*metrics.GPUHist) float64) []float64 {
	out := make([]float64, 0, len(p.hist))
	for i := range p.hist {
		var v float64
		for k := range p.hist[i].GPUs {
			if p.hist[i].GPUs[k].ID == id {
				v = f(&p.hist[i].GPUs[k])
				break
			}
		}
		out = append(out, v)
	}
	return out
}

func gpuFacts(g *metrics.GPUAdapter) [][2]string {
	var out [][2]string
	add := func(k, v string) { out = append(out, [2]string{k, v}) }
	// Utilization, vendor and memory are already on screen (headline, subtitle,
	// memory graph captions), so the fact grid keeps to what is not.
	notExposed := "Not exposed"
	if g.Vendor == "Intel" {
		notExposed = "Not exposed by Intel"
	}
	switch {
	case g.HasClock && g.MemClockMHz > 0:
		add("Clock speed", fmt.Sprintf("%.0f MHz · mem %.0f", g.ClockMHz, g.MemClockMHz))
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
func (p *perfPage) paintGPUBody(cv *walk.Canvas, s *metrics.Snapshot, g *metrics.GPUAdapter, x0, dw, top, h int) {
	v := p.v
	const gap = 8

	facts := gpuFacts(g)
	factRows := (len(facts) + 3) / 4
	if factRows > 3 {
		factRows = 3
	}
	procRows := len(g.Procs)
	if procRows > 7 {
		procRows = 7
	}
	if procRows == 0 {
		procRows = 1
	}
	memH := 92
	const minEng = 120

	layout := func() (engH, factH, procH int) {
		factH = factRows*50 + 4
		procH = 30 + procRows*20 + 6
		engH = h - top - 8 - memH - factH - procH - 3*gap
		return
	}
	engH, factH, _ := layout()
	for engH < minEng && procRows > 3 {
		procRows--
		engH, factH, _ = layout()
	}
	for engH < minEng && factRows > 2 {
		factRows--
		engH, factH, _ = layout()
	}
	for engH < minEng && procRows > 1 {
		procRows--
		engH, factH, _ = layout()
	}
	if engH < 70 {
		engH = 70
	}

	// ---- engines / overall -----------------------------------------------------
	y := top
	chipY := y - 2
	ov := rect{x0 + dw - 170, chipY, 80, 24}
	en := rect{x0 + dw - 86, chipY, 86, 24}
	for _, c := range []struct {
		r    rect
		text string
		id   string
		on   bool
	}{{ov, "Overall", "gpuview:overall", p.gpuOver}, {en, "Engines", "gpuview:engines", !p.gpuOver}} {
		fill := T.CardHi
		if c.on {
			fill = blend(T.Accent, T.Card, 0.45)
		}
		fillRound(cv, c.r, 12, fill)
		drawText(cv, c.text, uiFont(9), T.Text, c.r, taCenter)
		v.addItem(c.r, c.id, c.text+" graph", ctlRadioButton, c.on)
	}
	drawText(cv, "% utilization by engine", uiFont(9), T.Muted, rect{x0, y - 2, 300, 20}, taLeft)
	y += 24

	area := rect{x0, y, dw, engH - 24}
	if area.H < 60 {
		area.H = 60
	}
	switch {
	case p.gpuOver || len(g.Engines) == 0:
		fillRound(cv, rect{area.X - 1, area.Y - 1, area.W + 2, area.H + 2}, 10, T.Card)
		plotSeries(cv, area.inset(8), []series{{vals: p.gpuSeries(g.ID, func(h *metrics.GPUHist) float64 { return h.Pct }), color: T.GPU, fill: true}},
			graphStyle{bg: T.Card, max: 100, capacity: p.capacity(), grid: true, lineW: 2})
		strokeRound(cv, area, 10, T.Stroke)
		drawText(cv, "100%", uiFont(8), T.Faint, rect{area.X + 14, area.Y + 10, 80, 14}, taLeft)
		drawText(cv, fmt.Sprintf("%s ago", time.Duration(p.app.cfg.HistorySeconds)*time.Second), uiFont(8), T.Faint,
			rect{area.X + 14, area.Y + area.H - 24, 120, 14}, taLeft)
	default:
		n := len(g.Engines)
		cols := 4
		if dw < 640 {
			cols = 3
		}
		if n > 16 && dw >= 900 {
			cols = 5
		}
		rows := (n + cols - 1) / cols
		cellH := (area.H - (rows-1)*gap) / rows
		for cellH < 30 && cols < 8 {
			cols++
			rows = (n + cols - 1) / cols
			cellH = (area.H - (rows-1)*gap) / rows
		}
		if cellH > 74 {
			cellH = 74
		}
		cw := (dw - (cols-1)*gap) / cols
		maxShown := rows * cols
		for i, e := range g.Engines {
			if i >= maxShown {
				break
			}
			cx := x0 + (i%cols)*(cw+gap)
			cy := y + (i/cols)*(cellH+gap)
			cr := rect{cx, cy, cw, cellH}
			fillRound(cv, cr, 8, T.Card)
			name := e.Name
			vals := p.gpuSeries(g.ID, func(h *metrics.GPUHist) float64 { return h.Eng[name] })
			plotSeries(cv, rect{cr.X + 4, cr.Y + 4, cr.W - 8, cr.H - 8},
				[]series{{vals: vals, color: heatColorGPU(e.Pct), fill: true}},
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
	y = top + 24 + engH - 24 + gap

	// ---- memory history --------------------------------------------------------
	half := (dw - gap) / 2
	for i, m := range []struct {
		title      string
		used, tot  uint64
		pick       func(*metrics.GPUHist) float64
		colorIsMem bool
	}{
		{"Dedicated GPU memory", g.DedicatedUsed, g.DedicatedTotal, func(x *metrics.GPUHist) float64 { return x.Ded }, false},
		{"Shared GPU memory", g.SharedUsed, g.SharedTotal, func(x *metrics.GPUHist) float64 { return x.Shr }, true},
	} {
		cr := rect{x0 + i*(half+gap), y, half, memH}
		fillRound(cv, cr, 8, T.Card)
		col := T.GPU
		if m.colorIsMem {
			col = T.Mem
		}
		maxV := float64(m.tot)
		plotSeries(cv, rect{cr.X + 4, cr.Y + 20, cr.W - 8, cr.H - 24},
			[]series{{vals: p.gpuSeries(g.ID, m.pick), color: col, fill: true}},
			graphStyle{bg: T.Card, max: maxV, capacity: p.capacity(), grid: true, lineW: 2})
		strokeRound(cv, cr, 8, T.Stroke)
		drawText(cv, m.title, uiFont(8), T.Muted, rect{cr.X + 8, cr.Y + 3, cr.W - 150, 16}, taLeft)
		drawText(cv, memPair(m.used, m.tot), uiFont(8), T.Text, rect{cr.X + cr.W - 168, cr.Y + 3, 160, 16}, taRight)
		v.addText(cr, m.title+": "+memPair(m.used, m.tot))
	}
	y += memH + gap

	// ---- facts -----------------------------------------------------------------
	cw := dw / 4
	if len(facts) > factRows*4 {
		facts = facts[:factRows*4]
	}
	for i, f := range facts {
		sx := x0 + (i%4)*cw
		sy := y + (i/4)*50
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
	title := "Memory and engine use by process"
	if len(g.Procs) > procRows {
		end := p.gpuScroll + procRows
		if end > len(g.Procs) {
			end = len(g.Procs)
		}
		title += fmt.Sprintf("  ·  %d–%d of %d, scroll for more", p.gpuScroll+1, end, len(g.Procs))
	}
	drawText(cv, title, uiBold(9), T.Text, rect{hdr.X + 8, hdr.Y + 4, dw*40/100 - 16, 18}, taLeft)
	v.addText(hdr, title)
	colName, colPID, colDed, colShr, colPct := x0+8, x0+dw*40/100, x0+dw*56/100, x0+dw*72/100, x0+dw*88/100
	for _, c := range []struct {
		x    int
		text string
	}{{colPID, "PID"}, {colDed, "Dedicated (resident)"}, {colShr, "Shared"}, {colPct, "Engine"}} {
		drawText(cv, c.text, uiFont(8), T.Muted, rect{c.x, hdr.Y + 6, dw / 8, 16}, taLeft)
	}
	ry := y + 30
	if len(g.Procs) == 0 {
		drawText(cv, "No process is using this adapter right now.", uiFont(9), T.Faint, rect{colName, ry + 2, dw - 16, 18}, taLeft)
		v.addText(rect{colName, ry + 2, dw - 16, 18}, "No process is using this adapter right now.")
		return
	}
	p.gpuList = rect{x0, y, dw, h - y}
	p.gpuRows, p.gpuProcs = procRows, len(g.Procs)
	p.clampGPUScroll()
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
		drawText(cv, fmt.Sprint(pr.PID), uiFont(9), T.Muted, rect{colPID, ry, 80, 16}, taLeft)
		drawText(cv, fmtBytes(pr.Dedicated), uiFont(9), T.Text, rect{colDed, ry, dw/8 + 20, 16}, taLeft)
		drawText(cv, fmtBytes(pr.Shared), uiFont(9), T.Text, rect{colShr, ry, dw/8 + 20, 16}, taLeft)
		drawText(cv, fmt.Sprintf("%.1f%%", pr.Pct), uiFont(9), heatColorGPU(pr.Pct), rect{colPct, ry, 80, 16}, taLeft)
		v.addText(rect{colName, ry, dw - 16, 16}, fmt.Sprintf("%s, PID %d, dedicated %s, shared %s, engine %.1f%%",
			pr.Name, pr.PID, fmtBytes(pr.Dedicated), fmtBytes(pr.Shared), pr.Pct))
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
