//go:build windows

package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

func TestRecorderStatusAtConstrainedSize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	runNativeTest(t, func() {
		oldScale := gs
		gs = 1
		defer func() { gs = oldScale }()
		const width, height = 412, 290
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: width, Height: height}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		r := metrics.NewRecorder(time.Hour, 2*time.Second, dir)
		defer r.Close(context.Background())
		snap := &metrics.Snapshot{T: 1700000000000, CPU: 24, Procs: []metrics.Proc{{Name: "worker.exe", PID: 42, CPU: 24}}}
		p := newRecorderPage(&App{rec: r, snap: snap})
		paint := func() {
			p.view.items = nil
			p.view.hits = nil
			p.paint(cv, width, height)
			for _, it := range p.view.items {
				b := it.r
				if b.W <= 0 || b.H <= 0 || b.X < 0 || b.Y < 0 || b.X+b.W > width || b.Y+b.H > height {
					t.Errorf("offscreen recorder item %q at %+v", it.name, b)
				}
			}
		}
		find := func(prefix string) rect {
			for _, it := range p.view.items {
				if strings.HasPrefix(it.name, prefix) {
					return it.r
				}
			}
			t.Errorf("missing accessible recorder text %q", prefix)
			return rect{}
		}
		paint()
		find("Recording off")
		find("Start recording")
		if len(p.view.hits) != 0 {
			t.Error("empty recording advertises selectable samples")
		}
		r.SetEnabled(true)
		paint()
		find("Starting recording")
		find("Waiting for the first sample")
		r.SetPersist(true)
		r.Add(snap)
		if err := r.Flush(context.Background()); err != nil {
			panic(err)
		}
		if r.DiskErr() == nil {
			panic("expected a persistence error writing beneath a regular file")
		}
		paint()
		state := find("Recording on")
		warning := find("Disk warning")
		if warning.Y < state.Y+state.H || warning.W < 320 {
			t.Errorf("disk warning needs its own row, state=%+v warning=%+v", state, warning)
		}
		find("Timeline, live")
		find("Busiest · worker.exe")
		r.SetEnabled(false)
		paint()
		find("Recording paused")
		find("Timeline, latest")
		p.pageKey(walk.KeyHome, 0)
		paint()
		latest := find("Go to latest")
		if latest.W < 75 || latest.H < 24 || p.view.hitAt(latest.X+2, latest.Y+2) != "live" {
			t.Errorf("return-to-latest target missing or clipped: %+v", latest)
		}
	})
}

func TestRecorderCommandsFitAtConstrainedSize(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		a := navigationTestApp(mw)
		p := newRecorderPage(a)
		if err := p.build().Create(decl.NewBuilder(a.right)); err != nil {
			panic(err)
		}
		body := walk.CreateLayoutItemsForContainer(p.view.cw.Parent())
		// The test window stays hidden. Include its native children in Walk's
		// layout just as it does for a visible recorder page.
		var includeChildren func(walk.ContainerLayoutItem)
		includeChildren = func(parent walk.ContainerLayoutItem) {
			for _, child := range parent.Children() {
				child.Geometry().ConsumingSpaceWhenInvisible = true
				if container, ok := child.(walk.ContainerLayoutItem); ok {
					includeChildren(container)
				}
			}
		}
		includeChildren(body)
		// Start with an expanded window before measuring the user's contraction.
		body.Children()[0].Geometry().ClientSize = walk.Size{Width: walk.IntFrom96DPI(1200, mw.DPI()), Height: 34}
		narrowBody := walk.Size{Width: walk.IntFrom96DPI(460, mw.DPI()), Height: 400}
		if min := body.MinSizeForSize(narrowBody); min.Width > narrowBody.Width {
			t.Errorf("wide recorder prevents shrinking to %d: native minimum %d", narrowBody.Width, min.Width)
		}
		for _, width := range []int{412, 850, 1200} {
			pixels := walk.IntFrom96DPI(width, mw.DPI())
			body.Geometry().ClientSize = walk.Size{Width: pixels + walk.IntFrom96DPI(48, mw.DPI()), Height: walk.IntFrom96DPI(460, mw.DPI())}
			bar := body.PerformLayout()[0]
			controls := bar.Item.(walk.ContainerLayoutItem)
			controls.Geometry().ClientSize = walk.Size{Width: bar.Bounds.Width, Height: bar.Bounds.Height}
			for _, control := range controls.PerformLayout() {
				b := control.Bounds
				if b.X+b.Width > pixels || b.Y+b.Height > bar.Bounds.Height {
					t.Errorf("toolbar clips a command at %+v within %+v", b, bar.Bounds)
				}
			}
			if width == 1200 && bar.Bounds.Height > walk.IntFrom96DPI(45, mw.DPI()) {
				t.Errorf("single row of commands consumes %d pixels at width %d", bar.Bounds.Height, pixels)
			}
		}
	})
}
