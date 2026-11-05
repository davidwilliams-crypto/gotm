//go:build windows

package ui

import (
	"fmt"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"

	"gotm/internal/metrics"
)

const pageBuildTimer = 0x474f544d

var pageTimerOnce sync.Once
var pageTimerCallback uintptr
var pageBuilders = map[win.HWND]*App{} // UI-thread owned, only while a build is pending

// buildStep is one unit of staged page construction. An alone step runs in a
// timer callback of its own: the build yields to pending input and paint
// both before and after it.
type buildStep struct {
	name  string
	alone bool
	run   func() error
}

// stepTiming records how long one construction step took (profiling, tests).
type stepTiming struct {
	name  string
	took  time.Duration
	slice int // index of the timer callback that ran it
}

type pageConstruction struct {
	id       int
	steps    []buildStep
	maxSlice time.Duration
	slices   int
	timings  []stepTiming
	palette  palette
	clears   []func()
	revealed bool // the show step made the page visible
	posted   bool // a posted continuation is pending
}

// prefiller is a page that can build its first rows while still hidden.
type prefiller interface{ prefill() }

// paintSyncer is a table page that can record its visible rows as painted
// once they were exposed (paintDiff.markPainted).
type paintSyncer interface{ syncPaint() }

// sliceBudget is how long one timer callback keeps running small steps.
const sliceBudget = 4 * time.Millisecond

// widget splits our declarative composites and table columns into native
// operations. Each Builder still finishes its own properties and deferred work.
func (b *pageConstruction) widget(parent walk.Container, widget decl.Widget) buildStep {
	step := buildStep{name: fmt.Sprintf("%T", widget)}
	if _, ok := widget.(decl.TableView); ok {
		step.alone = true // the native list views: the largest single step
	}
	step.run = func() error {
		// Every AssignTo published during a cancelled build must become nil again;
		// background recorder state changes may otherwise touch disposed canvases.
		value := reflect.ValueOf(widget)
		if value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
		var assigned reflect.Value
		if value.IsValid() && value.Kind() == reflect.Struct {
			assigned = value.FieldByName("AssignTo")
		}
		if assigned.IsValid() && assigned.Kind() == reflect.Pointer && !assigned.IsNil() && assigned.Elem().CanSet() {
			target := assigned.Elem()
			b.clears = append(b.clears, func() { target.SetZero() })
		}
		var next []buildStep
		switch w := widget.(type) {
		case decl.Composite:
			children, assign := w.Children, w.AssignTo
			var created *walk.Composite
			w.Children = nil
			w.AssignTo = &created
			if err := w.Create(decl.NewBuilder(parent)); err != nil {
				return err
			}
			if assign != nil {
				*assign = created
			}
			for _, child := range children {
				next = append(next, b.widget(created, child))
			}
		case decl.TableView:
			// Custom per-column style adapters are assembled by Walk at declaration
			// time; leave those uncommon declarations whole to preserve that contract.
			styled := false
			for _, c := range w.Columns {
				styled = styled || c.StyleCell != nil
			}
			if w.AssignTo == nil || styled {
				return w.Create(decl.NewBuilder(parent))
			}
			cols := w.Columns
			w.Columns = nil
			if err := w.Create(decl.NewBuilder(parent)); err != nil {
				return err
			}
			tv := *w.AssignTo
			for _, column := range cols {
				column := column
				next = append(next, buildStep{name: "column " + column.Name + column.Title, run: func() error { return column.Create(tv) }})
			}
		default:
			return widget.Create(decl.NewBuilder(parent))
		}
		b.steps = append(next, b.steps...)
		return nil
	}
	return step
}

