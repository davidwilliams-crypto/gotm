//go:build windows

package ui

import (
	"fmt"
	"strings"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

var (
	refreshChoices = []struct {
		label string
		ms    int
	}{{"0.5 s", 500}, {"1 s", 1000}, {"2 s", 2000}, {"5 s", 5000}}
	historyChoices = []struct {
		label string
		sec   int
	}{{"1 min", 60}, {"5 min", 300}, {"15 min", 900}, {"60 min", 3600}}
)

// settingsTab applies every change immediately; there is no Save button.
type settingsTab struct {
	app  *App
	root *walk.Composite
	v    *canvasView

	scroll  int // vertical offset, design units
	content int // total content height from the last paint
	viewH   int
}

func newSettingsTab(a *App) *settingsTab {
	t := &settingsTab{app: a}
	t.v = newCanvasView(t.paint)
	t.v.keyboard, t.v.keyName = true, "Settings. Tab selects a setting, Space changes it, Page Up or Page Down scroll."
	t.v.acc, t.v.accName, t.v.accCtl = true, "Settings", ctlPane
	t.v.onClick = t.onClick
	return t
}

func (t *settingsTab) build() decl.Widget {
	return decl.Composite{
		AssignTo:      &t.root,
		StretchFactor: 1,
		Background:    decl.SolidColorBrush{Color: T.Bg},
		Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			t.app.heads[pgSettings].declare(0, 76, 0, 76, 0),
			t.v.declare(0, 0, 0, 0, bodyStretch),
			t.app.foots[pgSettings].declare(0, 32, 0, 32, 0),
		},
	}
}

func (t *settingsTab) init() {
	t.app.registerRoot(pgSettings, t.root)
	t.v.onWheel = func(delta int) {
		t.scrollBy(-delta / 2) // one wheel notch (120) moves 60 design units
	}
	t.v.attachWheel()
}

// scrollBy moves the page, clamped to the content measured by the last paint.
func (t *settingsTab) scrollBy(d int) {
	max := t.content - t.viewH
	if max < 0 {
		max = 0
	}
	n := t.scroll + d
	if n < 0 {
		n = 0
	}
	if n > max {
		n = max
	}
	if n != t.scroll {
		t.scroll = n
		t.v.invalidate()
	}
}
func (t *settingsTab) shown()                   { t.v.invalidate() }
func (t *settingsTab) focusTarget() walk.Window { return t.v.cw }
func (t *settingsTab) reload()                  { t.v.invalidate() }

func (t *settingsTab) pageKey(key walk.Key, mods walk.Modifiers) bool {
	if key == walk.KeyTab && (mods == 0 || mods == walk.ModShift) || mods == 0 && (key == walk.KeyLeft || key == walk.KeyRight) {
		dir := 1
		if mods == walk.ModShift || key == walk.KeyLeft {
			dir = -1
		}
		if r, ok := t.v.moveKeyboard(dir); ok {
			y, h := int(float64(r.Y)/gs), int(float64(r.H)/gs)
			if y < 12 {
				t.scrollBy(y - 12)
			} else if y+h > t.viewH-12 {
				t.scrollBy(y + h - t.viewH + 12)
			}
		}
		return true
	}
	if mods != 0 {
		return false
	}
	switch key {
	case walk.KeyUp:
		t.scrollBy(-60)
	case walk.KeyDown:
		t.scrollBy(60)
	case walk.KeyPrior:
		t.scrollBy(-max(60, t.viewH-40))
	case walk.KeyNext:
		t.scrollBy(max(60, t.viewH-40))
	case walk.KeyHome:
		t.scrollBy(-t.content)
	case walk.KeyEnd:
		t.scrollBy(t.content)
	case walk.KeySpace, walk.KeyReturn:
		if t.v.keyHit != "" {
			t.onClick(t.v.keyHit, walk.LeftButton)
		}
	default:
		return false
	}
	return true
}
func (t *settingsTab) tick(*metrics.Snapshot) {
	if t.app.sensors.Ryzen() != nil {
		t.v.invalidate() // keeps the AMD SDK status line live
	}
}

