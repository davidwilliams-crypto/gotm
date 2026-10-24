# GOTM 0.7.0 polish pass

2026-10-14. This pass improves the existing native Windows application across its eleven pages.

## Changes

- Shared interface: 10-point table text, stronger secondary-text contrast, separate action and search rows, shorter window title, responsive header/footer text, and an Insights label for advanced tools.
- Controls: accessible button names and disabled states, tooltips, visible focus, Tab routing, keyboard activation, and released popup-menu resources.
- Performance: keyboard resource selection and CPU/GPU view toggles; selection stays in bounds.
- Processes: sorting survives subsequent settings changes; protected PIDs disable toolbar actions; process-tree confirmation mentions unsaved data.
- Startup: selection follows the unique entry ID rather than a possibly duplicated display name; refresh has a loading state.
- Services: valid start/stop states, progress feedback, and disabled controls while an action is running.
- Power and sensors: min/max tracking runs even when another page is open; search includes collapsed branches and matching groups with their descendants.
- Connections: immediate filtering from cached data, stable endpoint identities, selection-preserving refresh, and accurate binding/firewall wording.
- Installed apps and drivers: stable identities, loading feedback, current-filter application, and explicit no-match feedback.
- Flight Recorder: sorted and deduplicated replay, rejection of future history, immediate capture after Clear, a frozen paused window, markers at the selected time, guarded clearing, and keyboard scrubbing from the displayed sample.
- Disk Space: early cancellation, root errors, partial-result feedback, path-based selection, numeric share sorting, an escape from an empty Largest files view, and status restoration after filtering.
- Settings: atomic serialized saves, isolated slices, validation of columns/sort/alerts, retained running settings on failure, preserved empty alert lists, and keyboard navigation that scrolls controls into view.
- Packaging: checked native exit codes, shell location/environment restoration, matching app/file versions, corrected Inno discovery, and an uninstall script that leaves user-added files alone.

## Verification

- `build.ps1 -Version 0.7.0` passed in an isolated copy of the final source: resource generation, `go vet ./...`, `go test -count=1 ./...`, executable build, and portable packaging.
- `tools/test-build.ps1` passed. A failing native command stops packaging and restores the shell environment and directory.
- Regression tests were observed failing before the corresponding fixes, including config failure/aliasing/concurrency, sort preservation, numeric sorting, nested sensor groups, scan cancellation, recorder replay/clear/clock handling, and canvas keyboard behavior.
- The executable's Windows FileVersion, ProductVersion, and command-line version report 0.7.0.
- An independent read-only source review found no remaining Critical or Important issue after its corrections.

## Live verification limits

Live screenshots covered baseline Performance, Processes, Flight Recorder and Disk Space, plus the polished Processes preview and its accessible toolbar names/disabled states. Computer Use was stopped with the physical Escape key. Final native Tab/focus/activation behavior, compact-window/DPI layouts and all-page rendering were therefore not completed live.

No destructive process/service/startup/uninstall action or optional AMD driver-backed SDK activation was executed. Inno Setup 6 is unavailable, so installer compilation remains unverified.

## Release files

- `dist/gotm.exe`: updated default executable.
- `dist/gotm-0.7.0.exe`: identical versioned executable.
- `dist/gotm-0.7.0-portable.zip`: executable and README.
- `dist/gotm-before-0.7.0.exe`: previous executable, preserved without closing its running window.
- `dist/gotm-source-before-360-polish.zip`: source snapshot captured before production edits.

Open GOTM windows retain the code they started with. Restart GOTM to use the new release.
