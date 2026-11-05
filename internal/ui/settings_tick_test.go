//go:build windows

package ui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"gotm/internal/config"
	"gotm/internal/metrics"
	"gotm/internal/taskmanager"
)

// Until the first background query answers, Settings shows a neutral
// "Checking…" state with no action, not "Replace Task Manager / Off" that
// flips a moment later when gotm owns the routing.
func TestTaskManagerSettingShowsCheckingUntilFirstResult(t *testing.T) {
	runNativeTest(t, func() {
		oldScale := gs
		gs = 1
		defer func() { gs = oldScale }()
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		old := queryTaskManager
		defer func() { queryTaskManager = old }()
		release := make(chan taskManagerSetting, 2)
		queryTaskManager = func() taskManagerSetting { return <-release }
		a := &App{mw: mw, cfg: config.Default(), store: config.OpenAt(t.TempDir()), sensors: &metrics.SensorHub{}}
		a.host.Elevated = true
		tab := newSettingsTab(a)
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 900, Height: 1400}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		paint := func() (action bool, text string) {
			cv, err := walk.NewCanvasFromImage(bmp)
			if err != nil {
				panic(err)
			}
			defer cv.Dispose()
			tab.v.items, tab.v.hits = nil, nil
			tab.paint(cv, 900, 1400)
			for _, it := range tab.v.items {
				if it.id == "taskmanager" {
					action = true
				}
				if strings.HasPrefix(it.name, "Windows Task Manager replacement: ") {
					text = it.name
				}
			}
			return
		}
		tab.refreshTaskManager(true) // what init does
		label, _ := tab.taskManagerText()
		if label != "Checking…" || !tab.taskManagerChecking() {
			t.Errorf("before the first answer the setting shows %q", label)
		}
		if action, text := paint(); action || text == "" {
			t.Errorf("before the first answer: action offered=%v, neutral text %q", action, text)
		}
		release <- taskManagerSetting{exe: "gotm", checkedAt: time.Now(), state: taskmanager.State{Owned: true}}
		if !pumpNativeUntilOK(func() bool { return !tab.tmQuerying }) {
			t.Error("query result never applied")
			return
		}
		if label, _ := tab.taskManagerText(); label != "Restore Windows Task Manager" || tab.taskManagerChecking() {
			t.Errorf("after the answer the setting shows %q", label)
		}
		if action, _ := paint(); !action {
			t.Error("the action is not offered once the state is known")
		}

		// A first answer equal to the zero state must still leave "Checking…".
		tab2 := newSettingsTab(a)
		tab2.refreshTaskManager(true)
		release <- taskManagerSetting{}
		if !pumpNativeUntilOK(func() bool { return !tab2.tmQuerying }) {
			t.Error("second query result never applied")
			return
		}
		if label, _ := tab2.taskManagerText(); label != "Replace Task Manager" {
			t.Errorf("zero-state answer shows %q", label)
		}
	})
}

// pumpNativeUntilOK pumps messages until ready or 5 s; unlike pumpNativeUntil
// it never calls t.Fatal (which would end the shared native goroutine).
func pumpNativeUntilOK(ready func() bool) bool {
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
			return false
		}
	}
	return true
}

// The machine-wide Task Manager query runs off the UI thread, one at a time,
// and a stale answer never overwrites a newer synchronous read.
func TestTaskManagerQueryIsBackgroundAndSingleFlight(t *testing.T) {
	runNativeTest(t, func() {
		mw, err := walk.NewMainWindow()
		if err != nil {
			panic(err)
		}
		defer mw.Dispose()
		old := queryTaskManager
		defer func() { queryTaskManager = old }()
		var calls atomic.Int32
		release := make(chan struct{})
		queryTaskManager = func() taskManagerSetting {
			calls.Add(1)
			<-release
			return taskManagerSetting{exe: "slow", checkedAt: time.Now(), state: taskmanager.State{Owned: true}}
		}
		tab := newSettingsTab(&App{mw: mw, sensors: &metrics.SensorHub{}})
		start := time.Now()
		tab.refreshTaskManager(true)
		tab.refreshTaskManager(true) // already running: no second query
		tab.tick(nil)
		if time.Since(start) > 500*time.Millisecond {
			t.Error("refresh waited for the query on the UI thread")
		}
		close(release)
		pumpNativeUntil(t, func() bool { return !tab.tmQuerying })
		if calls.Load() != 1 || tab.taskManager.exe != "slow" || !tab.taskManager.state.Owned {
			t.Errorf("calls=%d state=%+v", calls.Load(), tab.taskManager)
		}

		// A synchronous read (a click) supersedes a query still running.
		gate := make(chan struct{})
		queryTaskManager = func() taskManagerSetting {
			<-gate
			return taskManagerSetting{exe: "stale"}
		}
		tab.refreshTaskManager(true)
		queryTaskManager = func() taskManagerSetting { return taskManagerSetting{exe: "fresh"} }
		tab.refreshTaskManagerNow()
		close(gate)
		pumpNativeUntil(t, func() bool { return !tab.tmQuerying })
		if tab.taskManager.exe != "fresh" {
			t.Errorf("stale background result replaced a newer read: %q", tab.taskManager.exe)
		}
	})
}
