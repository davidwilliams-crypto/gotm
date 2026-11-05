//go:build windows

package ui

import (
	"hash/maphash"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
)

// gridModel is a read-only, sortable table of strings in gotm's dark style.
type gridModel struct {
	walk.TableModelBase
	walk.SorterBase

	rows     [][]string
	keyCol   int // column whose text identifies a row across refreshes
	sortCol  int
	sortDesc bool
	ready    bool
	numeric  map[int]bool
	noSort   bool
	tv       *walk.TableView
	// optional per-row check boxes (Startup apps)
	isChecked func(row int) bool
	setCheck  func(row int, v bool) error
	// tint returns an optional foreground colour for a cell.
	tint func(row, col int, text string) (walk.Color, bool)

	paint paintDiff // what the visible rows show
	// sort scratch, reused between refreshes
	keys []gridKey
	perm []int32
	// the slice update last sorted, so a following set need not sort again
	sorted gridSorted
}

type gridKey struct {
	num  float64
	ok   bool   // num is valid (numeric columns)
	text string // lower-cased cell text otherwise
}

type gridSorted struct {
	first *[]string
	n     int
	col   int
	desc  bool
}

func (m *gridModel) RowCount() int { return len(m.rows) }

func (m *gridModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.rows) || col < 0 || col >= len(m.rows[row]) {
		return ""
	}
	return m.rows[row][col]
}

func (m *gridModel) ColumnSortable(int) bool { return !m.noSort }

// Checked / SetChecked implement walk.ItemChecker when check boxes are used.
func (m *gridModel) Checked(row int) bool {
	return m.isChecked != nil && m.isChecked(row)
}

func (m *gridModel) SetChecked(row int, v bool) error {
	if m.setCheck == nil {
		return nil
	}
	return m.setCheck(row, v)
}

func (m *gridModel) Sort(col int, order walk.SortOrder) error {
	if !m.ready || m.noSort || col < 0 {
		return nil
	}
	m.sortCol, m.sortDesc = col, order == walk.SortDescending
	// A header click arrives as a list-view notification: repaint without
	// flushing bands (no UpdateWindow) from inside it.
	inListMessage(func() { m.set(m.rows) })
	return m.SorterBase.Sort(col, order)
}

func (m *gridModel) resort() { m.sortRows(m.rows) }

// sortRows orders rows by the sorted column in place: numbers (with units)
// numerically, unavailable values last in either direction, text without
// regard to case; equal rows keep their order. Each row's key is computed
// once (decorate-sort) rather than in every comparison.
func (m *gridModel) sortRows(rows [][]string) {
	m.sorted = gridSorted{}
	if m.sortCol < 0 || len(rows) < 2 {
		return
	}
	c, desc, numeric := m.sortCol, m.sortDesc, m.numeric[m.sortCol]
	keys := m.keys[:0]
	for _, r := range rows {
		s := cell(r, c)
		var k gridKey
		if numeric {
			k.num, k.ok = sortValue(s)
		}
		if !k.ok {
			k.text = strings.ToLower(s)
		}
		keys = append(keys, k)
	}
	perm := m.perm[:0]
	for i := range rows {
		perm = append(perm, int32(i))
	}
	slices.SortFunc(perm, func(i, j int32) int {
		a, b := &keys[i], &keys[j]
		if a.ok != b.ok { // only numeric columns parse; unavailable stays at the bottom
			if a.ok {
				return -1
			}
			return 1
		}
		var v int
		if a.ok {
			v = cmpFloat(a.num, b.num)
		} else {
			v = strings.Compare(a.text, b.text)
		}
		if desc {
			v = -v
		}
		if v != 0 {
			return v
		}
		return int(i - j)
	})
	permute(rows, perm)
	clear(keys) // do not pin the previous rows' text
	m.keys, m.perm = keys, perm
	m.sorted = gridSorted{first: &rows[0], n: len(rows), col: c, desc: desc}
}

// permute reorders s so that s'[k] = s[perm[k]], in place; perm is consumed.
func permute[T any](s []T, perm []int32) {
	for k := range perm {
		if perm[k] < 0 || int(perm[k]) == k {
			continue
		}
		tmp := s[k]
		j := k
		for {
			src := int(perm[j])
			perm[j] = -1
			if src == k {
				s[j] = tmp
				break
			}
			s[j] = s[src]
			j = src
		}
	}
}

