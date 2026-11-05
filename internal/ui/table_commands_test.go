//go:build windows

package ui

import (
	"testing"
	"unsafe"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"

	"gotm/internal/metrics"
)

func TestCommandFeedbackDistinguishesPressAndHover(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 100, Height: 34}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		for _, kind := range []string{"plain", "toggled", "danger"} {
			b := newButton("Command", "", 100, nil)
			b.active, b.danger = kind == "toggled", kind == "danger"
			colors := make(map[win.COLORREF]bool)
			for _, state := range []string{"rest", "hover", "press", "disabled"} {
				b.v.hover, b.v.down = "", ""
				if state == "hover" || state == "press" {
					b.v.hover = "b"
				}
				if state == "press" {
					b.v.down = "b"
				}
				b.enabled = state != "disabled"
				b.draw(cv, 100, 34)
				color := win.GetPixel(cv.HDC(), 80, 10)
				if colors[color] {
					t.Errorf("%s command reuses a prior fill in %s", kind, state)
				}
				colors[color] = true
			}
		}
	})
}

func TestProcessCommandsFitNarrowTableBody(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		if err := a.procs.build().Create(decl.NewBuilder(a.right)); err != nil {
			panic(err)
		}
		for _, command := range []*button{a.procs.bPrio, a.procs.bGroup} {
			bar := command.v.cw.Parent().(*walk.Composite)
			// Hidden test windows have no effective visibility. Include every command,
			// including the expand and collapse commands exposed in tree mode.
			for i := 0; i < bar.Children().Len(); i++ {
				bar.Children().At(i).SetAlwaysConsumeSpace(true)
			}
			item := walk.CreateLayoutItemsForContainer(bar)
			dpi := bar.DPI()
			for _, width := range []int{408, 450, 650, 1020} {
				px := walk.IntFrom96DPI(width, dpi)
				item.Geometry().ClientSize = walk.Size{Width: px, Height: walk.IntFrom96DPI(38, dpi)}
				results := item.PerformLayout()
				if len(results) < 5 {
					t.Errorf("expected visible toolbar commands, got %d", len(results))
					return
				}
				right := 0
				for _, child := range results {
					b := child.Bounds
					if b.X < right || b.X+b.Width > px {
						t.Errorf("toolbar containing %s clips a control at width %d: %+v", command.text, width, b)
						break
					}
					right = b.X + b.Width
				}
			}
		}
	})
}

func TestGridClearFilterRestoresRowsFromKeyboard(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		p := a.pages[pgDrivers].impl.(*driversPage)
		if err := p.build().Create(decl.NewBuilder(a.right)); err != nil {
			panic(err)
		}
		p.all = []metrics.DriverInfo{{Name: "alpha"}, {Name: "beta"}}
		p.apply()
		bar := p.filter.Parent().(*walk.Composite)
		var clear *walk.CustomWidget
		for i := 0; i < bar.Children().Len(); i++ {
			if cw, ok := bar.Children().At(i).(*walk.CustomWidget); ok && cw.ToolTipText() == "Clear search (Esc)" {
				clear = cw
			}
		}
		if clear == nil || clear.Enabled() {
			t.Error("empty filter needs a disabled Clear command")
			return
		}
		if err := p.filter.SetText("alpha"); err != nil {
			panic(err)
		}
		if !clear.Enabled() || p.model.RowCount() != 1 {
			t.Error("typing failed to filter rows and enable Clear")
		}
		win.SendMessage(clear.Handle(), win.WM_KEYDOWN, uintptr(walk.KeySpace), 0)
		win.SendMessage(clear.Handle(), win.WM_KEYUP, uintptr(walk.KeySpace), 0)
		if p.filter.Text() != "" || p.model.RowCount() != 2 || clear.Enabled() {
			t.Error("activating Clear failed to restore both rows and its disabled state")
		}
	})
}

func TestNativeSelectionPaintUsesPalette(t *testing.T) {
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
		before := len(styledTables)
		defer func() { delete(lvCache, tv); styledTables = styledTables[:before] }()
		if err := tv.SetBounds(walk.Rectangle{Width: 320, Height: 120}); err != nil {
			panic(err)
		}
		column := walk.NewTableViewColumn()
		column.SetTitle("Name")
		column.SetWidth(240)
		if err := tv.Columns().Add(column); err != nil {
			panic(err)
		}
		model := &gridModel{tv: tv, keyCol: 0, sortCol: -1, rows: [][]string{{"Alpha"}, {"Bravo"}}}
		if err := tv.SetModel(model); err != nil {
			panic(err)
		}
		styleTable(tv)
		if err := tv.SetCurrentIndex(1); err != nil {
			panic(err)
		}
		bmp, err := walk.NewBitmapForDPI(tv.SizePixels(), tv.DPI())
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		for _, lv := range listViews(tv) {
			if win.SendMessage(lv, win.LVM_GETCOLUMNWIDTH, 0, 0) == 0 {
				continue
			}
			var row win.RECT
			if win.SendMessage(lv, win.LVM_GETITEMRECT, 1, uintptr(unsafe.Pointer(&row))) == 0 {
				panic("missing selected item")
			}

			win.SendMessage(lv, win.WM_PRINTCLIENT, uintptr(cv.HDC()), 4 /* PRF_CLIENT */)
			// Probe beyond the caption. The themed list view can cover the cell
			// styler's selection background even when its model values are right.
			if got := uintptr(win.GetPixel(cv.HDC(), row.Right-20, (row.Top+row.Bottom)/2)); got != colorref(T.RowSel) {
				t.Errorf("native selected fill = %#06x, want palette %#06x", got, colorref(T.RowSel))
			}
			if win.SendMessage(lv, win.LVM_GETITEMSTATE, 1, win.LVIS_SELECTED)&win.LVIS_SELECTED == 0 {
				t.Error("native selection lost after paint")
			}
			return
		}
		t.Error("missing normal list view")
	})
}
