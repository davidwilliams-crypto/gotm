//go:build windows

package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

// recorderPage is the flight recorder: a timeline of recent history. Click or
// drag on it to pick a moment and see which processes were busiest then.
type recorderPage struct {
	app       *App
	root      *walk.Composite
	view      *canvasView
	rangeSecs int
	selT      int64 // selected time (unix ms); 0 = follow live
	live      bool

	bRec, bMark, bExport, bDisk, bClear *button
	bRange                              [4]*button
	rangeChoices                        [4]int
}

func newRecorderPage(a *App) *recorderPage {
	p := &recorderPage{app: a, rangeSecs: 900, live: true,
		rangeChoices: [4]int{300, 900, 3600, 10800}}
	p.view = newCanvasView(p.paint)
	p.view.keyboard, p.view.keyName = true, "Flight recording. Left or Right selects a sample, Home the oldest, End returns to live, Space pauses or resumes recording."
	p.view.acc, p.view.accName, p.view.accCtl = true, "Flight recording", ctlPane
	p.view.onClick = p.onClick
	p.view.onMove = func(x, y int) {
		if p.view.left {
			p.pick(x, y)
		}
	}

	p.bRec = newButton("Recording", "", 134, func() {
		a.rec.SetEnabled(!a.rec.Enabled())
		p.sync()
	})
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
		v := !a.rec.Persist()
		if a.saveConfig(func(c *configT) { c.RecorderDisk = v }) {
			a.rec.SetPersist(v)
		}
		p.sync()
	})
	p.bClear = newButton("Clear", "", 90, func() {
		if !a.confirm("Clear recording", "Clear the recording and markers from memory?\n\nSaved recordings on disk are kept.") {
			return
		}
		a.rec.Clear()
		p.selT, p.live = 0, true
		p.view.invalidate()
	})
	labels := [4]string{"5 min", "15 min", "1 hour", "3 hours"}
	for i := range p.bRange {
		i := i
		p.bRange[i] = newButton(labels[i], "", 92, func() {
			p.rangeSecs = p.rangeChoices[i]
			p.sync()
		})
		p.bRange[i].toggle = true
	}
	return p
}

func (p *recorderPage) build() decl.Widget {
	bar := []decl.Widget{p.bRec.widget(), p.bMark.widget(), p.bExport.widget(), p.bDisk.widget(), p.bClear.widget(), decl.HSpacer{}}
	ranges := []decl.Widget{decl.Label{Text: "Time range", TextColor: T.Muted}}
	for _, b := range p.bRange {
		ranges = append(ranges, b.widget())
	}
	ranges = append(ranges, decl.HSpacer{})
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
					decl.Composite{
						Background: decl.SolidColorBrush{Color: T.Bg},
						Layout:     decl.HBox{MarginsZero: true, Spacing: 8},
						MinSize:    decl.Size{Height: 38},
						MaxSize:    decl.Size{Height: 38},
						Children:   bar,
					},
					decl.Composite{Background: decl.SolidColorBrush{Color: T.Bg}, Layout: decl.HBox{MarginsZero: true, Spacing: 8}, MinSize: decl.Size{Height: 38}, MaxSize: decl.Size{Height: 38}, Children: ranges},
					p.view.declare(0, 0, 0, 0, bodyStretch),
				},
			},
			p.app.foots[pgRecorder].declare(0, 32, 0, 32, 0),
		},
	}
}

func (p *recorderPage) init() {
	p.app.registerRoot(pgRecorder, p.root)
	p.app.rec.SetPersist(p.app.cfg.RecorderDisk)
	// Old files are pruned first (even with persistence now off), and reading
	// the rest happens off the UI thread.
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
		p.bRec.setText("Paused")
	}
	p.bDisk.set(true, p.app.rec.Persist())
	hasSamples := p.app.rec.Len() > 0
	p.bExport.set(hasSamples, false)
	p.bClear.set(hasSamples, false)
	p.bMark.set(hasSamples, false)
	for i, b := range p.bRange {
		b.set(true, p.rangeSecs == p.rangeChoices[i])
	}
	p.view.invalidate()
}

func (p *recorderPage) export() {
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
	if err := p.app.rec.ExportCSV(path, 0, 0); err != nil {
		p.app.errBox("Export", err)
		return
	}
	openInExplorer(path)
}

