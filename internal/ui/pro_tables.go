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

// =============================================================================
// Power & Freq: a collapsible sensor tree with live, minimum and maximum values.
// =============================================================================

type powerPage struct {
	*gridPage
	collapsed map[string]bool
	rows      []metrics.SensorRow
	kinds     map[string]string // id -> kind, for colouring
	bExpand   *button
	bFold     *button
}

func newPowerPage(a *App) *powerPage {
	g := newGridPage(a, pgPower, []decl.TableViewColumn{
		col("Sensor", 460), colR("Value", 130), colR("Min", 130), colR("Max", 130),
	})
	// per-thread utilisation is long and also on the Performance page: start folded
	p := &powerPage{gridPage: g, collapsed: map[string]bool{"cpu.load": true}, kinds: map[string]string{}}
	g.noSort = true
	g.filterPlaceholder = "Search sensors"
	g.onFilter = func() { p.render() }
	g.activate = p.toggleSelected
	p.bExpand = newButton("Expand all", "", 108, func() { p.collapsed = map[string]bool{}; p.render() })
	p.bFold = newButton("Collapse all", "", 116, p.collapseAll)
	g.left = []decl.Widget{p.bExpand.widget(), p.bFold.widget()}
	g.model.keyCol = 4
	g.model.tint = func(row, col int, text string) (walk.Color, bool) {
		r := g.model.rows[row]
		kind := cell(r, 5)
		if cell(r, 6) == "g" { // group header
			return T.Text, true
		}
		if col == 0 {
			return T.Muted, kind == "note"
		}
		if col == 1 {
			return sensorColor(kind), true
		}
		return T.Muted, true
	}
	return p
}

func sensorColor(kind string) walk.Color {
	switch kind {
	case "temp":
		return T.Thermal
	case "load":
		return T.CPU
	case "clock":
		return T.Clock
	case "power":
		return T.Power
	case "mem":
		return T.Mem
	case "gpu":
		return T.GPU
	case "disk", "data":
		return T.Disk
	case "net":
		return T.Net
	}
	return T.Text
}

func (p *powerPage) build() decl.Widget { return p.gridPage.build() }
func (p *powerPage) init()              { p.gridPage.init() }
func (p *powerPage) shown()             { p.render() }
func (p *powerPage) reload()            { p.render() }
func (p *powerPage) tick(s *metrics.Snapshot) {
	p.render()
}

func (p *powerPage) collapseAll() {
	for _, r := range p.rows {
		if r.Group {
			p.collapsed[r.ID] = true
		}
	}
	p.render()
}

func (p *powerPage) toggleSelected() {
	if r := p.model.selectedRow(); r != nil && cell(r, 6) == "g" {
		id := cell(r, 4)
		if p.collapsed[id] {
			delete(p.collapsed, id)
		} else {
			p.collapsed[id] = true
		}
		p.render()
	}
}

// render flattens the tree honouring collapsed groups and the filter.
func (p *powerPage) render() {
	if len(p.rows) == 0 {
		return
	}
	q := p.filterText()
	var out [][]string
	hiddenBelow := -1 // depth of the collapsed group currently being skipped
	matchDepth := -1  // a matching group keeps its whole subtree while searching
	for _, r := range p.rows {
		if q == "" && hiddenBelow >= 0 && r.Depth > hiddenBelow {
			continue
		}
		hiddenBelow = -1
		if q != "" {
			if matchDepth >= 0 && r.Depth <= matchDepth {
				matchDepth = -1
			}
			if r.Group && matchDepth < 0 && strings.Contains(strings.ToLower(r.Name), q) {
				matchDepth = r.Depth
			}
			if !r.Group && matchDepth < 0 && !strings.Contains(strings.ToLower(r.Name), q) {
				continue
			}
		}
		name := strings.Repeat("    ", r.Depth) + r.Name
		g := ""
		if r.Group {
			g = "g"
			caret := "▼ "
			if q == "" && p.collapsed[r.ID] {
				caret = "▶ "
				hiddenBelow = r.Depth
			}
			name = strings.Repeat("    ", r.Depth) + caret + r.Name
		} else {
			name = strings.Repeat("    ", r.Depth) + "   " + r.Name
		}
		out = append(out, []string{name, r.Value, r.Min, r.Max, r.ID, r.Kind, g})
	}
	if q != "" { // while filtering, drop group headers that ended up empty
		out = pruneEmptyGroups(out)
	}
	if !p.model.update(out) {
		p.model.set(out)
	}
	p.setResultStatus(fmt.Sprintf("%d rows · double-click a group to fold or unfold it · Min/Max are since gotm started", len(out)))
}