func (a *App) startPageBuild(i int) {
	if old := a.abortPageBuild(); old != nil {
		a.settleCancelled(old)
	}
	b := &pageConstruction{id: i, palette: T}
	a.opening = b
	b.steps = []buildStep{b.widget(a.right, a.pageWidget(i)), {name: "init", alone: true, run: func() error {
		// A palette switch during construction must also update declarations that
		// were captured before the switch, without waiting for another theme event.
		if b.palette != T {
			setBackground(a.wraps[i], T.Bg)
			rethemeChildren(a.wraps[i])
		}
		a.pages[i].impl.init()
		a.pages[i].initialized = true
		return nil
	}}}
	if p, ok := a.pages[i].impl.(prefiller); ok {
		// The first rows are built while the page is still hidden, in their own
		// callback, so revealing it later only repaints.
		b.steps = append(b.steps, buildStep{name: "rows", alone: true, run: func() error {
			if a.snap != nil {
				p.prefill()
			}
			return nil
		}})
	}
	b.steps = append(b.steps,
		buildStep{name: "show", alone: true, run: func() error {
			b.revealed = true
			a.revealPage(i)
			// Its tables' rows were built while hidden; the exposure paint that
			// is now pending draws them all, so record them as painted rather
			// than repainting every visible row again on the next tick.
			if p, ok := a.pages[i].impl.(paintSyncer); ok {
				p.syncPaint()
			}
			return nil
		}},
		buildStep{name: "focus", alone: true, run: func() error { a.showPage(i); return nil }},
	)
	if !a.pageBuildHooked {
		a.pageBuildHooked = true
		a.mw.Disposing().Attach(a.cancelPageBuild)
	}
	a.side.invalidate()
	a.invalidateChrome()
	if a.loading != nil {
		a.loading.invalidate()
	}
	a.schedulePageBuild()
}

// revealPage makes a freshly built page visible and lays it out, the first
// half of showPage; the later "focus" step runs showPage itself, which then
// finds visibility and layout already done and only activates the page
// (shown, the current snapshot, focus and status).
func (a *App) revealPage(i int) {
	if a.loading != nil && a.loading.cw != nil {
		a.loading.cw.SetVisible(false)
	}
	a.cur = i
	for k, w := range a.wraps {
		// Compare the page's own visibility: Walk's Visible is false for every
		// page while the main window itself is hidden (tray, start-up), which
		// would leave the previous page shown alongside this one.
		if w != nil && windowShown(w) != (k == i) {
			w.SetVisible(k == i)
		}
	}
	a.mw.RequestImmediateLayout()
}

// windowShown reports whether w itself is shown (WS_VISIBLE), whether or not
// its ancestors are.
func windowShown(w walk.Window) bool {
	return win.GetWindowLong(win.HWND(w.Handle()), win.GWL_STYLE)&win.WS_VISIBLE != 0
}

func (a *App) schedulePageBuild() {
	pageTimerOnce.Do(func() {
		pageTimerCallback = syscall.NewCallback(func(hwnd, msg, id, now uintptr) (ret uintptr) {
			defer uiaGuard(&ret, 0)
			h := win.HWND(hwnd)
			win.KillTimer(h, pageBuildTimer)
			if a := pageBuilders[h]; a != nil {
				a.continuePageBuild()
			}
			return 0
		})
	})
	h := win.HWND(a.mw.Handle())
	pageBuilders[h] = a
	// WM_TIMER is lower priority than input and paint. Chained posted callbacks
	// alone would monopolize the posted-message queue and starve those messages.
	// A timer callback, though, waits for the next system timer tick (about
	// 16 ms) even on an idle queue. So when no input or paint is waiting, the
	// next step is posted instead: it still runs after every message already
	// queued, and the check is repeated before each step.
	if b := a.opening; b != nil && win.GetQueueStatus(win.QS_INPUT|win.QS_PAINT)>>16 == 0 {
		if !b.posted {
			b.posted = true
			a.mw.Synchronize(func() {
				b.posted = false
				if a.opening == b && pageBuilders[h] == a && !a.mw.IsDisposed() {
					win.KillTimer(h, pageBuildTimer)
					a.continuePageBuild()
				}
			})
		}
		return
	}
	win.SetTimer(h, pageBuildTimer, 1, pageTimerCallback)
}

