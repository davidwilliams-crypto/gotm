# gotm

A **native Windows task manager written in Go**: a metrics core that reads the
system directly (no cgo, no third-party runtime) behind a dark, owner-drawn
Win32 interface. No browser, no local web server.

```
.\build.ps1 -Version 0.9.5   # resources, vet, tests, gotm.exe, installer and portable zip in dist\release\gotm-0.9.5
.\dist\gotm.exe        # run; reopens the page you last used (or --tab performance|processes|startup|services|power|recorder|connections|apps|drivers|disk|settings)
.\tools\elevated-check.ps1   # one UAC prompt: runs the administrator-only self-tests and captures the elevated app
```

Windows 10/11 x64. Click **“Run as administrator”** in the footer to
relaunch as administrator (needed to end protected processes, control most
services, and read some drive details).

## Replace Windows Task Manager

Install gotm, open its installed copy, choose **Run as administrator**, then
open **Settings ▸ Windows Task Manager ▸ Replace Task Manager**. Once enabled,
launches of `taskmgr.exe` (including Ctrl+Shift+Esc and the usual Windows Task
Manager commands) open gotm's Processes page. The switch applies to every user
on the PC and remains off until enabled explicitly.

Choose **Restore Windows Task Manager** in the same place to turn it off.
Uninstall restores any routing still owned by that installation before removing
the executable. Existing replacements or active launch filters from another
program are preserved; restore those through their owning program first.
Portable and unprotected copies cannot enable replacement.

For recovery when the UI cannot open, use an administrator PowerShell window:

```powershell
& 'C:\Program Files\gotm\uninstall.exe' /taskmanager=off
```

Use the actual installation folder if a protected custom folder was chosen.
This recovery action also works when `gotm.exe` is missing. Setup and uninstall
maintain the setting across in-place upgrades and protected-folder relocations.

## Distributing

