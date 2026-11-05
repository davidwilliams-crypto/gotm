//go:build windows

package ui

import (
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

// recorderPage is the flight recorder: a timeline of recent history. Click or
// drag on it to pick a moment and see which processes were busiest then.
type recorderPage struct {
	exportJob         singleFileJob
	app               *App
	root              *walk.Composite
	view              *canvasView
	plot              rect // plot used by the most recent paint, for mouse scrubbing
	rangeSecs         int
	selT              int64 // selected time (unix ms); 0 = follow live
	live              bool
	backgroundStarted bool

	bRec, bMark, bExport, bDisk, bClear *button
	bRange                              [4]*button
	rangeChoices                        [4]int
}

func newRecorderPage(a *App) *recorderPage {
	p := &recorderPage{app: a, rangeSecs: 900, live: true,
		rangeChoices: [4]int{300, 900, 3600, 10800}}
	p.view = newCanvasView(p.paint)
	p.view.keyboard, p.view.keyName = true, "Flight recording. Left or Right selects a sample, Home the oldest, End returns to live, Space starts or pauses recording."
	p.view.acc, p.view.accName, p.view.accCtl = true, "Flight recording", ctlPane
	p.view.onClick = p.onClick
	p.view.onMove = func(x, y int) {
		if p.view.left {
			p.pick(x, y)
		}
	}

	p.bRec = newButton("Start recording", "", 150, func() {
		a.rec.SetEnabled(!a.rec.Enabled())
		p.sync()
	})
	p.bRec.tip = "Start or pause recording (Space)"
	p.bMark = newButton("Add marker", "", 132, func() {
		when := time.Now().UnixMilli()
		if !p.live && p.selT != 0 {
			when = p.selT
		}
		a.rec.AddMark(time.UnixMilli(when).Format("15:04:05"), when)
		p.view.invalidate()
	})
	p.bExport = newButton("Export CSV", "", 126, p.export)
	p.bDisk = newButton("Keep on disk", "", 138, func() {
		a.saveConfig(func(c *configT) { c.RecorderDisk = !c.RecorderDisk })
		p.sync()
	})
	p.bDisk.toggle = true
	p.bDisk.tip = "Keep recorded samples on disk"
	p.bClear = newButton("Clear", "", 90, func() {
		if !a.confirm("Clear recording", "Clear the recording and markers from memory?\n\nSaved recordings on disk are kept.") {
			return
		}
		a.rec.Clear()
		p.selT, p.live = 0, true
		p.view.invalidate()
	})
	p.bClear.danger = true
	labels := [4]string{"5 min", "15 min", "1 hour", "3 hours"}
	for i := range p.bRange {
		i := i
		p.bRange[i] = newButton(labels[i], "", 72, func() {
			p.rangeSecs = p.rangeChoices[i]
			p.sync()
		})
		p.bRange[i].toggle = true
	}
	return p
}

func (p *recorderPage) build() decl.Widget {
	record, export := p.bRec.widget(), p.bExport.widget()
	// Keep the state-changing labels visible even when the other native
	// buttons shrink to their icons. Their accessible names stay complete.
	record.MinSize.Width, export.MinSize.Width = 108, 90
	bar := []decl.Widget{record, p.bMark.widget(), export, p.bDisk.widget(), p.bClear.widget(), decl.HSpacer{}}
	ranges := []decl.Widget{decl.Label{Text: "Time range", TextColor: T.Muted}}
	for _, b := range p.bRange {
		ranges = append(ranges, b.widget())
	}
	ranges = append(ranges, decl.HSpacer{})
	commandRow := func(children []decl.Widget) decl.Composite {
		return decl.Composite{
			Background: decl.SolidColorBrush{Color: T.Bg},
			Layout:     decl.HBox{MarginsZero: true, Spacing: 6},
			MinSize:    decl.Size{Height: 34}, MaxSize: decl.Size{Height: 34},
			Children: children,
		}
	}
	return decl.Composite{
		AssignTo:      &p.root,
		StretchFactor: 1,
		Background:    decl.SolidColorBrush{Color: T.Bg},
		Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			p.app.heads[pgRecorder].declare(0, 76, 0, 76, 0),
			decl.Composite{
				StretchFactor: bodyStretch,
				Background:    decl.SolidColorBrush{Color: T.Bg},
				Layout:        decl.VBox{Margins: padBody, Spacing: 10},
				Children: []decl.Widget{
					commandRow(bar), commandRow(ranges),
					p.view.declare(0, 0, 0, 0, bodyStretch),
				},
			},
			p.app.foots[pgRecorder].declare(0, 32, 0, 32, 0),
		},
	}
}

