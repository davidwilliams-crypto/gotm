//go:build windows

package ui

import (
	"sort"
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
	m.set(m.rows)
	return m.SorterBase.Sort(col, order)
}

func (m *gridModel) resort() {
	if m.sortCol < 0 {
		return
	}
	c, desc := m.sortCol, m.sortDesc
	sort.SliceStable(m.rows, func(i, j int) bool {
		a, b := cell(m.rows[i], c), cell(m.rows[j], c)
		var less bool
		if m.numeric[c] {
			av, aok := sortValue(a)
			bv, bok := sortValue(b)
			if aok != bok {
				return aok
			} // unavailable values stay at the bottom
			if aok && bok {
				if av == bv {
					return false
				}
				less = av < bv
			} else {
				la, lb := strings.ToLower(a), strings.ToLower(b)
				if la == lb {
					return false
				}
				less = la < lb
			}
		} else {
			la, lb := strings.ToLower(a), strings.ToLower(b)
			if la == lb {
				return false
			}
			less = la < lb
		}
		if desc {
			return !less
		}
		return less
	})
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
	bg, fg := rowStyle(row, sel)
	if m.tint != nil && row != sel {
		if c, ok := m.tint(row, col, cell(m.rows[row], col)); ok {
			fg = c
		}
	}
	st.BackgroundColor, st.TextColor = bg, fg
}

// set replaces the rows, keeping the sort order and (when visible) the selection.
func (m *gridModel) set(rows [][]string) {
	var selKey string
	if m.tv != nil {
		if i := m.tv.CurrentIndex(); i >= 0 && i < len(m.rows) {
			selKey = cell(m.rows[i], m.keyCol)
		}
	}
	m.rows = rows
	m.resort()
	m.PublishRowsReset()
	if m.tv == nil {
		return
	}
	redrawVisible(m.tv)
	if selKey != "" {
		for i := range m.rows {
			if cell(m.rows[i], m.keyCol) == selKey {
				// re-select without scrolling: SetCurrentIndex would bring the row
				// into view on every refresh, yanking the list back while scrolling
				selectQuietly(m.tv, i)
				return
			}
		}
	}
	_ = m.tv.SetCurrentIndex(-1)
}

// update copies new cell text over the existing rows when the row identities
// (key column) are unchanged, so only values repaint. It reports whether it did.
func (m *gridModel) update(rows [][]string) bool {
	if m.keyCol < 0 {
		return false
	}
	// Compare identities in the displayed sort order, rather than API order.
	tmp := *m
	tmp.rows = rows
	tmp.resort()
	if len(rows) != len(m.rows) {
		return false
	}
	for i := range rows {
		if cell(rows[i], m.keyCol) != cell(m.rows[i], m.keyCol) {
			return false
		}
	}
	m.rows = rows
	if m.tv != nil {
		redrawVisible(m.tv)
	}
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
	extra  []decl.Widget // widgets placed after the spacer, before the filter
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
	filterBar := append([]decl.Widget{}, g.extra...)
	filterBar = append(filterBar, decl.LineEdit{AssignTo: &g.filter, CueBanner: g.filterPlaceholder,
		MinSize: decl.Size{Width: 280}, MaxSize: decl.Size{Width: 280},
		Accessibility: decl.Accessibility{Name: g.filterPlaceholder, Accelerator: "Ctrl+F"},
		Background:    decl.SolidColorBrush{Color: T.Card}, TextColor: T.Text,
		OnTextChanged: func() {
			if g.onFilter != nil {
				g.onFilter()
			}
		}}, decl.HSpacer{}, decl.Label{Text: "Ctrl+F to search · Esc to clear", TextColor: T.Faint})
	kids := []decl.Widget{
		decl.Composite{
			Background: decl.SolidColorBrush{Color: T.Bg},
			Layout:     decl.HBox{MarginsZero: true, Spacing: 8},
			MinSize:    decl.Size{Height: 38},
			MaxSize:    decl.Size{Height: 38},
			Children:   bar,
		},
		decl.Composite{
			Background: decl.SolidColorBrush{Color: T.Bg},
			Layout:     decl.HBox{MarginsZero: true, Spacing: 8},
			MinSize:    decl.Size{Height: 30}, MaxSize: decl.Size{Height: 30}, Children: filterBar,
		},
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
				if g.tv != nil {
					redrawVisible(g.tv)
				}
			},
		},
		decl.Label{AssignTo: &g.status, TextColor: T.Muted},
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
	_ = g.tv.SetModel(g.model)
	styleTable(g.tv)
	nameTable(g.tv, g.app.pages[g.id].title+" table")
	g.model.ready = true
	searchCue(g.filter, g.filterPlaceholder+"   (Ctrl+F)", g.tv)
}

func (g *gridPage) searchBox() *walk.LineEdit { return g.filter }
func (g *gridPage) focusTarget() walk.Window  { return g.tv }

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
