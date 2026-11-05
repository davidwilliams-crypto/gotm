//go:build windows

package ui

import (
	"testing"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"gotm/internal/metrics"
)

// Navigating away after a page's init step but before its rows/show/focus
// steps must keep the initialized page's controls: a later visit used to find
// nil fields (and panic in focusPage).
func TestCancelAfterInitKeepsInitializedPage(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		a.snap = &metrics.Snapshot{Procs: []metrics.Proc{{PID: 4, Name: "System"}, {PID: 10, Name: "a.exe"}}}
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		home := &lazyTestPage{app: a, id: pgPerformance}
		a.pages[pgPerformance] = &pageDef{title: "Performance", impl: home}
		if e := a.ensurePage(pgPerformance); e != nil {
			panic(e)
		}
		a.navigate(pgProcesses)
		pumpNativeUntil(t, func() bool { return a.opening == nil || a.pages[pgProcesses].initialized })
		// No t.Fatal/t.Skip here: they would Goexit the shared native-test
		// goroutine and hang every later native test.
		if a.opening == nil {
			t.Log("build finished in the same callback as init; nothing to cancel")
			return
		}
		a.navigate(pgPerformance) // cancels the remaining Processes steps
		if !a.pages[pgProcesses].initialized || a.procs.tv == nil || a.procs.filter == nil || a.wraps[pgProcesses] == nil {
			t.Errorf("cancelled build orphaned an initialized page: tv=%v filter=%v wrap=%v",
				a.procs.tv != nil, a.procs.filter != nil, a.wraps[pgProcesses] != nil)
			return
		}
		a.navigate(pgProcesses) // must not panic and must show the same page
		pumpNativeUntil(t, func() bool { return a.opening == nil })
		if a.cur != pgProcesses || a.procs.tv == nil {
			t.Errorf("revisit failed: cur=%d tv=%v", a.cur, a.procs.tv != nil)
		}
		// The cancelled build never ran its rows step: the first visit's tick
		// must still fill the table.
		if len(a.procs.model.rows) != 2 {
			t.Errorf("first visit after a cancelled rows step shows %d rows, want 2", len(a.procs.model.rows))
		}
	})
}

// ownVisible reports a page wrap's own visibility (WS_VISIBLE).
func ownVisible(w *walk.Composite) bool { return w != nil && windowShown(w) }

// A build cancelled after its "show" step (the page is visible and current)
// but before "focus", by navigating to a page that is not built yet: the
// revealed page stays on screen until the next one reveals itself, so it must
// be activated (shown, its snapshot) rather than left visible but inert; once
// the next page is built, it alone is visible and current.
func TestCancelAfterShowActivatesRevealedPage(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		a.snap = &metrics.Snapshot{Procs: []metrics.Proc{{PID: 4, Name: "System"}, {PID: 10, Name: "a.exe"}}}
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		home := &lazyTestPage{app: a, id: pgPerformance}
		a.pages[pgPerformance] = &pageDef{title: "Performance", impl: home}
		// showPage hides other pages only when they are visible on screen.
		_ = mw.SetLayout(walk.NewVBoxLayout())
		_ = a.right.SetLayout(walk.NewVBoxLayout())
		win.ShowWindow(win.HWND(mw.Handle()), win.SW_SHOWNOACTIVATE)
		a.navigate(pgPerformance)
		if !pumpNativeUntilOK(func() bool { return a.opening == nil || a.opening.revealed }) {
			t.Error("build did not reach its show step")
			return
		}
		if a.opening == nil {
			t.Log("build finished in the same callback as show; nothing to cancel")
			return
		}
		if home.shows != 0 {
			t.Errorf("page activated before its focus step: %d", home.shows)
		}
		a.navigate(pgProcesses) // cancels Performance's focus step
		if a.cur != pgPerformance || !ownVisible(a.wraps[pgPerformance]) {
			t.Errorf("revealed page lost while the next builds: cur=%d visible=%v", a.cur, ownVisible(a.wraps[pgPerformance]))
		}
		if home.shows != 1 {
			t.Errorf("revealed page was activated %d times, want 1", home.shows)
		}
		if !pumpNativeUntilOK(func() bool { return a.opening == nil }) {
			t.Error("next build did not finish")
			return
		}
		if a.cur != pgProcesses || !ownVisible(a.wraps[pgProcesses]) || ownVisible(a.wraps[pgPerformance]) {
			t.Errorf("after the next build: cur=%d processes visible=%v performance visible=%v",
				a.cur, ownVisible(a.wraps[pgProcesses]), ownVisible(a.wraps[pgPerformance]))
		}
		if len(a.procs.model.rows) != 2 {
			t.Errorf("processes page shows %d rows", len(a.procs.model.rows))
		}
		// Back to the cancelled page: activated again, nothing rebuilt.
		a.navigate(pgPerformance)
		if a.cur != pgPerformance || home.builds != 1 || home.shows != 2 || ownVisible(a.wraps[pgProcesses]) {
			t.Errorf("revisit: cur=%d builds=%d shows=%d", a.cur, home.builds, home.shows)
		}
	})
}

