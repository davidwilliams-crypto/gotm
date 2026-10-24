//go:build windows

package ui

import (
	"fmt"
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

type configT = config.Config

// colDef describes one process-table column.
type colDef struct {
	key     string
	title   string
	width   int
	numeric bool
	text    func(r *metrics.Row) string
	less    func(a, b *metrics.Row) bool
	heat    func(r *metrics.Row, total uint64) float64 // 0..1 background intensity
}

func colDefs() []colDef {
	pct := func(v float64) string { return fmtPct(v) }
	return []colDef{
		{key: "name", title: "Name", width: 300,
			text: func(r *metrics.Row) string { return r.Name },
			less: func(a, b *metrics.Row) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) }},
		{key: "pid", title: "PID", width: 70, numeric: true,
			text: func(r *metrics.Row) string {
				if r.Count > 1 {
					return fmt.Sprintf("×%d", r.Count)
				}
				return fmt.Sprint(r.PID)
			},
			less: func(a, b *metrics.Row) bool { return a.PID < b.PID }},
		{key: "ppid", title: "Parent", width: 70, numeric: true,
			text: func(r *metrics.Row) string {
				if r.Count > 1 {
					return ""
				}
				return fmt.Sprint(r.PPID)
			},
			less: func(a, b *metrics.Row) bool { return a.PPID < b.PPID }},
		{key: "cpu", title: "CPU", width: 76, numeric: true,
			text: func(r *metrics.Row) string { return pct(r.CPU) },
			less: func(a, b *metrics.Row) bool { return a.CPU < b.CPU },
			heat: func(r *metrics.Row, _ uint64) float64 { return r.CPU / 30 }},
		{key: "mem", title: "Memory", width: 96, numeric: true,
			text: func(r *metrics.Row) string { return fmtBytes(r.Mem) },
			less: func(a, b *metrics.Row) bool { return a.Mem < b.Mem },
			heat: func(r *metrics.Row, total uint64) float64 {
				if total == 0 {
					return 0
				}
				return float64(r.Mem) / float64(total) * 10
			}},
		{key: "private", title: "Private", width: 96, numeric: true,
			text: func(r *metrics.Row) string { return fmtBytes(r.Private) },
			less: func(a, b *metrics.Row) bool { return a.Private < b.Private }},
		{key: "io", title: "I/O", width: 100, numeric: true,
			text: func(r *metrics.Row) string {
				if r.IO == 0 {
					return "0"
				}
				return fmtRate(r.IO)
			},
			less: func(a, b *metrics.Row) bool { return a.IO < b.IO },
			heat: func(r *metrics.Row, _ uint64) float64 { return r.IO / 40e6 }},
		{key: "gpu", title: "GPU", width: 70, numeric: true,
			text: func(r *metrics.Row) string { return pct(r.GPU) },
			less: func(a, b *metrics.Row) bool { return a.GPU < b.GPU },
			heat: func(r *metrics.Row, _ uint64) float64 { return r.GPU / 40 }},
		{key: "threads", title: "Threads", width: 70, numeric: true,
			text: func(r *metrics.Row) string { return fmt.Sprint(r.Threads) },
			less: func(a, b *metrics.Row) bool { return a.Threads < b.Threads }},
		{key: "handles", title: "Handles", width: 76, numeric: true,
			text: func(r *metrics.Row) string { return fmt.Sprint(r.Handles) },
			less: func(a, b *metrics.Row) bool { return a.Handles < b.Handles }},
		{key: "prio", title: "Priority", width: 100,
			text: func(r *metrics.Row) string { return r.Prio },
			less: func(a, b *metrics.Row) bool { return a.Prio < b.Prio }},
		{key: "user", title: "User", width: 170,
			text: func(r *metrics.Row) string { return r.User },
			less: func(a, b *metrics.Row) bool { return strings.ToLower(a.User) < strings.ToLower(b.User) }},
		{key: "session", title: "Session", width: 66, numeric: true,
			text: func(r *metrics.Row) string { return fmt.Sprint(r.Session) },
			less: func(a, b *metrics.Row) bool { return a.Session < b.Session }},
		{key: "start", title: "Started", width: 150,
			text: func(r *metrics.Row) string {
				if r.Start == 0 {
					return ""
				}
				return time.UnixMilli(r.Start).Format("2006-01-02 15:04:05")
			},
			less: func(a, b *metrics.Row) bool { return a.Start < b.Start }},
		{key: "path", title: "Image path", width: 380,
			text: func(r *metrics.Row) string { return r.Path },
			less: func(a, b *metrics.Row) bool { return strings.ToLower(a.Path) < strings.ToLower(b.Path) }},
	}
}

