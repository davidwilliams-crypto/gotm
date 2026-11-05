//go:build windows

package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"

	"gotm/internal/metrics"
)

// ---- sidebar -------------------------------------------------------------------

// sidebarWidth uses two fixed widths so resizing within a band never causes
// another round of native layout work.
func sidebarWidth(clientWidth int) int {
	if clientWidth < 1030 {
		return 168
	}
	return 204
}

func (a *App) paintSidebar(cv *walk.Canvas, w, h int) {
	v := a.side
	if v.keyHit == "" {
		v.keyHit = fmt.Sprintf("nav:%d", a.cur)
	}
	brandH, sectionH, gap := 70, 25, 12
	if h < 540 {
		brandH, sectionH, gap = 46, 18, 5
	}
	itemH := clampI((h-brandH-2*sectionH-2*gap-8)/pgCount, 24, 40)
	if a.mw != nil && a.mw.Icon() != nil {
		_ = cv.DrawImageStretchedPixels(a.mw.Icon(), rect{18, (brandH - 32) / 2, 32, 32}.wr())
	}
	drawText(cv, "gotm", uiBold(17), T.Text, rect{60, (brandH - 44) / 2, w - 70, 29}, taLeft)
	drawText(cv, "Task Manager", uiFont(9), T.Muted, rect{61, (brandH-44)/2 + 28, w - 70, 17}, taLeft)
	v.addText(rect{18, 0, w - 32, brandH}, "gotm Task Manager")
	fillRect(cv, rect{w - 1, 0, 1, h}, T.Stroke)

	y := brandH
	section := func(name string) {
		r := rect{20, y, w - 36, sectionH}
		drawText(cv, name, uiBold(8), T.Faint, r, taLeft)
		v.addText(r, name)
		y += sectionH
	}
	item := func(i, y int) {
		p := a.pages[i]
		r := rect{8, y, w - 17, itemH - 3}
		id := fmt.Sprintf("nav:%d", i)
		name := p.title
		selected := i == a.cur && p.initialized
		pending := a.opening != nil && a.opening.id == i
		ink, glyphInk := T.Muted, T.Faint
		font := uiFont(10)
		switch {
		case pending:
			strokeRound(cv, r, 5, T.Accent)
			ink, glyphInk = T.Text, T.Accent
			name += ", opening"
		case selected:
			fill, marker := T.CardSel, T.Accent
			ink, glyphInk = T.Text, T.Accent
			if highContrast {
				fill, marker, ink, glyphInk = T.RowSel, T.RowSelText, T.RowSelText, T.RowSelText
			}
			fillRound(cv, r, 5, fill)
			fillRect(cv, rect{r.X, r.Y + r.H/4, 3, r.H / 2}, marker)
			font = uiSemibold(10)
		case v.hover == id:
			fillRound(cv, r, 5, T.CardHi)
			ink, glyphInk = T.Text, T.Text
		}
		drawText(cv, p.glyph, iconFont(12), glyphInk, rect{r.X + 12, r.Y, 22, r.H}, taCenter)
		drawText(cv, p.title, font, ink, rect{r.X + 45, r.Y, r.W - 50, r.H}, taLeft)
		v.addItem(r, id, name, ctlListItem, selected)
	}
	section("System")
	for i := pgPerformance; i <= pgServices; i++ {
		item(i, y)
		y += itemH
	}
	y += gap
	section("Insights")
	for i := pgPower; i <= pgDisk; i++ {
		item(i, y)
		y += itemH
	}
	sy := h - 6 - itemH
	hline(cv, 16, w-16, sy-3, T.Stroke)
	item(pgSettings, sy)
}

// ---- header --------------------------------------------------------------------

func (a *App) paintHeader(cv *walk.Canvas, w, h int, page int) {
	p, v := a.pages[page], a.heads[page]
	title := rect{24, 7, w - 48, 37}
	sub := rect{25, 44, w - 50, 23}
	subtitle := p.subtitle
	if a.opening != nil {
		subtitle = "Opening " + a.pages[a.opening.id].title + "…"
	}
	var alert rect
	if a.alertText != "" {
		if w >= 760 {
			aw := min(textWidth(cv, a.alertText, uiFont(9))+40, w/2)
			alert = rect{w - 24 - aw, 22, aw, 30}
			title.W, sub.W = alert.X-40, alert.X-40
		} else {
			// Give the warning its own line when it would squeeze the title.
			alert = rect{24, 45, w - 48, 25}
			sub.W = 0
		}
	}
	drawText(cv, p.title, uiSemibold(21), T.Text, title, taLeft)
	v.addText(title, p.title)
	if sub.W > 0 {
		drawText(cv, subtitle, uiFont(10), T.Muted, sub, taLeft)
		v.addText(sub, subtitle)
	}
	if alert.W > 0 {
		fillRound(cv, alert, 5, blend(T.Warn, T.Bg, 0.12))
		fillRect(cv, rect{alert.X, alert.Y + 4, 3, alert.H - 8}, T.Warn)
		drawText(cv, a.alertText, uiFont(9), T.Warn, rect{alert.X + 12, alert.Y, alert.W - 20, alert.H}, taLeft)
		v.addText(alert, "Alert: "+a.alertText)
	}
	hline(cv, 0, w, h-1, T.Stroke)
}

