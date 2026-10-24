# gotm 0.8.0 — 360 review and distribution

2026-10-24. Three independent read-only reviews covered gotm before it was packaged for other PCs:

- system-reading code (sampler, PDH, GPU, drives, sensors);
- system-changing code and security (process actions, services, startup, uninstall, AMD SDK, settings);
- the UI layer.

Each finding below was checked against the code before it was fixed. "Fixed" means changed and covered by a test or a live check on this PC.

## Critical

| Finding | Status |
|---|---|
| A new Windows callback was created on every table refresh. Go allows about 2,000, so gotm crashed after about 30 minutes on Processes. | Fixed. One callback, cached list views. `TestChildEnumerationDoesNotExhaustCallbacks` runs 2,500 enumerations. |
| Elevated "Uninstall" ran registry strings through `cmd.exe`: per-user (HKCU) entries, and `%windir%` taken from the user's environment. Either let a standard user run code as admin. | Fixed. No shell. HKCU entries are refused while elevated. `%VARS%` are expanded with system values only. The program is resolved like CreateProcess does, with no current-directory or PATH lookup when elevated. All 150 entries on this PC resolve. |
| "End tree" followed parent PIDs that Windows had reused (27 orphans here, including explorer.exe and winlogon.exe). The kill also used a newer snapshot than the one the user confirmed. | Fixed. A child must start after its parent. The confirmed list is shown and exactly that list is ended. |

## Important