// ---- table model -----------------------------------------------------------

type procModel struct {
	walk.TableModelBase
	walk.SorterBase

	tab       *procsTab
	rows      []metrics.Row
	cols      []colDef
	sortCol   int
	sortOrder walk.SortOrder
	ready     bool // false while the table is still attaching the model
	totalMem  uint64
	layout    metrics.Layout
}

func (m *procModel) RowCount() int { return len(m.rows) }

func (m *procModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.rows) {
		return ""
	}
	r := &m.rows[row]
	s := m.cols[col].text(r)
	if m.cols[col].key == "name" && m.layout == metrics.LayoutTree {
		glyph := "   "
		switch {
		case r.Collapsed:
			glyph = "▶ "
		case r.HasKids:
			glyph = "▼ "
		}
		s = glyph + s // the row itself is indented natively (indentRows)
		if r.Collapsed {
			s += fmt.Sprintf("   (+%d)", r.Desc)
		}
	}
	return s
}

// Image gives each row its program's icon. Walk resolves a file path through
// the shell's system image list (cached per path), so this costs nothing per
// refresh. Processes whose image path is hidden (System, protected processes
// when not elevated) get the generic application icon.
func (m *procModel) Image(row int) interface{} {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	if p := m.rows[row].Path; p != "" {
		return p
	}
	return genericExeIcon
}

var genericExeIcon = func() string {
	dir, _ := windows.GetSystemDirectory()
	return filepath.Join(dir, "svchost.exe")
}()

// ID lets the table identify rows across rebuilds.
func (m *procModel) ID(row int) interface{} {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	return m.rows[row].Key
}

func (m *procModel) ColumnSortable(col int) bool { return true }

// Sort is called by the table when a header is clicked.
func (m *procModel) Sort(col int, order walk.SortOrder) error {
	if !m.ready {
		return nil // the table's own initial Sort(0, ascending) must not win
	}
	if col != m.sortCol && m.cols[col].numeric {
		order = walk.SortDescending // biggest first is what you want for numbers
	}
	m.sortCol, m.sortOrder = col, order
	m.tab.app.cfg.SortKey = m.cols[col].key
	m.tab.app.cfg.SortDesc = order == walk.SortDescending
	m.tab.refresh()
	return m.SorterBase.Sort(col, order)
}

func (m *procModel) less() func(a, b *metrics.Row) bool {
	c := m.cols[m.sortCol]
	desc := m.sortOrder == walk.SortDescending
	return func(a, b *metrics.Row) bool {
		if c.less(a, b) {
			return !desc
		}
		if c.less(b, a) {
			return desc
		}
		return a.PID < b.PID
	}
}

// StyleCell paints the dark zebra rows, the selection, and heat tints.
func (m *procModel) StyleCell(st *walk.CellStyle) {
	row, col := st.Row(), st.Col()
	if row < 0 || row >= len(m.rows) || col < 0 || col >= len(m.cols) {
		return
	}
	bg, fg := rowStyle(row, m.tab.tv.CurrentIndex())
	if row != m.tab.tv.CurrentIndex() {
		if c := m.cols[col]; c.heat != nil {
			if a := c.heat(&m.rows[row], m.totalMem); a > 0.03 {
				if a > 1 {
					a = 1
				}
				bg = blend(T.Warn, bg, 0.05+0.30*a)
			}
		}
	}
	if m.cols[col].key == "cpu" && m.rows[row].CPU >= 50 && row != m.tab.tv.CurrentIndex() {
		fg = T.Warn
	}
	if m.rows[row].Collapsed && m.cols[col].numeric {
		fg = T.Muted
	}
	st.BackgroundColor, st.TextColor = bg, fg
}

// ---- tab -------------------------------------------------------------------

type procsTab struct {
	app   *App
	model *procModel
	tv    *walk.TableView
	root  *walk.Composite

	filter *walk.LineEdit
	count  *walk.Label

	bKill, bTree, bAff, bSusp, bRes, bLoc *button
	bPrio                                 *button
	bExpand, bFold, bTreeView, bGroup     *button

	collapsed map[uint32]bool
	selKey    string // key of the selected row ("" = none)
	updating  bool   // suppress selection events caused by our own refresh
	lastCount string
}