// The rows a page builds while hidden are drawn by its exposure: the first
// tick after the page is shown repaints only what changed, not every row.
func TestFirstTickAfterShowRepaintsNothingNew(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		_ = mw.SetLayout(walk.NewVBoxLayout())
		mw.SetFont(uiFont(10))
		a := navigationTestApp(mw)
		a.right.SetLayout(walk.NewVBoxLayout())
		procs := make([]metrics.Proc, 120)
		for i := range procs {
			procs[i] = metrics.Proc{PID: uint32(8 + 4*i), Name: "worker.exe", Mem: uint64(i) << 20}
		}
		a.snap = &metrics.Snapshot{Procs: procs}
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		_ = mw.SetBoundsPixels(walk.Rectangle{Width: 1100, Height: 800})
		win.ShowWindow(win.HWND(mw.Handle()), win.SW_SHOWNOACTIVATE)
		a.navigate(pgProcesses)
		if !pumpNativeUntilOK(func() bool { return a.opening == nil }) {
			t.Error("build did not finish")
			return
		}
		p := a.procs
		if !p.paint.valid || p.paint.rows == 0 {
			t.Log("table has no visible rows in this session")
			return
		}
		// The focus step delivered the (unchanged) snapshot through tick.
		if p.paint.lastRows != 0 || p.paint.lastCells != 0 {
			t.Errorf("first tick after show repainted %d rows %d cells", p.paint.lastRows, p.paint.lastCells)
		}
		until := time.Now().Add(150 * time.Millisecond)
		pumpNativeUntilOK(func() bool { return time.Now().After(until) })
		p.refresh() // the next tick, same snapshot
		if p.paint.lastRows != 0 || p.paint.lastCells != 0 {
			t.Errorf("first tick after show repainted %d rows %d cells", p.paint.lastRows, p.paint.lastCells)
		}
	})
}

// Closing the window mid-build stops the build: no step runs afterwards and
// the partial page's controls are released.
func TestWindowCloseMidBuildStopsBuild(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		a.snap = &metrics.Snapshot{Procs: []metrics.Proc{{PID: 4, Name: "System"}}}
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		a.navigate(pgProcesses)
		if !pumpNativeUntilOK(func() bool { return a.procs.tv != nil || a.opening == nil }) {
			t.Error("build did not start")
			return
		}
		b := a.opening
		h := win.HWND(mw.Handle())
		partial := !a.pages[pgProcesses].initialized
		mw.Dispose()
		if a.opening != nil {
			t.Error("closing the window left the build pending")
		}
		if _, ok := pageBuilders[h]; ok {
			t.Error("closed window's builder retained")
		}
		if partial && (a.procs.tv != nil || a.wraps[pgProcesses] != nil) {
			t.Error("partial page kept its controls after the window closed")
		}
		if b == nil {
			return
		}
		ran := len(b.timings)
		until := time.Now().Add(100 * time.Millisecond)
		pumpNativeUntilOK(func() bool { return time.Now().After(until) })
		if len(b.timings) != ran {
			t.Errorf("%d build steps ran after the window closed", len(b.timings)-ran)
		}
	})
}
