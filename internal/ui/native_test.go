//go:build windows

package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"testing"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

// Walk defers native class and GDI initialization until InitApp, which must
// run on the main goroutine before tests create hidden controls or bitmaps.
func TestMain(m *testing.M) {
	// go test does not inherit the application's embedded Common Controls v6
	// manifest. Give this test process the same native control dependency.
	dir, err := os.MkdirTemp("", "gotm-ui-test-")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "controls.manifest")
	err = os.WriteFile(path, []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
<assemblyIdentity version="1.0.0.0" processorArchitecture="*" name="gotm.ui.tests" type="win32"/>
<dependency><dependentAssembly><assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/></dependentAssembly></dependency>
</assembly>`), 0600)
	if err != nil {
		panic(err)
	}
	source, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		panic(err)
	}
	if ctx := win.CreateActCtx(&win.ACTCTX{Flags: 0x10, Source: source}); ctx == win.HANDLE(^uintptr(0)) {
		panic("could not activate Common Controls v6 test manifest")
	}
	if _, err := walk.InitApp(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	results := make(chan int)
	go func() { results <- m.Run() }()
	for {
		select {
		case run := <-nativeTests:
			run()
		case code := <-results:
			_ = os.RemoveAll(dir)
			os.Exit(code)
		}
	}
}

var nativeTests = make(chan func())

// Native controls and their tooltips share Walk's main OS thread. Keep all
// native setup, operations and disposal there, without showing any windows.
// Setup errors panic inside run; this helper turns them into test failures.
func runNativeTest(t *testing.T, run func()) {
	t.Helper()
	done := make(chan struct{})
	nativeTests <- func() {
		defer close(done)
		defer func() {
			if err := recover(); err != nil {
				t.Errorf("native test setup: %v\n%s", err, debug.Stack())
			}
		}()
		run()
	}
	<-done
}