func (p *recorderPage) init() {
	p.app.registerRoot(pgRecorder, p.root)
	p.sync()
}

// Persistence and retention belong to the application session, even if the
// recorder's native controls have not been created yet.
func (p *recorderPage) startBackground() {
	if p.backgroundStarted {
		return
	}
	p.backgroundStarted = true
	p.app.rec.SetPersist(p.app.cfg.RecorderDisk)
	load := p.app.cfg.RecorderDisk
	go func() {
		p.app.rec.PruneDisk(72 * time.Hour)
		if load {
			p.app.rec.LoadDisk(3 * time.Hour)
			p.app.mw.Synchronize(p.sync)
		}
	}()
	p.sync()
}

func (p *recorderPage) shown()                   { p.sync() }
func (p *recorderPage) tick(*metrics.Snapshot)   { p.sync() }
func (p *recorderPage) focusTarget() walk.Window { return p.view.cw }
func (p *recorderPage) reload()                  { p.sync() }
func (p *recorderPage) pageKey(key walk.Key, mods walk.Modifiers) bool {
	if mods != 0 {
		return false
	}
	samples := p.app.rec.Samples(0, 0)
	switch key {
	case walk.KeySpace:
		p.bRec.onClick()
		return true
	case walk.KeyEnd, walk.KeyReturn:
		p.live, p.selT = true, 0
	case walk.KeyLeft, walk.KeyRight, walk.KeyHome:
		if len(samples) == 0 {
			return true
		}
		i := len(samples) - 1
		if !p.live {
			displayed, _ := p.app.rec.Nearest(p.selT)
			for j, s := range samples {
				if s.T == displayed.T {
					i = j
					break
				}
			}
		}
		switch key {
		case walk.KeyHome:
			i = 0
		case walk.KeyLeft:
			i = max(0, i-1)
		case walk.KeyRight:
			i = min(len(samples)-1, i+1)
		}
		p.selT, p.live = samples[i].T, false
	default:
		return false
	}
	p.view.keyHit = "plot"
	p.view.invalidate()
	return true
}

func (p *recorderPage) sync() {
	on := p.app.rec.Enabled()
	p.bRec.set(true, on)
	if on {
		p.bRec.setText("Recording")
	} else {
		p.bRec.setText("Start recording")
	}
	persist := p.app.rec.Persist()
	p.bDisk.set(true, persist)
	if persist {
		p.bDisk.setText("Disk saving on")
	} else {
		p.bDisk.setText("Keep on disk")
	}
	hasSamples := p.app.rec.Len() > 0
	exporting := p.exportJob.active()
	p.bExport.set(hasSamples && !exporting, false)
	if exporting {
		p.bExport.setText("Exporting…")
	} else {
		p.bExport.setText("Export CSV")
	}
	p.bClear.set(hasSamples, false)
	p.bMark.set(hasSamples, false)
	for i, b := range p.bRange {
		b.set(true, p.rangeSecs == p.rangeChoices[i])
	}
	p.view.invalidate()
}

func (p *recorderPage) export() {
	if p.exportJob.active() {
		return
	}
	dlg := walk.FileDialog{
		Title:    "Export flight recording",
		FilePath: "gotm-recording.csv",
		Filter:   "CSV files (*.csv)|*.csv",
	}
	ok, err := dlg.ShowSave(p.app.mw)
	if err != nil {
		p.app.errBox("Export", err)
		return
	}
	if !ok {
		return
	}
	path := dlg.FilePath
	if filepath.Ext(path) == "" {
		path += ".csv"
	}
	p.exportAsync(path)
}

