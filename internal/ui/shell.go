//go:build windows

package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"

	"gotm/internal/metrics"
)

// ---- sidebar -------------------------------------------------------------------

const (
	sideItemH = 40
	sideTop   = 84
)

func (a *App) paintSidebar(cv *walk.Canvas, w, h int) {
	v := a.side
	if v.keyHit == "" {
		v.keyHit = fmt.Sprintf("nav:%d", a.cur) // keyboard focus starts on the current page
	}

	// brand
	drawText(cv, "gotm", uiBold(18), T.Text, rect{22, 16, w - 40, 32}, taLeft)
	drawText(cv, "TASK MANAGER", uiFont(8), T.Muted, rect{24, 46, w - 40, 16}, taLeft)
	v.addText(rect{22, 16, w - 40, 46}, "gotm task manager")

	// Ten items, the section label and the pinned Settings item must fit; on
	// short windows (small laptops at 125-150 %) the rows get tighter.
	itemH := sideItemH
	if avail := h - sideTop - 32 - 70; avail < 10*itemH {
		itemH = max(26, avail/10)
	}
	y := sideTop
	item := func(i int, y int) {
		p := a.pages[i]
		r := rect{10, y, w - 20, itemH - 4}
		id := fmt.Sprintf("nav:%d", i)
		switch {
		case i == a.cur:
			fillRound(cv, r, 8, blend(T.Accent, T.Side, 0.28))
			fillRect(cv, rect{r.X, r.Y + 9, 3, r.H - 18}, T.Accent)
		case v.hover == id:
			fillRound(cv, r, 8, blend(T.Text, T.Side, 0.07))
		}
		ink := T.Muted
		if i == a.cur || v.hover == id {
			ink = T.Text
		}
		drawText(cv, p.glyph, iconFont(12), ink, rect{r.X + 14, r.Y, 24, r.H}, taLeft)
		drawText(cv, p.title, uiFont(10), ink, rect{r.X + 46, r.Y, r.W - 54, r.H}, taLeft)
		v.addItem(r, id, p.title, ctlListItem, i == a.cur)
	}

	for i := pgPerformance; i <= pgServices; i++ {
		item(i, y)
		y += itemH
	}

	// Advanced monitoring tools share the same access as the main pages.
	y += 10
	drawText(cv, "INSIGHTS", uiBold(8), T.Muted, rect{24, y, 72, 16}, taLeft)
	v.addText(rect{24, y, 72, 16}, "Insights")
	hline(cv, 100, w-20, y+8, T.Stroke)
	y += 22
	for i := pgPower; i <= pgDisk; i++ {
		item(i, y)
		y += itemH
	}

	// settings pinned to the bottom, or just below the list if that is lower
	sy := max(h-16-itemH, y+8)
	hline(cv, 18, w-18, sy-6, T.Stroke)
	item(pgSettings, sy)
}

// ---- header --------------------------------------------------------------------

func (a *App) paintHeader(cv *walk.Canvas, w, h int, page int) {
	p := a.pages[page]
	titleW := w - 56
	if a.alertText != "" {
		titleW -= min(400, w/2) + 16
	}
	drawText(cv, p.title, uiLight(24), T.Text, rect{28, 8, titleW, 38}, taLeft)
	drawText(cv, p.subtitle, uiFont(10), T.Muted, rect{30, 46, w - 60, 20}, taLeft)
	a.heads[page].addText(rect{28, 8, titleW, 58}, p.title+". "+p.subtitle)
	if a.alertText != "" {
		aw := min(400, w/2)
		r := rect{w - aw - 24, 8, aw, 34}
		fillRound(cv, r, 8, blend(T.Warn, T.Bg, 0.22))
		strokeRound(cv, r, 8, blend(T.Warn, T.Bg, 0.6))
		drawText(cv, "⚠  "+a.alertText, uiFont(10), T.Warn, rect{r.X + 12, r.Y, r.W - 20, r.H}, taLeft)
		a.heads[page].addText(r, "Alert: "+a.alertText)
	}
	hline(cv, 0, w, h-1, T.Stroke)
}

// ---- footer --------------------------------------------------------------------