var priorityNames = []string{"Idle", "BelowNormal", "Normal", "AboveNormal", "High", "Realtime"}

func newProcsTab(a *App) *procsTab {
	t := &procsTab{app: a, collapsed: map[uint32]bool{}}
	t.model = &procModel{tab: t, cols: colDefs()}
	for i, c := range t.model.cols {
		if c.key == a.cfg.SortKey {
			t.model.sortCol = i
		}
	}
	if a.cfg.SortDesc {
		t.model.sortOrder = walk.SortDescending
	}
	return t
}

func (t *procsTab) build() decl.Widget {
	cols := make([]decl.TableViewColumn, len(t.model.cols))
	for i, c := range t.model.cols {
		al := decl.AlignDefault
		if c.numeric {
			al = decl.AlignFar
		}
		cols[i] = decl.TableViewColumn{Name: c.key, Title: c.title, Width: c.width, Alignment: al}
	}
	t.bKill = newButton("End task", "", 96, t.endTask)
	t.bKill.tip = "End the selected process (Delete)"
	t.bKill.danger = true
	t.bTree = newButton("End tree", "", 96, t.endTree)
	t.bTree.tip = "End the selected process and everything it started"
	t.bTree.danger = true
	t.bSusp = newButton("Suspend", "", 96, t.suspend)
	t.bSusp.tip = "Pause every thread of the selected process"
	t.bRes = newButton("Resume", "", 94, func() { t.eachPID("Resume", metrics.Resume) })
	t.bAff = newButton("", "", 42, t.affinity)
	t.bAff.tip = "CPU affinity…"
	t.bLoc = newButton("", "", 42, t.locate)
	t.bLoc.tip = "Open file location"
	t.bPrio = newButton("Priority", "", 104, t.priorityMenu)
	t.bPrio.glyphRight = true
	t.bPrio.tip = "Change the selected process's priority"
	t.bTreeView = newButton("Tree", "", 80, func() { t.setLayout(!t.app.cfg.TreeView, false) })
	t.bTreeView.tip = "Show processes under the process that started them"
	t.bTreeView.toggle = true
	t.bGroup = newButton("Group", "", 90, func() { t.setLayout(false, !t.app.cfg.GroupByName) })
	t.bGroup.tip = "Combine processes with the same name"
	t.bGroup.toggle = true
	t.bExpand = newButton("Expand all", "", 104, t.expandAll)
	t.bFold = newButton("Collapse all", "", 112, t.collapseAll)

	return decl.Composite{
		AssignTo:      &t.root,
		StretchFactor: 1,
		Background:    decl.SolidColorBrush{Color: T.Bg},
		Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			t.app.heads[pgProcesses].declare(0, 76, 0, 76, 0),
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
						Children: []decl.Widget{
							t.bKill.widget(), t.bTree.widget(), t.bSusp.widget(), t.bRes.widget(),
							t.bAff.widget(), t.bLoc.widget(),
							t.bPrio.widget(),
							decl.HSpacer{},
						},
					},
					decl.Composite{
						Background: decl.SolidColorBrush{Color: T.Bg},
						Layout:     decl.HBox{MarginsZero: true, Spacing: 8},
						MinSize:    decl.Size{Height: 38}, MaxSize: decl.Size{Height: 38},
						Children: []decl.Widget{
							decl.LineEdit{AssignTo: &t.filter, CueBanner: "Search name, PID, user, path",
								MinSize: decl.Size{Width: 260}, MaxSize: decl.Size{Width: 260},
								Accessibility: decl.Accessibility{Name: "Search processes", Accelerator: "Ctrl+F"},
								Background:    decl.SolidColorBrush{Color: T.Card}, TextColor: T.Text,
								OnTextChanged: func() { t.refresh() }},
							decl.HSpacer{},
							t.bExpand.widget(), t.bFold.widget(), t.bTreeView.widget(), t.bGroup.widget(),
						},
					},
					decl.TableView{
						AssignTo:              &t.tv,
						StretchFactor:         bodyStretch,
						Model:                 t.model,
						Columns:               cols,
						ColumnsOrderable:      true,
						ContextMenuItems:      t.contextMenu(),
						OnItemActivated:       t.toggleSelected,
						OnKeyDown:             t.onKeyDown,
						OnCurrentIndexChanged: t.onSelectionChanged,
					},
					decl.Label{AssignTo: &t.count, Text: "", TextColor: T.Muted},
				},
			},
			t.app.foots[pgProcesses].declare(0, 32, 0, 32, 0),
		},
	}
}

