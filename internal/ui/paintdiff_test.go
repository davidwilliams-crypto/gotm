//go:build windows

package ui

import (
	"fmt"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

func TestDiffPaintListsOnlyChangedRowsAndCells(t *testing.T) {
	const width = 5 // own + 4 cells
	win := func(top int, rows ...[]uint64) []uint64 {
		var out []uint64
		for _, r := range rows {
			out = append(out, r...)
		}
		return out
	}
	old := win(10,
		[]uint64{1, 10, 20, 30, 40}, // row 10
		[]uint64{1, 11, 21, 31, 41}, // row 11
		[]uint64{1, 12, 22, 32, 42}, // row 12
		[]uint64{1, 13, 23, 33, 43}, // row 13
	)
	cur := win(10,
		[]uint64{1, 10, 20, 30, 40}, // unchanged
		[]uint64{1, 11, 99, 31, 41}, // one cell (sub-item 1)
		[]uint64{2, 12, 22, 32, 42}, // selection/icon changed: whole row
		[]uint64{1, 13, 98, 97, 96}, // most cells: whole row
	)
	got := diffPaint(nil, 10, 4, old, 10, 4, cur, width)
	want := []paintOp{{11, 1}, {12, -1}, {13, -1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ops %v, want %v", got, want)
	}

	// Scrolled down by two: rows 12 and 13 are compared, 14 and 15 are new.
	scrolled := win(12,
		[]uint64{1, 12, 22, 32, 42},
		[]uint64{1, 13, 23, 33, 43},
		[]uint64{1, 14, 24, 34, 44},
		[]uint64{1, 15, 25, 35, 45},
	)
	got = diffPaint(nil, 10, 4, old, 12, 4, scrolled, width)
	want = []paintOp{{14, -1}, {15, -1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after scroll ops %v, want %v", got, want)
	}

	// Name column (sub-item 0, carrying icon and indent) repaints the row.
	name := append([]uint64(nil), old...)
	name[1] = 77
	if got = diffPaint(nil, 10, 4, old, 10, 4, name, width); !reflect.DeepEqual(got, []paintOp{{10, -1}}) {
		t.Fatalf("name change ops %v", got)
	}
	// Unknown cache: everything.
	if got = diffPaint(nil, 0, 0, nil, 10, 2, old[:2*width], width); len(got) != 2 || got[0].sub != -1 {
		t.Fatalf("cold cache ops %v", got)
	}
	// Nothing changed: nothing to do, and no allocation.
	ops := make([]paintOp, 0, 8)
	if allocs := testing.AllocsPerRun(10, func() { ops = diffPaint(ops[:0], 10, 4, old, 10, 4, old, width) }); allocs != 0 || len(ops) != 0 {
		t.Fatalf("steady state ops=%v allocs=%v", ops, allocs)
	}
}

// paintProbe counts, per row, the list-view item paints the table's parent
// is notified about.
type paintProbe struct {
	prev  uintptr
	rows  map[int]int
	cells int
}

var activeProbe *paintProbe

func TestPaintDiffInvalidatesOnlyChangedCells(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		tv, err := walk.NewTableView(mw)
		if err != nil {
			panic(err)
		}
		defer delete(lvCache, tv)
		before := len(styledTables)
		defer func() { styledTables = styledTables[:before] }()
		for i := 0; i < 4; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(90)
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		m := &gridModel{keyCol: 0, sortCol: -1, tv: tv}
		rows := func(changed map[[2]int]string) [][]string {
			out := make([][]string, 100)
			for i := range out {
				out[i] = []string{fmt.Sprint("row", i), "a", "b", "c"}
				for k, v := range changed {
					if k[0] == i {
						out[i][k[1]] = v
					}
				}
			}
			return out
		}
		m.rows = rows(nil)
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		m.ready = true // after the table's own initial Sort
		styleTable(tv)
		if err := tv.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 400}); err != nil {
			panic(err)
		}
		m.repaint()
		if m.paint.lastRows == 0 {
			t.Error("first repaint did not redraw the visible rows")
			return
		}
		m.repaint()
		if m.paint.lastRows != 0 || m.paint.lastCells != 0 {
			t.Errorf("unchanged table repainted %d rows %d cells", m.paint.lastRows, m.paint.lastCells)
			return
		}
		if !m.update(rows(map[[2]int]string{{2, 1}: "x", {4, 3}: "y"})) {
			t.Error("same identities were not updated in place")
			return
		}
		if m.paint.lastRows != 0 || m.paint.lastCells != 2 {
			t.Errorf("two changed cells repainted %d rows %d cells (window top %d rows %d vis %v ops %v)", m.paint.lastRows, m.paint.lastCells, m.paint.top, m.paint.rows, m.paint.vis, m.paint.ops)
			return
		}
		// Selection repaints just the newly selected row.
		selectQuietly(tv, 4)
		m.repaint()
		if m.paint.lastRows != 1 || m.paint.lastCells != 0 {
			t.Errorf("selection repainted %d rows %d cells", m.paint.lastRows, m.paint.lastCells)
			return
		}
		// Reorder with identical rows: selection follows, no reset needed.
		r := rows(map[[2]int]string{{2, 1}: "x", {4, 3}: "y"})
		r[3], r[4] = r[4], r[3]
		m.set(r)
		if tv.CurrentIndex() != 3 || cell(m.rows[3], 0) != "row4" {
			t.Errorf("selection did not follow its row: %d", tv.CurrentIndex())
			return
		}
		if m.paint.lastRows != 2 {
			t.Errorf("swap repainted %d rows", m.paint.lastRows)
			return
		}
		// A column layout change repaints everything once.
		_ = tv.Columns().At(2).SetVisible(false)
		m.repaint()
		if m.paint.lastRows == 0 {
			t.Error("column change did not repaint")
			return
		}
		// Rows removed and added keep the cache consistent.
		m.set(r[:3])
		if tv.CurrentIndex() != -1 {
			t.Error("removed selected row stayed selected")
			return
		}
		m.set(r)
		if m.paint.lastRows == 0 {
			t.Error("new rows not painted")
			return
		}
	})
}