// presorted reports whether rows is exactly the slice sortRows last ordered
// under the current sort.
func (m *gridModel) presorted(rows [][]string) bool {
	if m.sortCol < 0 || len(rows) < 2 {
		return true
	}
	return m.sorted.first == &rows[0] && m.sorted.n == len(rows) && m.sorted.col == m.sortCol && m.sorted.desc == m.sortDesc
}
func cell(r []string, c int) string {
	if c >= 0 && c < len(r) {
		return r[c]
	}
	return ""
}

// sortValue parses "12.5 %", "1.4 GB", "5 MB/s", "1,234" into a comparable number.
func sortValue(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" || !(unicode.IsDigit(rune(s[0])) || s[0] == '-') {
		return 0, false
	}
	i := 0
	for i < len(s) && (unicode.IsDigit(rune(s[i])) || s[i] == '.' || s[i] == '-') {
		i++
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	// a date like 2026-10-11 should sort as text, not as the number 2026
	if i < len(s) && strings.Count(s, "-") >= 2 {
		return 0, false
	}
	switch strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(s[i:], "/s"))) {
	case "KB":
		v *= 1 << 10
	case "MB":
		v *= 1 << 20
	case "GB":
		v *= 1 << 30
	case "TB":
		v *= 1 << 40
	case "GHZ":
		v *= 1000
	}
	return v, true
}

// StyleCell paints dark zebra rows, the selection and optional tints.
func (m *gridModel) StyleCell(st *walk.CellStyle) {
	row, col := st.Row(), st.Col()
	if row < 0 || row >= len(m.rows) {
		return
	}
	sel := -1
	if m.tv != nil {
		sel = m.tv.CurrentIndex()
	}
	st.BackgroundColor, st.TextColor = m.cellColors(row, col, sel)
}

func (m *gridModel) cellColors(row, col, sel int) (bg, fg walk.Color) {
	bg, fg = rowStyle(row, sel)
	if m.tint != nil && row != sel {
		if c, ok := m.tint(row, col, cell(m.rows[row], col)); ok {
			fg = c
		}
	}
	return bg, fg
}

// rowSigs describes a visible row for paintDiff.
func (m *gridModel) rowSigs(row int, vis []int, out []uint64) {
	sel := m.tv.CurrentIndex()
	own := uint64(2)
	if row == sel {
		own |= 1
	}
	if m.isChecked != nil && m.isChecked(row) {
		own |= 4
	}
	out[0] = own * 0x9E3779B97F4A7C15
	r := m.rows[row]
	for k, c := range vis {
		bg, fg := m.cellColors(row, c, sel)
		out[k+1] = cellSig(cell(r, c), bg, fg)
	}
}

// rowID identifies a row by its key column (paintDiff reorder announcements).
func (m *gridModel) rowID(row int) uint64 {
	if m.keyCol < 0 || row < 0 || row >= len(m.rows) {
		return uint64(row)
	}
	return maphash.String(sigSeed, cell(m.rows[row], m.keyCol))
}

// observe records rows that came into view since the last repaint, before
// the model changes (paintDiff.observe).
func (m *gridModel) observe() {
	if m.tv != nil {
		m.paint.observe(m.tv, len(m.rows), m)
	}
}

// markPainted records the visible rows as painted after a full invalidate.
func (m *gridModel) markPainted() {
	if m.tv != nil {
		m.paint.markPainted(m.tv, len(m.rows), m)
	}
}

// repaintAsync is repaint from inside a list-view notification.
func (m *gridModel) repaintAsync() {
	if m.tv != nil {
		m.paint.repaintAsync(m.tv, len(m.rows), m)
	}
}

// repaint redraws the visible rows whose content changed.
func (m *gridModel) repaint() {
	if m.tv != nil {
		m.paint.repaint(m.tv, len(m.rows), m)
	}
}