func (t *procsTab) contextMenu() []decl.MenuItem {
	return []decl.MenuItem{
		decl.Action{Text: "End task", OnTriggered: t.endTask},
		decl.Action{Text: "End process tree", OnTriggered: t.endTree},
		decl.Separator{},
		decl.Action{Text: "Suspend", OnTriggered: t.suspend},
		decl.Action{Text: "Resume", OnTriggered: func() { t.eachPID("Resume", metrics.Resume) }},
		decl.Action{Text: "Set affinity…", OnTriggered: t.affinity},
		decl.Menu{Text: "Set priority", Items: t.priorityItems()},
		decl.Separator{},
		decl.Action{Text: "Open file location", OnTriggered: t.locate},
		decl.Action{Text: "Copy image path", OnTriggered: t.copyPath},
		decl.Separator{},
		decl.Action{Text: "Expand / collapse branch", OnTriggered: t.toggleSelected},
	}
}

func (t *procsTab) init() {
	t.app.registerRoot(pgProcesses, t.root)
	t.tv.SetRestoringCurrentItemOnReset(false) // selection is restored by refresh()
	_ = t.tv.SetModel(t.model)
	styleTable(t.tv)
	nameTable(t.tv, "Processes table")
	searchCue(t.filter, "Search name, PID, user, path   (Ctrl+F)", t.tv)
	t.bExpand.v.cw.SetVisible(false) // only shown in tree layout; keeps the minimum width small
	t.bFold.v.cw.SetVisible(false)
	t.applyColumns()
	indentRows(t.tv, func(row int) int {
		if t.model.layout == metrics.LayoutTree && row >= 0 && row < len(t.model.rows) {
			return t.model.rows[row].Depth
		}
		return 0
	})
	t.model.ready = true
	_ = t.model.SorterBase.Sort(t.model.sortCol, t.model.sortOrder) // shows the sort arrow
	t.syncToggles()
}

func (t *procsTab) searchBox() *walk.LineEdit { return t.filter }
func (t *procsTab) focusTarget() walk.Window  { return t.tv }
func (t *procsTab) reload()                   { t.refresh() }

func (t *procsTab) shown()                 { t.refresh() }
func (t *procsTab) tick(*metrics.Snapshot) { t.refresh() }

func (t *procsTab) syncToggles() {
	t.bTreeView.set(true, t.app.cfg.TreeView)
	t.bGroup.set(true, t.app.cfg.GroupByName)
}

// applyColumns shows exactly the columns chosen in Settings (Name is always on).
func (t *procsTab) applyColumns() {
	want := map[string]bool{"name": true}
	for _, k := range t.app.cfg.Columns {
		want[k] = true
	}
	cols := t.tv.Columns()
	for i, c := range t.model.cols {
		_ = cols.At(i).SetVisible(want[c.key])
	}
}

func (t *procsTab) layout() metrics.Layout {
	switch {
	case t.app.cfg.TreeView:
		return metrics.LayoutTree
	case t.app.cfg.GroupByName:
		return metrics.LayoutGroup
	}
	return metrics.LayoutFlat
}

// refresh rebuilds the rows from the newest snapshot and repaints only what
// changed. Same rows in the same order is the common case and costs one
// repaint of the visible rows; a reorder resets the model.
func (t *procsTab) refresh() {
	s := t.app.snap
	if s == nil || t.tv == nil || t.model.cols == nil || !t.model.ready {
		return
	}
	m := t.model
	m.layout = t.layout()
	m.totalMem = t.app.host.TotalMem
	rows := metrics.BuildRows(s.Procs, metrics.View{
		Layout: m.layout, Filter: t.filter.Text(), HideIdle: t.app.cfg.HideSystemIdle,
		Collapsed: t.collapsed, Less: m.less(),
	})

	oldIdx := t.indexOf(m.rows, t.selKey)
	wasVisible := oldIdx >= 0 && t.tv.ItemVisible(oldIdx)

	same := len(rows) == len(m.rows)
	if same {
		for i := range rows {
			if rows[i].Key != m.rows[i].Key || rows[i].Collapsed != m.rows[i].Collapsed {
				same = false
				break
			}
		}
	}

	t.updating = true
	m.rows = rows
	if same {
		redrawVisible(t.tv)
	} else {
		m.PublishRowsReset() // the table clears its selection here…
		redrawVisible(t.tv)
		if newIdx := t.indexOf(rows, t.selKey); newIdx >= 0 && wasVisible {
			_ = t.tv.SetCurrentIndex(newIdx) // …and we put it back, scrolling only if needed
		} else {
			t.selKey = ""
		}
	}
	t.updating = false
	t.updateControls()

	unit := "processes"
	switch m.layout {
	case metrics.LayoutGroup:
		unit = "groups"
	case metrics.LayoutTree:
		unit = "shown"
	}
	if txt := fmt.Sprintf("%d %s", len(rows), unit); txt != t.lastCount {
		t.lastCount = txt
		_ = t.count.SetText(txt)
	}
}