// TestNativeListPaintsOnlyInvalidatedItems measures what comctl32 does with
// a sparse invalidation on a shown window: logged for profiling, asserted
// only that the changed rows were among those painted.
func TestNativeListPaintsOnlyInvalidatedItems(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		_ = mw.SetLayout(walk.NewVBoxLayout())
		tv, err := walk.NewTableView(mw)
		if err != nil {
			panic(err)
		}
		defer delete(lvCache, tv)
		before := len(styledTables)
		defer func() { styledTables = styledTables[:before] }()
		for i := 0; i < 4; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(90)
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		m := &gridModel{keyCol: 0, sortCol: -1, tv: tv}
		m.rows = make([][]string, 100)
		for i := range m.rows {
			m.rows[i] = []string{fmt.Sprint("row", i), "a", "b", "c"}
		}
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		m.ready = true // after the table's own initial Sort
		styleTable(tv)
		_ = mw.SetBoundsPixels(walk.Rectangle{X: 0, Y: 0, Width: 500, Height: 600})
		win.ShowWindow(win.HWND(mw.Handle()), win.SW_SHOWNOACTIVATE)
		pump := func(d time.Duration) {
			until := time.Now().Add(d)
			for time.Now().Before(until) {
				var msg win.MSG
				if win.PeekMessage(&msg, 0, 0, 0, win.PM_REMOVE) {
					win.TranslateMessage(&msg)
					win.DispatchMessage(&msg)
				} else {
					time.Sleep(time.Millisecond)
				}
			}
		}
		pump(150)
		// Wait for layout to settle: the cache must cover the whole window.
		for i := 0; i < 20; i++ {
			m.repaint()
			pump(30)
			if i > 0 && m.paint.lastRows == 0 && m.paint.rows > 15 {
				break
			}
		}

		p := &paintProbe{rows: map[int]int{}}
		h := win.HWND(tv.Handle())
		cb := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
			if msg == win.WM_NOTIFY && lp != 0 {
				nmh := (*win.NMHDR)(ptrFrom(lp))
				if nmh.Code == win.NM_CUSTOMDRAW {
					cd := (*win.NMLVCUSTOMDRAW)(ptrFrom(lp))
					switch cd.Nmcd.DwDrawStage {
					case win.CDDS_ITEMPREPAINT:
						p.rows[int(cd.Nmcd.DwItemSpec)]++
					case win.CDDS_ITEMPREPAINT | win.CDDS_SUBITEM:
						p.cells++
					}
				}
			}
			return win.CallWindowProc(p.prev, win.HWND(hwnd), uint32(msg), wp, lp)
		})
		p.prev = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, cb)
		defer win.SetWindowLongPtr(h, win.GWLP_WNDPROC, p.prev)

		for _, lv := range listViews(tv) {
			win.UpdateWindow(lv)
		}
		pump(50)
		t.Logf("idle paint: %d rows %d sub-items", len(p.rows), p.cells)
		p.rows, p.cells = map[int]int{}, 0
		next := make([][]string, len(m.rows))
		for i := range next {
			next[i] = append([]string(nil), m.rows[i]...)
		}
		next[2][1], next[14][3] = "x", "y"
		m.update(next)
		for _, lv := range listViews(tv) {
			win.UpdateWindow(lv)
		}
		var painted []int
		for r := range p.rows {
			painted = append(painted, r)
		}
		sort.Ints(painted)
		t.Logf("diff invalidated %d rows %d cells; native painted rows %v (%d sub-item paints)", m.paint.lastRows, m.paint.lastCells, painted, p.cells)
		if len(painted) == 0 {
			t.Log("window did not paint in this session")
			return
		}
		if p.rows[2] == 0 || p.rows[14] == 0 {
			t.Errorf("changed rows not painted: %v", painted)
			return
		}
		// Only the two changed rows: not those between the bands, and not the
		// row below each changed cell (whose top edge the cell rect touches).
		for _, r := range painted {
			if r != 2 && r != 14 {
				t.Errorf("unchanged row %d painted; painted rows %v, want [2 14]", r, painted)
			}
		}
	})
}

