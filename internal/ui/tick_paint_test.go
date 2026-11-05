//go:build windows

package ui

import (
	"fmt"
	"math/rand"
	"syscall"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"gotm/internal/metrics"
)

// TestProcessTickPaintCost compares, on a shown Processes table, the native
// paint work of one tick when every visible row is redrawn (the previous
// behaviour) with the changed-cells repaint. It logs the numbers; it asserts
// only that the diff paints less.
func TestProcessTickPaintCost(t *testing.T) {
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
		before := len(styledTables)
		defer func() {
			for _, tv := range styledTables[before:] {
				delete(lvCache, tv)
			}
			styledTables = styledTables[:before]
		}()
		r := rand.New(rand.NewSource(3))
		procs := make([]metrics.Proc, 230)
		for i := range procs {
			procs[i] = metrics.Proc{PID: uint32(4 * (i + 1)), Name: fmt.Sprintf("Program%03d.exe", i), Mem: uint64(r.Intn(1 << 28)),
				Threads: uint32(r.Intn(50)), Handles: uint32(r.Intn(900)), Prio: "Normal", Start: 1}
		}
		a.cfg.Columns = []string{"pid", "cpu", "mem", "private", "io", "gpu", "threads", "handles", "user"}
		a.cfg.SortKey, a.cfg.SortDesc = "name", false
		a.snap = &metrics.Snapshot{Procs: procs}
		a.procs = newProcsTab(a)
		a.pages[pgProcesses].impl = a.procs
		if e := a.ensurePage(pgProcesses); e != nil {
			panic(e)
		}
		a.wraps[pgProcesses].SetVisible(true)
		_ = mw.SetBoundsPixels(walk.Rectangle{Width: 1200, Height: 800})
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
		p := a.procs
		p.refresh()
		pump(200)

		cells := 0
		h := win.HWND(p.tv.Handle())
		var prev uintptr
		cb := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
			if msg == win.WM_NOTIFY && lp != 0 && (*win.NMHDR)(ptrFrom(lp)).Code == win.NM_CUSTOMDRAW {
				if (*win.NMLVCUSTOMDRAW)(ptrFrom(lp)).Nmcd.DwDrawStage == win.CDDS_ITEMPREPAINT|win.CDDS_SUBITEM {
					cells++
				}
			}
			return win.CallWindowProc(prev, win.HWND(hwnd), uint32(msg), wp, lp)
		})
		prev = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, cb)
		defer win.SetWindowLongPtr(h, win.GWLP_WNDPROC, prev)

		// An idle-ish machine: a few processes use some CPU each tick.
		tick := func() {
			next := append([]metrics.Proc(nil), procs...)
			for k := 0; k < 12; k++ {
				i := r.Intn(len(next))
				next[i].CPU = float64(r.Intn(400)) / 10
				next[i].Mem += uint64(r.Intn(1 << 22))
			}
			a.snap = &metrics.Snapshot{Procs: next}
		}
		measure := func(name string, paintAll bool) (int, time.Duration) {
			cells = 0
			var spent time.Duration
			const ticks = 20
			for i := 0; i < ticks; i++ {
				tick()
				start := time.Now()
				p.refresh()
				if paintAll {
					redrawVisible(p.tv)
				}
				for _, lv := range listViews(p.tv) {
					win.UpdateWindow(lv)
				}
				spent += time.Since(start)
				pump(5)
			}
			t.Logf("%s: %d sub-item paints/tick, %s refresh+paint per tick", name, cells/ticks, spent/ticks)
			return cells / ticks, spent / ticks
		}
		all, _ := measure("redraw every visible row (before)", true)
		diff, _ := measure("changed cells only (after)", false)
		if all == 0 {
			t.Log("window did not paint in this session")
			return
		}
		if diff >= all {
			t.Errorf("diff painted %d cells per tick, full redraw %d", diff, all)
		}
	})
}
