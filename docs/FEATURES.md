# gotm — feature spec

Step 2 of the build plan. This is the requirements list gotm is built against.
It describes **what a task manager shows and does**, using public Windows
concepts only; it is not derived from any other product's code. Edit the
**Keep / Change / Drop** column to steer the next iteration.

Legend: ✅ implemented · 🟡 partial · ⬜ not yet

## Processes

| Feature | Status | Source (Windows API) | Keep / Change / Drop |
|---|---|---|---|
| Live table: name, PID, parent, CPU %, memory, private bytes, I/O rate, GPU %, threads, handles, priority, user, session, start time, image path | ✅ | `NtQuerySystemInformation(SystemProcessInformation)`, `QueryFullProcessImageName`, token → SID lookup | Keep |
| Per-process CPU % (share of all logical CPUs) | ✅ | Δ(kernel+user time) ÷ Δ(total CPU time) | Keep |
| Per-process GPU % (busiest engine type) | ✅ | PDH `\GPU Engine(*)\Utilization Percentage` | Keep |
| Sort by any column, text filter (name / PID / user / path) | ✅ | client-side | Keep |
| Group by executable name | ✅ | client-side | Keep |
| Column chooser, persisted | ✅ | `config.json` | Keep |
| End task / End process tree | ✅ | `TerminateProcess` | Keep |
| Set priority class | ✅ | `SetPriorityClass` | Keep |
| Set CPU affinity (group 0, ≤ 64 CPUs) | 🟡 | `Get/SetProcessAffinityMask` | Change: processor-group aware for >64 CPUs |
| Suspend / Resume | ✅ | `NtSuspendProcess` / `NtResumeProcess` | Keep |
| Open file location | ✅ | `explorer.exe /select,` | Keep |
| Process tree view (indent by parent, collapse, subtree totals) | ✅ | PPID + start time (guards against PID reuse) | Keep |
| Per-process network I/O | ⬜ | needs ETW (kernel network provider) | Add if wanted |

## Performance

| Feature | Status | Source | Keep / Change / Drop |
|---|---|---|---|
| CPU total + per-logical-CPU graphs | ✅ | `SystemProcessorPerformanceInformation` | Keep |
| Memory: in use, available, committed, cached, paged / non-paged pool | ✅ | `GetPerformanceInfo` | Keep |
| Disk: read/write bytes/s, active time, volume capacity | ✅ | PDH `\PhysicalDisk(_Total)\…`, `GetDiskFreeSpaceEx` | Keep |
| Network: send/receive bytes/s across adapters | ✅ | PDH `\Network Interface(*)\…` | Keep |
| GPU: utilization, dedicated memory | ✅ | PDH `\GPU Engine(*)`, `\GPU Adapter Memory(*)` | Keep |
| Adjustable history length (1–60 min) and refresh (0.5–5 s) | ✅ | config | Keep |
| CPU frequency / temperature / power | ⬜ | vendor sensors or WMI; no portable API | Decide |

## Startup

| Feature | Status | Source | Keep / Change / Drop |
|---|---|---|---|
| Registry Run keys (HKCU, HKLM, HKLM 32-bit) | ✅ | `registry` | Keep |
| Startup folders (user, all users) | ✅ | filesystem | Keep |
| Scheduled tasks triggered at logon / startup | ✅ | `schtasks /query /fo CSV /v` | Keep |
| Enable / disable | ✅ | `StartupApproved` registry values (same mechanism Windows uses); tasks via `schtasks /change` | Keep |
| Startup impact rating | ⬜ | needs boot-trace data | Drop? |

## Services

| Feature | Status | Source | Keep / Change / Drop |
|---|---|---|---|
| List with state, start type, PID | ✅ | `EnumServicesStatusEx`, `QueryServiceConfig` | Keep |
| Start / Stop / Restart | ✅ | Service Control Manager (needs admin for most) | Keep |
| Change start type | ⬜ | `ChangeServiceConfig` | Add if wanted |

## PRO pages

| Feature | Status | Source | Keep / Change / Drop |
|---|---|---|---|
| Power & Freq sensor tree with live / min / max | ✅ | PDH, `CallNtPowerInformation`, storage IOCTLs | Keep |
| CPU package + per-core power | ✅ | Windows Energy Meter counters (no driver) | Keep |
| CPU die temperature (Tctl/Tdie) | ✅ | opt-in: AMD Ryzen Master Monitoring SDK (Settings) or LibreHardwareMonitor over loopback; both need a kernel driver, gotm ships none | Keep |
| Drive temperature + NVMe S.M.A.R.T. | ✅ | `IOCTL_STORAGE_QUERY_PROPERTY` | Keep |
| SATA S.M.A.R.T. attributes | ⬜ | ATA pass-through, usually needs admin | Add if wanted |
| Flight recorder: timeline, scrub, markers, CSV | ✅ | sampler | Keep |
| Flight recorder: screen / video capture | ⬜ | Media Foundation | Decide |
| Connections with owner + exposure | ✅ | `GetExtendedTcp/UdpTable` | Keep |
| Close a connection | ⬜ | `SetTcpEntry` (admin) | Add if wanted |
| Installed apps + uninstall | ✅ | Uninstall registry keys | Keep |
| Drivers with publisher and version | ✅ | SCM + file version info | Keep |
| Driver signature verification | ⬜ | `WinVerifyTrust` | Add if wanted |
| Disk space: folders and largest files | ✅ | directory walk | Keep |
| App history (per-app resource use over days) | ⬜ | SRUM database (ESE) | Decide |
| Users page (sessions) | 🟡 | `WTSEnumerateSessions` reader exists, no page yet | Add if wanted |
| Skins (JSON themes) | ⬜ | the palette is already one struct | Decide |

## App shell

| Feature | Status | Notes |
|---|---|---|
| Native Win32 window (no browser, no server) | ✅ | Walk toolkit; owner-drawn shell, native list views |
| Dark theme, sidebar navigation, flicker-free refresh | ✅ | back-buffered painting, double-buffered tables |
| Tray icon with live CPU/RAM tooltip | ✅ | alert balloons |
| Alerts (metric above N % for S seconds) | ✅ | banner + tray balloon |
| Run as administrator | ✅ | `ShellExecute runas` (UAC), from the status bar |
| Dark native menus / dialogs | 🟡 | native context menus and table headers are dark; Windows file pickers and message boxes retain their system theme |
| Keyboard access to page controls | ✅ | toolbar Tab/Shift+Tab and activation; resource selection; recorder scrubbing; Settings navigation and scrolling |
| Stable table sorting and selection | ✅ | typed numeric sorts, identity-based selection across refreshes, collapsed-group search |
| Atomic settings persistence | ✅ | failures keep the running configuration; concurrent writes are serialized |
| Fail-fast versioned packaging | ✅ | native exit codes checked; app and Windows metadata share one version |
| Global hotkey | ⬜ | |
