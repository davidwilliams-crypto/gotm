//go:build windows

package ui

import (
	"context"
	"errors"
	"slices"
	"sync"

	"gotm/internal/config"
)

const settingsQueueSize = 64

// settingsWriter keeps disk I/O and Store's write lock off the UI thread.
// Mutations run serially against the last successful commit, so failed changes
// are never inherited by later settings and rapid toggles remain ordered.
type settingsWriter struct {
	mu       sync.Mutex
	requests chan func(*config.Config)
	done     chan struct{}
	closed   bool
	firstErr error // worker-owned until done closes
}

func copySettings(c config.Config) config.Config {
	c.Columns = slices.Clone(c.Columns)
	c.Alerts = slices.Clone(c.Alerts)
	return c
}

func newSettingsWriter(initial config.Config, save func(config.Config) (config.Config, error), notify func(config.Config, error)) *settingsWriter {
	w := &settingsWriter{requests: make(chan func(*config.Config), settingsQueueSize+1), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		current := copySettings(initial)
		for mutate := range w.requests {
			next := copySettings(current)
			mutate(&next)
			saved, err := save(next)
			if err == nil {
				current = copySettings(saved)
			} else if w.firstErr == nil {
				w.firstErr = err
			}
			notify(copySettings(current), err)
		}
	}()
	return w
}

// The extra queue slot is reserved for the final window-state mutation.
func (w *settingsWriter) submit(mutate func(*config.Config), final bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || (!final && len(w.requests) >= settingsQueueSize) {
		return false
	}
	select {
	case w.requests <- mutate:
		return true
	default:
		return false
	}
}

// close stops accepting changes and waits only up to the caller's deadline.
// Windows can block an in-flight filesystem operation; one worker remains
// bounded even then, and exiting the process cannot wait indefinitely on it.
func (w *settingsWriter) close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.requests)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return w.firstErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// singleFileJob gives explicit exports one bounded background slot. A blocked
// path never creates a growing goroutine queue or stalls the message loop.
type singleFileJob struct {
	mu      sync.Mutex
	running bool
	done    chan struct{}
}

func (j *singleFileJob) active() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.running }
func (j *singleFileJob) start(work func() error, notify func(error)) bool {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return false
	}
	j.running = true
	j.done = make(chan struct{})
	done := j.done
	j.mu.Unlock()
	go func() {
		err := work()
		j.mu.Lock()
		j.running = false
		close(done)
		j.mu.Unlock()
		notify(err)
	}()
	return true
}
func (j *singleFileJob) wait(ctx context.Context) error {
	j.mu.Lock()
	done := j.done
	j.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errSettingsQueueFull = errors.New("settings were not saved: too many changes are waiting for the disk; try again when it responds")

// settingsMailbox bounds completed disk saves while the UI is busy. Persisted
// state can skip intermediate repaints; failures are retained and reported.
type settingsMailbox struct {
	mu       sync.Mutex
	saved    *config.Config
	err      error
	failures int
	queued   bool
}

func (m *settingsMailbox) offer(saved config.Config, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		copy := copySettings(saved)
		m.saved = &copy
	} else {
		if m.err == nil {
			m.err = err
		}
		m.failures++
	}
	if m.queued {
		return false
	}
	m.queued = true
	return true
}
func (m *settingsMailbox) take() (*config.Config, error, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	saved, err, failures := m.saved, m.err, m.failures
	m.saved, m.err, m.failures, m.queued = nil, nil, 0, false
	return saved, err, failures
}
