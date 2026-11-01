//go:build windows

package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
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
	stallBeat  atomic.Int64 // unix ms of the last UI-thread activity
	stallStart = time.Now()
	stallPath  string
	stallDumps atomic.Int32
)

// stallPhase marks a start-up phase boundary (also counts as UI activity).
func stallPhase(name string) {
	stallBeat.Store(time.Now().UnixMilli())
	if stallPath == "" {
		return
	}
	stallLine(fmt.Sprintf("phase %-22s +%6d ms", name, msSinceStart()))
}

// stallWatch runs on its own goroutine for the life of the process.
func stallWatch(logPath string) {
	stallPath = logPath
	f, err := os.OpenFile(stallPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "gotm stall log, started %s\n", stallStart.Format("2006-01-02 15:04:05"))
		f.Close()
	}
	stallPhase("instrumentation ready")
	for {
		time.Sleep(200 * time.Millisecond)
		gap := time.Since(time.UnixMilli(stallBeat.Load()))
		if gap < 1500*time.Millisecond {
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
	if stallPath == "" {
		return
	}
	f, err := os.OpenFile(stallPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
	if stallPath == "" {
		return
	}
	f, err := os.OpenFile(stallPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, s)
	f.Close()
}

func msSinceStart() int { return int(time.Since(stallStart).Milliseconds()) }

// InitStallLog starts recording to dir\stall.log; dir "" disables.
func InitStallLog(dir string) {
	if dir == "" {
		return
	}
	go stallWatch(filepath.Join(dir, "stall.log"))
}