func (t *procsTab) indexOf(rows []metrics.Row, key string) int {
	if key == "" {
		return -1
	}
	for i := range rows {
		if rows[i].Key == key {
			return i
		}
	}
	return -1
}

func (t *procsTab) selected() *metrics.Row {
	if i := t.indexOf(t.model.rows, t.selKey); i >= 0 {
		return &t.model.rows[i]
	}
	return nil
}

func (t *procsTab) onSelectionChanged() {
	if t.updating {
		return
	}
	i := t.tv.CurrentIndex()
	if i >= 0 && i < len(t.model.rows) {
		t.selKey = t.model.rows[i].Key
	} else {
		t.selKey = ""
	}
	t.updateControls()
	redrawVisible(t.tv)
}

func (t *procsTab) updateControls() {
	r := t.selected()
	single := r != nil && r.Count == 1
	canAct := r != nil
	if r != nil {
		for _, pid := range r.PIDs {
			if metrics.IsProtected(pid) {
				canAct = false
				break
			}
		}
	}
	t.bKill.set(canAct, false)
	t.bTree.set(single && canAct, false)
	t.bAff.set(single && canAct, false)
	t.bSusp.set(canAct, false)
	t.bRes.set(canAct, false)
	t.bLoc.set(r != nil && r.Path != "", false)
	t.bPrio.set(single && canAct, false)
	tree := t.app.cfg.TreeView
	if t.bExpand.v.cw != nil && t.bExpand.v.cw.Visible() != tree {
		t.bExpand.v.cw.SetVisible(tree)
		t.bFold.v.cw.SetVisible(tree)
	}
	if single {
		t.bPrio.setText(prioLabel(r.Prio))
	} else {
		t.bPrio.setText("Priority")
	}
}

// ---- layout / tree controls -------------------------------------------------

func (t *procsTab) setLayout(tree, group bool) {
	if tree && group {
		group = false
	}
	t.selKey = ""
	t.app.saveConfig(func(c *configT) { c.TreeView, c.GroupByName = tree, group })
	t.syncToggles()
	t.refresh()
}

func (t *procsTab) toggleSelected() {
	r := t.selected()
	if r == nil || !t.app.cfg.TreeView || !r.HasKids {
		return
	}
	if t.collapsed[r.PID] {
		delete(t.collapsed, r.PID)
	} else {
		t.collapsed[r.PID] = true
	}
	t.refresh()
}

func (t *procsTab) expandAll() {
	t.collapsed = map[uint32]bool{}
	t.refresh()
}

func (t *procsTab) collapseAll() {
	s := t.app.snap
	if s == nil {
		return
	}
	parents := map[uint32]bool{}
	for _, p := range s.Procs {
		parents[p.PPID] = true
	}
	for _, p := range s.Procs {
		if parents[p.PID] {
			t.collapsed[p.PID] = true
		}
	}
	t.refresh()
}

func (t *procsTab) onKeyDown(key walk.Key) {
	r := t.selected()
	switch key {
	case walk.KeyDelete:
		t.endTask()
	case walk.KeyLeft:
		if r != nil && t.app.cfg.TreeView && r.HasKids && !r.Collapsed {
			t.toggleSelected()
		}
	case walk.KeyRight:
		if r != nil && t.app.cfg.TreeView && r.HasKids && r.Collapsed {
			t.toggleSelected()
		}
	case walk.KeyF5:
		t.refresh()
	}
}

// ---- actions ---------------------------------------------------------------

func (t *procsTab) label(r *metrics.Row) string {
	if r.Count > 1 {
		return fmt.Sprintf("all %d “%s” processes", r.Count, r.Name)
	}
	return fmt.Sprintf("“%s” (PID %d)", r.Name, r.PID)
}

func (t *procsTab) endTask() {
	r := t.selected()
	if r == nil {
		return
	}
	if !t.app.confirm("End task", fmt.Sprintf("End %s?\n\nUnsaved data in the process will be lost.", t.label(r))) {
		return
	}
	t.eachPIDOf(r, "End task", metrics.KillProcess)
}

