# Task Manager Replacement Plan

**Goal:** Let users opt in to opening gotm through normal Windows Task Manager launches, and restore Windows routing safely.

**Design:** Use Windows IFEO's machine-wide `taskmgr.exe` `Debugger` value with `"<protected installed gotm.exe>" --task-manager --`. The final argument boundary isolates Windows-appended arguments from gotm options. Settings reads state; the installed setup/uninstaller owns all writes. The existing protected-destination guard is shared through `internal/installpath`, so the GUI validates the helper before executing it and the helper independently validates its installation. Replacement is off until explicitly enabled. Existing foreign replacements or active path filters are not overwritten. No Windows binary is changed.

**Alternatives:** A custom shortcut covers only that shortcut; keyboard interception misses other launch paths. IFEO covers launches of taskmgr.exe and has an established Windows implementation. Share the existing helper's ACL/path policy with the GUI instead of maintaining two implementations.

## Constraints

- Windows 10/11 x64, current dependencies only.
- Never modify the real Task Manager routing during development tests. Registry tests use isolated HKCU scratch keys.
- The installed helper must verify its own registered location and protected destination before enabling. Refuse portable/uninstalled copies, insecure paths, aliases and foreign routing.
- Restore deletes only the exact owned Debugger value and preserves every unrelated value/subkey. Restore failure aborts uninstall before any executable deletion.
- Retarget an owned old installation only after the new protected payload exists; failed updates must leave usable routing. An old uninstaller must preserve routing to the newer install.
- Machine scope is explicit in Settings. Non-elevated users get the existing Run as administrator path. No automatic UAC or global activation during development.

## Task 1: Registry controller

Own new `internal/taskmanager` package and tests. Interfaces:

```go
type Store struct { Root registry.Key; Path string }
type State struct { Owned bool; Conflict bool; Command string }
func MachineStore() Store
func DebuggerCommand(exe string) (string, error)
func (s Store) Query(exe string) (State, error)
func (s Store) Enable(exe string) error
func (s Store) Restore(exe string) error
func (s Store) Retarget(oldExe, newExe string) (bool, error)
```

- [x] Red/green scratch-key tests: absent, exact owned, foreign, malformed/types, active/unsupported UseFilter, unrelated values/subkeys, repeated calls, read/write errors and retargeting.
- [x] Implement REG_SZ command with quoted absolute executable and `--task-manager --`; explicit shared Windows registry view; reject command injection/path controls.
- [x] Refuse conflicts; retain all unrelated state; expose errors rather than reporting success after denied writes.
- [x] Run package tests; self-review and report.

## Task 2: Installed helper and lifecycle

Own `tools/setup` changes and tests. Consume Task 1 API. Add internal test-layout store override so existing temporary install tests never access machine routing.

- [x] Parse `/taskmanager=on|off`, reject ambiguous/conflicting modes, and dispatch before opening embedded payload.
- [x] The mode operates only through installed `uninstall.exe`, verified against the recorded install location and destination guard. No enablement simply by running setup.
- [x] Add CLI maintenance error messages and `/S` exit-code behavior. Use helper flow in tests without changing real HKLM.
- [x] Restore owned routing before uninstall payload deletion; refuse deletion on restoration failure. Allow recovery with a missing gotm executable.
- [x] Preserve same-path upgrades; retarget owned routing on relocation with conditional rollback if metadata registration fails. Leave foreign routing unchanged.
- [x] Red/green tests for helper authorization/path, conflicts, lifecycle order/failure/relocation and old-uninstaller preservation; run setup checks.
- [x] Match native directory and helper identities before selecting the recorded uninstall path. Native 8.3 launch and registration tests verify restoration before deletion; unknown identity fails safely.

## Task 3: App launch, Settings and release

Own `main.go`, app flag tests, `internal/ui/settings.go`/tests, docs and version files. Consume Task 1 API and Task 2 helper mode.

- [x] Add `--task-manager` that opens Processes and ignores every appended argument after the explicit boundary. Test target path, `/7`, apparent gotm options and diagnostic isolation.
- [x] Add Settings machine-wide status/control with accessible name, keyboard support, busy/error handling and restore behavior. Run the verified installed helper on a worker goroutine, then refresh state on the UI thread. Avoid per-frame registry reads.
- [x] Guide unelevated or portable users to elevated installed gotm. Handle foreign replacement without replacing it.
- [x] Document enable/restore, all-user scope, launch coverage, safe uninstall and recovery CLI.
- [x] Independent review of complete feature, full non-elevated suite, vet and packaging checks.

Verified `go test -count=1 -timeout 180s -skip '^TestElevated' ./...`, `go vet ./...`, `tools/test-build.ps1` and `git diff --check`. Both native short-path uninstall regressions executed without skips. Independent review found a filtered-routing status issue and an alias-uninstall issue; both were repaired and rechecked, with no remaining Important/Critical findings. Tests ran with the normal Windows user token outside AppContainer, using fake payloads and isolated HKCU routing keys.

## Release verification

After committing the reviewed sources, build v0.9.4 and validate the release checksums, portable ZIP, embedded installer payload, executable metadata, manifests and clean commit provenance. Record the results with the release output. Do not execute setup or activate replacement during validation. Live Task Manager launches and UAC integration remain an installation test for the user.
