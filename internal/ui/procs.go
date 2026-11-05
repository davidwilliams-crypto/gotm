//go:build windows

package ui

import (
	"cmp"
	"fmt"
	"hash/maphash"
	"strconv"
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
	cmp     func(a, b *metrics.Row) int                // ascending; ties are broken by PID
	heat    func(r *metrics.Row, total uint64) float64 // 0..1 background intensity
}

func colDefs() []colDef {
	pct := func(v float64) string { return fmtPct(v) }
	return []colDef{
		{key: "name", title: "Name", width: 300,
			text: func(r *metrics.Row) string { return r.Name },
			cmp:  func(a, b *metrics.Row) int { return strings.Compare(a.NameFold(), b.NameFold()) }},
		{key: "pid", title: "PID", width: 70, numeric: true,
			text: func(r *metrics.Row) string {
				if r.Count > 1 {
					return "×" + strconv.Itoa(r.Count)
				}
				return strconv.FormatUint(uint64(r.PID), 10)
			},
			cmp: func(a, b *metrics.Row) int { return cmp.Compare(a.PID, b.PID) }},
		{key: "ppid", title: "Parent", width: 70, numeric: true,
			text: func(r *metrics.Row) string {
				if r.Count > 1 {
					return ""
				}
				return strconv.FormatUint(uint64(r.PPID), 10)
			},
			cmp: func(a, b *metrics.Row) int { return cmp.Compare(a.PPID, b.PPID) }},
		{key: "cpu", title: "CPU", width: 76, numeric: true,
			text: func(r *metrics.Row) string { return pct(r.CPU) },
			cmp:  func(a, b *metrics.Row) int { return cmpFloat(a.CPU, b.CPU) },
			heat: func(r *metrics.Row, _ uint64) float64 { return r.CPU / 30 }},
		{key: "mem", title: "Memory", width: 96, numeric: true,
			text: func(r *metrics.Row) string { return fmtBytes(r.Mem) },
			cmp:  func(a, b *metrics.Row) int { return cmp.Compare(a.Mem, b.Mem) },
			heat: func(r *metrics.Row, total uint64) float64 {
				if total == 0 {
					return 0
				}
				return float64(r.Mem) / float64(total) * 10
			}},
		{key: "private", title: "Private", width: 96, numeric: true,
			text: func(r *metrics.Row) string { return fmtBytes(r.Private) },
			cmp:  func(a, b *metrics.Row) int { return cmp.Compare(a.Private, b.Private) }},
		{key: "io", title: "I/O", width: 100, numeric: true,
			text: func(r *metrics.Row) string {
				if r.IO == 0 {
					return "0"
				}
				return fmtRate(r.IO)
			},
			cmp:  func(a, b *metrics.Row) int { return cmpFloat(a.IO, b.IO) },
			heat: func(r *metrics.Row, _ uint64) float64 { return r.IO / 40e6 }},
		{key: "gpu", title: "GPU", width: 70, numeric: true,
			text: func(r *metrics.Row) string {
				if r.GPUUnavailable {
					return "n/a"
				}
				return pct(r.GPU)
			},
			cmp: func(a, b *metrics.Row) int { return cmpFloat(a.GPU, b.GPU) },
			heat: func(r *metrics.Row, _ uint64) float64 {
				if r.GPUUnavailable {
					return 0
				}
				return r.GPU / 40
			}},
		{key: "threads", title: "Threads", width: 70, numeric: true,
			text: func(r *metrics.Row) string { return strconv.FormatUint(uint64(r.Threads), 10) },
			cmp:  func(a, b *metrics.Row) int { return cmp.Compare(a.Threads, b.Threads) }},
		{key: "handles", title: "Handles", width: 76, numeric: true,
			text: func(r *metrics.Row) string { return strconv.FormatUint(uint64(r.Handles), 10) },
			cmp:  func(a, b *metrics.Row) int { return cmp.Compare(a.Handles, b.Handles) }},
		{key: "prio", title: "Priority", width: 100,
			text: func(r *metrics.Row) string { return r.Prio },
			cmp:  func(a, b *metrics.Row) int { return strings.Compare(a.Prio, b.Prio) }},
		{key: "user", title: "User", width: 170,
			text: func(r *metrics.Row) string { return r.User },
			cmp:  func(a, b *metrics.Row) int { return strings.Compare(a.UserFold(), b.UserFold()) }},
		{key: "session", title: "Session", width: 66, numeric: true,
			text: func(r *metrics.Row) string { return strconv.FormatUint(uint64(r.Session), 10) },
			cmp:  func(a, b *metrics.Row) int { return cmp.Compare(a.Session, b.Session) }},
		{key: "start", title: "Started", width: 150,
			text: func(r *metrics.Row) string {
				if r.Start == 0 {
					return ""
				}
				return startedLabel(time.UnixMilli(r.Start))
			},
			cmp: func(a, b *metrics.Row) int { return cmp.Compare(a.Start, b.Start) }},
		{key: "path", title: "Image path", width: 380,
			text: func(r *metrics.Row) string { return r.Path },
			cmp:  func(a, b *metrics.Row) int { return strings.Compare(a.PathFold(), b.PathFold()) }},
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

func (m *procModel) Value(row, col int) interface{} { return m.text(row, col) }

// text is the displayed text of a cell.
func (m *procModel) text(row, col int) string {
	if row < 0 || row >= len(m.rows) {
		return ""
	}
	r := &m.rows[row]
	s := m.cols[col].text(r)
	if m.cols[col].key == "name" && m.layout == metrics.LayoutTree {
		glyph := "   "
		switch {
		case r.Collapsed:
			glyph = "▶ "
		case r.HasKids:
			glyph = "▼ "
		}
		s = glyph + s // the row itself is indented natively (indentRows)
		if r.Collapsed {
			s += "   (+" + strconv.Itoa(r.Desc) + ")"
		}
	}
	return s
}

// iconSlot gives each row its program's icon from the shared image list:
// handing Walk a file PATH instead would make the UI thread call
// SHGetFileInfo (shell + file access), which on a cold machine or a sleeping
// disk blocks for seconds and the window flags "not responding" — see
// procIconCache. Until a worker has extracted the program's icon, the row
// shows the stock application icon (slot 0).
func (m *procModel) iconSlot(row int) int32 {
	if row < 0 || row >= len(m.rows) {
		return 0
	}
	return procIcons.slot(m.rows[row].Path)
}

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
	// A header click arrives as a list-view notification (LVN_COLUMNCLICK):
	// the repaint must not flush bands with UpdateWindow from inside it.
	inListMessage(m.tab.refresh)
	return m.SorterBase.Sort(col, order)
}

// cmp orders rows by the sorted column, ties by PID ascending in either
// direction (rows with equal keys then keep snapshot order).
func (m *procModel) cmp() func(a, b *metrics.Row) int {
	c := m.cols[m.sortCol].cmp
	desc := m.sortOrder == walk.SortDescending
	return func(a, b *metrics.Row) int {
		v := c(a, b)
		if desc {
			v = -v
		}
		if v != 0 {
			return v
		}
		return cmp.Compare(a.PID, b.PID)
	}
}

// cmpFloat orders like <, treating values that are neither smaller nor
// larger (NaN) as equal.
func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// StyleCell paints the dark zebra rows, the selection, and heat tints.
func (m *procModel) StyleCell(st *walk.CellStyle) {
	row, col := st.Row(), st.Col()
	if row < 0 || row >= len(m.rows) || col < 0 || col >= len(m.cols) {
		return
	}
	st.BackgroundColor, st.TextColor = m.cellColors(row, col, m.tab.tv.CurrentIndex())
}

func (m *procModel) cellColors(row, col, selected int) (bg, fg walk.Color) {
	bg, fg = rowStyle(row, selected)
	if row != selected {
		if c := m.cols[col]; c.heat != nil {
			if a := c.heat(&m.rows[row], m.totalMem); a > 0.03 {
				if a > 1 {
					a = 1
				}
				bg = blend(T.Warn, bg, 0.05+0.30*a)
			}
		}
		if m.cols[col].key == "cpu" && m.rows[row].CPU >= 50 {
			fg = T.Warn
		}
		if m.rows[row].Collapsed && m.cols[col].numeric {
			fg = T.Muted
		}
	}
	return bg, fg
}

// rowSigs describes a visible row for paintDiff: selection, indent and icon,
// then each visible cell's text and colours.
func (m *procModel) rowSigs(row int, vis []int, out []uint64) {
	sel := m.tab.tv.CurrentIndex()
	r := &m.rows[row]
	own := uint64(m.iconSlot(row))<<32 | uint64(uint32(r.Depth))<<1
	if row == sel {
		own |= 1
	}
	out[0] = own * 0x9E3779B97F4A7C15
	for k, c := range vis {
		bg, fg := m.cellColors(row, c, sel)
		out[k+1] = cellSig(m.text(row, c), bg, fg)
	}
}

// rowID identifies a row by its process (or group) key, for paintDiff's
// reorder announcements.
func (m *procModel) rowID(row int) uint64 {
	if row < 0 || row >= len(m.rows) {
		return 0
	}
	return maphash.String(sigSeed, m.rows[row].Key)
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
	builder   metrics.RowBuilder // reuses per-process text between ticks
	paint     paintDiff          // what the visible rows show
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
	want := map[string]bool{"name": true}
	for _, key := range t.app.cfg.Columns {
		want[key] = true
	}
	cols := make([]decl.TableViewColumn, len(t.model.cols))
	for i, c := range t.model.cols {
		al := decl.AlignDefault
		if c.numeric {
			al = decl.AlignFar
		}
		cols[i] = decl.TableViewColumn{Name: c.key, Title: c.title, Width: c.width, Alignment: al, Hidden: !want[c.key]}
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
	t.bTreeView = newButton("Tree", "", 80, func() { t.app.toggleProcessLayout(true) })
	t.bTreeView.tip = "Show processes under the process that started them"
	t.bTreeView.toggle = true
	t.bGroup = newButton("Group", "", 90, func() { t.app.toggleProcessLayout(false) })
	t.bGroup.tip = "Combine processes with the same name"
	t.bGroup.toggle = true
	t.bExpand = newButton("Expand all", "\uE70D", 104, t.expandAll)
	t.bFold = newButton("Collapse all", "\uE70E", 112, t.collapseAll)

	filterBar := tableFilter(&t.filter, processFilterCue, "Search processes", t.refresh)
	filterBar = append(filterBar, decl.HSpacer{}, t.bExpand.widget(), t.bFold.widget(), t.bTreeView.widget(), t.bGroup.widget())

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
					commandRow([]decl.Widget{
						t.bKill.widget(), t.bTree.widget(), t.bSusp.widget(), t.bRes.widget(),
						t.bAff.widget(), t.bLoc.widget(),
						t.bPrio.widget(),
						decl.HSpacer{},
					}),
					commandRow(filterBar),
					decl.TableView{
						AssignTo:              &t.tv,
						StretchFactor:         bodyStretch,
						Model:                 t.model,
						Columns:               cols,
						ColumnsOrderable:      true,
						ContextMenuItems:      t.contextMenu(),
						OnItemActivated:       t.activateRow,
						OnKeyDown:             t.onKeyDown,
						OnCurrentIndexChanged: t.onSelectionChanged,
					},
					decl.Label{AssignTo: &t.count, TextColor: T.Muted, EllipsisMode: decl.EllipsisEnd},
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
		decl.Action{Text: "Copy details", OnTriggered: t.copyRow},
		decl.Action{Text: "Copy image path", OnTriggered: t.copyPath},
		decl.Separator{},
		decl.Action{Text: "Expand / collapse branch", OnTriggered: t.toggleSelected},
	}
}

const processFilterCue = "Search name, PID, user or path"

func (t *procsTab) init() {
	t.app.registerRoot(pgProcesses, t.root)
	t.tv.SetRestoringCurrentItemOnReset(false) // selection is restored by refresh()
	styleTable(t.tv)
	nameTable(t.tv, "Processes table")
	searchCue(t.filter, processFilterCue+"  (Ctrl+F)", t.tv)
	t.bExpand.v.cw.SetVisible(false) // only shown in tree layout; keeps the minimum width small
	t.bFold.v.cw.SetVisible(false)
	t.applyColumns()
	indentRows(t.tv, func(row int) int {
		if t.model.layout == metrics.LayoutTree && row >= 0 && row < len(t.model.rows) {
			return t.model.rows[row].Depth
		}
		return 0
	})
	tv := t.tv
	iconRows(tv, func(f func()) bool {
		mw := t.app.mw
		if mw == nil || mw.IsDisposed() {
			return false
		}
		mw.Synchronize(f)
		return true
	}, func() {
		if t.tv == tv && !tv.IsDisposed() {
			t.repaint() // one coalesced repaint of rows whose icon arrived
		}
	}, t.model.iconSlot)
	t.model.ready = true
	_ = t.model.SorterBase.Sort(t.model.sortCol, t.model.sortOrder) // shows the sort arrow
	onFullRepaint(tv, t.syncPaint)
	t.syncToggles()
}

func (t *procsTab) searchBox() *walk.LineEdit { return t.filter }
func (t *procsTab) focusTarget() walk.Window  { return t.tv }
func (t *procsTab) reload()                   { t.refresh() }

// shown records the rows the page's exposure paints; showPage then delivers
// the latest snapshot through tick, which repaints only what changed.
func (t *procsTab) shown()                 { t.syncPaint() }
func (t *procsTab) tick(*metrics.Snapshot) { t.refresh() }

// syncPaint records the visible rows as painted after the whole list was
// invalidated or exposed (paintDiff.markPainted).
func (t *procsTab) syncPaint() {
	if t.tv == nil || !t.model.ready {
		return
	}
	procIcons.touchGeneration()
	t.paint.markPainted(t.tv, len(t.model.rows), t.model)
}

func (t *procsTab) syncToggles() {
	t.bTreeView.set(true, t.app.cfg.TreeView)
	t.bGroup.set(true, t.app.cfg.GroupByName)
}

// applyColumns shows exactly the columns chosen in Settings (Name is always on).
func (t *procsTab) applyColumns() {
	if t.tv == nil || t.tv.Columns().Len() != len(t.model.cols) {
		return
	} // Settings can change columns before the first visit.
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

// refresh rebuilds the rows from the newest snapshot and repaints only the
// visible cells whose text or colour changed. The list view is never reset:
// a changed row count is published as rows added or removed at the end, and
// the selection follows its process to its new position.
func (t *procsTab) refresh() {
	s := t.app.snap
	if s == nil || t.tv == nil || t.model.cols == nil || !t.model.ready {
		return
	}
	m := t.model
	// Rows that scrolled or were laid out into view since the last repaint
	// show the model as it is now: record them before it changes.
	procIcons.touchGeneration()
	t.paint.observe(t.tv, len(m.rows), m)
	m.layout = t.layout()
	m.totalMem = t.app.host.TotalMem
	rows := t.builder.Build(s.Procs, metrics.View{
		Layout: m.layout, Filter: t.filter.Text(), HideIdle: t.app.cfg.HideSystemIdle,
		Collapsed: t.collapsed, Cmp: m.cmp(),
	})
	t.setRows(rows)

	unit := "processes"
	switch m.layout {
	case metrics.LayoutGroup:
		unit = "groups"
	case metrics.LayoutTree:
		unit = "shown"
	}
	txt := strconv.Itoa(len(rows)) + " " + unit
	if len(rows) == 0 {
		if q := strings.TrimSpace(t.filter.Text()); q != "" {
			txt = fmt.Sprintf("No processes match “%s” — Esc clears the search", q)
		}
	}
	if txt != t.lastCount {
		t.lastCount = txt
		_ = t.count.SetText(txt)
	}
}

// prefill builds the first rows while a newly constructed page is still
// hidden, so showing it does not also have to build them.
func (t *procsTab) prefill() { t.refresh() }

// setRows replaces the model rows.
func (t *procsTab) setRows(rows []metrics.Row) {
	m := t.model
	oldIdx := t.indexOf(m.rows, t.selKey)
	wasVisible := oldIdx >= 0 && t.tv.ItemVisible(oldIdx)
	oldCount := len(m.rows)

	t.updating = true
	m.rows = rows
	switch {
	case len(rows) > oldCount:
		m.PublishRowsInserted(oldCount, len(rows)-1)
	case len(rows) < oldCount:
		m.PublishRowsRemoved(len(rows), oldCount-1) // clears a selection past the end
	}
	newIdx := t.indexOf(rows, t.selKey)
	if newIdx < 0 {
		t.selKey = ""
	}
	if cur := t.tv.CurrentIndex(); newIdx != cur {
		if newIdx >= 0 && wasVisible {
			_ = t.tv.SetCurrentIndex(newIdx) // a row the reader saw stays in view
		} else {
			selectQuietly(t.tv, newIdx) // never scroll the reader's viewport
		}
	}
	t.repaint()
	t.updating = false
	t.updateControls()
}

// repaint redraws the visible rows whose content changed.
func (t *procsTab) repaint() {
	procIcons.touchGeneration()
	t.paint.repaint(t.tv, len(t.model.rows), t.model)
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
	procIcons.touchGeneration()
	t.paint.repaintAsync(t.tv, len(t.model.rows), t.model) // the previous and the new selected row
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

// activateRow handles a double-click: expand or collapse a branch in the tree
// layout, otherwise open the program's folder.
func (t *procsTab) activateRow() {
	r := t.selected()
	if r == nil {
		return
	}
	if t.app.cfg.TreeView && r.HasKids {
		t.toggleSelected()
		return
	}
	t.locate()
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

// onKeyDown runs inside the list view's own WM_KEYDOWN: rows refreshed from
// here repaint without flushing bands (no re-entrant UpdateWindow).
func (t *procsTab) onKeyDown(key walk.Key) {
	r := t.selected()
	switch key {
	case walk.KeyDelete:
		t.endTask()
	case walk.KeyLeft:
		if r != nil && t.app.cfg.TreeView && r.HasKids && !r.Collapsed {
			inListMessage(t.toggleSelected)
		}
	case walk.KeyRight:
		if r != nil && t.app.cfg.TreeView && r.HasKids && r.Collapsed {
			inListMessage(t.toggleSelected)
		}
	case walk.KeyF5:
		inListMessage(t.refresh)
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
	r := t.selected()
	if r == nil || r.Count < 1 {
		return
	}
	// Reveal the path the user saw on this row, not a fresh lookup by PID:
	// the process may have exited and its ID reused since the row was drawn.
	if r.Path != "" {
		if err := metrics.RevealPath(r.Path); err != nil {
			t.app.errBox("Open file location", err)
		}
		return
	}
	if err := metrics.OpenFileLocation(r.PIDs[0]); err != nil {
		t.app.errBox("Open file location", err)
	}
}

// copyRow puts the selected row's visible columns on the clipboard,
// tab-separated with a header line (Ctrl+C), so a pasted bug report carries
// the column names.
func (t *procsTab) copyRow() {
	r := t.selected()
	if r == nil || t.tv == nil {
		return
	}
	cols := t.tv.Columns()
	var head, vals []string
	for i, c := range t.model.cols {
		if cols.At(i).Visible() {
			head = append(head, c.title)
			vals = append(vals, c.text(r))
		}
	}
	_ = walk.Clipboard().SetText(strings.Join(head, "\t") + "\n" + strings.Join(vals, "\t"))
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