// suspend freezes the selected process(es) after a confirmation that says
// how to undo it; a suspended program looks hung until it is resumed.
func (t *procsTab) suspend() {
	r := t.selected()
	if r == nil {
		return
	}
	if !t.app.confirm("Suspend", fmt.Sprintf("Suspend %s?\n\nIt stops responding until you select it and click Resume.", t.label(r))) {
		return
	}
	t.eachPIDOf(r, "Suspend", metrics.Suspend)
}

func (t *procsTab) endTree() {
	r := t.selected()
	if r == nil || r.Count != 1 || t.app.snap == nil {
		return
	}
	root := r.Targets()[0]
	// The list is computed once and shown; exactly these processes are ended,
	// each re-checked by creation time, never a fresh snapshot after the dialog.
	desc := metrics.Descendants(root, t.app.snap.Procs)
	var names []string
	for i, d := range desc {
		if i == 8 {
			names = append(names, fmt.Sprintf("… and %d more", len(desc)-8))
			break
		}
		names = append(names, fmt.Sprintf("%s (%d)", d.Name, d.PID))
	}
	msg := fmt.Sprintf("End %s and everything it started?\n\nUnsaved data in these processes will be lost.", t.label(r))
	if len(desc) > 0 {
		msg = fmt.Sprintf("End %s and the %d process(es) it started?\n\n%s\n\nUnsaved data in these processes will be lost.",
			t.label(r), len(desc), strings.Join(names, "\n"))
	}
	if !t.app.confirm("End process tree", msg) {
		return
	}
	if err := metrics.KillTree(root, desc); err != nil {
		t.app.errBox("End process tree", err)
	}
}

func (t *procsTab) eachPID(title string, fn func(metrics.Target) error) {
	if r := t.selected(); r != nil {
		t.eachPIDOf(r, title, fn)
	}
}

func (t *procsTab) eachPIDOf(r *metrics.Row, title string, fn func(metrics.Target) error) {
	var errs []string
	for _, tg := range r.Targets() {
		if err := fn(tg); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		walk.MsgBox(t.app.mw, title, strings.Join(dedupe(errs), "\n"), walk.MsgBoxIconWarning)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("… and %d more", len(out)-6))
	}
	return out
}

// priorityMenu drops a menu of priority classes under the Priority button,
// with the process's current class ticked.
func (t *procsTab) priorityMenu() {
	r := t.selected()
	if r == nil || r.Count != 1 {
		return
	}
	var items []menuItem
	for _, n := range priorityNames {
		name := n
		items = append(items, menuItem{text: prioLabel(name), checked: name == r.Prio, do: func() { t.setPriority(name) }})
	}
	popupMenu(t.bPrio.v.cw, items)
}

func (t *procsTab) setPriority(name string) {
	r := t.selected()
	if r == nil || r.Count != 1 || name == r.Prio {
		return
	}
	if name == "Realtime" && !t.app.confirm("Realtime priority",
		"Realtime priority can make Windows unresponsive.\n\nContinue?") {
		return
	}
	if err := metrics.SetPriority(r.Targets()[0], name); err != nil {
		t.app.errBox("Set priority", err)
	}
	t.refresh()
}

// prioLabel spells a priority class the way Task Manager does.
func prioLabel(n string) string {
	switch n {
	case "BelowNormal":
		return "Below normal"
	case "AboveNormal":
		return "Above normal"
	case "Realtime":
		return "Realtime"
	}
	return n
}

func (t *procsTab) locate() {
	if r := t.selected(); r != nil && r.Count >= 1 {
		if err := metrics.OpenFileLocation(r.PIDs[0]); err != nil {
			t.app.errBox("Open file location", err)
		}
	}
}

func (t *procsTab) copyPath() {
	if r := t.selected(); r != nil && r.Path != "" {
		_ = walk.Clipboard().SetText(r.Path)
	}
}

func (t *procsTab) affinity() {
	if r := t.selected(); r != nil && r.Count == 1 {
		showAffinityDialog(t.app, r.Targets()[0])
	}
}

func (t *procsTab) priorityItems() []decl.MenuItem {
	var out []decl.MenuItem
	for _, n := range priorityNames {
		name := n
		out = append(out, decl.Action{Text: prioLabel(name), OnTriggered: func() { t.setPriority(name) }})
	}
	return out
}