// plot geometry shared by paint and click handling
// The chart takes half the height, but leaves the detail panels below it the
// ~190 units they need on short windows (small laptops at 125-150 %).
func (p *recorderPage) plotRect(w, h int) rect {
	ph := min(h/2-40, h-318)
	if ph < 120 {
		ph = 120
	}
	return rect{24, 56, w - 48, ph}
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

// fitting reports whether the chart is showing less than the chosen range.
func (p *recorderPage) fitting() bool {
	from, to := p.window()
	return to-from < int64(p.rangeSecs)*1000-2000
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
	pr := p.plotRect(p.view.w, p.view.h)
	if !pr.contains(x, y) && !(y > pr.Y-30 && y < pr.Y+pr.H+30) {
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

	// header strip: state + selected time
	state := "Recording"
	ink := T.Bad
	if !rec.Enabled() {
		state, ink = "Paused", T.Warn
	}
	fillRound(cv, rect{26, 20, 8, 8}, 4, ink)
	span := fmt.Sprintf("%s window", humanDur(time.Duration(p.rangeSecs)*time.Second))
	if p.fitting() {
		span = fmt.Sprintf("showing %s recorded so far (the %s window fills as samples arrive)",
			humanDur(time.Duration(to-from)*time.Millisecond), humanDur(time.Duration(p.rangeSecs)*time.Second))
	}
	drawText(cv, fmt.Sprintf("%s · one sample every 2 s · %s", state, span),
		uiFont(10), T.Muted, rect{44, 12, w - 360, 24}, taLeft)
	p.view.addText(rect{26, 12, w - 342, 24}, fmt.Sprintf("%s, one sample every 2 seconds, %s", state, span))

	pr := p.plotRect(w, h)
	fillRound(cv, rect{pr.X - 12, pr.Y - 12, pr.W + 24, pr.H + 52}, 10, T.Card)
	strokeRound(cv, rect{pr.X - 12, pr.Y - 12, pr.W + 24, pr.H + 52}, 10, T.Stroke)

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
		{vals: mk(func(s metrics.RecSample) float64 { return s.GPU }), times: times, color: T.GPU},
		{vals: mk(func(s metrics.RecSample) float64 { return s.CPU }), times: times, color: T.CPU, fill: true},
	}
	plotSeries(cv, pr, ser, graphStyle{bg: T.Card, max: 100, timeFrom: from, timeTo: to, grid: true, lineW: 2})
	drawText(cv, "100%", uiFont(8), T.Faint, rect{pr.X + 6, pr.Y + 2, 50, 14}, taLeft)
	drawText(cv, "0", uiFont(8), T.Faint, rect{pr.X + 6, pr.Y + pr.H - 16, 30, 14}, taLeft)

	// legend (top right) + time axis
	lx := w - 300
	for _, l := range []struct {
		n string
		c walk.Color
	}{{"CPU", T.CPU}, {"Memory", T.Mem}, {"GPU", T.GPU}} {
		fillRound(cv, rect{lx, 17, 10, 10}, 3, l.c)
		drawText(cv, l.n, uiFont(9), T.Muted, rect{lx + 16, 10, 70, 24}, taLeft)
		lx += 92
	}
	tf := "15:04"
	if to-from < 10*60_000 { // a short span needs seconds, or every label reads the same
		tf = "15:04:05"
	}
	for i := 0; i <= 4; i++ {
		t := time.UnixMilli(from + int64(i)*(to-from)/4)
		x := pr.X + i*pr.W/4
		fl := walk.TextCenter | walk.TextVCenter | walk.TextSingleLine
		drawText(cv, t.Format(tf), uiFont(8), T.Faint, rect{x - 40, pr.Y + pr.H + 6, 80, 22}, fl)
	}

	// markers
	for _, m := range rec.Marks() {
		if m.T < from || m.T > to {
			continue
		}
		x := pr.X + int(timePosition(m.T, from, to)*float64(pr.W))
		fillRect(cv, rect{x, pr.Y, 1, pr.H}, T.Warn)
		drawText(cv, m.Label, uiFont(8), T.Warn, rect{x + 4, pr.Y + 2, 70, 14}, taLeft)
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
	if !p.live {
		plotName = "Timeline, at " + time.UnixMilli(selT).Format("15:04:05")
	}
	if cur, ok := rec.Nearest(selT); ok {
		plotName += fmt.Sprintf(": CPU %.0f%%, memory %.0f%%, GPU %.0f%%", cur.CPU, cur.Mem, cur.GPU)
	}
	p.view.addItem(rect{pr.X, pr.Y - 14, pr.W, pr.H + 28}, "plot", plotName, ctlSlider, false)

	// detail panel for the selected moment
	dy := pr.Y + pr.H + 56
	dh := h - dy - 16
	if dh < 150 {
		p.paintCompactDetail(cv, selT, w, pr.Y+pr.H+34, h)
		return
	}
	s, ok := rec.Nearest(selT)
	if !ok {
		message := "Waiting for the first sample…"
		if !rec.Enabled() {
			message = "Recording is paused. Select Paused above to start collecting samples."
		}
		drawText(cv, message, uiFont(11), T.Muted, rect{24, dy, w - 48, 30}, taLeft)
		return
	}
	left := rect{24, dy, 330, dh}
	right := rect{370, dy, w - 394, dh}
	fillRound(cv, left, 10, T.Card)
	strokeRound(cv, left, 10, T.Stroke)
	fillRound(cv, right, 10, T.Card)
	strokeRound(cv, right, 10, T.Stroke)

	when := time.UnixMilli(s.T).Format("Mon 15:04:05")
	title := "At " + when
	if p.live {
		title = "Live · " + when
	}
	drawText(cv, title, uiBold(11), T.Text, rect{left.X + 16, left.Y + 10, left.W - 32, 24}, taLeft)
	p.view.addText(rect{left.X + 16, left.Y + 10, left.W - 120, 24}, title)
	if !p.live {
		p.view.addItem(rect{left.X + left.W - 96, left.Y + 10, 84, 24}, "live", "Go live", ctlButton, false)
		fillRound(cv, rect{left.X + left.W - 96, left.Y + 10, 84, 24}, 6, blend(T.Accent, T.Card, 0.3))
		drawText(cv, "Go live", uiFont(9), T.Text, rect{left.X + left.W - 96, left.Y + 10, 84, 24}, taCenter)
	}
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
	stat("GPU", s.GPU, T.GPU, "%")
	stat("Disk busy", s.DiskP, T.Disk, "%")
	drawText(cv, "Disk "+fmtRate(s.DiskBps)+"   Network "+fmtRate(s.NetBps), uiFont(9), T.Muted,
		rect{left.X + 16, y + 2, left.W - 32, 20}, taLeft)
	p.view.addText(rect{left.X + 16, y + 2, left.W - 32, 20}, "Disk "+fmtRate(s.DiskBps)+", network "+fmtRate(s.NetBps))

	// process table at that moment
	drawText(cv, "Busiest processes at that moment", uiBold(11), T.Text, rect{right.X + 16, right.Y + 10, right.W - 32, 24}, taLeft)
	p.view.addText(rect{right.X + 16, right.Y + 10, right.W - 32, 24}, "Busiest processes at that moment")
	ty := right.Y + 44
	hdr := walk.TextRight | walk.TextVCenter | walk.TextSingleLine
	// columns from the right edge; PID is dropped when the panel is narrow
	memX, cpuX, pidX := right.X+right.W-160, right.X+right.W-250, right.X+right.W-330
	showPID := right.W >= 440
	nameEnd := pidX - 12
	if !showPID {
		nameEnd = cpuX - 12
	}
	nameW := max(60, nameEnd-(right.X+16))
	drawText(cv, "Process", uiFont(9), T.Faint, rect{right.X + 16, ty, nameW, 20}, taLeft)
	if showPID {
		drawText(cv, "PID", uiFont(9), T.Faint, rect{pidX, ty, 60, 20}, hdr)
	}
	drawText(cv, "CPU", uiFont(9), T.Faint, rect{cpuX, ty, 70, 20}, hdr)
	drawText(cv, "Memory", uiFont(9), T.Faint, rect{memX, ty, 100, 20}, hdr)
	ty += 22
	maxRows := (right.H - 70) / 24
	for i, pr := range s.Top {
		if i >= maxRows {
			break
		}
		c := T.Text
		if pr.CPU >= 25 {
			c = T.Warn
		}
		drawText(cv, pr.Name, uiFont(10), c, rect{right.X + 16, ty, nameW, 22}, taLeft)
		if showPID {
			drawText(cv, fmt.Sprint(pr.PID), uiFont(10), T.Muted, rect{pidX, ty, 60, 22}, hdr)
		}
		drawText(cv, fmtPct(pr.CPU), uiFont(10), c, rect{cpuX, ty, 70, 22}, hdr)
		drawText(cv, fmtBytes(pr.Mem), uiFont(10), T.Muted, rect{memX, ty, 100, 22}, hdr)
		p.view.addText(rect{right.X + 16, ty, right.W - 32, 22}, fmt.Sprintf("%s, PID %d, CPU %s, memory %s", pr.Name, pr.PID, fmtPct(pr.CPU), fmtBytes(pr.Mem)))
		ty += 24
	}
}

// paintCompactDetail is the selected moment in a single line, for windows too
// short for the two detail panels.
func (p *recorderPage) paintCompactDetail(cv *walk.Canvas, selT int64, w, y, h int) {
	s, ok := p.app.rec.Nearest(selT)
	if !ok || h-y < 20 {
		return
	}
	when := time.UnixMilli(s.T).Format("15:04:05")
	if p.live {
		when = "Live " + when
	}
	line := fmt.Sprintf("%s · CPU %.0f%% · Memory %.0f%% · GPU %.0f%% · Disk %.0f%%", when, s.CPU, s.Mem, s.GPU, s.DiskP)
	if len(s.Top) > 0 {
		var names []string
		for i, pr := range s.Top {
			if i == 3 {
				break
			}
			names = append(names, fmt.Sprintf("%s %s", pr.Name, fmtPct(pr.CPU)))
		}
		line += " · busiest: " + strings.Join(names, ", ")
	}
	drawText(cv, line, uiFont(9), T.Muted, rect{24, y, w - 48 - 96, 20}, taLeft)
	p.view.addText(rect{24, y, w - 48 - 96, 20}, strings.ReplaceAll(line, " · ", ", "))
	if !p.live {
		r := rect{w - 24 - 84, y - 2, 84, 24}
		fillRound(cv, r, 6, blend(T.Accent, T.Card, 0.3))
		drawText(cv, "Go live", uiFont(9), T.Text, r, taCenter)
		p.view.addItem(r, "live", "Go live", ctlButton, false)
	}
}