func pruneEmptyGroups(rows [][]string) [][]string {
	// Work from the leaves up so a parent with only empty subgroups is pruned too.
	keep := make([]bool, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if cell(r, 6) == "g" {
			depth := len(cell(r, 0)) - len(strings.TrimLeft(cell(r, 0), " \u00a0"))
			for j := i + 1; j < len(rows); j++ {
				nextDepth := len(cell(rows[j], 0)) - len(strings.TrimLeft(cell(rows[j], 0), " \u00a0"))
				if nextDepth <= depth {
					break
				}
				if keep[j] {
					keep[i] = true
					break
				}
			}
		} else {
			keep[i] = true
		}
	}
	var res [][]string
	for i, r := range rows {
		if keep[i] {
			res = append(res, r)
		}
	}
	return res
}

// =============================================================================
// Connections
// =============================================================================

type connsPage struct {
	*gridPage
	listenOnly *button
	hideLocal  *button
	bRefr      *button
	loading    bool
	last       time.Time
	names      map[uint32]string
	all        []metrics.Connection
}

func newConnsPage(a *App) *connsPage {
	g := newGridPage(a, pgConnections, []decl.TableViewColumn{
		col("Proto", 64), col("Local address", 250), col("Remote address", 250), col("State", 110),
		colR("PID", 66), col("Process", 190), col("Exposure", 100),
	})
	p := &connsPage{gridPage: g, names: map[uint32]string{}}
	g.filterPlaceholder = "Search address, port, process"
	g.onFilter = p.apply
	p.bRefr = newButton("Refresh", "", 110, p.load)
	p.listenOnly = newButton("Listening only", "", 150, func() { p.listenOnly.set(true, !p.listenOnly.active); p.apply() })
	p.listenOnly.toggle = true
	p.hideLocal = newButton("Hide loopback", "", 140, func() { p.hideLocal.set(true, !p.hideLocal.active); p.apply() })
	p.hideLocal.toggle = true
	g.left = []decl.Widget{p.bRefr.widget(), p.listenOnly.widget(), p.hideLocal.widget()}
	g.model.keyCol = 7 // protocol, endpoints and owning PID
	g.model.tint = func(row, col int, text string) (walk.Color, bool) {
		switch {
		case col == 3 && text == "Established":
			return T.Good, true
		case col == 3 && text == "Listen":
			return T.Accent, true
		case col == 6 && text == "Network":
			return T.Warn, true
		case col == 6 && text == "Local only":
			return T.Faint, true
		}
		return T.Text, false
	}
	return p
}

func (p *connsPage) build() decl.Widget { return p.gridPage.build() }
func (p *connsPage) init()              { p.gridPage.init() }
func (p *connsPage) shown()             { p.load() }
func (p *connsPage) tick(s *metrics.Snapshot) {
	p.names = make(map[uint32]string, len(s.Procs))
	for _, pr := range s.Procs {
		p.names[pr.PID] = pr.Name
	}
	if time.Since(p.last) > 2*time.Second {
		p.load()
	}
}

func (p *connsPage) load() {
	if p.loading {
		return
	}
	p.loading = true
	p.bRefr.set(false, false)
	if len(p.all) == 0 {
		p.setStatus("Reading TCP and UDP endpoints…")
	}
	p.last = time.Now()
	go func() {
		conns := metrics.ReadConnections()
		p.app.mw.Synchronize(func() {
			p.loading = false
			p.bRefr.set(true, false)
			p.all = conns
			p.apply()
		})
	}()
}

func (p *connsPage) apply() {
	if p.loading && len(p.all) == 0 {
		return
	}
	q := p.filterText()
	listen, noLoop := p.listenOnly.active, p.hideLocal.active
	var rows [][]string
	established, listening := 0, 0
	for _, c := range p.all {
		if c.State == "Established" {
			established++
		}
		if c.State == "Listen" {
			listening++
		}
		if listen && c.State != "Listen" && c.State != "—" {
			continue
		}
		if noLoop && (c.Local == "127.0.0.1" || c.Local == "::1") &&
			(c.Remote == "" || c.Remote == "127.0.0.1" || c.Remote == "::1") {
			continue
		}
		name := p.names[c.PID]
		if c.PID == 0 {
			name = "System Idle"
		}
		local := metrics.Endpoint(c.Local, c.LocalPort)
		remote := metrics.Endpoint(c.Remote, c.RemotePort)
		if !matchesAny(q, c.Proto, local, remote, c.State, name, fmt.Sprint(c.PID)) {
			continue
		}
		key := fmt.Sprintf("%s|%s|%s|%d", c.Proto, local, remote, c.PID)
		rows = append(rows, []string{c.Proto, local, remote, c.State, fmt.Sprint(c.PID), name, c.Exposure, key})
	}
	if !p.model.update(rows) {
		p.model.set(rows)
	}
	p.setResultStatus(fmt.Sprintf("%d shown · %d established · %d listening · Network = not loopback; firewall rules still apply", len(rows), established, listening))
}

