//go:build windows

package ui

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tailscale/walk"
	"gotm/internal/metrics"
	"gotm/internal/taskmanager"
)

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