// window is the time span drawn. Until the recording covers the chosen range,
// it shows what has been recorded (at least a minute) rather than a nearly
// empty chart with a sliver of data at its right edge.
func (p *recorderPage) window() (from, to int64) {
	to = time.Now().UnixMilli()
	if s := p.app.snap; s != nil {
		to = s.T
	}
	if !p.app.rec.Enabled() {
		if last, ok := p.app.rec.Nearest(to); ok {
			to = last.T
		}
	}
	from = to - int64(p.rangeSecs)*1000
	if oldest, ok := p.app.rec.Oldest(); ok && oldest > from {
		from = oldest
		if to-from < 60_000 {
			from = to - 60_000
		}
	}
	return from, to
}

// humanDur prints 45 s, 15 min, 1 h 30 min.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

func (p *recorderPage) pick(x, y int) {
	x, y = int(float64(x)/gs), int(float64(y)/gs) // mouse is in pixels, layout in design units
	pr := p.plot
	if pr.W <= 0 || y < pr.Y-14 || y >= pr.Y+pr.H+14 {
		return
	}
	from, to := p.window()
	f := float64(x-pr.X) / float64(pr.W)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	p.selT = from + int64(f*float64(to-from))
	p.live = false
	p.view.invalidate()
}

func (p *recorderPage) onClick(id string, _ walk.MouseButton) {
	switch id {
	case "plot":
		p.pick(p.view.mx, p.view.my)
	case "live":
		p.live, p.selT = true, 0
		p.view.invalidate()
	}
}