// chip draws a pill button and registers its hit area. group names the
// setting a choice chip belongs to ("Refresh every"); an empty group makes the
// chip an on/off switch.
func (t *settingsTab) chip(cv *walk.Canvas, x, y, w int, label string, on bool, id, group string) int {
	r := rect{x, y, w, 30}
	fill, ink := T.CardHi, T.Muted
	if on {
		fill, ink = blend(T.Accent, T.Card, 0.45), T.Text
	} else if t.v.hover == id {
		ink = T.Text
	}
	fillRound(cv, r, 15, fill)
	drawText(cv, label, uiFont(9), ink, r, taCenter)
	if group != "" {
		t.v.addItem(r, id, group+": "+label, ctlRadioButton, on)
	} else {
		t.v.addItem(r, id, label, ctlCheckBox, on)
	}
	return w + 8
}

func (t *settingsTab) section(cv *walk.Canvas, y int, w int, title string, height int) int {
	r := rect{24, y, w - 48, height}
	fillRound(cv, r, 12, T.Card)
	strokeRound(cv, r, 12, T.Stroke)
	drawText(cv, title, uiBold(11), T.Text, rect{r.X + 20, r.Y + 14, 400, 22}, taLeft)
	t.v.addText(rect{r.X + 20, r.Y + 14, 400, 22}, title)
	return y + height + 14
}

// note draws explanatory text and exposes it to assistive technology.
func (t *settingsTab) note(cv *walk.Canvas, text string, f *walk.Font, ink walk.Color, r rect) {
	drawText(cv, text, f, ink, r, taLeft)
	t.v.addText(r, text)
}