// ---- footer --------------------------------------------------------------------

func (a *App) paintFooter(cv *walk.Canvas, w, h int, page int) {
	v, s := a.foots[page], a.snap
	hline(cv, 0, w, 0, T.Stroke)
	recOn := a.rec.Enabled()
	rec, recName, recInk := "Recording off", "Flight recorder off; turn on", T.Muted
	if recOn {
		rec, recInk = "Recording on", T.Bad
		if first, ok := a.rec.Oldest(); ok {
			if last, ok := a.rec.Nearest(time.Now().UnixMilli()); ok {
				rec = "Recording  " + humanDur(time.Duration(last.T-first)*time.Millisecond)
			}
		}
		recName = rec + "; turn off"
	}
	admin := "Run as administrator"
	if a.host.Elevated {
		admin = "Administrator"
		if a.elevatedAs != "" {
			admin += " (" + a.elevatedAs + ")"
		}
	}
	font := uiFont(9)
	aw := min(textWidth(cv, admin, font)+20, max(128, w/4))
	ar := rect{w - 8 - aw, 3, aw, h - 6}
	rw := min(156, textWidth(cv, rec, font)+34)
	rr := rect{ar.X - 8 - rw, 3, rw, h - 6}
	live, dot := "Live", T.Good
	if s == nil {
		live, dot = "Starting…", T.Faint
	}
	lw := textWidth(cv, live, uiSemibold(9)) + 4
	fillRound(cv, rect{16, h/2 - 3, 6, 6}, 3, dot)
	drawText(cv, live, uiSemibold(9), T.Text, rect{28, 0, lw, h}, taLeft)
	v.addText(rect{16, 0, lw + 12, h}, live)
	x := 28 + lw + 14
	if s != nil && rr.X-x > 96 {
		details := fmt.Sprintf("%d processes", s.Mem.Processes)
		if rr.X-x >= 510 {
			details += fmt.Sprintf("   ·   %d threads", s.Mem.Threads)
			if a.localIP != "" && rr.X-x >= 610 {
				details += "   ·   " + a.localIP
			}
		}
		if rr.X-x >= 220 {
			details += "   ·   Up " + fmtDur(s.Uptime)
		}
		r := rect{x, 0, rr.X - x - 8, h}
		drawText(cv, details, font, T.Muted, r, taLeft)
		v.addText(r, strings.ReplaceAll(details, "   ·   ", ", "))
	}
	if v.hover == "rec" || v.down == "rec" {
		fillRound(cv, rr, 4, T.CardHi)
	}
	ring := rect{rr.X + 9, h/2 - 4, 9, 9}
	if recOn {
		fillRound(cv, ring, 5, recInk)
	} else {
		strokeRound(cv, ring, 5, recInk)
	}
	drawText(cv, rec, font, recInk, rect{rr.X + 24, rr.Y, rr.W - 28, rr.H}, taLeft)
	v.addItem(rr, "rec", recName, ctlCheckBox, recOn)

	fillRect(cv, rect{ar.X - 4, h/2 - 6, 1, 12}, T.Stroke)
	ink := T.Muted
	if !a.host.Elevated {
		ink = T.Accent
		if v.hover == "admin" {
			fillRound(cv, ar, 4, T.CardHi)
		}
	}
	drawText(cv, admin, font, ink, ar.inset(2), taCenter)
	if a.host.Elevated {
		v.addText(ar, admin)
	} else {
		v.addItem(ar, "admin", admin, ctlButton, false)
	}
	if v.keyHit == "" {
		v.keyHit = "rec"
	}
}

// ---- alerts + tray -------------------------------------------------------------

// alertValue is the snapshot reading an alert rule watches (0 for an unknown metric).
func alertValue(s *metrics.Snapshot, metric string) float64 {
	switch metric {
	case "cpu":
		return s.CPU
	case "mem":
		return s.Mem.Percent
	case "disk":
		return s.Disk.Percent
	case "gpu":
		return s.GPU.Percent
	}
	return 0
}