func (p *recorderPage) paint(cv *walk.Canvas, w, h int) {
	rec := p.app.rec
	from, to := p.window()
	samples := rec.Samples(from, to)
	on, hasSamples := rec.Enabled(), rec.Len() > 0
	state, ink := "Recording off", T.Muted
	switch {
	case on && hasSamples:
		state, ink = "Recording on", T.Bad
	case on:
		state, ink = "Starting recording", T.Bad
	case hasSamples:
		state, ink = "Recording paused", T.Warn
	}
	badge := rect{12, 6, 148, 30}
	fillRound(cv, badge, 6, T.Card)
	strokeRound(cv, badge, 6, T.Stroke)
	fillRound(cv, rect{24, 17, 8, 8}, 4, ink)
	drawText(cv, state, uiBold(10), ink, rect{40, 6, 112, 30}, taLeft)
	p.view.addText(badge, state)

	legendX, legendY := w-182, 6
	plotY := 56
	if legendX < badge.X+badge.W+12 {
		legendX, legendY = 16, 34
		plotY += 28
	}
	span := humanDur(time.Duration(to-from)*time.Millisecond) + " shown"
	if available := legendX - 176; available >= 110 {
		if available >= 176 {
			span += " · every 2 s"
		}
		r := rect{172, 6, available, 30}
		drawText(cv, span, uiFont(9), T.Muted, r, taLeft)
		p.view.addText(r, span)
	}
	for _, l := range []struct {
		name  string
		color walk.Color
		width int
	}{{"CPU", T.CPU, 50}, {"Memory", T.Mem, 74}, {"GPU", T.GPU, 48}} {
		fillRound(cv, rect{legendX, legendY + 13, 12, 4}, 2, l.color)
		drawText(cv, l.name, uiFont(9), T.Muted, rect{legendX + 17, legendY, l.width - 17, 30}, taLeft)
		legendX += l.width
	}
	if err := rec.DiskErr(); rec.Persist() && err != nil {
		r := rect{12, plotY - 14, w - 24, 26}
		fillRound(cv, r, 5, blend(T.Warn, T.Bg, 0.14))
		warning := "Disk warning · Some samples are only in memory. " + err.Error()
		drawText(cv, warning, uiFont(9), T.Warn, rect{r.X + 10, r.Y, r.W - 20, r.H}, taLeft)
		p.view.addText(r, warning)
		plotY += 34
	}

	// On a short window reserve room for the selected sample and its return
	// button. The normal height leaves room for the two full detail panels.
	plotH := min(max(120, min(h/2-40, h-318)), max(30, h-plotY-148))
	pr := rect{24, plotY, w - 48, plotH}
	card := rect{pr.X - 12, pr.Y - 12, pr.W + 24, pr.H + 50}
	p.plot = rect{}
	if !hasSamples {
		card.H = max(54, min(200, h-card.Y-12))
		fillRound(cv, card, 10, T.Card)
		strokeRound(cv, card, 10, T.Stroke)
		message := "Start recording to collect samples."
		if on {
			message = "Waiting for the first sample…"
		}
		drawText(cv, message, uiFont(11), T.Text, card.inset(8), taCenter)
		p.view.addText(card.inset(8), message)
		return
	}
	p.plot = pr
	fillRound(cv, card, 10, T.Card)
	strokeRound(cv, card, 10, T.Stroke)

	times := make([]int64, len(samples))
	for i, s := range samples {
		times[i] = s.T
	}
	mk := func(get func(s metrics.RecSample) float64) []float64 {
		out := make([]float64, len(samples))
		for i, s := range samples {
			out[i] = get(s)
		}
		return out
	}
	ser := []series{
		{vals: mk(func(s metrics.RecSample) float64 { return s.Mem }), times: times, color: T.Mem},
		{vals: mk(func(s metrics.RecSample) float64 {
			if s.GPUUnavailable {
				return math.NaN()
			}
			return s.GPU
		}), times: times, color: T.GPU},
		{vals: mk(func(s metrics.RecSample) float64 { return s.CPU }), times: times, color: T.CPU, fill: true},
	}
	plotSeries(cv, pr, ser, graphStyle{bg: T.Card, max: 100, timeFrom: from, timeTo: to, grid: true, lineW: 2})
	drawText(cv, "100%", uiFont(8), T.Faint, rect{pr.X + 6, pr.Y + 2, 50, 14}, taLeft)
	drawText(cv, "0", uiFont(8), T.Faint, rect{pr.X + 6, pr.Y + pr.H - 16, 30, 14}, taLeft)

	tf := "15:04"
	if to-from < 10*60_000 { // a short span needs seconds, or every label reads the same
		tf = "15:04:05"
	}
	divisions := 4
	if pr.W < 380 {
		divisions = 2
	}
	for i := 0; i <= divisions; i++ {
		t := time.UnixMilli(from + int64(i)*(to-from)/int64(divisions))
		x := pr.X + i*pr.W/divisions - 32
		align := taCenter
		if i == 0 {
			x, align = pr.X, taLeft
		} else if i == divisions {
			x, align = pr.X+pr.W-64, taRight
		}
		drawText(cv, t.Format(tf), uiFont(8), T.Muted, rect{x, pr.Y + pr.H + 6, 64, 22}, align)
	}

	// markers
	for _, m := range rec.Marks() {
		if m.T < from || m.T > to {
			continue
		}
		x := pr.X + int(timePosition(m.T, from, to)*float64(pr.W))
		fillRect(cv, rect{x, pr.Y, 1, pr.H}, T.Warn)
		labelX := min(x+4, pr.X+pr.W-70)
		drawText(cv, m.Label, uiFont(8), T.Warn, rect{labelX, pr.Y + 2, 70, 14}, taLeft)
	}

	// selection cursor
	selT := p.selT
	if p.live || selT == 0 {
		selT = to
	}
	if selT >= from && selT <= to {
		x := pr.X + int(timePosition(selT, from, to)*float64(pr.W))
		fillRect(cv, rect{x - 1, pr.Y, 2, pr.H}, T.Text)
		fillRound(cv, rect{x - 5, pr.Y - 8, 10, 10}, 5, T.Text)
	}
	plotName := "Timeline, live"
	if p.live && !on {
		plotName = "Timeline, latest"
	} else if !p.live {
		plotName = "Timeline, at " + time.UnixMilli(selT).Format("15:04:05")
	}
	s, ok := rec.Nearest(selT)
	if ok {
		plotName += fmt.Sprintf(", CPU %.0f%%, memory %.0f%%, GPU %s", s.CPU, s.Mem, recordedGPUText(s))
	}
	p.view.addItem(rect{pr.X, pr.Y - 14, pr.W, pr.H + 28}, "plot", plotName, ctlSlider, false)

	// detail panel for the selected moment
	if !ok {
		return
	}
	dy := card.Y + card.H + 12
	dh := h - dy - 8
	if dh < 164 || w < 610 {
		p.paintCompactDetail(cv, s, rect{12, dy, w - 24, dh})
		return
	}
	left := rect{12, dy, min(330, (w-36)*45/100), dh}
	rightX := left.X + left.W + 12
	right := rect{rightX, dy, w - rightX - 12, dh}
	fillRound(cv, left, 10, T.Card)
	strokeRound(cv, left, 10, T.Stroke)
	p.paintMoment(cv, s, rect{left.X + 16, left.Y + 8, left.W - 32, 28})
	y := left.Y + 44
	step := 28
	if left.H < 190 {
		step = max(20, (left.H-70)/4)
	}
	stat := func(label string, v float64, c walk.Color, unit string) {
		drawText(cv, label, uiFont(10), T.Muted, rect{left.X + 16, y, 120, 22}, taLeft)
		bar := rect{left.X + 110, y + 7, left.W - 110 - 80, 8}
		fillRound(cv, bar, 4, T.Grid)
		fillRound(cv, rect{bar.X, bar.Y, int(float64(bar.W) * v / 100), bar.H}, 4, c)
		drawText(cv, fmt.Sprintf("%.0f%s", v, unit), uiBold(10), T.Text, rect{left.X + left.W - 70, y, 54, 22}, taRight)
		p.view.addText(rect{left.X + 16, y, left.W - 32, 22}, fmt.Sprintf("%s %.0f%s", label, v, unit))
		y += step
	}
	stat("CPU", s.CPU, T.CPU, "%")
	stat("Memory", s.Mem, T.Mem, "%")
	if s.GPUUnavailable {
		drawText(cv, "GPU", uiFont(10), T.Muted, rect{left.X + 16, y, 120, 22}, taLeft)
		drawText(cv, "n/a", uiBold(10), T.Text, rect{left.X + left.W - 70, y, 54, 22}, taRight)
		p.view.addText(rect{left.X + 16, y, left.W - 32, 22}, "GPU unavailable")
		y += step
	} else {
		stat("GPU", s.GPU, T.GPU, "%")
	}
	stat("Disk busy", s.DiskP, T.Disk, "%")
	drawText(cv, "Disk "+fmtRate(s.DiskBps)+"   Network "+fmtRate(s.NetBps), uiFont(9), T.Muted,
		rect{left.X + 16, y + 2, left.W - 32, 20}, taLeft)
	p.view.addText(rect{left.X + 16, y + 2, left.W - 32, 20}, "Disk "+fmtRate(s.DiskBps)+", network "+fmtRate(s.NetBps))

	p.paintProcesses(cv, s.Top, right)
}