// A repaint started from inside a list-view notification (a header click
// sorting the table) must not flush bands with UpdateWindow; outside one it
// still paints separated bands one at a time.
func TestNotificationRepaintDoesNotFlushBands(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		tv, err := walk.NewTableView(mw)
		if err != nil {
			panic(err)
		}
		defer delete(lvCache, tv)
		before := len(styledTables)
		defer func() { styledTables = styledTables[:before] }()
		for i := 0; i < 3; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(90)
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		// Sorting by column 1 swaps only rows 2 and 14: two separated bands.
		m := &gridModel{keyCol: 0, sortCol: -1, tv: tv, numeric: map[int]bool{}}
		m.rows = make([][]string, 60)
		for i := range m.rows {
			m.rows[i] = []string{fmt.Sprint("row", i), fmt.Sprintf("%03d", i), "x"}
		}
		m.rows[2][1], m.rows[14][1] = "014", "002"
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		m.ready = true
		styleTable(tv)
		if err := tv.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 500}); err != nil {
			panic(err)
		}
		m.repaint()
		// Outside a notification: the first band is flushed before the second.
		r := make([][]string, len(m.rows))
		for i := range r {
			r[i] = append([]string(nil), m.rows[i]...)
		}
		r[2][2], r[14][2] = "y", "y"
		if !m.update(r) || m.paint.lastBands != 1 {
			t.Errorf("separated bands outside a notification: %d flushes, want 1", m.paint.lastBands)
		}
		// A header click, delivered the way the list view sends it.
		var lv win.HWND
		for _, h := range listViews(tv) {
			hdr := win.HWND(win.SendMessage(h, lvmGetHeader, 0, 0))
			if win.SendMessage(hdr, win.HDM_GETITEMCOUNT, 0, 0) == 3 {
				lv = h
			}
		}
		nm := win.NMLISTVIEW{Hdr: win.NMHDR{HwndFrom: lv, Code: win.LVN_COLUMNCLICK}, ISubItem: 1}
		win.SendMessage(tv.Handle(), win.WM_NOTIFY, 0, uintptr(unsafe.Pointer(&nm)))
		if m.sortCol != 1 || cell(m.rows[2], 0) != "row14" || cell(m.rows[14], 0) != "row2" {
			t.Errorf("header click did not sort: col %d, rows 2/14 = %s/%s", m.sortCol, cell(m.rows[2], 0), cell(m.rows[14], 0))
			return
		}
		if m.paint.lastRows != 2 || m.paint.lastBands != 0 {
			t.Errorf("sort from a header click repainted %d rows with %d UpdateWindow flushes, want 2 rows, 0 flushes", m.paint.lastRows, m.paint.lastBands)
		}
		if listNotifyDepth != 0 {
			t.Errorf("notification depth leaked: %d", listNotifyDepth)
		}
	})
}