// set replaces the rows, keeping the sort order and the selection. The list
// view is not reset: a changed row count is published as rows added or
// removed at the end, only changed cells repaint, and the selected row is
// re-selected at its new position without scrolling.
func (m *gridModel) set(rows [][]string) {
	m.observe() // before sorting: rows may be m.rows itself (Sort)
	var selKey string
	if m.tv != nil {
		if i := m.tv.CurrentIndex(); i >= 0 && i < len(m.rows) {
			selKey = cell(m.rows[i], m.keyCol)
		}
	}
	if !m.presorted(rows) {
		m.sortRows(rows)
	}
	m.sorted = gridSorted{}
	oldCount := len(m.rows)
	m.rows = rows
	if m.tv == nil {
		m.PublishRowsReset()
		return
	}
	switch {
	case len(rows) > oldCount:
		m.PublishRowsInserted(oldCount, len(rows)-1)
	case len(rows) < oldCount:
		m.PublishRowsRemoved(len(rows), oldCount-1)
	}
	want := -1
	if selKey != "" {
		for i := range rows {
			if cell(rows[i], m.keyCol) == selKey {
				want = i
				break
			}
		}
	}
	if want != m.tv.CurrentIndex() {
		if want >= 0 {
			// re-select without scrolling: SetCurrentIndex would bring the row
			// into view on every refresh, yanking the list back while scrolling
			selectQuietly(m.tv, want)
		} else {
			_ = m.tv.SetCurrentIndex(-1)
		}
	}
	m.repaint()
}

// update copies new cell text over the existing rows when the row identities
// (key column) are unchanged, so only changed values repaint. It reports
// whether it did; otherwise the rows are left sorted for set.
func (m *gridModel) update(rows [][]string) bool {
	if m.keyCol < 0 {
		return false
	}
	m.observe()
	// Compare identities in the displayed sort order, rather than API order.
	m.sortRows(rows)
	if len(rows) != len(m.rows) {
		return false
	}
	for i := range rows {
		if cell(rows[i], m.keyCol) != cell(m.rows[i], m.keyCol) {
			return false
		}
	}
	m.sorted = gridSorted{}
	m.rows = rows
	m.repaint()
	return true
}

// selectedRow returns the selected row's cells, or nil.
func (m *gridModel) selectedRow() []string {
	if m.tv == nil {
		return nil
	}
	if i := m.tv.CurrentIndex(); i >= 0 && i < len(m.rows) {
		return m.rows[i]
	}
	return nil
}

// ---- page scaffold ------------------------------------------------------------

// gridPage is the shared shell of the table pages: a toolbar with page
// buttons, a filter box and a status line above a dark table.
type gridPage struct {
	app    *App
	id     int
	root   *walk.Composite
	tv     *walk.TableView
	filter *walk.LineEdit
	status *walk.Label
	model  *gridModel
	cols   []decl.TableViewColumn
	top    []decl.Widget // widgets placed between the toolbar and the table
	left   []decl.Widget // buttons placed before the spacer
	last   string

	filterPlaceholder string
	checkBoxes        bool
	noSort            bool
	onFilter          func()
	onSelect          func()
	context           []decl.MenuItem
	activate          func()
}

func newGridPage(a *App, id int, cols []decl.TableViewColumn, numeric ...int) *gridPage {
	g := &gridPage{app: a, id: id, cols: cols, filterPlaceholder: "Search"}
	g.model = &gridModel{keyCol: 0, sortCol: -1, numeric: map[int]bool{}}
	for _, n := range numeric {
		g.model.numeric[n] = true
	}
	for i, c := range cols {
		if c.Alignment == decl.AlignFar {
			g.model.numeric[i] = true
		}
	}
	return g
}

func (g *gridPage) build() decl.Widget {
	bar := append([]decl.Widget{}, g.left...)
	bar = append(bar, decl.HSpacer{})
	filterBar := tableFilter(&g.filter, g.filterPlaceholder, g.filterPlaceholder, g.onFilter)
	filterBar = append(filterBar, decl.HSpacer{})
	kids := []decl.Widget{
		commandRow(bar),
		commandRow(filterBar),
	}
	kids = append(kids, g.top...)
	kids = append(kids,
		decl.TableView{
			AssignTo:         &g.tv,
			StretchFactor:    bodyStretch,
			Model:            g.model,
			Columns:          g.cols,
			ColumnsOrderable: true,
			CheckBoxes:       g.checkBoxes,
			ContextMenuItems: g.context,
			OnItemActivated: func() {
				if g.activate != nil {
					g.activate()
				}
			},
			OnCurrentIndexChanged: func() {
				if g.onSelect != nil {
					g.onSelect()
				}
				g.model.repaintAsync() // the previous and the new selected row
			},
		},
		decl.Label{AssignTo: &g.status, TextColor: T.Muted, EllipsisMode: decl.EllipsisEnd},
	)
	return decl.Composite{
		AssignTo:      &g.root,
		StretchFactor: 1,
		Background:    decl.SolidColorBrush{Color: T.Bg},
		Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			g.app.heads[g.id].declare(0, 76, 0, 76, 0),
			decl.Composite{
				StretchFactor: bodyStretch,
				Background:    decl.SolidColorBrush{Color: T.Bg},
				Layout:        decl.VBox{Margins: padBody, Spacing: 10},
				Children:      kids,
			},
			g.app.foots[g.id].declare(0, 32, 0, 32, 0),
		},
	}
}

