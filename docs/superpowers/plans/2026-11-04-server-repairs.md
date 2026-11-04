# Server repairs implementation plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task by task.

**Goal:** Fix the four observed test failures and three UI issues, rebuild with all checks enabled, and update the installed server app.

**Architecture:** Preserve the native Go/Walk architecture and installation security checks. Repair path validation, normalize test fixture paths, bound sampled CPU rates, share GPU summary inputs, and keep native table backgrounds consistent with the palette.

**Tech Stack:** Go, Win32, Walk, PowerShell.

**Spec:** User request in this chat: fix the reported failures and UI issues, rebuild, and update this server.

## Constraints and review focus

- Use the current checkout; no dependency upgrades or unrelated features.
- Preserve rejection of aliased installation destinations, including nonexistent child folders.
- Preserve stale-helper ownership checks; tests must use canonical fixture paths.
- GPU summary must exclude the same software/unknown devices as adapter cards.
- Preserve high-contrast colors and table selection during loading and refresh.
- Leave Task Manager replacement and optional sensor SDK settings unchanged.

## Tasks

- [x] Paths: extend `internal/installpath/fixes_test.go` to cover missing descendants; confirm failure; repair `NormalizeInstall` to inspect existing ancestors. Canonicalize `tools/setup` fixture roots; run installer/path tests.
- [x] Metrics: add regression coverage for CPU overrun, GPU summary consistency, and Intel temperature guidance; confirm failures; repair `sampler.go`, `perfcounters.go`, and `sensors.go`; run metrics tests.
- [x] Tables: reproduce background color loss with native controls; repair shared table styling; run UI tests and inspect live Drivers loading.
- [x] Verify: full vet/test/build without skips, build failure checks, review diff, and address regressions.
- [x] Deploy: close the old app normally, install the new build, verify version/hash/shortcut, launch and inspect repaired pages.

## Evidence and decisions

- Initial test rerun: installer shortcut and stale-helper tests fail with TEMP using ADMINI~1. Alias rejection test now passes only because the app is installed; add a missing-descendant regression.
- Initial live GPU test: visible DXGI adapter has no matching engine counters, while the separate summary includes other PDH adapters.
- Ruling: retain production stale-alias refusal; canonicalize test fixtures rather than weakening uninstall ownership validation.
- Regression tests observed failing before fixes: missing destination below short-path ancestor; process CPU counter skew; live GPU summary/card mismatch; Intel/unknown CPU guidance; native table container background. All affected package tests passed after fixes.
- Full `build.ps1 -Version 0.9.6` passed vet, all package tests, and packaging without SkipTests. `tools/test-build.ps1` passed all three checks. Executable file/product versions are 0.9.6.
- Independent read-only review: no Critical or Important findings. Minor deferred: GPU.OK reflects adapter presence rather than confirmed matching telemetry; adapters without data still show zero instead of unavailable. Synthetic unmatched process-counter coverage was also suggested.
- Review scope resolution: hardware utilization accuracy cannot be certified without a reference sensor; the tested requirement is consistency across views. Live installed-app checks cover the reviewer's deferred rendering and deployment checks.
- Installed 0.9.6 through the normal setup upgrade after closing 0.9.5. Installer exit 0; registry and binary SHA256 match the built release. Live Intel sensor wording is corrected, Drivers remained dark in inspected load/refresh states, recorder and Performance agree. Recording off, app left on Performance.
- Source changes retained on local branch `fix/server-validation-ui`; no remote push or merge performed.
