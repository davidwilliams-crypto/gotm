//go:build windows

package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"gotm/internal/config"
	"gotm/internal/metrics"
)

func TestShellActionsFitShortWindow(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 512, Height: 400}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		a := &App{side: new(canvasView), pages: make([]*pageDef, pgCount), opening: &pageConstruction{id: pgDrivers}}
		for i := range a.pages {
			a.pages[i] = &pageDef{title: fmt.Sprintf("Page %d", i), initialized: true}
		}
		a.pages[pgDrivers].initialized = false
		a.paintSidebar(cv, 168, 380)
		if len(a.side.hits) != pgCount {
			t.Errorf("sidebar has %d actions", len(a.side.hits))
		}
		for _, hit := range a.side.hits {
			if hit.r.H < 20 || hit.r.Y < 0 || hit.r.Y+hit.r.H > 380 {
				t.Errorf("action outside short window: %+v", hit)
			}
		}
		pending := false
		for _, item := range a.side.items {
			if item.id == fmt.Sprintf("nav:%d", pgDrivers) {
				pending = strings.Contains(strings.ToLower(item.name), "opening") && !item.selected
			}
		}
		if !pending {
			t.Error("pending navigation needs its own accessible state")
		}
	})
}

func TestFooterActionsAndTextStaySeparate(t *testing.T) {
	runNativeTest(t, func() {
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 1200, Height: 32}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		a := &App{foots: []*canvasView{{}}, rec: metrics.NewRecorder(time.Hour, time.Second, ""), snap: &metrics.Snapshot{}}
		for _, width := range []int{360, 480, 700, 1200} {
			v := a.foots[0]
			v.items, v.hits = nil, nil
			a.paintFooter(cv, width, 32, 0)
			if len(v.hits) != 2 {
				t.Errorf("width %d: want recorder and elevation actions", width)
				continue
			}
			right := 0
			for _, item := range v.items {
				if item.r.W <= 0 || item.r.X < right || item.r.X+item.r.W > width {
					t.Errorf("width %d: overlapping or clipped status item %+v", width, item)
				}
				right = item.r.X + item.r.W
			}
		}
	})
}

func TestCompactSettingsActionsStayInReach(t *testing.T) {
	dir := t.TempDir()
	runNativeTest(t, func() {
		a := &App{cfg: config.Default(), store: config.OpenAt(dir), sensors: metrics.NewSensorHubDeferred()}
		p := newSettingsTab(a)
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 450, Height: 360}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		p.paint(cv, 450, 360)
		for _, h := range p.v.hits {
			if h.r.X < 0 || h.r.X+h.r.W > 450 {
				t.Errorf("clipped control %s at %+v", h.id, h.r)
			}
		}
		p.v.keyHit = "quit"
		if p.pageKey(walk.KeyTab, 0) {
			t.Error("Tab should leave the last Settings control")
		}
	})
}

func TestSidebarUsesSystemSelectionInHighContrast(t *testing.T) {
	runNativeTest(t, func() {
		original, mode := T, highContrast
		defer func() { T, highContrast = original, mode }()
		T, highContrast = highContrastPalette(), true
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 204, Height: 540}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		a := &App{side: new(canvasView), pages: make([]*pageDef, pgCount)}
		for i := range a.pages {
			a.pages[i] = &pageDef{title: "Page", initialized: true}
		}
		a.paintSidebar(cv, 204, 540)
		cv.Dispose()
		im, err := bmp.ToImage()
		if err != nil {
			panic(err)
		}
		row := a.side.hits[pgPerformance].r
		if !hasColorNear(im, row.X+row.W-12, row.Y+row.H/2, sysColor(colorHighlight), 0) {
			t.Error("the selected page did not use Windows' selection background")
		}
	})
}

func TestSearchAfterStagedProcessNavigation(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		before := len(styledTables)
		defer func() {
			delete(lvCache, a.procs.tv)
			styledTables = styledTables[:before]
		}()
		a.cfg = config.Default()
		a.snap = &metrics.Snapshot{Procs: []metrics.Proc{{Name: "alpha.exe", PID: 10}, {Name: "bravo.exe", PID: 11}}}
		a.navigate(pgProcesses)
		pumpNativeUntil(t, func() bool { return a.opening == nil })
		p := a.procs
		// Exercise the native edit notification path after lazy construction.
		for _, ch := range "bravo" {
			win.SendMessage(p.filter.Handle(), win.WM_CHAR, uintptr(ch), 1)
		}
		if p.filter.Text() != "bravo" || p.model.RowCount() != 1 || p.model.rows[0].PID != 11 {
			t.Errorf("native filter %q produced %d rows", p.filter.Text(), p.model.RowCount())
		}
		win.SendMessage(p.filter.Handle(), win.WM_KEYDOWN, uintptr(walk.KeyEscape), 1)
		if p.filter.Text() != "" || p.model.RowCount() != 2 {
			t.Error("Escape did not restore the complete process table")
		}
	})
}