func (a *App) continuePageBuild() {
	b := a.opening
	if b == nil || a.mw.IsDisposed() {
		return
	}
	start := time.Now()
	slice := b.slices
	b.slices++
	defer func() {
		if elapsed := time.Since(start); elapsed > b.maxSlice {
			b.maxSlice = elapsed
		}
	}()
	ran := 0
	for len(b.steps) > 0 {
		step := b.steps[0]
		if step.alone && ran > 0 {
			break // yield before a large step
		}
		b.steps[0] = buildStep{}
		b.steps = b.steps[1:]
		began := time.Now()
		err := step.run()
		b.timings = append(b.timings, stepTiming{name: step.name, took: time.Since(began), slice: slice})
		ran++
		if err != nil {
			a.cancelPageBuild()
			a.errBox("Open page", err)
			return
		}
		if a.opening != b {
			return // the step navigated elsewhere or cancelled the build
		}
		if a.wraps[b.id] != nil && !b.revealed {
			a.wraps[b.id].SetVisible(false)
		}
		if step.alone || time.Since(start) >= sliceBudget {
			break // yield after a large step or a full slice
		}
	}
	if len(b.steps) > 0 {
		a.schedulePageBuild()
		return
	}
	delete(pageBuilders, win.HWND(a.mw.Handle()))
	a.opening = nil
	a.side.invalidate()
	a.invalidateChrome()
}
func (a *App) cancelPageBuild() { a.abortPageBuild() }

// settleCancelled finishes the state a cancelled build left behind when
// another page's build replaces it. A build cancelled after its "show" step
// already made its page visible and current (a.cur), and that page stays on
// screen until the next page reveals itself (or for good, if that build
// fails) — exactly as an already-built page stays while another one builds.
// Only its "focus" step did not run, so activate it the way showPage does:
// sensors for that page, shown() and the current snapshot. Keyboard focus is
// left alone: the reader has just chosen another page.
func (a *App) settleCancelled(b *pageConstruction) {
	i := b.id
	if !b.revealed || a.cur != i || !a.pages[i].initialized || a.wraps[i] == nil || a.mw.IsDisposed() {
		return
	}
	metrics.SetGPUSensorsWanted(a.gpuSensorsWanted())
	a.sensors.SetDetailWanted(i == pgPower)
	a.pages[i].impl.shown()
	if a.snap != nil {
		a.pages[i].impl.tick(a.snap)
	}
}

// abortPageBuild stops the pending build, if any, and returns it.
func (a *App) abortPageBuild() *pageConstruction {
	if a.mw != nil {
		h := win.HWND(a.mw.Handle())
		win.KillTimer(h, pageBuildTimer)
		delete(pageBuilders, h)
	}
	b := a.opening
	if b != nil {
		a.opening = nil
		a.side.invalidate()
		a.invalidateChrome()
		// A page whose init already ran is complete apart from its rows/show/focus
		// steps: keep its controls (the next showPage reveals and activates it).
		// Clearing its AssignTo targets here would orphan live controls and leave
		// an initialized page with nil fields.
		if !a.pages[b.id].initialized {
			if a.wraps[b.id] != nil {
				a.wraps[b.id].Dispose()
				a.wraps[b.id] = nil
				a.pages[b.id].root = nil
			}
			for _, clear := range b.clears {
				clear()
			}
		}
	}
	return b
}

// initialPageWidget keeps even direct Processes/Task Manager launches responsive:
// the shell paints before any table construction starts.
func (a *App) initialPageWidget(start int) decl.Widget {
	a.cur = start
	a.loading = newCanvasView(func(cv *walk.Canvas, w, h int) {
		target := a.cur
		if a.opening != nil {
			target = a.opening.id
		}
		text := "Opening " + a.pages[target].title + "…"
		area := rect{24, 0, w - 48, h}
		drawText(cv, text, uiFont(11), T.Muted, area, taCenter)
		a.loading.addText(area, text)
	})
	a.loading.acc, a.loading.accName, a.loading.accCtl = true, "Page loading", ctlPane
	return a.loading.declare(0, 0, 0, 0, bodyStretch)
}
