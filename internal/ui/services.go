//go:build windows

package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

type servicesTab struct {
	*gridPage
	all      []metrics.Service
	loading  bool
	busy     bool
	lastLoad time.Time

	bStart, bStop, bRestart, bRefr, bRunning *button
}

func newServicesTab(a *App) *servicesTab {
	g := newGridPage(a, pgServices, []decl.TableViewColumn{
		col("Name", 210), col("Display name", 380), col("State", 90), col("Start type", 100), colR("PID", 70),
	})
	t := &servicesTab{gridPage: g}
	g.filterPlaceholder = "Search services"
	g.onFilter = t.apply
	g.onSelect = t.updateButtons

	t.bStart = newButton("Start", "", 94, func() { t.control("start") })
	t.bStop = newButton("Stop", "", 90, func() { t.control("stop") })
	t.bRestart = newButton("Restart", "", 108, func() { t.control("restart") })
	t.bRefr = newButton("Refresh", "", 110, t.load)
	t.bRunning = newButton("Running only", "", 130, func() { t.bRunning.set(true, !t.bRunning.active); t.apply() })
	t.bRunning.toggle = true
	g.left = []decl.Widget{t.bStart.widget(), t.bStop.widget(), t.bRestart.widget(), t.bRefr.widget(), t.bRunning.widget()}

	g.model.keyCol = 0
	g.model.tint = func(row, col int, text string) (walk.Color, bool) {
		if col == 2 {
			switch text {
			case "Running":
				return T.Good, true
			case "Stopped":
				return T.Faint, true
			}
		}
		return T.Text, false
	}
	return t
}

func (t *servicesTab) build() decl.Widget { return t.gridPage.build() }
func (t *servicesTab) reload()            { t.load() }
func (t *servicesTab) init() {
	t.gridPage.init()
	t.updateButtons()
}
func (t *servicesTab) shown() { t.load() }

// tick refreshes the list every few seconds while the page is open.
func (t *servicesTab) tick(*metrics.Snapshot) {
	if time.Since(t.lastLoad) > 5*time.Second {
		t.load()
	}
}

func (t *servicesTab) load() {
	if t.loading {
		return
	}
	t.loading = true
	t.bRefr.set(false, false)
	if len(t.all) == 0 {
		t.setStatus("Reading Windows services…")
	}
	t.lastLoad = time.Now()
	go func() {
		list, err := metrics.ListServices()
		t.app.mw.Synchronize(func() {
			t.loading = false
			t.bRefr.set(true, false)
			if err != nil {
				t.setStatus("Cannot list services: " + err.Error())
				return
			}
			t.all = list
			t.apply()
		})
	}()
}

func (t *servicesTab) apply() {
	if t.loading && len(t.all) == 0 {
		return
	}
	q := t.filterText()
	var rows [][]string
	running := 0
	for _, s := range t.all {
		if s.State == "Running" {
			running++
		}
		if t.bRunning.active && s.State != "Running" {
			continue
		}
		if !matchesAny(q, s.Name, s.Display) {
			continue
		}
		pid := ""
		if s.PID != 0 {
			pid = fmt.Sprint(s.PID)
		}
		rows = append(rows, []string{s.Name, s.Display, s.State, s.StartType, pid})
	}
	if !t.model.update(rows) {
		t.model.set(rows)
	}
	t.setResultStatus(fmt.Sprintf("%d of %d services · %d running", len(rows), len(t.all), running))
	t.updateButtons()
}

func (t *servicesTab) selectedService() (name, state string) {
	if r := t.model.selectedRow(); r != nil {
		return r[0], r[2]
	}
	return "", ""
}

func (t *servicesTab) updateButtons() {
	name, state := t.selectedService()
	t.bStart.set(!t.busy && name != "" && state == "Stopped", false)
	t.bStop.set(!t.busy && name != "" && state == "Running", false)
	t.bRestart.set(!t.busy && name != "" && state == "Running", false)
}

func (t *servicesTab) control(action string) {
	name, _ := t.selectedService()
	if name == "" || t.busy {
		return
	}
	if action != "start" && !t.app.confirm("Services",
		fmt.Sprintf("%s service “%s”?", strings.ToUpper(action[:1])+action[1:], name)) {
		return
	}
	t.busy = true
	t.updateButtons()
	t.setStatus(fmt.Sprintf("Applying %s to %s…", action, name))
	go func() {
		err := metrics.ServiceControl(name, action)
		t.app.mw.Synchronize(func() {
			t.busy = false
			t.updateButtons()
			if err != nil {
				t.app.errBox("Services", err)
			}
			time.AfterFunc(600*time.Millisecond, func() { t.app.mw.Synchronize(t.load) })
		})
	}()
}
