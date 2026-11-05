//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
	"gotm/internal/metrics"
	"syscall"
	"testing"
	"time"
)

func navigationTestApp(mw *walk.MainWindow) *App {
	right, e := walk.NewComposite(mw)
	if e != nil {
		panic(e)
	}
	a := &App{mw: mw, right: right, pages: make([]*pageDef, pgCount), wraps: make([]*walk.Composite, pgCount), heads: make([]*canvasView, pgCount), foots: make([]*canvasView, pgCount), side: newCanvasView(func(*walk.Canvas, int, int) {}), sensors: metrics.NewSensorHubDeferred()}
	for i := 0; i < pgCount; i++ {
		a.heads[i] = newCanvasView(func(*walk.Canvas, int, int) {})
		a.foots[i] = newCanvasView(func(*walk.Canvas, int, int) {})
	}
	a.procs = newProcsTab(a)
	a.pages[pgProcesses] = &pageDef{title: "Processes", impl: a.procs}
	a.pages[pgDrivers] = &pageDef{title: "Drivers", impl: newDriversPage(a)}
	return a
}
func TestNativeTableOpeningProfile(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		mw.SetFont(uiFont(10))
		a := navigationTestApp(mw)
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		for _, id := range []int{pgProcesses, pgDrivers} {
			start := time.Now()
			if e := a.pageWidget(id).Create(decl.NewBuilder(a.right)); e != nil {
				panic(e)
			}
			built := time.Now()
			a.pages[id].impl.init()
			t.Logf("%s native create=%s init/style=%s", a.pages[id].title, built.Sub(start), time.Since(built))
		}
	})
}

func pumpNativeUntil(t *testing.T, ready func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for !ready() {
		var msg win.MSG
		if win.PeekMessage(&msg, 0, 0, 0, win.PM_REMOVE) {
			win.TranslateMessage(&msg)
			win.DispatchMessage(&msg)
		} else {
			time.Sleep(time.Millisecond)
		}
		if time.Now().After(until) {
			t.Fatal("native navigation did not complete")
		}
	}
}
func TestNativeTableNavigationYieldsToMessages(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		mw.SetFont(uiFont(10))
		a := navigationTestApp(mw)
		a.snap = &metrics.Snapshot{Procs: make([]metrics.Proc, 2000)}
		for i := range a.snap.Procs {
			a.snap.Procs[i] = metrics.Proc{PID: uint32(i + 10), Name: "worker.exe", CPU: float64(i % 100)}
		}
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		const probe = win.WM_APP + 97
		h := win.HWND(mw.Handle())
		var prev uintptr
		during := 0
		callback := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
			if msg == probe {
				if a.opening != nil && a.wraps[pgProcesses] != nil && !a.pages[pgProcesses].initialized {
					during++
				}
				return 0
			}
			return win.CallWindowProc(prev, win.HWND(hwnd), uint32(msg), wp, lp)
		})
		prev = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, callback)
		defer win.SetWindowLongPtr(h, win.GWLP_WNDPROC, prev)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
					win.PostMessage(h, probe, 0, 0)
				}
			}
		}()
		start := time.Now()
		a.navigate(pgProcesses)
		build := a.opening
		if build == nil || a.wraps[pgProcesses] != nil {
			t.Fatal("navigation constructed native controls synchronously")
		}
		pumpNativeUntil(t, func() bool { return a.opening == nil })
		if !a.pages[pgProcesses].initialized || a.procs.tv.Columns().Len() != len(a.procs.model.cols) {
			t.Fatal("staged table incomplete")
		}
		if during == 0 {
			t.Fatal("native window messages were not dispatched between table creation stages")
		}
		t.Logf("staged Processes total=%s longest callback=%s dispatched probes=%d", time.Since(start), build.maxSlice, during)
	})
}
func TestNativeNavigationCancelsPartialTable(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
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
		pumpNativeUntil(t, func() bool { return a.procs.tv != nil })
		if a.pages[pgProcesses].initialized {
			t.Fatal("fixture did not pause during native table construction")
		}
		partial, table := a.wraps[pgProcesses], a.procs.tv
		// Settings and palette changes can arrive while native columns are partial.
		a.cfg.Columns = []string{"cpu", "gpu"}
		a.procs.applyColumns()
		old := T
		defer func() { T = old }()
		T.Bg = walk.RGB(248, 248, 248)
		a.navigate(pgPerformance)
		if a.opening != nil || a.wraps[pgProcesses] != nil || !partial.IsDisposed() || !table.IsDisposed() || a.procs.tv != nil || a.procs.filter != nil {
			t.Fatal("abandoned native page retained assigned controls")
		}
		if _, ok := pageBuilders[win.HWND(mw.Handle())]; ok {
			t.Fatal("abandoned builder retained")
		}
		a.navigate(pgProcesses)
		pumpNativeUntil(t, func() bool { return a.opening == nil })
		if a.procs.tv == nil || a.procs.tv == table || !a.procs.model.ready || a.procs.tv.Columns().Len() != len(a.procs.model.cols) {
			t.Fatal("reopened table incomplete")
		}
		for i, c := range a.procs.model.cols {
			want := c.key == "name" || c.key == "cpu" || c.key == "gpu"
			if a.procs.tv.Columns().At(i).Visible() != want {
				t.Fatalf("column %s lost pending setting", c.key)
			}
		}
		if bg, ok := a.procs.root.Background().(*walk.SolidColorBrush); !ok || bg.Color() != T.Bg {
			t.Fatal("reopened page lost current theme")
		}
	})
}
func TestDirectTableStartupBuildsShellBeforeTable(t *testing.T) {
	runNativeTest(t, func() {
		mw, e := walk.NewMainWindow()
		if e != nil {
			panic(e)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		if e := a.initialPageWidget(pgProcesses).Create(decl.NewBuilder(a.right)); e != nil {
			panic(e)
		}
		if a.procs.tv != nil || a.wraps[pgProcesses] != nil || a.loading.cw == nil {
			t.Fatal("direct table startup constructed more than shell")
		}
		// Shortcut dispatch must be harmless until the target controls exist.
		a.focusSearch()
		a.focusPage()
		a.copyRows()
		a.navigate(pgProcesses)
		if a.procs.tv != nil || a.pages[pgProcesses].initialized {
			t.Fatal("direct table launch blocked on native controls")
		}
		a.cancelPageBuild()
	})
}
