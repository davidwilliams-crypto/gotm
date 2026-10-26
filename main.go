// gotm is a native Windows task manager written in Go. A metrics core reads
// the system directly (NtQuerySystemInformation, PDH, SCM, registry) and a
// Win32 front end (Walk) shows it: no browser, no local server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/tailscale/walk"
	"golang.org/x/sys/windows"

	"gotm/internal/config"
	"gotm/internal/metrics"
	"gotm/internal/ui"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "0.9.3-dev"

func init() {
	// Take the current directory out of the DLL search path (the classic
	// planted-DLL attack). gotm's own loads name System32 explicitly, so this
	// is the only directory the search order could add. SetDefaultDllDirectories
	// would be stricter, but it makes LoadLibraryEx(LOAD_WITH_ALTERED_SEARCH_PATH)
	// fail process-wide, and AMD's Ryzen Monitoring SDK loads its device
	// library that way: with it in place the SDK's Init fails with error 87.
	_ = windows.SetDllDirectory("")
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	tab := flag.String("tab", "",
		"page to open: performance, processes, startup, services, power, recorder, connections, apps, drivers, disk, settings (default: the page open when gotm last closed)")
	probe := flag.String("ryzen-probe", "",
		"diagnostic: start the AMD Ryzen Monitoring SDK inside this process and write the result to FILE (run as administrator)")
	flag.Parse()
	if *probe != "" {
		ryzenProbe(*probe)
		return
	}
	ui.StartTab = -1
	if n, ok := ui.PageIDs[*tab]; ok {
		ui.StartTab = n
	}
	if *showVersion {
		fmt.Println("gotm", version)
		return
	}

	// Walk requires InitApp to be the first toolkit call, on the main goroutine.
	if _, err := walk.InitApp(); err != nil {
		fmt.Fprintln(os.Stderr, "gotm:", err)
		os.Exit(1)
	}

	store := config.Open()
	c := store.Get()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sampler := metrics.NewSampler(c.RefreshMs, c.HistorySeconds)
	go sampler.Run(ctx)

	if err := ui.Run(version, store, sampler); err != nil {
		walk.MsgBox(nil, "gotm", "gotm could not start:\n\n"+err.Error(), walk.MsgBoxIconError)
		os.Exit(1)
	}
}

// ryzenProbe runs the SDK diagnostic in the app's own process: after the GUI
// toolkit's package initialisation (its COM runtime) and the System32-only
// DLL search, both before and after the toolkit's own InitApp.
func ryzenProbe(path string) {
	f, err := os.Create(path)
	if err != nil {
		os.Exit(1)
	}
	defer f.Close()
	fmt.Fprintf(f, "gotm %s\n", version)
	metrics.RyzenProbe(f, "gotm.exe before InitApp")
	if _, err := walk.InitApp(); err != nil {
		fmt.Fprintf(f, "InitApp: %v\n", err)
		return
	}
	metrics.RyzenProbe(f, "gotm.exe after InitApp")
}
