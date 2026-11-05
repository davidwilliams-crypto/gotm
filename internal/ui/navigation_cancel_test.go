//go:build windows

package ui

import (
	"testing"

	"github.com/tailscale/walk"
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
	})
}