// After the whole list was invalidated (a palette change) or exposed (its
// page shown), the visible rows are recorded as painted: the next repaint
// paints only real changes, not every visible row a second time.
func TestFullInvalidateRecordsRowsAsPainted(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		tv, err := walk.NewTableView(mw)
		if err != nil {
			panic(err)
		}
		defer delete(lvCache, tv)
		before := len(styledTables)
		defer func() { styledTables = styledTables[:before] }()
		for i := 0; i < 3; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(90)
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		m := &gridModel{keyCol: 0, sortCol: -1, tv: tv}
		m.rows = make([][]string, 100)
		for i := range m.rows {
			m.rows[i] = []string{fmt.Sprint("row", i), "a", "b"}
		}
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		m.ready = true
		styleTable(tv)
		onFullRepaint(tv, m.markPainted)
		// Rows built while the table is small (a hidden page before layout).
		_ = tv.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 80})
		m.repaint()
		small := m.paint.rows
		// Laid out and exposed: the exposure paints everything.
		_ = tv.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 500})
		m.markPainted()
		if m.paint.rows <= small {
			t.Errorf("marked window %d rows, not the laid-out %d+", m.paint.rows, small)
		}
		m.repaint()
		if m.paint.lastRows != 0 || m.paint.lastCells != 0 {
			t.Errorf("first tick after show repainted %d rows %d cells", m.paint.lastRows, m.paint.lastCells)
		}
		// A palette change invalidates every table once; the next tick is free.
		old := T
		defer func() { T = old }()
		T.RowAlt = walk.RGB(1, 2, 3)
		recolorTables()
		m.repaint()
		if m.paint.lastRows != 0 || m.paint.lastCells != 0 {
			t.Errorf("tick after a palette change repainted %d rows %d cells", m.paint.lastRows, m.paint.lastCells)
		}
		// Real changes still paint.
		next := append([][]string(nil), m.rows...)
		next[3] = []string{"row3", "changed", "b"}
		m.update(next)
		if m.paint.lastCells != 1 {
			t.Errorf("changed cell after marking: %d rows %d cells", m.paint.lastRows, m.paint.lastCells)
		}
		tv.Dispose()
		if _, ok := paintSyncs[tv]; ok {
			t.Error("disposed table kept its paint sync")
		}
	})
}

// A reorder of the visible rows, which no longer resets the model, is
// announced once with EVENT_OBJECT_REORDER; value changes and scrolling are not.
func TestVisibleReorderIsAnnounced(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		_ = mw.SetLayout(walk.NewVBoxLayout())
		tv, err := walk.NewTableView(mw)
		if err != nil {
			panic(err)
		}
		defer delete(lvCache, tv)
		before := len(styledTables)
		defer func() { styledTables = styledTables[:before] }()
		for i := 0; i < 2; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(90)
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		m := &gridModel{keyCol: 0, sortCol: -1, tv: tv}
		m.rows = make([][]string, 200)
		for i := range m.rows {
			m.rows[i] = []string{fmt.Sprint("row", i), "a"}
		}
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		m.ready = true
		styleTable(tv)
		_ = mw.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 500})
		win.ShowWindow(win.HWND(mw.Handle()), win.SW_SHOWNOACTIVATE)
		_ = tv.SetBoundsPixels(walk.Rectangle{Width: 380, Height: 400})
		m.repaint()
		m.repaint() // visible at the previous repaint
		if !m.paint.shown || m.paint.rows < 6 {
			t.Log("window not visible in this session")
			return
		}
		events := m.paint.reorderEvents
		next := append([][]string(nil), m.rows...)
		next[1] = []string{"row1", "b"}
		m.update(next)
		if m.paint.reorderEvents != events {
			t.Error("a value change was announced as a reorder")
		}
		r := append([][]string(nil), m.rows...)
		r[3], r[4] = r[4], r[3]
		m.set(r)
		if m.paint.reorderEvents != events+1 {
			t.Errorf("visible swap raised %d reorder events, want 1 (window %d+%d ids %d valid %v count %d shown %v)", m.paint.reorderEvents-events,
				m.paint.top, m.paint.rows, len(m.paint.ids), m.paint.idsValid, m.paint.count, m.paint.shown)
		}
		m.repaint()
		r = append([][]string(nil), m.rows...)
		r[150], r[151] = r[151], r[150] // off screen
		m.set(r)
		for _, lv := range listViews(tv) {
			win.SendMessage(lv, win.LVM_SCROLL, 0, 200)
		}
		m.repaint()
		if m.paint.top == 0 {
			t.Error("fixture did not scroll")
		}
		if m.paint.reorderEvents != events+1 {
			t.Errorf("off-screen reorder or scrolling announced: %d events", m.paint.reorderEvents-events)
		}
	})
}
