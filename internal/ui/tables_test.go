//go:build windows

package ui

import (
	"fmt"
	"testing"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

// Each refresh enumerates list views. Go allows only ~2000 callbacks per
// process and aborts the program past that, so the enumeration must reuse one.
func TestChildEnumerationDoesNotExhaustCallbacks(t *testing.T) {
	for i := 0; i < 2500; i++ {
		_ = childrenOfClass(0, "SysListView32") // parent 0 = top-level windows; cheap
	}
}

func TestSelectQuietlyKeepsToolkitAndNativeSelectionWithoutScrolling(t *testing.T) {
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
		if err := tv.SetBoundsPixels(walk.Rectangle{Width: 400, Height: 200}); err != nil {
			panic(err)
		}
		for i := 0; i < 2; i++ {
			c := walk.NewTableViewColumn()
			c.SetTitle(fmt.Sprint(i))
			c.SetWidth(150)
			if i == 0 {
				c.SetFrozen(true)
			}
			if err := tv.Columns().Add(c); err != nil {
				panic(err)
			}
		}
		m := &gridModel{keyCol: -1, sortCol: -1, rows: make([][]string, 200)}
		for i := range m.rows {
			m.rows[i] = []string{fmt.Sprint(i), "value"}
		}
		if err := tv.SetModel(m); err != nil {
			panic(err)
		}
		lvs := listViews(tv)
		if len(lvs) != 2 {
			panic(fmt.Sprintf("need frozen and normal native list views, got %d", len(lvs)))
		}
		if err := tv.SetCurrentIndex(150); err != nil {
			panic(err)
		}
		// Reset clears Walk's current index. Restore a row outside the viewport,
		// exactly as a reordered table does while the reader stays at its scroll.
		m.PublishRowsReset()
		for _, lv := range lvs {
			win.SendMessage(lv, win.LVM_ENSUREVISIBLE, 0, 0)
		}
		if tv.CurrentIndex() != -1 {
			t.Error("reset did not clear Walk's current index")
		}
		assertSelection := func(row int) {
			t.Helper()
			tops := make([]uintptr, len(lvs))
			for j, lv := range lvs {
				tops[j] = win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0)
			}
			selectQuietly(tv, row)
			if got := tv.CurrentIndex(); got != row {
				t.Errorf("Walk CurrentIndex=%d, want %d", got, row)
			}
			if selected := tv.SelectedIndexes(); (row == -1 && len(selected) != 0) || (row >= 0 && (len(selected) != 1 || selected[0] != row)) {
				t.Errorf("Walk SelectedIndexes=%v, want row %d", selected, row)
			}
			for j, lv := range lvs {
				if got := win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0); got != tops[j] {
					t.Errorf("list %d scrolled from %d to %d selecting %d", j, tops[j], got, row)
				}
				got := int32(win.SendMessage(lv, win.LVM_GETNEXTITEM, ^uintptr(0), win.LVNI_SELECTED))
				if int(got) != row {
					t.Errorf("list %d selected=%d, want %d", j, got, row)
				}
			}
		}
		assertSelection(150) // offscreen after reset
		assertSelection(2)   // visible, removing the previous selection
		assertSelection(-1)  // explicitly clear selection
		if len(quietSelectPrev) != 0 {
			t.Error("selection left a temporary native hook installed")
		}
		if err := tv.SetCurrentIndex(150); err != nil {
			panic(err)
		}
		for j, lv := range lvs {
			if win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0) == 0 {
				t.Errorf("list %d normal selection stopped scrolling after quiet selection", j)
			}
		}
	})
}
