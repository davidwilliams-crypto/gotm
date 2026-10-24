//go:build windows

package ui

import (
	"fmt"
	"strings"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

// startupTab lists what runs at sign-in; the tick box enables or disables it.
type startupTab struct {
	*gridPage
	items   map[string]metrics.StartupItem // by ID
	all     []metrics.StartupItem
	loading bool
	showMS  *button
	bRefr   *button
}

func newStartupTab(a *App) *startupTab {
	g := newGridPage(a, pgStartup, []decl.TableViewColumn{
		col("Name", 270), col("Source", 210), col("Command", 620),
	})
	t := &startupTab{gridPage: g, items: map[string]metrics.StartupItem{}}
	g.checkBoxes = true
	g.model.keyCol = 3
	g.filterPlaceholder = "Search startup entries"
	g.onFilter = t.apply

	t.bRefr = newButton("Refresh", "", 110, t.load)
	t.showMS = newButton("Microsoft tasks", "", 150, func() { t.showMS.set(true, !t.showMS.active); t.apply() })
	t.showMS.toggle = true
	g.left = []decl.Widget{t.bRefr.widget(), t.showMS.widget()}

	g.model.isChecked = func(row int) bool {
		it, ok := t.items[cell(g.model.rows[row], 3)]
		return ok && it.Enabled
	}
	g.model.setCheck = func(row int, v bool) error {
		id := cell(g.model.rows[row], 3)
		if strings.HasPrefix(id, "task|") {
			// schtasks takes up to a couple of seconds: run it in the background
			go func() {
				err := metrics.SetStartupEnabled(id, v)
				t.app.mw.Synchronize(func() {
					if err != nil {
						t.app.errBox("Startup", err)
					}
					t.load()
				})
			}()
			return nil
		}
		if err := metrics.SetStartupEnabled(id, v); err != nil {
			t.app.errBox("Startup", err)
			return nil // leave the box as it was
		}
		it := t.items[id]
		it.Enabled = v
		t.items[id] = it
		for i := range t.all {
			if t.all[i].ID == id {
				t.all[i].Enabled = v
			}
		}
		return nil
	}
	g.model.tint = func(row, col int, text string) (walk.Color, bool) {
		if it, ok := t.items[cell(g.model.rows[row], 3)]; ok && !it.Enabled {
			return T.Faint, true
		}
		return T.Text, false
	}
	return t
}

func (t *startupTab) build() decl.Widget     { return t.gridPage.build() }
func (t *startupTab) init()                  { t.gridPage.init() }
func (t *startupTab) shown()                 { t.load() }
func (t *startupTab) tick(*metrics.Snapshot) {}

// load gathers startup entries off the UI thread (scheduled tasks take a moment).
func (t *startupTab) load() {
	if t.loading {
		return
	}
	t.loading = true
	t.bRefr.set(false, false)
	t.setStatus("Loading startup entries…")
	go func() {
		items := metrics.ListStartup()
		t.app.mw.Synchronize(func() {
			t.loading = false
			t.bRefr.set(true, false)
			t.all = items
			t.apply()
		})
	}()
}

func (t *startupTab) apply() {
	if t.loading && len(t.all) == 0 {
		return
	}
	q := t.filterText()
	var rows [][]string
	t.items = map[string]metrics.StartupItem{}
	on := 0
	for _, it := range t.all {
		if !t.showMS.active && it.Source == "Scheduled task" && strings.HasPrefix(it.Name, `\Microsoft\`) {
			continue
		}
		if !matchesAny(q, it.Name, it.Source, it.Command) {
			continue
		}
		t.items[it.ID] = it
		if it.Enabled {
			on++
		}
		rows = append(rows, []string{it.Name, it.Source, it.Command, it.ID})
	}
	t.model.set(rows)
	t.setResultStatus(fmt.Sprintf("%d entries · %d enabled · checked = starts at sign-in · machine-wide entries need administrator", len(rows), on))
}

func (t *startupTab) reload() { t.load() }