| Finding | Status |
|---|---|
| Process actions could hit a different process after PID reuse. | Fixed. Rows and actions carry the creation time, and each handle is verified before acting. Test with a spawned process. |
| No guard for csrss, winlogon, lsass, dwm, services, wininit or smss. Suspend had no confirmation. | Fixed. Refused when the process has BreakOnTermination set, or has a core name and runs from System32. Suspend asks first. Resume always works. |
| Elevated gotm created, wrote and deleted files in the user-writable `%APPDATA%\gotm`. Through a junction, that becomes an admin file create or delete. The SDK opt-in also lived in `config.json`. | Fixed. The SDK switch and its crash marker are in HKLM. When elevated, settings and recorder writes and prunes refuse redirected folders and links (`internal/safepath`, with a junction test). The settings folder comes from the shell, not `%APPDATA%`. |
| The Startup page showed no scheduled tasks: the CSV columns were off by one and English-only. | Fixed. Reads `schtasks /xml`, which doesn't depend on the display language. 42 logon/boot tasks found here. |
| "Run as administrator" elevated a copy sitting in a user-writable folder. | Mitigated. gotm warns and points to the installer. Every library gotm and its toolkits load is named from System32, and the current directory is removed from the search path (0.9.1: the stricter `SetDefaultDllDirectories` of 0.8.0 broke AMD's SDK, see the 0.9.0 table). The installer installs to Program Files. |
| The Priority button menu did nothing: it was disposed before Windows delivered the click. | Fixed. Disposal is deferred until after the command arrives. |
| COM vtable calls could lose their stack arguments (`uintptr` without `uintptrescapes`). | Fixed in `vcall`, and in the installer's `comCall` (which also keeps its strings alive). |
| More than 64 logical CPUs: only one processor group was read, so per-process CPU could exceed 100%. | Fixed. Every group is read through `NtQuerySystemInformationEx`. Counters that go backwards are skipped. |
| Drive temperatures: wrong property ID (12, not 52) and wrong offset (18, not 26). | Fixed. 35 °C and 29 °C read here. |
| NVML was polled every second, which can keep a laptop's dGPU awake. | Fixed. It is polled only while a GPU view is on screen; otherwise the last values are shown. |
| The Disk page read every drive on the UI thread, so a disconnected network drive froze the window. | Fixed. Drives are read in the background. |
| Disk "active time" used `% Disk Time`, which is queue length × 100. | Fixed. It is now 100 − `% Idle Time` per disk, and the busiest disk for the summary. |
| The minimum window size was taller than a 1366×768 screen at 125%. | Fixed. The minimum comes from the work area. The sidebar, Performance cards, per-core grid and Flight Recorder detail all adapt; checked at 1000×560. |
| Tables turned white after a theme, wallpaper or policy change. | Fixed. The list-view hook keeps the dark colours. |

## Minor (fixed)

| Area | Fix |
|---|---|
| LibreHardwareMonitor client | No redirects and no proxy (test). |
| External CPU temperature | Only CPU sensor IDs count; a Radeon, Arc or SSD temperature is no longer shown as the CPU's (test). |
| Identical NVIDIA cards | Matched by PCI bus from D3DKMT (checked against the registry). |
| NPUs | Compute-only adapters without a DXGI record are hidden. |
| NVML | Every entry point is checked, so an old NVML can't crash the sampler. |
| Sampler | A panic costs one sample, not the app. The first baseline sample is no longer shown as 0%. |
| Clock steps backwards | History and recorder carry on instead of pausing. |
| Recorder | Old files are pruned first; only relevant days are loaded, in the background. |
| Drives | Drive health is read once, then only while Power & Freq is open (drives can sleep). |
| Bus names | RAID, SAS, Storage Spaces, iSCSI, MMC, SCM and UFS added. |
| Network totals | vEthernet, VPN and tunnel adapters are excluded from the totals, which double-counted traffic. |
| Language | The SYSTEM account name is localized. |
| Startup and Drivers folders | Taken from the shell and the Windows API, not environment variables. |
| Services | Each action asks only for the rights it needs. |
| AMD SDK | `UnInit` runs after a partial init. The DLLs must be signed by AMD, not just validly signed. |
| Open folder | Explorer is only ever given folders. |
| Error messages | "No longer exists" is no longer reported for other errors. |
| Different admin account | The footer shows which account gotm is elevated as. |
| Tray | "Show gotm" restores a minimized window. |
| Window state | The restored size is saved, also at sign-out and shutdown. |
| Connections | The list no longer jumps back to the selection while you scroll. |
| Scheduled tasks | Toggling runs off the UI thread. |
| Large high-DPI screens | Graph supersampling is capped (about 24 MB per graph). |
| Disk Space | Drive cards narrow to fit, with a note when some drives don't fit. |

## Left open in 0.8.0, closed in 0.9.0 (2026-10-24)

| 0.8.0 limit | 0.9.0 |
|---|---|
| High-contrast mode isn't honoured. | Fixed. `applyTheme` builds the palette from the system colours when a high-contrast theme is active (window, text, selection, disabled text; outlines in the text colour; resource hues pushed away from the background) and re-applies it live on WM_SYSCOLORCHANGE / WM_THEMECHANGED, recolouring native tables, labels and search boxes too. `GOTM_HIGH_CONTRAST=1` forces it; checked on Performance, Processes, Power & Freq and Settings. |
| Screen readers get little from the sidebar and the drawn pages. | Fixed. `internal/ui/uia.go` is a UI Automation provider (COM objects without cgo) for every owner-drawn surface: sidebar items, resource cards, per-core tiles, facts, GPU engine/memory/process rows, settings chips and steppers, recorder timeline and stats, drive cards, header text and status-bar buttons are elements with names, control types, rectangles, Invoke / SelectionItem / Toggle patterns and focus events. Native tables are named. A UI Automation client enumerated 69 elements on Performance and navigated by invoking a sidebar item. |
| AMD integrated-GPU temperature and clocks aren't shown. | Fixed. `internal/metrics/adl.go` binds the driver's own `atiadlxx.dll` (`ADL2_New_QueryPMLogData_Get`, Overdrive 5 fallback), matched to DXGI adapters by PCI bus. The Radeon iGPU here reports 39 °C graphics / 45 °C SoC, 600 MHz core, 3000 MHz memory, 1.14 V and the APU package power, shown on the GPU card and as a group in Power & Freq. Polled only while a GPU view is open, except on APUs (nothing to wake). |
| Process-tree icons don't follow the indentation. | Fixed. A hook above Walk's table procedure fills `iIndent` in LVN_GETDISPINFO, so the native list view indents icon and name together. |
| Not tested live: elevated process and service actions, and the AMD sensor path as administrator. | `internal/metrics/elevated_test.go` + `tools/elevated-check.ps1`: a temporary service built from the test binary runs as SYSTEM; the test starts, restarts and stops it through `ServiceControl`, ends its SYSTEM-owned child through `KillProcess` (and proves a stale creation time is refused), runs every process action elevated, and starts the Ryzen Monitoring SDK for a real reading. See "Elevated run" below for the result on this PC. |

An independent read of the new code (UI Automation vtables, ADL structures, the hooks, the elevated tests) confirmed the bindings against the Windows SDK and ADL headers and found 15 smaller issues, all fixed the same day: the AMD polling gate exempted every AMD adapter once any one was an APU (now per adapter, using the driver's ASIC family type); a transient ADL error demoted an adapter to the legacy path for good; the live ADL test compared a hex vendor id with a decimal one and so checked nothing; runtime ids were hashes (now sequential per canvas); list roots lacked the Selection pattern; native callbacks had no panic guard; hooks and provider references were not released when a window died; re-theming leaked brushes; the elevated test did not wait for a stale service to disappear. The installer's "transient" test failure also turned out to be real: shortcut creation initialised COM on one OS thread and used it on another; it now pins its thread.

### Elevated run

`tools\elevated-check.ps1` ran on this PC on 2026-10-24 (from the user's elevated window). All three tests pass: a temporary SYSTEM service was started, restarted and stopped through `ServiceControl`; its SYSTEM-owned child was ended through `KillProcess` and a stale creation time was refused; priority, affinity, suspend, resume and end all worked elevated; the Ryzen Monitoring SDK reported the 7950X3D at 47–52 °C with 16 per-core readings.

With 0.9.1 the elevated capture shows the SDK rows inside the app: CPU Package (Tctl/Tdie) 56.5 °C, sixteen per-core temperatures, the 89 °C thermal limit, PPT and per-core clocks. The same capture showed the board's ACPI zone `\_TZ.TZ10` at a constant 16.9 °C (a 290 K firmware placeholder); 0.9.2 labels such zones as placeholders instead of temperatures.

The run also found a real bug: the elevated app showed "AMD SDK Init failed" although the same code passed in the test binary. Re-running the SDK test under each condition the app adds (`GOTM_SDK_EMULATE`) and `gotm.exe --ryzen-probe` pinned it to the `SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` hardening from 0.8.0: AMD's `Device.dll` loads its own module with `LOAD_WITH_ALTERED_SEARCH_PATH`, which Windows refuses with error 87 once default DLL directories are set. 0.9.1 replaces it with `SetDllDirectory("")` (the current directory leaves the search path; every library gotm, Walk, the Win32 bindings and wingoes load is already named from System32, so nothing else changes) and the SDK starts inside the app.

Still open:
- The executables are unsigned, so SmartScreen warns on other PCs until they are code-signed. The build side is ready: `build.ps1 -TrustedSigning` (Azure Trusted Signing, settings in `signing.json`, one-time Azure setup in `docs/SIGNING.md`) or `-SignThumbprint` for a store certificate; both verify every signature. What remains is the Azure account and Microsoft's identity validation of the organisation.
- The installer's own elevated install/uninstall has not been run on this PC (the user runs setup.exe).
- Dark menus and table headers are chosen once at start-up, so an app started in high-contrast mode keeps the system look for those if high contrast is turned off while it runs (the palette itself switches live).

## Distribution

`.\build.ps1 -Version 0.8.0` produces `dist\release\gotm-0.8.0\`:

- `gotm-0.8.0-setup.exe` — the Go installer (`tools/setup`), with gotm.exe, README.txt and THIRD-PARTY-NOTICES.txt zipped inside. It installs for all users to Program Files, adds a Start menu shortcut and an Apps & features entry. `/S`, `/desktop` and `/D=` are supported. The uninstaller removes only its own files and asks before deleting settings.
- `gotm-0.8.0-portable.zip`
- `README.txt` — end-user instructions.
- `SHA256SUMS.txt`

THIRD-PARTY-NOTICES.txt is generated from the modules actually linked: Go, walk, win, wingoes, x/sys, x/exp, govaluate. Their BSD/MIT licenses require these notices in any binary distribution.