func (t *settingsTab) paint(cv *walk.Canvas, w, h int) {
	cfg := t.app.cfg
	t.viewH = h
	y := 12 - t.scroll

	// ---- sampling ----
	top := y
	y = t.section(cv, y, w, "Sampling", 118)
	x := 44
	drawText(cv, "Refresh every", uiFont(10), T.Muted, rect{x, top + 50, 130, 30}, taLeft)
	cx := x + 140
	for _, c := range refreshChoices {
		cx += t.chip(cv, cx, top+50, 64, c.label, cfg.RefreshMs == c.ms, fmt.Sprintf("refresh:%d", c.ms), "Refresh every")
	}
	drawText(cv, "Graph history", uiFont(10), T.Muted, rect{x, top + 82, 130, 30}, taLeft)
	cx = x + 140
	for _, c := range historyChoices {
		cx += t.chip(cv, cx, top+82, 72, c.label, cfg.HistorySeconds == c.sec, fmt.Sprintf("history:%d", c.sec), "Graph history")
	}

	// ---- processes ----
	top = y
	defs := colDefs()
	perRow := (w - 48 - 60) / 124
	if perRow < 1 {
		perRow = 1
	}
	rows := (len(defs) + perRow - 1) / perRow
	y = t.section(cv, y, w, "Processes", 124+rows*38)
	t.chip(cv, 44, top+48, 230, "Hide “System Idle Process”", cfg.HideSystemIdle, "hideidle", "")
	drawText(cv, "Columns", uiFont(10), T.Muted, rect{44, top + 88, 100, 24}, taLeft)
	want := map[string]bool{"name": true}
	for _, k := range cfg.Columns {
		want[k] = true
	}
	for i, d := range defs {
		cx := 44 + (i%perRow)*124
		cy := top + 116 + (i/perRow)*38
		id := "col:" + d.key
		if d.key == "name" {
			id = "" // always on
		}
		r := rect{cx, cy, 116, 30}
		fill, ink := T.CardHi, T.Muted
		if want[d.key] {
			fill, ink = blend(T.Accent, T.Card, 0.45), T.Text
		}
		fillRound(cv, r, 15, fill)
		drawText(cv, d.title, uiFont(9), ink, r, taCenter)
		if id != "" {
			t.v.addItem(r, id, "Column "+d.title, ctlCheckBox, want[d.key])
		} else {
			t.v.addText(r, "Column "+d.title+" (always shown)")
		}
	}

	// ---- alerts ----
	top = y
	y = t.section(cv, y, w, "Alerts — banner in the header and a tray notification", 62+len(cfg.Alerts)*44)
	for i, al := range cfg.Alerts {
		ry := top + 50 + i*44
		t.chip(cv, 44, ry, 150, metricName(al.Metric)+" above", al.Enabled, fmt.Sprintf("al:%d:en", i), "")
		t.stepper(cv, 214, ry, fmt.Sprintf("%.0f%%", al.Above), fmt.Sprintf("al:%d:above", i), metricName(al.Metric)+" alert threshold")
		drawText(cv, "for", uiFont(10), T.Muted, rect{356, ry, 30, 30}, taCenter)
		t.stepper(cv, 394, ry, fmt.Sprintf("%d s", al.ForSeconds), fmt.Sprintf("al:%d:for", i), metricName(al.Metric)+" alert duration")
	}

	// ---- recorder ----
	top = y
	y = t.section(cv, y, w, "Flight recorder", 96)
	t.chip(cv, 44, top+48, 230, "Keep recording on disk", cfg.RecorderDisk, "recdisk", "")
	t.note(cv, "Off by default: samples (including process names) stay in memory only.",
		uiFont(9), T.Muted, rect{290, top + 48, w - 340, 30})

	// ---- AMD Ryzen sensors ----
	top = y
	y = t.section(cv, y, w, "AMD Ryzen sensors", 124)
	rz := t.app.sensors.Ryzen()
	t.chip(cv, 44, top+48, 230, "Use AMD Monitoring SDK", t.app.ryzenOn, "ryzensdk", "")
	t.note(cv, "Off by default. Needs administrator rights and AMD's SDK; gotm only reads, it never changes CPU settings.",
		uiFont(9), T.Muted, rect{290, top + 48, w - 340, 30})
	status, ink := "Off", T.Faint
	if t.app.ryzenOn && rz == nil {
		status = "On for this PC; it runs when gotm is started as administrator."
	}
	if rz != nil {
		st, detail := rz.State()
		status = detail
		switch st {
		case metrics.RyzenRunning:
			cpu, pkg := rz.Identity()
			status = "Running: " + cpu
			if pkg != "" {
				status += "  ·  " + pkg
			}
			ink = T.Good
		case metrics.RyzenUnavailable:
			ink = T.Warn
		}
	}
	t.note(cv, status, uiFont(9), ink, rect{44, top + 84, w - 88, 30})

	// ---- keyboard ----
	top = y
	y = t.section(cv, y, w, "Keyboard shortcuts", 52+len(shortcutHelp)*24)
	for i, k := range shortcutHelp {
		ky := top + 48 + i*24
		drawText(cv, k[0], uiBold(9), T.Text, rect{44, ky, 220, 22}, taLeft)
		drawText(cv, k[1], uiFont(9), T.Muted, rect{270, ky, w - 330, 22}, taLeft)
		t.v.addText(rect{44, ky, w - 104, 22}, k[0]+": "+k[1])
	}

	// ---- actions ----
	ax := 24
	btn := func(label string, id string, c walk.Color) {
		r := rect{ax, y + 4, 190, 36}
		fill := T.Card
		if t.v.hover == id {
			fill = T.CardHi
		}
		fillRound(cv, r, 8, fill)
		strokeRound(cv, r, 8, T.Stroke)
		drawText(cv, label, uiFont(10), c, r, taCenter)
		t.v.addItem(r, id, label, ctlButton, false)
		ax += 202
	}
	if !t.app.host.Elevated {
		btn("Run as administrator", "elevate", T.Text)
	}
	btn("Open settings folder", "folder", T.Text)
	btn("Quit gotm", "quit", T.Bad)

	t.note(cv, fmt.Sprintf("gotm %s  ·  %s", t.app.version, strings.TrimSpace(t.app.store.File("config.json"))),
		uiFont(8), T.Faint, rect{24, y + 52, w - 48, 20})

	t.content = y + t.scroll + 52 + 20 + 16
	t.scrollBy(0)
	if t.content > h {
		track := h - 8
		thumb := track * h / t.content
		if thumb < 28 {
			thumb = 28
		}
		off := 0
		if span := t.content - h; span > 0 {
			off = (track - thumb) * t.scroll / span
		}
		fillRound(cv, rect{w - 10, 4 + off, 5, thumb}, 3, T.Stroke)
	}
}