// =============================================================================
// Installed Apps
// =============================================================================

type appsPage struct {
	*gridPage
	all     []metrics.InstalledApp
	loading bool
	loaded  bool
	bRefr   *button
	bLoc    *button
	bUnin   *button
	byKey   map[string]metrics.InstalledApp
}

func newAppsPage(a *App) *appsPage {
	g := newGridPage(a, pgApps, []decl.TableViewColumn{
		col("Name", 340), col("Version", 120), col("Publisher", 230), col("Installed", 100),
		colR("Size", 90), col("Scope", 120),
	})
	p := &appsPage{gridPage: g, byKey: map[string]metrics.InstalledApp{}}
	g.filterPlaceholder = "Search installed apps"
	g.onFilter = p.apply
	g.onSelect = p.updateButtons
	g.model.keyCol = 6
	p.bRefr = newButton("Refresh", "", 110, func() { p.loaded = false; p.load() })
	p.bLoc = newButton("Open folder", "", 124, p.openLocation)
	p.bUnin = newButton("Uninstall…", "", 124, p.uninstall)
	p.bUnin.danger = true
	g.left = []decl.Widget{p.bRefr.widget(), p.bLoc.widget(), p.bUnin.widget()}
	return p
}

func (p *appsPage) build() decl.Widget { return p.gridPage.build() }
func (p *appsPage) init() {
	p.gridPage.init()
	p.updateButtons()
}
func (p *appsPage) shown()                 { p.load() }
func (p *appsPage) tick(*metrics.Snapshot) {}

func (p *appsPage) load() {
	if p.loading || p.loaded {
		return
	}
	p.loading = true
	p.bRefr.set(false, false)
	p.setStatus("Reading installed programs…")
	go func() {
		list := metrics.ReadInstalledApps()
		p.app.mw.Synchronize(func() {
			p.loading, p.loaded = false, true
			p.bRefr.set(true, false)
			p.all = list
			p.apply()
		})
	}()
}

func (p *appsPage) apply() {
	if p.loading && len(p.all) == 0 {
		return
	}
	q := p.filterText()
	var rows [][]string
	p.byKey = map[string]metrics.InstalledApp{}
	var total uint64
	for _, a := range p.all {
		if !matchesAny(q, a.Name, a.Publisher, a.Version) {
			continue
		}
		size := ""
		if a.SizeKB > 0 {
			size = fmtBytes(a.SizeKB * 1024)
			total += a.SizeKB * 1024
		}
		key := fmt.Sprintf("%q|%q|%q|%q|%q", a.Name, a.Version, a.Publisher, a.Scope, a.Uninstall)
		p.byKey[key] = a
		rows = append(rows, []string{a.Name, a.Version, a.Publisher, a.Installed, size, a.Scope, key})
	}
	p.model.set(rows)
	p.setResultStatus(fmt.Sprintf("%d of %d apps · %s reported by installers", len(rows), len(p.all), fmtBytes(total)))
	p.updateButtons()
}

func (p *appsPage) current() (metrics.InstalledApp, bool) {
	if r := p.model.selectedRow(); r != nil {
		a, ok := p.byKey[cell(r, 6)]
		return a, ok
	}
	return metrics.InstalledApp{}, false
}

func (p *appsPage) updateButtons() {
	a, ok := p.current()
	p.bLoc.set(ok && a.Location != "", false)
	p.bUnin.set(ok && a.Uninstall != "", false)
}

func (p *appsPage) openLocation() {
	a, ok := p.current()
	if !ok || a.Location == "" {
		return
	}
	// InstallLocation is only a hint; never hand Explorer something that is not
	// a folder, because Explorer runs a program it is given.
	if !metrics.IsDirectory(a.Location) {
		p.app.errBox("Open folder", fmt.Errorf("the folder recorded for “%s” does not exist:\n%s", a.Name, a.Location))
		return
	}
	openInExplorer(a.Location)
}

