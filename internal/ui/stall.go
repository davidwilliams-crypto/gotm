//go:build windows

package ui

import (
	"fmt"
	"github.com/tailscale/win"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
)

// UI-thread stall instrumentation. A "not responding" report from a user is
// a ≥5 s gap in the message loop; this records every phase of start-up and,
// whenever the UI thread goes quiet for more than 1.5 s, appends ALL
// goroutine stacks — the UI goroutine's stack shows exactly what it was
// blocked on. The log lives at <settings>\stall.log and is rewritten at each
// launch (capped, never grows unbounded).

var (
	stallWindow atomic.Uintptr
	stallBeat   atomic.Int64 // unix ms of the last UI-thread activity
	stallStart  = time.Now()
	stallLog    stallLogConfig
	stallDumps  atomic.Int32
	stallLines  = make(chan string, 32)
	// stallHidden is set while nobody can see the main window. The watchdog
	// then wakes less often and only treats long silences as stalls, so an
	// idle, minimized gotm is not woken every 1.5 s just to prove it is alive.
	stallHidden atomic.Bool
)

// stallPollPeriod is how often the watchdog checks the UI heartbeat.
func stallPollPeriod(hidden bool) time.Duration {
	if hidden {
		return time.Second
	}
	return 200 * time.Millisecond
}

// stallQuietLimit is the silence after which the UI thread is probed (and,
// if the probe also goes unanswered, its stacks are logged).
func stallQuietLimit(hidden bool) time.Duration {
	if hidden {
		return 10 * time.Second
	}
	return 1500 * time.Millisecond
}

// stallPhase marks a start-up phase boundary (also counts as UI activity).
func stallPhase(name string) {
	stallBeat.Store(time.Now().UnixMilli())
	if stallLog.path == "" {
		return
	}
	stallLine(fmt.Sprintf("phase %-22s +%6d ms", name, msSinceStart()))
}

// stallWatch runs on its own goroutine for the life of the process.
func stallWatch(logPath string) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "gotm stall log, started %s\n", stallStart.Format("2006-01-02 15:04:05"))
		f.Close()
	}
	stallPhase("instrumentation ready")
	var probeBeat int64
	period := stallPollPeriod(false)
	timer := time.NewTicker(period)
	defer timer.Stop()
	for {
		select {
		case line := <-stallLines:
			writeStallLine(line)
			continue
		case <-timer.C:
		}
		hidden := stallHidden.Load()
		if p := stallPollPeriod(hidden); p != period {
			period = p
			timer.Reset(p)
		}
		beat := stallBeat.Load()
		gap := time.Since(time.UnixMilli(beat))
		if gap < stallQuietLimit(hidden) {
			continue
		}
		// A quiet GetMessage loop is healthy. Probe it once before treating
		// silence as a stall; a genuinely blocked UI cannot dispatch WM_NULL.
		if hwnd := stallWindow.Load(); hwnd != 0 && probeBeat != beat {
			probeBeat = beat
			win.PostMessage(win.HWND(hwnd), win.WM_NULL, 0, 0)
			continue
		}
		if stallDumps.Load() >= 8 {
			stallBeat.Store(time.Now().UnixMilli()) // stay quiet; enough captured
			continue
		}
		stallDumps.Add(1)
		stallBeat.Store(time.Now().UnixMilli())
		dumpStall(gap)
	}
}

func dumpStall(gap time.Duration) {
	if stallLog.path == "" {
		return
	}
	f, err := os.OpenFile(stallLog.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st != nil && st.Size() > 256*1024 {
		return // capped
	}
	fmt.Fprintf(f, "\n=== UI THREAD QUIET FOR %s at +%d ms ===\n", gap.Round(10*time.Millisecond), msSinceStart())
	_ = pprof.Lookup("goroutine").WriteTo(f, 1)
}

func stallLine(s string) {
	// Diagnostics must not introduce the startup stalls they measure. The
	// existing watchdog owns all log writes; discard excess phase messages
	// if its disk is blocked rather than growing work or waiting on the UI.
	select {
	case stallLines <- s:
	default:
	}
}

func writeStallLine(s string) {
	if stallLog.path == "" {
		return
	}
	f, err := os.OpenFile(stallLog.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, s)
	f.Close()
}

func msSinceStart() int { return int(time.Since(stallStart).Milliseconds()) }

// InitStallLog starts recording to dir\stall.log; dir "" disables.
type stallLogConfig struct {
	once sync.Once
	path string
}

func (c *stallLogConfig) initialize(dir string, launch func(string)) {
	if dir == "" {
		return
	}
	c.once.Do(func() {
		c.path = filepath.Join(dir, "stall.log")
		launch(c.path)
	})
}
func InitStallLog(dir string) {
	stallLog.initialize(dir, func(path string) { go stallWatch(path) })
}