// stepper draws "−  value  +"; what names the value for assistive technology.
func (t *settingsTab) stepper(cv *walk.Canvas, x, y int, value, id, what string) {
	minus := rect{x, y, 30, 30}
	plus := rect{x + 100, y, 30, 30}
	for _, b := range []struct {
		r    rect
		s    string
		i    string
		name string
	}{{minus, "−", id + ":-", "Lower " + what + " (" + value + ")"}, {plus, "+", id + ":+", "Raise " + what + " (" + value + ")"}} {
		fill := T.CardHi
		if t.v.hover == b.i {
			fill = T.CardSel
		}
		fillRound(cv, b.r, 8, fill)
		drawText(cv, b.s, uiBold(12), T.Text, b.r, taCenter)
		t.v.addItem(b.r, b.i, b.name, ctlButton, false)
	}
	drawText(cv, value, uiFont(10), T.Text, rect{x + 30, y, 70, 30}, taCenter)
	t.v.addText(rect{x + 30, y, 70, 30}, what+": "+value)
}

func (t *settingsTab) onClick(id string, _ walk.MouseButton) {
	a := t.app
	var n, i int
	var kind, step string
	switch {
	case strings.HasPrefix(id, "refresh:"):
		fmt.Sscanf(id, "refresh:%d", &n)
		a.saveConfig(func(c *configT) { c.RefreshMs = n })
	case strings.HasPrefix(id, "history:"):
		fmt.Sscanf(id, "history:%d", &n)
		a.saveConfig(func(c *configT) { c.HistorySeconds = n })
	case id == "hideidle":
		a.saveConfig(func(c *configT) { c.HideSystemIdle = !c.HideSystemIdle })
		a.procs.refresh()
	case strings.HasPrefix(id, "col:"):
		key := strings.TrimPrefix(id, "col:")
		a.saveConfig(func(c *configT) {
			on := false
			var keep []string
			for _, k := range c.Columns {
				if k == key {
					on = true
					continue
				}
				keep = append(keep, k)
			}
			if !on {
				// keep the canonical column order so the table does not shuffle
				keep = append(keep, key)
				ordered := []string{}
				for _, d := range colDefs() {
					for _, k := range keep {
						if k == d.key {
							ordered = append(ordered, k)
						}
					}
				}
				keep = ordered
			}
			c.Columns = keep
		})
		a.procs.applyColumns()
	case strings.HasPrefix(id, "al:"):
		parts := strings.Split(id, ":")
		if len(parts) >= 3 {
			fmt.Sscanf(parts[1], "%d", &i)
			kind = parts[2]
			if len(parts) == 4 {
				step = parts[3]
			}
			a.saveConfig(func(c *configT) {
				if i < 0 || i >= len(c.Alerts) {
					return
				}
				al := &c.Alerts[i]
				d := 1
				if step == "-" {
					d = -1
				}
				switch kind {
				case "en":
					al.Enabled = !al.Enabled
				case "above":
					al.Above = clampF(al.Above+float64(5*d), 5, 100)
				case "for":
					al.ForSeconds = clampI(al.ForSeconds+5*d, 5, 3600)
				}
			})
		}
	case id == "recdisk":
		v := !a.rec.Persist()
		if a.saveConfig(func(c *configT) { c.RecorderDisk = v }) {
			a.rec.SetPersist(v)
		}
	case id == "ryzensdk":
		if !a.host.Elevated {
			walk.MsgBox(a.mw, "AMD Ryzen sensors",
				"Turning the AMD Monitoring SDK on or off is a machine-wide setting.\n\nUse “Run as administrator” first, then change it here.",
				walk.MsgBoxIconInformation)
			return
		}
		v := !a.ryzenOn
		if err := metrics.SetRyzenSDKEnabled(v); err != nil {
			a.errBox("AMD Ryzen sensors", err)
			return
		}
		a.ryzenOn = v
		a.setRyzenSDK(v)
	case id == "elevate":
		a.elevate()
	case id == "folder":
		openInExplorer(config.Dir())
	case id == "quit":
		a.mw.Close()
	}
	t.v.invalidate()
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
