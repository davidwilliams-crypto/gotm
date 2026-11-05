//go:build windows

package ui

import (
	"context"
	"fmt"
	"slices"
	"time"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

// queueSettings reports acceptance, not completion. Only applySavedSettings,
// called after the worker's successful commit, changes running configuration.
func (a *App) queueSettings(mutate func(*config.Config), final bool) bool {
	if a.ioClosing.Load() && !final {
		return false
	}
	if a.configWrites == nil {
		a.configWrites = newSettingsWriter(a.cfg, a.store.Set, func(saved config.Config, err error) {
			if !a.ioClosing.Load() && a.settingResults.offer(saved, err) {
				a.fileResult(func() {
					saved, failure, count := a.settingResults.take()
					if saved != nil {
						a.applySavedSettings(*saved)
					}
					if failure != nil {
						if count > 1 {
							failure = fmt.Errorf("%d settings changes could not be saved: %w", count, failure)
						}
						a.errBox("Settings", failure)
					}
				})
			}
		})
	}
	key, desc := a.cfg.SortKey, a.cfg.SortDesc
	accepted := a.configWrites.submit(func(c *config.Config) {
		c.SortKey, c.SortDesc = key, desc
		mutate(c)
	}, final)
	if !accepted {
		if final {
			debugf("window state: %v", errSettingsQueueFull)
		} else if a.mw != nil {
			a.errBox("Settings", errSettingsQueueFull)
		}
	}
	return accepted
}

// fileResult never waits for the UI and discards completions after shutdown.
func (a *App) fileResult(apply func()) {
	if a.ioClosing.Load() || a.mw == nil {
		return
	}
	a.mw.Synchronize(func() {
		if !a.ioClosing.Load() {
			apply()
		}
	})
}

func (a *App) applySavedSettings(saved config.Config) {
	previous := a.cfg
	alertsChanged := !slices.Equal(previous.Alerts, saved.Alerts)
	layoutChanged := previous.TreeView != saved.TreeView || previous.GroupByName != saved.GroupByName
	columnsChanged := !slices.Equal(previous.Columns, saved.Columns)
	// Sorting is live and can change while an earlier disk write is in flight.
	saved.SortKey, saved.SortDesc = a.cfg.SortKey, a.cfg.SortDesc
	a.cfg = saved
	if a.sampler != nil {
		if previous.RefreshMs != saved.RefreshMs {
			a.sampler.SetInterval(saved.RefreshMs)
		}
		if previous.HistorySeconds != saved.HistorySeconds {
			a.sampler.SetHistorySeconds(saved.HistorySeconds)
		}
	}
	if alertsChanged {
		a.alertSince = map[int]int64{}
		a.alertShown = map[int]time.Time{}
	}
	if a.rec != nil && previous.RecorderDisk != saved.RecorderDisk {
		a.rec.SetPersist(saved.RecorderDisk)
	}
	if a.procs != nil && a.procs.tv != nil {
		if layoutChanged {
			a.procs.selKey = ""
			a.procs.syncToggles()
		}
		if columnsChanged {
			a.procs.applyColumns()
		}
		if a.cur == pgProcesses && (layoutChanged || columnsChanged || previous.HideSystemIdle != saved.HideSystemIdle) {
			a.procs.refresh()
		}
	}
	if a.settings != nil {
		a.settings.v.invalidate()
	}
	if len(a.pages) > pgRecorder && a.pages[pgRecorder] != nil {
		if p, ok := a.pages[pgRecorder].impl.(*recorderPage); ok {
			p.sync()
		}
	}
	a.invalidateChrome()
}

func (a *App) toggleProcessLayout(tree bool) {
	a.saveConfig(func(c *config.Config) {
		if tree {
			c.TreeView, c.GroupByName = !c.TreeView, false
		} else {
			c.TreeView, c.GroupByName = false, !c.GroupByName
		}
	})
}

// finishFileWork runs after the message loop has ended. Share one fixed budget
// across settings, recording and export; no Close handler waits for slow I/O.
func (a *App) finishFileWork() {
	a.ioClosing.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := a.configWrites.close(ctx); err != nil {
		debugf("settings shutdown: %v", err)
	}
	if a.rec != nil {
		if err := a.rec.Close(ctx); err != nil {
			debugf("recorder shutdown: %v", err)
		}
	}
	if len(a.pages) > pgRecorder && a.pages[pgRecorder] != nil {
		if p, ok := a.pages[pgRecorder].impl.(*recorderPage); ok {
			if err := p.exportJob.wait(ctx); err != nil {
				debugf("export shutdown: %v", err)
			}
		}
	}
}

// Export errors are delivered only while the app still has a live message loop.
func (p *recorderPage) exportAsync(path string) bool {
	if p.app.ioClosing.Load() || p.exportJob.active() {
		return false
	}
	// Freeze this export before any filesystem operation; Clear or new samples
	// while a slow destination opens must not change the selected recording.
	samples := p.app.rec.Samples(0, 0)
	started := p.exportJob.start(func() error {
		if err := metrics.ExportSamplesCSV(path, samples); err != nil {
			return err
		}
		if !p.app.ioClosing.Load() {
			openInExplorer(path)
		}
		return nil
	}, func(err error) {
		p.app.fileResult(func() {
			p.sync()
			if err != nil {
				p.app.errBox("Export", err)
			}
		})
	})
	if started {
		p.sync()
	}
	return started
}
