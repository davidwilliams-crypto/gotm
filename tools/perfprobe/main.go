//go:build windows

// perfprobe measures a GOTM window's message-loop latency without changing its
// UI state. Optional bounded CPU workers exit with the probe. It never changes
// process priorities, affinity, or system configuration.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"
)

func main() {
	pid := flag.Uint("pid", 0, "existing process ID")
	exe := flag.String("launch", "", "optional executable to start")
	tab := flag.String("tab", "performance", "GOTM page to open when launching")
	seconds := flag.Int("seconds", 15, "measurement duration (1..60 seconds)")
	load := flag.Int("load", 0, "bounded busy CPU workers (0..logical CPUs)")
	flag.Parse()
	if *seconds < 1 || *seconds > 60 || *load < 0 || *load > runtime.NumCPU() {
		panic("invalid probe bounds")
	}
	var stop atomic.Bool
	var sink atomic.Uint64
	for i := 0; i < *load; i++ {
		go func(seed uint64) {
			for !stop.Load() {
				for j := 0; j < 65536; j++ {
					seed = seed*6364136223846793005 + 1
				}
				sink.Store(seed)
			}
		}(uint64(i + 1))
	}
	defer stop.Store(true)
	if *load > 0 {
		time.Sleep(time.Second)
	}
	start := time.Now()
	if *exe != "" {
		cmd := exec.Command(*exe, "--tab", *tab)
		if err := cmd.Start(); err != nil {
			panic(err)
		}
		*pid = uint(cmd.Process.Pid)
		_ = cmd.Process.Release()
	}
	if *pid == 0 {
		panic("specify -pid or -launch")
	}
	user := windows.NewLazySystemDLL("user32.dll")
	enum := user.NewProc("EnumWindows")
	getPID := user.NewProc("GetWindowThreadProcessId")
	visible := user.NewProc("IsWindowVisible")
	getTitle := user.NewProc("GetWindowTextW")
	getClass := user.NewProc("GetClassNameW")
	send := user.NewProc("SendMessageTimeoutW")
	var hwnd uintptr
	cb := windows.NewCallback(func(w uintptr, _ uintptr) uintptr {
		var p uint32
		getPID.Call(w, uintptr(unsafe.Pointer(&p)))
		if p != uint32(*pid) {
			return 1
		}
		v, _, _ := visible.Call(w)
		if v == 0 {
			return 1
		}
		var class [128]uint16
		getClass.Call(w, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
		if !strings.Contains(windows.UTF16ToString(class[:]), "Walk_MainWindow_Class") {
			return 1
		}
		var title [256]uint16
		getTitle.Call(w, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
		if strings.HasPrefix(strings.ToLower(windows.UTF16ToString(title[:])), "gotm") {
			hwnd = w
			return 0
		}
		return 1
	})
	for hwnd == 0 && time.Since(start) < 30*time.Second {
		enum.Call(cb, 0)
		if hwnd == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if hwnd == 0 {
		panic("no visible window within 30 seconds")
	}
	first := time.Since(start).Milliseconds()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(*pid))
	if err != nil {
		panic(err)
	}
	defer windows.CloseHandle(h)
	type memCounters struct {
		Size, Faults                                                                          uint32
		PeakWS, WS, PeakPaged, Paged, PeakNonPaged, NonPaged, Pagefile, PeakPagefile, Private uintptr
	}
	getMem := windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	var samples []float64
	timeouts := 0
	sendErrors := map[string]int{}
	var firstResponse int64
	var peakPrivate, peakWS uintptr
	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	for time.Now().Before(deadline) {
		now := time.Now()
		var result uintptr
		// Use the actual deadline, not SMTO_ABORTIFHUNG's earlier heuristic.
		ok, _, sendErr := send.Call(hwnd, 0, 0, 0, 1, 1000, uintptr(unsafe.Pointer(&result)))
		elapsed := time.Since(now)
		samples = append(samples, float64(elapsed.Microseconds())/1000)
		if ok == 0 {
			if sendErr == windows.ERROR_TIMEOUT || elapsed >= time.Second {
				timeouts++
			} else {
				sendErrors[sendErr.Error()]++
			}
		} else if firstResponse == 0 {
			firstResponse = time.Since(start).Milliseconds()
		}
		m := memCounters{}
		m.Size = uint32(unsafe.Sizeof(m))
		getMem.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.Size))
		if m.Private > peakPrivate {
			peakPrivate = m.Private
		}
		if m.WS > peakWS {
			peakWS = m.WS
		}
		time.Sleep(50 * time.Millisecond)
	}
	sort.Float64s(samples)
	report := map[string]any{"pid": *pid, "load_workers": *load, "samples": len(samples), "timeouts": timeouts, "p50_ms": samples[len(samples)/2], "p95_ms": samples[(len(samples)-1)*95/100], "max_ms": samples[len(samples)-1], "peak_private_bytes": peakPrivate, "peak_working_set_bytes": peakWS}
	if len(sendErrors) > 0 {
		report["send_errors"] = sendErrors
	}
	if *exe != "" {
		report["visible_window_ms"] = first
		report["first_response_ms"] = firstResponse
	}
	b, _ := json.Marshal(report)
	fmt.Println(string(b))
	if timeouts > 0 || len(sendErrors) > 0 {
		os.Exit(1)
	}
}