func (p *recorderPage) paintProcesses(cv *walk.Canvas, procs []metrics.RecProc, right rect) {
	fillRound(cv, right, 10, T.Card)
	strokeRound(cv, right, 10, T.Stroke)
	title := rect{right.X + 16, right.Y + 8, right.W - 32, 24}
	drawText(cv, "Busiest processes", uiBold(11), T.Text, title, taLeft)
	p.view.addText(title, "Busiest processes at this sample")
	ty := right.Y + 44
	if len(procs) == 0 {
		r := rect{right.X + 16, ty, right.W - 32, 22}
		drawText(cv, "No process data for this sample", uiFont(10), T.Muted, r, taLeft)
		p.view.addText(r, "No process data for this sample")
		return
	}
	memX := right.X + right.W - 96
	cpuX, pidX := memX-60, memX-130
	showPID := right.W >= 440
	nameEnd := pidX - 6
	if !showPID {
		nameEnd = cpuX - 6
	}
	nameW := nameEnd - (right.X + 16)
	drawText(cv, "Process", uiFont(9), T.Faint, rect{right.X + 16, ty, nameW, 20}, taLeft)
	if showPID {
		drawText(cv, "PID", uiFont(9), T.Faint, rect{pidX, ty, 60, 20}, taRight)
	}
	drawText(cv, "CPU", uiFont(9), T.Faint, rect{cpuX, ty, 48, 20}, taRight)
	drawText(cv, "Memory", uiFont(9), T.Faint, rect{memX, ty, 80, 20}, taRight)
	ty += 22
	maxRows := (right.H - 70) / 24
	for i, pr := range procs {
		if i >= maxRows {
			break
		}
		c := T.Text
		if pr.CPU >= 25 {
			c = T.Warn
		}
		drawText(cv, pr.Name, uiFont(10), c, rect{right.X + 16, ty, nameW, 22}, taLeft)
		if showPID {
			drawText(cv, fmt.Sprint(pr.PID), uiFont(10), T.Muted, rect{pidX, ty, 60, 22}, taRight)
		}
		drawText(cv, fmtPct(pr.CPU), uiFont(10), c, rect{cpuX, ty, 48, 22}, taRight)
		drawText(cv, fmtBytes(pr.Mem), uiFont(10), T.Muted, rect{memX, ty, 80, 22}, taRight)
		p.view.addText(rect{right.X + 16, ty, right.W - 32, 22}, fmt.Sprintf("%s, PID %d, CPU %s, memory %s", pr.Name, pr.PID, fmtPct(pr.CPU), fmtBytes(pr.Mem)))
		ty += 24
	}
}

