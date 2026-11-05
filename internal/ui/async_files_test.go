//go:build windows

package ui

import (
	"context"
	"errors"
	"gotm/internal/config"
	"testing"
	"time"
)

func TestSettingsWriterBlockedDiskDoesNotBlockSubmitOrClose(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	results := make(chan config.Config, 4)
	writer := newSettingsWriter(config.Default(), func(c config.Config) (config.Config, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return c, nil
	}, func(c config.Config, err error) {
		if err == nil {
			results <- c
		}
	})
	if !writer.submit(func(c *config.Config) { c.HideSystemIdle = !c.HideSystemIdle }, false) {
		t.Fatal("first request rejected")
	}
	<-entered
	returned := make(chan bool, 1)
	go func() {
		returned <- writer.submit(func(c *config.Config) { c.HideSystemIdle = !c.HideSystemIdle; c.RefreshMs = 2000 }, false)
	}()
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("second request rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("UI submission waited on disk")
	}
	select {
	case <-results:
		t.Fatal("uncommitted settings published")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := writer.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close while disk blocked: %v", err)
	}
	close(release)
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, second := <-results, <-results
	if first.HideSystemIdle || !second.HideSystemIdle || second.RefreshMs != 2000 {
		t.Fatalf("rapid mutations lost: %+v %+v", first, second)
	}
}

func TestSettingsWriterFailureDoesNotApplyOrPoisonNextSave(t *testing.T) {
	results := make(chan struct {
		c   config.Config
		err error
	}, 2)
	attempts := 0
	writer := newSettingsWriter(config.Default(), func(c config.Config) (config.Config, error) {
		attempts++
		if attempts == 1 {
			return c, errors.New("disk failed")
		}
		return c, nil
	}, func(c config.Config, err error) {
		results <- struct {
			c   config.Config
			err error
		}{c, err}
	})
	writer.submit(func(c *config.Config) { c.HideSystemIdle = false; c.Columns[0] = "bad" }, false)
	writer.submit(func(c *config.Config) { c.RefreshMs = 2000 }, false)
	if err := writer.close(context.Background()); err == nil {
		t.Fatal("failed save omitted from close result")
	}
	first, second := <-results, <-results
	if first.err == nil || second.err != nil || !second.c.HideSystemIdle || second.c.Columns[0] != "name" {
		t.Fatalf("failed settings carried forward: %+v %+v", first, second)
	}
}

func TestSettingsWriterQueueBoundedAndReservesWindowState(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	writer := newSettingsWriter(config.Default(), func(c config.Config) (config.Config, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return c, nil
	}, func(config.Config, error) {})
	writer.submit(func(*config.Config) {}, false)
	<-entered
	accepted := 0
	for i := 0; i < 1000; i++ {
		if writer.submit(func(*config.Config) {}, false) {
			accepted++
		}
	}
	if accepted != settingsQueueSize {
		t.Fatalf("queue accepted %d, want %d", accepted, settingsQueueSize)
	}
	if !writer.submit(func(c *config.Config) { c.LastPage = "settings" }, true) {
		t.Fatal("window state has no reserved queue slot")
	}
	close(release)
	if err := writer.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.submit(func(*config.Config) {}, false) {
		t.Fatal("accepted after close")
	}
}

func TestFileExportBlockedDiskIsSingleFlight(t *testing.T) {
	var job singleFileJob
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	if !job.start(func() error { close(entered); <-release; return errors.New("write failed") }, func(err error) { result <- err }) {
		t.Fatal("first export rejected")
	}
	<-entered
	if !job.active() || job.start(func() error { return nil }, func(error) {}) {
		t.Fatal("unbounded concurrent exports")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := job.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked export wait: %v", err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("export failure suppressed")
	}
	if err := job.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job.active() {
		t.Fatal("completed export still busy")
	}
}

func TestSettingsCompletionCoalescesBehindBusyUI(t *testing.T) {
	var mailbox settingsMailbox
	cfg := config.Default()
	queued := 0
	for i := 0; i < 1000; i++ {
		cfg.WindowW = 640 + i
		if mailbox.offer(cfg, nil) {
			queued++
		}
	}
	failure := errors.New("disk offline")
	mailbox.offer(cfg, failure)
	cfg.WindowW = 1777
	mailbox.offer(cfg, nil)
	if queued != 1 {
		t.Fatalf("queued %d UI callbacks", queued)
	}
	saved, err, failures := mailbox.take()
	if saved == nil || saved.WindowW != 1777 || !errors.Is(err, failure) || failures != 1 {
		t.Fatalf("completion lost: %+v %v %d", saved, err, failures)
	}
	if !mailbox.offer(cfg, nil) {
		t.Fatal("mailbox did not rearm")
	}
}

func TestApplySavedSettingsKeepsLiveSortAndAppliesOnlyCommittedValues(t *testing.T) {
	a := &App{cfg: config.Default()}
	original := a.cfg.RefreshMs
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan struct {
		c   config.Config
		err error
	}, 1)
	a.configWrites = newSettingsWriter(a.cfg, func(c config.Config) (config.Config, error) {
		close(entered)
		<-release
		return c, errors.New("disk failed")
	}, func(c config.Config, err error) {
		result <- struct {
			c   config.Config
			err error
		}{c, err}
	})
	if !a.saveConfig(func(c *config.Config) { c.RefreshMs = 2000 }) {
		t.Fatal("request not accepted")
	}
	<-entered
	if a.cfg.RefreshMs != original {
		t.Fatal("uncommitted config applied")
	}
	close(release)
	a.configWrites.close(context.Background())
	completion := <-result
	if completion.err == nil || a.cfg.RefreshMs != original {
		t.Fatal("failed setting applied")
	}
	saved := config.Default()
	saved.RefreshMs = 3000
	a.cfg.SortKey, a.cfg.SortDesc = "pid", false
	a.applySavedSettings(saved)
	if a.cfg.RefreshMs != 3000 || a.cfg.SortKey != "pid" || a.cfg.SortDesc {
		t.Fatalf("successful commit lost live state: %+v", a.cfg)
	}
}
