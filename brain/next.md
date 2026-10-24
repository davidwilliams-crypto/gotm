# Next

## v0.9.0 — the last open limits
- [?] High-contrast palette from system colours, live re-theme (theme.go, app.go retheme)
- [?] UI Automation provider for the drawn surfaces + keyboard on sidebar/status bar/drive strip (uia.go, widgets.go, keys.go)
- [?] AMD GPU/APU sensors via the driver's ADL (adl.go), per-GPU groups in Power & Freq, GPU card facts
- [?] Process tree: native indent so icons follow the names (tables.go indentRows)
- [?] Elevated live checks ran on this PC (2026-10-24): service control, SYSTEM process, process actions, Ryzen SDK all pass; found + fixed the SetDefaultDllDirectories/SDK conflict
- [?] Confirmed: elevated 0.9.1 capture shows Tctl/Tdie 56.5 °C + 16 core temperatures + cHTC (2026-10-24)
- [?] ACPI placeholder zone (constant 290 K / 16.9 °C) shown as a note, not a temperature (0.9.2)
- [?] Release kit 0.9.2 (build.ps1 -Version 0.9.2) delivered: dist\release\gotm-0.9.2\ (setup, portable, README, SHA256SUMS); 0.9.0/0.9.1 superseded

## v0.8.0 — 360 review and distribution
- [?] Independent 3-way review; all Critical/Important fixed, most Minor (docs/REVIEW-0.8.md)
- [?] Self-contained installer (tools/setup) + notices + end-user README + checksums (dist\release\gotm-0.8.0)
- [ ] Run gotm-0.8.0-setup.exe once on a real PC (needs an administrator prompt): install, open from Start, uninstall
- [?] Build-side signing ready: `build.ps1 -TrustedSigning` + signing.json + tools/get-signing-client.ps1 + docs/SIGNING.md (2026-10-24)
- [ ] Azure side (user): subscription, Trusted Signing account (East US), organisation identity validation, certificate profile, Certificate Profile Signer role, sign in, fill signing.json, then build signed
- [?] High-contrast support and screen-reader exposure for the owner-drawn pages — done in v0.9.0

## Follow-up after v0.7.0 polish
- [x] Shared readability/layout, loading and no-match states, stable sorting and selection
- [x] Atomic settings and recorder/disk reliability fixes
- [x] Keyboard controls and regression coverage for Performance, Settings and Recorder
- [x] Release vet/tests/build, portable package and version verification
- [ ] Resume live UI verification when requested: final Tab/focus/activation, all pages and compact/DPI layouts (Computer Use was stopped with Escape)
- [x] Compile installer when Inno Setup 6 is available — superseded: own Go installer in v0.8.0
- Details and limits: docs/POLISH.md

## Build plan (from the TMOG breakdown)
- [?] 1. Pick the stack — Go, native Win32 via Walk
- [?] 2. Feature spec — docs/FEATURES.md (edit the Keep/Change/Drop column)
- [?] 3. Metrics core
- [?] 4. 1-second sampler with history ring buffer
- [?] 5. Native UI
- [?] 6. Actions — end task/tree, priority, affinity, suspend/resume, open location, run-as-admin
- [~] 7. Your customizations — palette, columns, metrics and pages are single-place edits; your actual changes still to come
- [?] 8. Package — build.ps1 release kit: installer, portable zip, notices, checksums

## Ryzen temperature (AMD Monitoring SDK)
- [?] Extract the SDK (admin install `/a`, no system change) and read headers/sample
- [?] Go binding + sensor-tree integration + Settings toggle + tests (ryzensdk.go)
- [?] Settings page scrolling
- [~] Live check, elevated: TestElevatedRyzenSDK via tools/elevated-check.ps1 (v0.9.0); comparing Tctl with Ryzen Master side by side is still the user's call
- [ ] Decide how the SDK gets installed for real (AMD installer vs. extracted folder + registry key `HKLM\SOFTWARE\AMD\RyzenMasterMonitoringSDK\InstallationPath`)

## Polish pass (v0.6.0)
- [?] Readable headers, dark menus, flex column, search hints, shortcuts, focus
- [?] Processes icons, priority menu, tooltips; Performance CPU card + GPU list scroll; sensor order; recorder auto-fit; disk % used; reopen last page
- [?] Process tree: icons sit at the left edge while names are indented — fixed in v0.9.0 (LVN_GETDISPINFO indent hook)

## GPU page
- [?] Per-adapter cards, engine graphs, memory graphs, per-process memory, NVML sensors
- [?] AMD iGPU clock/power/temperature via AMD ADL — done in v0.9.0 (adl.go)
- [ ] GPU page: scroll the process list (shows ~4-7 rows today)

## Requested follow-ups
- [?] Process tree view
- [?] Native Windows application instead of a local web server
- [?] Redesign inspired by TMOG (dark theme, sidebar, cards, smooth graphs)
- [?] Fix refresh blink (measured: no collapsed frames)
- [?] All PRO pages: Power & Freq, Flight Recorder, Connections, Installed Apps, Drivers, Disk Space

## Verification still owed
- [ ] Run as administrator from the footer chip and confirm the elevated window
- [~] Try service Start/Stop, End task / priority / affinity — covered elevated by elevated_test.go (v0.9.0); startup enable/disable still manual
- [ ] Try Uninstall… on a throwaway app (it only launches the app's own uninstaller after a confirmation)
- [x] Compile installer\gotm.iss once Inno Setup 6 is installed — superseded by the Go installer
- [ ] Decide on the open items in docs/FEATURES.md

## Ideas from the TMOG analysis (not built)
- [ ] JSON skins (palette, typography, which cards show) — gotm's palette is already one struct
- [ ] Pop-out mini window (always-on-top) and deep links per page (--tab exists)
- [ ] App history from the SRUM database (per-app CPU/network over days)
- [ ] Users page (WTS sessions reader already exists)
- [ ] WinVerifyTrust signature check for processes / startup / drivers
