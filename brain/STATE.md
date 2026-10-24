## Objective
gotm is your own Windows task manager in Go: a native Win32 application (no browser, no local server) over a metrics core that reads the system directly. Built clean-room, using TMOG only as a feature-category and design inspiration.

## Status (2026-10-24) — v0.9.2 (SDK confirmed in-app; placeholder thermal zone)
- Signing: build.ps1 -TrustedSigning implemented and tested up to the Azure call (signtool loads the verified Microsoft dlib and reaches the service; fails only for lack of an account). Waiting on the user's Azure-side setup (docs/SIGNING.md); then `.uild.ps1 -Version x.y.z -TrustedSigning` produces signed kits.
- Elevated capture of 0.9.1: SDK rows present in the app (Tctl/Tdie 56.5 °C, Core #0–#15, cHTC 89 °C). Elevated tests all pass; gotm.exe --ryzen-probe Init OK (59 °C).
- User spotted the ACPI zone \_TZ.TZ10 at a constant 16.9 °C: a 290 K firmware placeholder. sensors.go now shows zones < 20 °C as a "placeholder, not a sensor" note (thermalPlaceholder + test).
- Release kit 0.9.2 built from this state; 0.9.0 (SDK broken in-app) and 0.9.1 (placeholder shown as a temperature) superseded.
- The machine-wide SDK switch is left OFF (as found); the user turns it on in Settings while elevated for everyday use.

## Status (2026-10-24) — v0.9.1 (elevated checks done; SDK-in-app fix)
- tools\elevated-check.ps1 ran from the user's elevated window: all elevated tests pass (service start/restart/stop via ServiceControl on a temporary SYSTEM service, KillProcess on its SYSTEM child + stale-start refusal, priority/affinity/suspend/resume/end, Ryzen SDK 47–52 °C, 16 cores). AMD's own sample app also reads fine.
- Found: the app's SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32) (0.8.0 hardening) makes AMD's Device.dll LoadLibraryEx(LOAD_WITH_ALTERED_SEARCH_PATH) fail (error 87) → "AMD SDK Init failed" in the elevated app only. Proven with GOTM_SDK_EMULATE (dlldirs FAIL / comsec PASS / both FAIL) and `gotm.exe --ryzen-probe`. Fix: main.go uses SetDllDirectory("") (drops CWD only); audit: every DLL load in gotm/walk/win/wingoes is NewLazySystemDLL.
- Release kit 0.9.1 built (tests pass): dist\release\gotm-0.9.1\ (setup 7,209,984 B, portable 4,633,235 B). 0.9.0 superseded.
- Open: user confirms the elevated 0.9.1 capture shows SDK rows; binaries unsigned; installer's own elevated run is the user's.

## Status (2026-10-24) — v0.9.0 (the last open limits)
- High contrast: `theme.go` builds the palette from system colours when a HC theme is on (SPI_GETHIGHCONTRAST), live re-theme on WM_SYSCOLORCHANGE/WM_THEMECHANGED (app.go retheme: composites, labels, line edits, tables). `GOTM_HIGH_CONTRAST=1` forces it for testing; captures of Performance/Processes/Power/Settings look right.
- Screen readers: `internal/ui/uia.go` = UI Automation provider (raw COM vtables, no cgo, UseComThreading) for every canvasView with `acc`: items registered via addItem/addText during paint become elements (Name, ControlType, rect, Invoke/SelectionItem/Toggle, focus events). Sidebar, header, footer, Performance (+GPU body), Settings, Recorder, drive strip covered; native tables named (nameTable). Verified with a PowerShell UIAutomationClient: 69 elements on Performance, Invoke on "Processes" navigated. Sidebar/footer/drive strip now keyboard-operable (canvasKey).
- AMD GPU sensors: `internal/metrics/adl.go` binds atiadlxx.dll (ADL2_New_QueryPMLogData_Get, OD5 fallback), adapters matched by PCI bus (ADL vendor id is decimal 1002; iEnumConnectedAdapters must be 0 or an iGPU without a monitor is missing). Live: iGPU 39 °C gfx / 45 °C SoC, 600 MHz, mem 3000, 1.14 V, APU package power. Shown on the GPU card and as per-GPU groups in Power & Freq; APU CPU temp/power as a CPU fallback when the driver reports it (not on this 7950X3D).
- Tree view: list-view indent via LVN_GETDISPINFO hook above Walk (tables.go indentRows) — icons move with names.
- Elevated checks: `internal/metrics/elevated_test.go` (temporary SYSTEM service from the test binary, ServiceControl start/restart/stop, KillProcess on a SYSTEM child, stale-start refusal, all process actions, Ryzen SDK reading) + `tools/elevated-check.ps1` (one UAC prompt, log + elevated Power & Freq capture; restores the HKLM SDK switch).
- Release kit built and verified: dist\release\gotm-0.9.0\ (setup 7,205,888 B, portable 4,629,498 B, README.txt, SHA256SUMS.txt); test-build.ps1 PASS; UIA check on the shipped exe. Elevated self-tests NOT executed: the UAC prompt was cancelled/timed out twice; user runs tools\elevated-check.ps1 once. Review fixes applied (see changelog). Installer test flake root-caused (COM thread affinity in createShortcut).
- Version 0.9.0 (main.go, build.ps1). Docs: README (Accessibility section, data sources, limits), docs/README-user.txt, docs/REVIEW-0.8.md (0.9.0 table).

## Status (2026-10-24) — v0.8.0 (360 review + distribution)
- Release kit: `dist\release\gotm-0.8.0\` = gotm-0.8.0-setup.exe (own Go installer, tools/setup: Program Files, Start menu, Apps & features, /S /desktop /D=), portable zip, README.txt (end users), SHA256SUMS.txt; THIRD-PARTY-NOTICES.txt generated by tools/notices and shipped in both. Inno Setup dropped.
- 360 review (3 independent reviewers) → docs/REVIEW-0.8.md. Fixed: callback-exhaustion crash (~30 min), elevated Uninstall via cmd/HKCU/%windir% (admin escalation), End tree on reused PIDs, PID-reuse-safe actions, critical-process guard, elevated writes in %APPDATA% (SDK switch + marker → HKLM, internal/safepath), scheduled tasks missing (XML), Priority menu dead, >64 CPUs, drive temps, NVML waking dGPU, disk freeze, disk active time, small-screen layouts, tables turning white, plus ~25 minor.
- Tests: full suite passes incl. new installer, safepath, actions, uninstall, signer, startup XML, drive temp, D3DKMT address tests. Visual checks at default and 1000×560.
- NOT done live: elevated install/uninstall run of setup.exe, elevated process/service actions, AMD SDK while elevated. Unsigned binaries (SmartScreen will warn).
- Incident: a config test wrote the real %APPDATA%\gotm\config.json while Dir() moved to KnownFolderPath; file reset to defaults (user's window size/columns/sort/alerts lost). Tests now use OpenAt(tempdir).

## Status (2026-10-14) — v0.7.0 (360 polish)
- Shared readability, toolbar layout, accessible names/disabled states, keyboard focus and activation; sidebar advanced tools now labelled Insights.
- Stable sorting/selection, cached connection filtering, loading/no-match feedback, collapsed sensor search and continuous min/max tracking.
- Atomic settings persistence, validation, failure rollback, slice isolation, and saved process-sort preservation.
- Recorder replay/cancellation/clear/keyboard fixes; path-based disk selection, root errors and partial results.
- Build stops on native errors, restores environment/location, stamps app and Windows metadata consistently; installer removes only its installed files.
- Verified: complete staged build (vet, full tests, executable and portable zip), build-failure test, version metadata, independent review. See docs/POLISH.md.
- Live UI checks stopped on physical Escape: final native keyboard and all-page/DPI checks remain uncompleted. No destructive actions or optional SDK activation tested. Inno Setup unavailable.
- dist/gotm.exe and gotm-0.7.0.exe are the release; open old windows were preserved. Restart to load the updated code.

## Status (2026-10-12) — v0.6.0 (polish pass)
- Every table: readable header captions, dark context menus, spare width goes to the main text column, search placeholders that stay visible with focus (Ctrl+F, Esc, ↓).
- Keyboard: Ctrl+1…0 pages, Ctrl+, Settings, Ctrl+F search, F5 reload (keyRouter pre-translate handler; Walk's ShortcutActions never fire for native list views). List shown in Settings and README.
- Processes: program icons, Priority menu button (+ right-click ▸ Set priority) replacing the blank combo box, tooltips on toolbar buttons; focus goes to the table on every page switch.
- Performance: compact CPU card caption (GHz · W · °C), scrollable GPU process list, GPU facts no longer repeat on-screen numbers.
- Power & Freq: Temperatures → Clocks → Powers → Voltages → Utilization (folded by default); clocks no longer red.
- Flight Recorder: chart fits what has been recorded until the range fills; seconds on short spans. Disk Space: "% used" on drive cards. gotm reopens the last page.
- Verified by window captures (PrintWindow) and a guarded keystroke test (Down, Shift+F10, Ctrl+F + typing, Esc, Ctrl+4).

## Status (2026-10-12) — v0.5.0
- GPU overhaul: one card per adapter (DXGI + PDH, LUID-keyed), per-engine utilization graphs (Overall/Engines toggle), dedicated and shared memory history, facts (NVIDIA clocks/power/temp/fan via NVML; driver, date, PCI location), per-process video-memory list. Verified live: RTX 5080 (GPU 0) and Radeon iGPU (GPU 1).
- CPU clocks include boost (Processor Frequency x % Processor Performance).
- Note: an older elevated gotm instance may still be running — relaunch dist\gotm.exe to see these changes.

## Status (2026-10-12) — v0.4.0
- New: optional AMD Ryzen Master Monitoring SDK reader (`internal/metrics/ryzensdk.go`): Tctl/Tdie, per-core temperature, effective clocks, PPT/TDC/EDC, VDD/SoC power, voltages. Opt-in toggle in Settings; sensor tree + Performance "CPU temperature" prefer it over LibreHardwareMonitor.
- Settings page now scrolls (the Flight recorder and action buttons were clipped below the fold at 800 px).
- SDK extracted without installing it: `third_party/RyzenMasterSDK-msi` (headers, docs, sample, Platform.dll/Device.dll 3.1.1.5478, AMD-signed). Binding follows the documented vtable order and is tested against an in-memory fake (struct layout, calls, decode, signature check, crash marker).
- NOT verified live: the SDK needs an elevated process and its Device.dll creates a kernel-driver service `AMDRyzenMasterDriverV32` (this PC has V27/V28/V31 from Ryzen Master/Performance Profile Client; V32 is absent) — awaiting the user's go-ahead.

## Status (2026-10-11)
- v0.3.0: dark owner-drawn UI with sidebar navigation, resource cards, anti-aliased graphs, native dark tables.
- Pages: Performance, Processes (flat/tree/group), Startup apps, Services; **PRO**: Power & Freq (sensor tree, live/min/max), Flight Recorder, Connections, Installed Apps, Drivers, Disk Space; Settings.
- Flicker fixed and measured: ~75 fps screen sampling per page, content never collapsed in any frame.
- Tests pass: config (incl. BOM), metrics on the live machine (connections, apps, drivers, drive health, topology, disk scan, recorder, sensors, tree).
- Verified by real-window capture: every page renders with live data on this machine.
- Packaging: build.ps1 builds exe + portable zip. Inno installer script written, NOT compiled (Inno Setup 6 not installed).

## Not verified (need an interactive/elevated session)
UAC relaunch, service start/stop, startup enable/disable, end task / priority / affinity, uninstall, tray notification.

## Layout
`main.go`, `internal/{metrics,ui,config}`, `winres/` + `tools/genicon`, `docs/FEATURES.md`, `installer/gotm.iss`, `build.ps1`.
