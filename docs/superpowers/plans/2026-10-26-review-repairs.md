# Review Repairs Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development or superpowers:executing-plans to implement these tasks with regression checks.

**Goal:** Repair all seven findings from the project review and independently check the complete patch.

**Architecture:** Preserve the native Go/Win32 application and public metrics APIs. Correct time handling at the recorder/chart boundaries, keep native and toolkit selection synchronized, separate slow sensor polling, implement service enumeration continuation, and enforce protected installation destinations.

**Tech Stack:** Go 1.26.6, Windows APIs, Walk, PowerShell.

**Spec:** The seven findings in the preceding project review, authorized for repair by the user on 2026-10-26.

## Global Constraints

- No new dependencies or unrelated redesign.
- Preserve protected custom installation paths and in-place upgrades; reject unsafe paths before elevated writes and machine-wide registration.
- Tests must not install/uninstall the real application, change actual services or power plans, or enable optional hardware SDKs.
- Use targeted failing regression tests before fixes where feasible, then owning-package checks.
- Work in the existing clean checkout, keep changes uncommitted, and preserve unrelated user changes.
- Windows API integration tests may run outside the sandbox; exclude `^TestElevated`.

## Review Focus

- Protected custom destinations, user-owned or user-writable ancestors, reparse points, and existing executable files: Task 1.
- Five/ten-second sampling, recording gaps, exact chart edges and marker/cursor alignment: Task 2.
- Offscreen selection after reset without scrolling, including normal visible selection: Task 2.
- Multiple enumeration pages and errors; inexpensive power refresh with drive polling suspended: Task 3.
- Backward or duplicate timestamps, bounded memory, nearest/range lookup, persistence replay: Task 4.

## Task 1: Installer boundary and signing

**Files:** `tools/setup/{main.go,install.go,setup_test.go}`, focused destination helper/test files as needed, `build.ps1`, `tools/test-build.ps1`.

**Interfaces:** Retain `/D=`, machine-wide install and upgrade behavior for protected paths. No changes to UI or metrics APIs.

- [x] Add and run regressions for unsafe writable/user-owned destinations and ancestors, redirected paths, and valid protected destinations. Use temporary fixtures or injected system checks, with at least one real Windows ACL check.
- [x] Enforce destination security before payload/uninstaller writes and registration, including upgrades and existing owned files. Reconcile the independent security-boundary investigation before implementation.
- [x] Add a signing regression showing a preceding native exit code of 7 cannot invalidate a successful PowerShell helper; remove the stale exit-code check.
- [x] Run setup tests, packaging tests, and self-review the security trigger, alternate unsafe input, legitimate control, direct callers, errors and validation/use transitions.
- [x] Report files, evidence, and any limitations; do not commit.

## Task 2: Recorder plotting and table selection

**Files:** `internal/ui/{pro_recorder.go,gfx.go,tables.go}`, focused UI regression tests.

**Interfaces:** Continue consuming `metrics.RecSample.T`, existing recorder methods and `walk.TableView`. Coordinate with Task 4 without requiring new metrics APIs.

- [x] Add and run a failing regression proving samples with five-second cadence/gaps use the same timestamp-to-x mapping as markers and scrubbing, including chart edges.
- [x] Render recorder samples using their timestamps rather than contiguous two-second indices; retain general graph behavior for other pages.
- [x] Add a meaningful regression for preserving Walk current index and native selection when the row is offscreen after reset, without forcing scrolling.
- [x] Synchronize both forms of selection through the existing toolkit APIs, preserving visible-row and no-selection behavior.
- [x] Run UI tests and inspect the pinned Walk dependency where necessary. Report files, evidence, and limitations; do not commit.

## Task 3: Enumeration continuation and power refresh

**Files:** `internal/metrics/{services.go,drivers.go,sensors.go}`, focused metrics regression tests and a shared enumeration helper if appropriate.

**Interfaces:** Preserve `ListServices()`, `ReadDrivers()`, `SensorHub.Power()` and `SetDetailWanted(bool)` signatures.

- [x] Add and run a failing regression for multi-page service enumeration, preserving returned rows and resume handles; cover no rows, subsequent-page errors, and normal one-page success.
- [x] Implement continuation for both service and driver readers without oversized allocations or infinite retry loops.
- [x] Add and run a failing regression showing power state refreshes after its interval while drive polling remains suspended outside Power & Freq.
- [x] Separate inexpensive power-state refresh from gated drive-health polling, preserving initial drive discovery and detail-page refresh.
- [x] Run focused metrics tests and report files, evidence, and limitations; do not commit.

## Task 4: Recorder ordering and final verification

**Files:** `internal/metrics/recorder.go`, `internal/metrics/pro_test.go` or focused recorder tests.

**Interfaces:** Preserve all existing recorder APIs and chronological, unique timestamps required by binary search.

- [x] Add and run failing regressions for samples at `t`, `t+2s`, `t-10s`, subsequent recovery, and duplicate timestamps; assert range and nearest lookup return the intended samples.
- [x] Restore chronological, unique in-memory history when the clock moves backward, keeping bounded capacity, recording interval behavior and disk replay compatible.
- [x] Run recorder regressions and owning-package tests.
- [x] Inspect every task diff, run `go vet ./...`, `go test -count=1 -timeout 180s -skip '^TestElevated' ./...`, and `tools/test-build.ps1`.
- [x] Obtain fresh independent reviews of each task and the complete patch, including a security bypass/regression review. Repair confirmed residual issues and rerun affected checks.
- [x] Confirm a clean diff check, no temporary files, and report remaining validation limits honestly.
## Verification outcome

- All seven review findings repaired, with regression coverage.
- Independent metrics/recorder and UI reviews: specification and quality pass; no actionable findings.
- Installer bypass review found a SUBST-drive alias route; native root/volume identity checks now reject it and mapped-network roots. Scoped security re-review: addressed, no new breakage.
- Independent complete-patch review: no important actionable findings or verified cross-component regressions.
- `go vet ./...`: passed.
- `go test -count=1 -timeout 180s -skip '^TestElevated' ./...`: passed with normal Windows access.
- `tools/test-build.ps1`: all three packaging regressions passed.
- App and setup compilation: passed. The rebuilt app is `dist/review-20261026/gotm.exe`; the setup compilation artifact was only a build check.
- Real elevated installation/uninstallation, administrator-only live actions, actual signing, and interactive multi-DPI inspection were not executed. Native ACL/root-volume controls, temporary HKCU lifecycle fixtures, bitmap rendering and hidden table controls were exercised.
- Repairs were initially left uncommitted as requested by the implementation workflow; no dependencies changed.

## Release follow-up

The user subsequently authorized committing the repairs and rebuilding the installer for testing. Package this patch as v0.9.3, with matching app and installer version metadata, a portable archive, notices and checksums. Run the non-administrator test suite before committing; packaging may use `-SkipTests` after that successful run so administrator-only integration tests are not invoked. The installer is built for the user to test; do not execute it automatically.