// checkAlerts runs on every tick, visible or not. Its per-rule state lives in
// the App's maps (reset only when the rules change), and nothing is formatted
// unless a rule is firing.
func (a *App) checkAlerts(s *metrics.Snapshot) {
	if a.alertSince == nil {
		a.alertSince = map[int]int64{}
	}
	first, firing := "", 0
	for i, al := range a.cfg.Alerts {
		v := alertValue(s, al.Metric)
		if !al.Enabled || (al.Metric == "gpu" && !s.GPU.OK) || v <= al.Above {
			delete(a.alertSince, i)
			continue
		}
		since, ok := a.alertSince[i]
		if !ok {
			since = s.T
			a.alertSince[i] = since
		}
		secs := (s.T - since) / 1000
		if secs < int64(al.ForSeconds) {
			continue
		}
		firing++
		notify := a.tray != nil && time.Since(a.alertShown[i]) > 2*time.Minute
		if firing > 1 && !notify {
			continue // only counted in the header
		}
		msg := fmt.Sprintf("%s above %.0f%% for %d s (now %.0f%%)", metricName(al.Metric), al.Above, secs, v)
		if firing == 1 {
			first = msg
		}
		if notify {
			if a.alertShown == nil {
				a.alertShown = map[int]time.Time{}
			}
			a.alertShown[i] = time.Now()
			_ = a.tray.ShowWarning("gotm alert", msg)
		}
	}
	text := first
	if firing > 1 {
		text += fmt.Sprintf("  (+%d more)", firing-1)
	}
	if text != a.alertText {
		a.alertText = text
		a.invalidateChrome()
	}
}

func metricName(m string) string {
	switch m {
	case "cpu":
		return "CPU"
	case "mem":
		return "Memory"
	case "disk":
		return "Disk"
	case "gpu":
		return "GPU"
	}
	return m
}

func (a *App) setupTray(icon *walk.Icon) {
	ni, err := walk.NewNotifyIcon()
	if err != nil {
		return
	}
	a.tray = ni
	_ = ni.SetIcon(icon)
	_ = ni.SetToolTip("gotm")

	show := walk.NewAction()
	_ = show.SetText("Show gotm")
	show.Triggered().Attach(a.showWindow)
	quit := walk.NewAction()
	_ = quit.SetText("Exit")
	quit.Triggered().Attach(func() { a.mw.Close() })
	ni.ContextMenu().Actions().Add(show)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	ni.ContextMenu().Actions().Add(quit)

	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showWindow()
		}
	})
	_ = ni.SetVisible(true)
}

func (a *App) showWindow() {
	h := win.HWND(a.mw.Handle())
	a.userPresent() // the tray was clicked: the screen is on and unlocked
	a.mw.Show()
	if win.IsIconic(h) {
		win.ShowWindow(h, win.SW_RESTORE)
	}
	win.SetForegroundWindow(h)
	_ = a.mw.SetFocus()
}

// updateTray keeps the tray tooltip current. Shell_NotifyIcon(NIM_MODIFY) is
// a synchronous call into Explorer that can stall the UI thread while
// Explorer is busy, so it is made only when the displayed (rounded) text
// changes, and at most once per trayTipInterval.
func (a *App) updateTray(s *metrics.Snapshot) {
	if a.tray != nil && a.tip.offer(s.CPU, s.Mem.Percent, time.Now()) {
		_ = a.tray.SetToolTip(a.tip.text())
	}
}

const trayTipInterval = 2 * time.Second

// trayTip throttles and de-duplicates the tray tooltip. The newest values are
// always retained, so the tooltip converges on them with the first offer (or
// flush) after the throttle window, even if the readings stop changing.
type trayTip struct {
	cpu, ram           float64 // newest values, rounded as displayed
	shownCPU, shownRAM float64
	shown              bool // a tooltip has been sent
	last               time.Time
}

// offer records the newest readings and reports whether the tooltip should
// be updated now (then text returns it).
func (t *trayTip) offer(cpu, ram float64, now time.Time) bool {
	t.cpu, t.ram = math.RoundToEven(cpu), math.RoundToEven(ram)
	return t.flush(now)
}

// flush reports whether the retained readings should be sent now.
func (t *trayTip) flush(now time.Time) bool {
	if t.shown && t.cpu == t.shownCPU && t.ram == t.shownRAM {
		return false // displayed text would not change
	}
	if t.shown && now.Sub(t.last) < trayTipInterval {
		return false // kept pending until the window has passed
	}
	t.shownCPU, t.shownRAM, t.shown, t.last = t.cpu, t.ram, true, now
	return true
}

func (t *trayTip) text() string {
	return fmt.Sprintf("gotm — CPU %.0f%% · RAM %.0f%%", t.shownCPU, t.shownRAM)
}