func (p *recorderPage) paintMoment(cv *walk.Canvas, s metrics.RecSample, row rect) {
	when := time.UnixMilli(s.T).Format("Mon 15:04:05")
	title := "Selected · " + when
	if p.live {
		title = "Latest · " + when
		if p.app.rec.Enabled() {
			title = "Live · " + when
		}
	} else {
		label := "Go live"
		if !p.app.rec.Enabled() {
			label = "Go to latest"
		}
		r := rect{row.X + row.W - 90, row.Y, 90, row.H}
		bg := T.Card
		if p.view.hover == "live" || p.view.down == "live" {
			bg = T.CardHi
		}
		fillRound(cv, r, 5, bg)
		strokeRound(cv, r, 5, T.Accent)
		drawText(cv, label, uiFont(9), T.Text, r, taCenter)
		p.view.addItem(r, "live", label, ctlButton, false)
		row.W -= r.W + 8
	}
	drawText(cv, title, uiBold(11), T.Text, row, taLeft)
	p.view.addText(row, title)
}

func (p *recorderPage) paintCompactDetail(cv *walk.Canvas, s metrics.RecSample, panel rect) {
	if panel.H < 28 {
		return
	}
	row := rect{panel.X + 8, panel.Y, panel.W - 16, 28}
	p.paintMoment(cv, s, row)
	row.Y += 32
	row.H = 20
	if row.Y+row.H > panel.Y+panel.H {
		return
	}
	for i, v := range []string{
		"CPU " + fmtPct(s.CPU), "Memory " + fmtPct(s.Mem), "GPU " + recordedGPUText(s), "Disk " + fmtPct(s.DiskP),
	} {
		cell := rect{row.X + i*row.W/4, row.Y, row.W / 4, row.H}
		drawText(cv, v, uiFont(10), T.Text, cell, taLeft)
		p.view.addText(cell, v)
	}
	row.Y += 24
	remaining := panel.Y + panel.H - row.Y
	if remaining >= 2*row.H+8 || len(s.Top) == 0 && remaining >= row.H {
		rates := "Disk " + fmtRate(s.DiskBps) + "   Network " + fmtRate(s.NetBps)
		drawText(cv, rates, uiFont(9), T.Muted, row, taLeft)
		p.view.addText(row, rates)
		row.Y += 28
		remaining -= 28
	}
	if remaining >= 94 {
		p.paintProcesses(cv, s.Top, rect{panel.X, row.Y, panel.W, remaining})
	} else if len(s.Top) > 0 && remaining >= row.H {
		top := fmt.Sprintf("Busiest · %s  %s CPU", s.Top[0].Name, fmtPct(s.Top[0].CPU))
		drawText(cv, top, uiFont(9), T.Muted, row, taLeft)
		p.view.addText(row, top)
	}
}

func recordedGPUText(s metrics.RecSample) string {
	if s.GPUUnavailable {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", s.GPU)
}