func (g *gridPage) init() {
	g.app.registerRoot(g.id, g.root)
	g.model.tv = g.tv
	g.model.noSort = g.noSort
	g.tv.SetRestoringCurrentItemOnReset(false)
	styleTable(g.tv)
	nameTable(g.tv, g.app.pages[g.id].title+" table")
	g.model.ready = true
	onFullRepaint(g.tv, g.model.markPainted)
	searchCue(g.filter, g.filterPlaceholder+"  (Ctrl+F)", g.tv)
}

// syncPaint records the table's visible rows as painted when its page is
// revealed: the exposure repaints the whole list (paintDiff.markPainted).
func (g *gridPage) syncPaint() { g.model.markPainted() }

func commandRow(children []decl.Widget) decl.Composite {
	return decl.Composite{
		Background: decl.SolidColorBrush{Color: T.Bg},
		Layout:     decl.HBox{MarginsZero: true, Spacing: 5},
		MinSize:    decl.Size{Height: 34},
		MaxSize:    decl.Size{Height: 34},
		Children:   children,
	}
}

// Keep the native edit for keyboard editing and assistive technology. Clear
// is independently focusable while there is text, including a whitespace query.
func tableFilter(edit **walk.LineEdit, cue, name string, changed func()) []decl.Widget {
	clear := newButton("Clear", "\uE894", 68, func() {
		_ = (*edit).SetFocus()
		_ = (*edit).SetText("")
	})
	clear.enabled, clear.tip = false, "Clear search (Esc)"
	return []decl.Widget{
		decl.LineEdit{
			AssignTo: edit, CueBanner: cue + "  (Ctrl+F)",
			MinSize: decl.Size{Width: 116, Height: 26}, MaxSize: decl.Size{Width: 306, Height: 26},
			Accessibility: decl.Accessibility{Name: name, Accelerator: "Ctrl+F", Description: "Type to filter. Escape clears the search. Down moves to the results."},
			Background:    decl.SolidColorBrush{Color: T.Card}, TextColor: T.Text,
			OnTextChanged: func() {
				clear.set((*edit).Text() != "", false)
				if changed != nil {
					changed()
				}
			},
		}, clear.widget(),
	}
}

func (g *gridPage) searchBox() *walk.LineEdit { return g.filter }
func (g *gridPage) focusTarget() walk.Window  { return g.tv }

// copyRow puts the selected row on the clipboard, tab-separated, header first,
// so a pasted bug report carries the column names (Ctrl+C).
func (g *gridPage) copyRow() {
	row := g.model.selectedRow()
	if row == nil {
		return
	}
	var head []string
	for _, c := range g.cols {
		head = append(head, c.Title)
	}
	_ = walk.Clipboard().SetText(strings.Join(head, "\t") + "\n" + strings.Join(row, "\t"))
}

func (g *gridPage) setStatus(s string) {
	if s != g.last {
		g.last = s
		_ = g.status.SetText(s)
	}
}

func (g *gridPage) setResultStatus(s string) {
	if len(g.model.rows) == 0 && g.filterText() != "" {
		s = "No matches. Press Esc to clear the search.  " + s
	}
	g.setStatus(s)
}

func (g *gridPage) filterText() string { return strings.ToLower(strings.TrimSpace(g.filter.Text())) }

// matches reports whether any cell contains the filter text.
func matchesAny(q string, cells ...string) bool {
	if q == "" {
		return true
	}
	for _, c := range cells {
		if strings.Contains(strings.ToLower(c), q) {
			return true
		}
	}
	return false
}

func col(title string, width int) decl.TableViewColumn {
	return decl.TableViewColumn{Title: title, Width: width}
}

func colR(title string, width int) decl.TableViewColumn {
	return decl.TableViewColumn{Title: title, Width: width, Alignment: decl.AlignFar}
}