`build.ps1` produces `dist\release\gotm-<version>\`:

| File | For |
|---|---|
| `gotm-<v>-setup.exe` | normal installs. Asks for administrator rights, installs to `C:\Program Files\gotm`, adds a Start menu shortcut and an Apps & features entry; re-running upgrades in place. `/S` silent, `/desktop` adds a desktop shortcut, `/D=<folder>` custom folder. |
| `gotm-<v>-portable.zip` | no install: unzip and run `gotm.exe`. |
| `README.txt` | end-user instructions (also inside both packages). |
| `SHA256SUMS.txt` | checksums to check copies against. |

gotm installs for all users on purpose: it can relaunch itself elevated, and an
elevated program should only ever run from a folder that standard users cannot
modify. Uninstall removes only the files it installed and asks before deleting
the user's settings. Custom `/D=` destinations must be local folders protected
from standard-user changes, with trusted ownership throughout their parent
chain. Setup rejects user-owned or writable paths, reparse points and ambiguous
path names before installing or upgrading.

Unsigned executables make SmartScreen warn on first run on other PCs. Two ways
to sign both executables (each build also verifies its signatures):

- `.\build.ps1 -Version x.y.z -TrustedSigning` — Azure Trusted Signing, with
  the account settings in `signing.json`; the one-time Azure setup is in
  [docs/SIGNING.md](docs/SIGNING.md).
- `.\build.ps1 -Version x.y.z -SignThumbprint <SHA-1 thumbprint>` — a
  code-signing certificate in the Windows certificate store (token or HSM).

## Pages

| Page | What it does |
|---|---|
| **Performance** | resource cards (CPU, Memory, **one per GPU**, Disk, Network) with live sparklines; GPU detail with per-engine graphs, dedicated/shared memory history, clocks/power/temperature (NVIDIA via NVML, AMD via the driver's ADL library), driver and PCI location, and which processes hold video memory; big graph; per-core grid with clocks; facts grid (speed, sockets, cores, caches, virtualization, power plan…) |
| **Processes** | program icons; sort, search, **tree view** (collapse, subtree totals), group by name; end task / tree, priority (button menu or right-click ▸ Set priority), affinity, suspend / resume, open location |
| **Startup apps** | registry Run keys, Startup folders, logon scheduled tasks; tick to enable/disable |
| **Services** | state, start type, PID; start / stop / restart |
| **Insights ▸ Power & Freq** | sensor tree with live / min / max: CPU clocks per core, ACPI thermal zones, utilization, memory, one group per GPU (temperatures, clocks, power, voltage, fan, engines, memory), drive temperature and NVMe health, power plan; search includes collapsed branches |
| **Insights ▸ Flight Recorder** | manually started timeline of CPU / memory / GPU; scrub with the mouse or keyboard; markers at the selected moment; CSV export; optional on-disk history |
| **Insights ▸ Connections** | TCP/UDP endpoints with owning process, state and binding (Network vs Local only); binding alone does not determine firewall reachability |
| **Insights ▸ Installed Apps** | programs registered with Windows: version, publisher, size, date; open folder; uninstall |
| **Insights ▸ Drivers** | kernel / file-system drivers with publisher and version; filter to running or non-Microsoft |
| **Insights ▸ Disk Space** | drive capacity; folder-size breakdown and largest files for any folder; drill in, cancel, open in Explorer; partial and unreadable results are labelled |
| **Settings** | refresh rate, graph history, process columns, alert thresholds, recorder persistence, AMD Ryzen sensors (scrolls) |

## Keyboard

| Keys | Action |
|---|---|
| Ctrl+1 … Ctrl+9, Ctrl+0 | go to a page, in sidebar order |
| Ctrl+, | Settings |
| Ctrl+F | search the current page (Esc clears, ↓ or Enter moves into the list) |
| F5 | reload the current page |
| Delete | end the selected task (Processes) |
| ← / → | collapse / expand a branch in the process tree |
| Tab / Shift+Tab | move between controls; Space or Enter activates toolbar buttons |
| ↑ / ↓, Enter (sidebar, status bar, drive strip) | choose a page, a status-bar button or a drive |
| ↑ / ↓, Home / End | select a resource on Performance; Space or Enter toggles CPU/GPU graph views |
| ← / →, Home / End | move between recorded samples; Home selects the oldest, End returns to live |
| Space / Enter (recorder chart) | start/pause recording / return to live |
| Tab / Shift+Tab, Space / Enter (Settings) | select and change a setting; focus scrolls into view |
| PgUp / PgDn, ↑ / ↓, Home / End (Settings) | scroll the Settings page |

The same list is on the Settings page.

Alerts (CPU/memory/disk/GPU above N % for S seconds) show a chip in the header
and a tray notification. All pages are available; the sidebar groups advanced tools under **Insights**.

## Accessibility

- **Screen readers.** The owner-drawn surfaces (sidebar, Performance, Settings, Flight Recorder, drive strip, header and status bar) carry their own UI Automation provider (`internal/ui/uia.go`): every card, chip, row of facts and button is an element with a name, a control type, a bounding rectangle and the Invoke / SelectionItem / Toggle patterns, and keyboard focus inside a canvas raises focus-changed events. Tables, buttons and search boxes are native and named. Verified with a UI Automation client; invoking a sidebar item navigates.
- **Keyboard.** Every drawn surface is reachable with Tab and operated with the arrow keys, Home/End, Space and Enter; Ctrl+1…0 reach every page directly.
- **High contrast.** When a Windows high-contrast theme is active, gotm builds its palette from the system colours (window, text, selection, disabled text), outlines every card and separator in the text colour, keeps the resource hues legible against the chosen background, and switches live when the theme changes. `GOTM_HIGH_CONTRAST=1` forces that palette for testing.

## v0.7.0 polish

- Larger table text, stronger secondary-text contrast, separate action/search rows, accessible button names, and visible keyboard focus.
- Stable row identities preserve selections through sorting and refreshes. Numeric columns sort by value; text columns sort by their full text.
- Settings save through an atomic file replacement. Failed saves retain the previous running configuration; process sorting survives other settings changes.
- Recorder replay sorts and deduplicates saved history and ignores future samples. Pausing freezes the visible timeline; clearing immediately allows a new sample.
- Packaging stops at the first failed command, restores the shell environment, and stamps the same release version into the app and Windows file properties.

Run `go vet ./...`, `go test -count=1 ./...`, and `.\tools\test-build.ps1` for automated checks. Windows integration tests need ordinary access to services, processes and loopback sockets; a restricted sandbox can block those APIs.

## How it avoids flicker

- Navigation, cards and graphs are owner-drawn into a back buffer in one paint, so a refresh replaces the whole frame atomically.
- Graphs are drawn 3× and halftone-downsampled for smooth, anti-aliased curves.
- Tables are native list views with `LVS_EX_DOUBLEBUFFER`; an update repaints only the visible rows (`LVM_REDRAWITEMS`) and only resets the model when the row order actually changed.

Verified by sampling the window ~75 times a second for 8 s per page: the visible content never collapsed in any frame.

## Layout

| Path | Purpose |
|---|---|
| `main.go` | init, flags |
| `internal/metrics` | sampler + Windows readers: processes, CPU, memory, PDH, services, startup, **power, drive health, connections, apps, drivers, users, disk scan, flight recorder, sensor tree**; `tree.go` builds flat / tree / grouped process rows |
| `internal/ui` | Walk-based UI: `app.go` window, `shell.go` sidebar/header/footer/tray/alerts, `theme.go` (dark and high-contrast palettes) + `gfx.go` drawing, `widgets.go` canvas views and buttons, `uia.go` UI Automation provider for the drawn surfaces, `grid.go` table pages, one file per page |
| `internal/config` | `%APPDATA%\gotm\config.json` |
| `winres/`, `tools/genicon` | icon, version info, manifest → `rsrc_windows_amd64.syso` |
| `tools/setup` | installer/uninstaller (Go, no third-party tool): embeds gotm.exe, installs to Program Files for all users, Start menu shortcut, Apps & features entry |
| `tools/notices` | writes THIRD-PARTY-NOTICES.txt from the modules linked into gotm.exe |
| `tools/elevated-check.ps1` | runs the administrator-only self-tests (`internal/metrics/elevated_test.go`: service control, ending a SYSTEM process, the AMD SDK) through one UAC prompt |
| `docs/README-user.txt` | the README.txt shipped to end users |
| `docs/FEATURES.md` | feature spec and status |

## Data sources

| Data | API |
|---|---|
| Processes, per-process CPU / memory / I/O | `NtQuerySystemInformation(SystemProcessInformation)` |
| Per-core CPU | `NtQuerySystemInformation(SystemProcessorPerformanceInformation)` |
| Memory, commit, pools, handles | `GetPerformanceInfo` |
| Disk, network, GPU engines and memory (per adapter and per process), per-core clock, thermal zones, power meters | PDH counters (disk active time = 100 − `% Idle Time`) |
| GPU names, memory sizes, LUIDs, PCI location | DXGI (`CreateDXGIFactory1`, vtable calls), D3DKMT adapter address; driver version/date from the registry |
| NVIDIA GPU clock, power, temperature, fan | NVML (`nvml.dll`, shipped with the driver) |
| AMD GPU / APU clocks, temperatures, power, voltage, fan (and the CPU side of an APU where the driver reports it) | AMD Display Library (`atiadlxx.dll`, shipped with the driver): `ADL2_New_QueryPMLogData_Get`, falling back to Overdrive 5; adapters matched by PCI bus |
| CPU topology, caches, ratings | `GetLogicalProcessorInformationEx`, `CallNtPowerInformation` |
| Power plan, battery, Fast Startup | `PowerGetActiveScheme`, `GetSystemPowerStatus`, registry |
| Drive model, temperature, NVMe health | `IOCTL_STORAGE_QUERY_PROPERTY` on `\\.\PhysicalDriveN` |
| Connections | `GetExtendedTcpTable`, `GetExtendedUdpTable` |
| Installed apps | `…\CurrentVersion\Uninstall` (HKLM, WOW6432Node, HKCU) |
| Drivers | `EnumServicesStatusEx(SERVICE_DRIVER)` + `GetFileVersionInfo` |
| Services, startup | SCM, registry Run keys, `StartupApproved`, `schtasks` |
| Ryzen temperature, PPT/TDC/EDC, per-core clocks (optional) | AMD Ryzen Master Monitoring SDK: `Platform.dll` `GetPlatform` → `ICPUEx::GetCPUParameters` (≤ 1 call/s) |

## Customizing

1. **Settings page / `config.json`** — refresh, history, columns, alerts, recorder.
2. **Theme** — one `palette` in `internal/ui/theme.go` (and `blend()` for tints).
3. **Process columns** — `colDefs()` in `internal/ui/procs.go`.
4. **Metrics** — add a field to `Snapshot` (`internal/metrics/sampler.go`), read it in `tick()`, draw it on a page.
5. **Pages** — implement the small `page` interface (`build/init/shown/tick`), add it to the list in `app.go`.

## Known limits

- **Ryzen power** (package and per core) comes from Windows' own Energy Meter counters and needs no driver.
- **Ryzen die temperature (Tctl/Tdie), per-core temperature, PPT/TDC/EDC, rail power, effective clocks** are not exposed by Windows; they need a kernel driver. gotm never ships or installs one. Two optional sources, both off until you turn them on:
  - **AMD Ryzen Master Monitoring SDK** (Settings ▸ AMD Ryzen sensors). Install AMD's SDK first (it brings its signed driver); gotm then loads `Platform.dll` from the path the SDK registers in HKLM, requires a valid Authenticode signature *by AMD*, and only reads. The switch is machine-wide (HKLM\SOFTWARE\gotm), so it is changed while gotm runs as administrator; a marker there turns it off after a crash during start-up.
  - **LibreHardwareMonitor** with its web server on `127.0.0.1:8085` (loopback only).
  If neither is available the tree says so instead of inventing a number.
- AMD GPU sensors come from the driver's ADL library, which reports what the hardware exposes: an APU shows its graphics temperature, core and memory clocks, voltage and the package power shared with the CPU (labelled as such), not a separate graphics-only power figure. Intel graphics expose no clocks or temperature through a public Windows API, and gotm says so instead of guessing.
- Per-process GPU memory is "resident in video memory" (the `Local Usage` counter). The per-process `Dedicated Usage` counter reports virtual allocations (dwm.exe read 46 GB on a 15 GB card) and is not used.
- NVMe S.M.A.R.T. is read natively; SATA drives show model and temperature where the drive reports it.
- ACPI thermal zones are shown as board temperatures, except zones that report a constant placeholder (many desktop boards fill an unused zone with 290 K, shown by Windows as 16.9 °C): those are labelled as placeholders, not temperatures.
- Affinity covers processor group 0 only (≤ 64 logical CPUs); CPU usage itself reads every processor group.
- GPU figures need WDDM counters (Windows 10 1709+).
- Scheduled-task startup entries come from `schtasks /query /xml` (independent of the display language).
- Screen readers hear the drawn surfaces through gotm's own UI Automation provider; the graphs themselves are summarised by their captions and values rather than read point by point.
- Dark theme (or the system's high-contrast colours). Context menus and table headers are dark (via the same uxtheme dark-mode switches Explorer uses, looked up defensively); native dialogs such as file pickers and message boxes stay light.
- The executable is unsigned; SmartScreen and some AV products may warn.
- UI models and keyboard logic are unit-tested without creating windows. Actual window rendering and native dialogs require the embedded Windows manifest and live inspection.

## License

MIT, see [LICENSE](LICENSE). The third-party components linked into the binaries and their licences are listed in THIRD-PARTY-NOTICES.txt, generated at build time.
