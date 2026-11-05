//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"gotm/internal/metrics"
	"testing"
	"time"
)

type lazyTestPage struct {
	app                      *App
	id, builds, inits, shows int
	root                     *walk.Composite
}

func (p *lazyTestPage) build() decl.Widget {
	p.builds++
	return decl.Composite{AssignTo: &p.root, Background: decl.SolidColorBrush{Color: T.Bg}}
}
func (p *lazyTestPage) init()                  { p.inits++; p.app.registerRoot(p.id, p.root) }
func (p *lazyTestPage) shown()                 { p.shows++ }
func (p *lazyTestPage) tick(*metrics.Snapshot) {}
func TestLazyPagesBuildOnNavigationOnce(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		right, err := walk.NewComposite(mw)
		if err != nil {
			panic(err)
		}
		a := &App{mw: mw, right: right, pages: make([]*pageDef, 2), wraps: make([]*walk.Composite, 2), side: newCanvasView(func(*walk.Canvas, int, int) {}), sensors: metrics.NewSensorHubDeferred()}
		first := &lazyTestPage{app: a, id: 0}
		second := &lazyTestPage{app: a, id: 1}
		a.pages[0] = &pageDef{impl: first}
		a.pages[1] = &pageDef{impl: second}
		if err := a.pageWidget(0).Create(decl.NewBuilder(right)); err != nil {
			panic(err)
		}
		if err := a.ensurePage(0); err != nil {
			panic(err)
		}
		if first.builds != 1 || first.inits != 1 || second.builds != 0 || a.wraps[1] != nil {
			t.Fatal("hidden page constructed at startup")
		}
		old := T
		defer func() { T = old }()
		T.Bg = walk.RGB(248, 248, 248)
		a.navigate(1)
		pumpNativeUntil(t, func() bool { return a.opening == nil })
		created := a.wraps[1]
		if second.builds != 1 || second.inits != 1 || second.shows != 1 || a.cur != 1 {
			t.Fatal("first navigation did not initialize page")
		}
		if bg, ok := second.root.Background().(*walk.SolidColorBrush); !ok || bg.Color() != T.Bg {
			t.Fatal("lazy page did not use current theme")
		}
		a.navigate(0)
		a.navigate(1)
		if a.wraps[1] != created || second.builds != 1 || second.inits != 1 || second.shows != 2 {
			t.Fatal("navigation rebuilt page and lost state")
		}
	})
}
func TestProcessColumnsCanChangeBeforeFirstVisit(t *testing.T) {
	a := &App{}
	p := newProcsTab(a)
	a.cfg.Columns = []string{"cpu", "gpu"}
	p.applyColumns()
	p.refresh()
	if p.tv != nil {
		t.Fatal("settings created hidden process controls")
	}
}
func TestRecorderBackgroundStartsWithoutNativePage(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		a := &App{mw: mw, rec: metrics.NewRecorder(time.Hour, time.Second, "")}
		a.cfg.RecorderDisk = true
		p := newRecorderPage(a)
		p.startBackground()
		if !a.rec.Persist() || p.root != nil || p.view.cw != nil {
			t.Fatal("recorder persistence depended on native controls")
		}
		a.rec.SetEnabled(true)
		p.sync() // footer toggles before the first recorder visit
		if p.bRec.text != "Recording" {
			t.Fatal("unbuilt recorder lost recording state")
		}
		a.rec.SetPersist(false)
		p.startBackground()
		if a.rec.Persist() {
			t.Fatal("background lifecycle restarted on revisit")
		}
	})
}
