//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
	"reflect"
	"sync"
	"syscall"
	"time"
)

const pageBuildTimer = 0x474f544d

var pageTimerOnce sync.Once
var pageTimerCallback uintptr
var pageBuilders = map[win.HWND]*App{} // UI-thread owned, only while a build is pending

type pageConstruction struct {
	id       int
	steps    []func() error
	maxSlice time.Duration
	yield    bool
	palette  palette
	clears   []func()
}

// widget splits our declarative composites and table columns into native
// operations. Each Builder still finishes its own properties and deferred work.
func (b *pageConstruction) widget(parent walk.Container, widget decl.Widget) func() error {
	return func() error {
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
		var next []func() error
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
			b.yield = true
			for _, column := range cols {
				column := column
				next = append(next, func() error { return column.Create(tv) })
			}
		default:
			return widget.Create(decl.NewBuilder(parent))
		}
		b.steps = append(next, b.steps...)
		return nil
	}
}

func (a *App) startPageBuild(i int) {
	a.cancelPageBuild()
	b := &pageConstruction{id: i, palette: T}
	a.opening = b
	b.steps = []func() error{b.widget(a.right, a.pageWidget(i)), func() error {
		// A palette switch during construction must also update declarations that
		// were captured before the switch, without waiting for another theme event.
		if b.palette != T {
			setBackground(a.wraps[i], T.Bg)
			rethemeChildren(a.wraps[i])
		}
		a.pages[i].impl.init()
		a.pages[i].initialized = true
		return nil
	}}
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
	win.SetTimer(h, pageBuildTimer, 1, pageTimerCallback)
}

func (a *App) continuePageBuild() {
	b := a.opening
	if b == nil || a.mw.IsDisposed() {
		return
	}
	start := time.Now()
	defer func() {
		if elapsed := time.Since(start); elapsed > b.maxSlice {
			b.maxSlice = elapsed
		}
	}()
	for len(b.steps) > 0 {
		step := b.steps[0]
		b.steps[0] = nil
		b.steps = b.steps[1:]
		if err := step(); err != nil {
			a.cancelPageBuild()
			a.errBox("Open page", err)
			return
		}
		if a.wraps[b.id] != nil {
			a.wraps[b.id].SetVisible(false)
		}
		if b.yield || time.Since(start) >= 4*time.Millisecond {
			b.yield = false
			break
		}
	}
	if len(b.steps) > 0 {
		a.schedulePageBuild()
		return
	}
	delete(pageBuilders, win.HWND(a.mw.Handle()))
	a.opening = nil
	a.showPage(b.id)
}

func (a *App) cancelPageBuild() {
	if a.mw != nil {
		h := win.HWND(a.mw.Handle())
		win.KillTimer(h, pageBuildTimer)
		delete(pageBuilders, h)
	}
	if b := a.opening; b != nil {
		a.opening = nil
		a.side.invalidate()
		a.invalidateChrome()
		if !a.pages[b.id].initialized && a.wraps[b.id] != nil {
			a.wraps[b.id].Dispose()
			a.wraps[b.id] = nil
			a.pages[b.id].root = nil
		}
		for _, clear := range b.clears {
			clear()
		}
	}
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