func (p *appsPage) uninstall() {
	a, ok := p.current()
	if !ok || a.Uninstall == "" {
		return
	}
	_, line, err := metrics.UninstallCommand(a, p.app.host.Elevated)
	if err != nil {
		p.app.errBox("Uninstall", err)
		return
	}
	if !p.app.confirm("Uninstall", fmt.Sprintf("Run the uninstaller for “%s”?\n\n%s", a.Name, line)) {
		return
	}
	done, err := metrics.StartUninstaller(a, p.app.host.Elevated)
	if err != nil {
		p.app.errBox("Uninstall", err)
		return
	}
	// The uninstaller has its own window; report how it ended and refresh.
	go func() {
		res := <-done
		p.app.mw.Synchronize(func() {
			switch {
			case res == metrics.ErrRestartNeeded:
				walk.MsgBox(p.app.mw, "Uninstall", res.Error(), walk.MsgBoxIconInformation)
			case res != nil:
				p.app.errBox("Uninstall", fmt.Errorf("%s: %w", a.Name, res))
			}
			p.loaded = false
			p.load()
		})
	}()
}

// =============================================================================
// Drivers
// =============================================================================

type driversPage struct {
	*gridPage
	all     []metrics.DriverInfo
	loading bool
	loaded  bool
	running *button
	nonMS   *button
	bRefr   *button
}

func newDriversPage(a *App) *driversPage {
	g := newGridPage(a, pgDrivers, []decl.TableViewColumn{
		col("Name", 190), col("Description", 300), col("State", 80), col("Start", 86), col("Type", 90),
		col("Publisher", 210), col("Version", 150),
	})
	p := &driversPage{gridPage: g}
	g.filterPlaceholder = "Search drivers"
	g.onFilter = p.apply
	g.model.keyCol = 0
	p.bRefr = newButton("Refresh", "", 110, func() { p.loaded = false; p.load() })
	p.running = newButton("Running only", "", 130, func() { p.running.set(true, !p.running.active); p.apply() })
	p.running.toggle = true
	p.nonMS = newButton("Non-Microsoft", "", 136, func() { p.nonMS.set(true, !p.nonMS.active); p.apply() })
	p.nonMS.toggle = true
	g.left = []decl.Widget{p.bRefr.widget(), p.running.widget(), p.nonMS.widget()}
	g.model.tint = func(row, col int, text string) (walk.Color, bool) {
		if col == 2 && text == "Running" {
			return T.Good, true
		}
		if col == 2 {
			return T.Faint, true
		}
		return T.Text, false
	}
	return p
}

func (p *driversPage) build() decl.Widget     { return p.gridPage.build() }
func (p *driversPage) init()                  { p.gridPage.init() }
func (p *driversPage) shown()                 { p.load() }
func (p *driversPage) tick(*metrics.Snapshot) {}

func (p *driversPage) load() {
	if p.loading || p.loaded {
		return
	}
	p.loading = true
	p.bRefr.set(false, false)
	p.setStatus("Reading driver services and file versions…")
	go func() {
		list, err := metrics.ReadDrivers()
		p.app.mw.Synchronize(func() {
			p.loading = false
			p.bRefr.set(true, false)
			if err != nil {
				p.setStatus("Cannot list drivers: " + err.Error())
				return
			}
			p.loaded = true
			p.all = list
			p.apply()
		})
	}()
}

func (p *driversPage) apply() {
	if p.loading && len(p.all) == 0 {
		return
	}
	q := p.filterText()
	var rows [][]string
	running := 0
	for _, d := range p.all {
		if d.State == "Running" {
			running++
		}
		if p.running.active && d.State != "Running" {
			continue
		}
		if p.nonMS.active && (d.Company == "" || strings.Contains(strings.ToLower(d.Company), "microsoft")) {
			continue
		}
		desc := d.Desc
		if desc == "" {
			desc = d.Display
		}
		if !matchesAny(q, d.Name, desc, d.Company, d.Version, d.Path) {
			continue
		}
		rows = append(rows, []string{d.Name, desc, d.State, d.StartType, d.Kind, d.Company, d.Version})
	}
	p.model.set(rows)
	p.setResultStatus(fmt.Sprintf("%d of %d drivers · %d running", len(rows), len(p.all), running))
}

func (p *connsPage) reload()   { p.load() }
func (p *appsPage) reload()    { p.loaded = false; p.load() }
func (p *driversPage) reload() { p.loaded = false; p.load() }