func (a *App) paintFooter(cv *walk.Canvas, w, h int, page int) {
	hline(cv, 0, w, 0, T.Stroke)
	v := a.foots[page]
	s := a.snap

	dot := T.Good
	state := "Live"
	if s == nil {
		dot, state = T.Warn, "Starting…"
	}
	fillRound(cv, rect{20, h/2 - 4, 8, 8}, 4, dot)
	parts := state
	if s != nil {
		parts += fmt.Sprintf("  •  %d processes", s.Mem.Processes)
		if w >= 1100 {
			parts += fmt.Sprintf("  •  %d threads", s.Mem.Threads)
		}
		if a.localIP != "" && w >= 1100 {
			parts += "  •  IP " + a.localIP
		}
		parts += "  •  Up " + fmtDur(s.Uptime)
	}
	drawText(cv, parts, uiFont(9), T.Muted, rect{38, 0, w - 360, h}, taLeft)
	v.addText(rect{20, 0, w - 342, h}, strings.ReplaceAll(parts, "  •  ", ", "))

	// recorder indicator
	rec := "REC off"
	recC := T.Faint
	if a.rec.Enabled() {
		span := time.Duration(0)
		if first, ok := a.rec.Oldest(); ok {
			if last, ok := a.rec.Nearest(time.Now().UnixMilli()); ok {
				span = time.Duration(last.T-first) * time.Millisecond
			}
		}
		rec = "REC  " + humanDur(span)
		recC = T.Bad
	}
	rr := rect{w - 300, 4, 104, h - 8}
	if v.hover == "rec" {
		fillRound(cv, rr, 6, blend(T.Text, T.Side, 0.08))
	}
	fillRound(cv, rect{rr.X + 10, h/2 - 4, 8, 8}, 4, recC)
	drawText(cv, rec, uiFont(9), recC, rect{rr.X + 24, 0, rr.W - 24, h}, taLeft)
	recName := "Flight recorder off; turn on"
	if a.rec.Enabled() {
		recName = "Flight recorder on, " + strings.TrimPrefix(rec, "REC  ") + " recorded; turn off"
	}
	v.addItem(rr, "rec", recName, ctlButton, a.rec.Enabled())

	// admin chip
	label, ink := "Administrator", T.Good
	if a.elevatedAs != "" {
		label = "Administrator (" + a.elevatedAs + ")" // a different account's settings and startup items
	}
	if !a.host.Elevated {
		label, ink = "Run as administrator", T.Muted
	}
	ar := rect{w - 188, 4, 172, h - 8}
	if v.hover == "admin" && !a.host.Elevated {
		fillRound(cv, ar, 6, blend(T.Accent, T.Side, 0.25))
		ink = T.Text
	}
	drawText(cv, label, uiFont(9), ink, ar, taRight)
	if !a.host.Elevated {
		v.addItem(ar, "admin", "Run as administrator", ctlButton, false)
	} else {
		v.addText(ar, label)
	}
	if v.keyHit == "" {
		v.keyHit = "rec"
	}
}

// ---- alerts + tray -------------------------------------------------------------

func (a *App) checkAlerts(s *metrics.Snapshot) {
	val := map[string]float64{"cpu": s.CPU, "mem": s.Mem.Percent, "disk": s.Disk.Percent, "gpu": s.GPU.Percent}
	var firing []string
	for i, al := range a.cfg.Alerts {
		if !al.Enabled || val[al.Metric] <= al.Above {
			delete(a.alertSince, i)
			continue
		}
		if _, ok := a.alertSince[i]; !ok {
			a.alertSince[i] = s.T
		}
		secs := (s.T - a.alertSince[i]) / 1000
		if secs < int64(al.ForSeconds) {
			continue
		}
		msg := fmt.Sprintf("%s above %.0f%% for %d s (now %.0f%%)", metricName(al.Metric), al.Above, secs, val[al.Metric])
		firing = append(firing, msg)
		if a.tray != nil && time.Since(a.alertShown[i]) > 2*time.Minute {
			a.alertShown[i] = time.Now()
			_ = a.tray.ShowWarning("gotm alert", msg)
		}
	}
	text := ""
	if len(firing) > 0 {
		text = firing[0]
		if len(firing) > 1 {
			text += fmt.Sprintf("  (+%d more)", len(firing)-1)
		}
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
	a.mw.Show()
	if win.IsIconic(h) {
		win.ShowWindow(h, win.SW_RESTORE)
	}
	win.SetForegroundWindow(h)
	_ = a.mw.SetFocus()
}

func (a *App) updateTray(s *metrics.Snapshot) {
	if a.tray != nil {
		_ = a.tray.SetToolTip(fmt.Sprintf("gotm — CPU %.0f%% · RAM %.0f%%", s.CPU, s.Mem.Percent))
	}
}
