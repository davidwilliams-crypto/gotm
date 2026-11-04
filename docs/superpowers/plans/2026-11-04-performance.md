# Responsiveness and allocation pass

Preserve all existing features, configured refresh rates, history retention, graph quality, accessibility and recording behavior. Optimize measured bottlenecks in the existing native Go/Win32 implementation. Do not change process priority or permanently alter machine settings.

Implementation scope:
- Remove full-window allocations during sampler/recorder eviction; clear expired references. Preserve clock rollback and chronological queries.
- Keep recorder error-status reads independent of slow file writes.
- Bound process leader selection to the displayed top ten CPU and top three memory entries.
- Reduce redundant GPU discovery/instance parsing work without losing hotplug or sensor availability.
- Batch graph area fills, preserving clipping, gaps and supersampling; measure before retaining any extra native bitmap memory.
- Move sensor construction and hardware reads off the UI thread, publish the newest pending sample, and make startup/idle-stall instrumentation accurate.

Baseline measurements on this 56-logical-CPU Xeon server:
- Full 5400-sample recorder: 193653 ns/op, 483458 B/op, 6 allocations/op.
- Top process selection (5000 processes): 1107803 ns/op, 42096 B/op, 12 allocations/op.
- Filled graph: approximately 5.7 ms at 160px and 25 ms at 800px, 240px high.
- Startup log: window529 ms, pages743 ms, message loop760 ms. Old watchdog falsely reported idle GetMessage as stalls.

Validation: regression tests and allocation benchmarks, full tests/vet/build, live UI startup/navigation and bounded CPU-contention probes. Report measured improvements separately from limitations; no guarantee under arbitrary machine-wide resource exhaustion. No GitHub publication is requested for this pass.

## Results

Implemented the scoped changes, plus lazy native-page creation (one-time first navigation), and a bounded 256-entry SID/account cache. Recorder retention/loading remains active independently of whether its page has been constructed. Native graph surfaces are released after every draw; no persistent bitmap cache was added. Configured refresh rates, retention, supersampling, sensor accounting and features remain available.

| Measurement | 0.9.7 baseline | 0.9.8 |
| --- | ---: | ---: |
| Recorder full-window insertion | 193653 ns, 483458 B, 6 allocs | 23013 ns, 80 B, 1 alloc |
| 5000-process leader selection | 1107803 ns, 42096 B, 12 allocs | 95601 ns, 576 B, 1 alloc |
| Hidden GPU reader | 703–752 us, 141 KB, 2384 allocs | 599–601 us, 104 KB, 1612 allocs |
| Small filled graph | 5.69–5.77 ms | 3.55–3.61 ms |
| Main filled graph | 24.71–25.71 ms | 13.08–13.84 ms |
| Visible window under 48 busy workers | 2212 ms | 374 ms |
| Message response p95, same startup probe | 110.192 ms | 18.854 ms |
| Peak private bytes in startup probe | 74416128 | 69804032 |
| Peak working-set bytes in startup probe | 60297216 | 55488512 |

The live startup probes ran for 15 seconds each, on the Performance page, with 48 bounded CPU workers on a 56-logical-CPU server. Both had zero one-second timeouts. Final build first response was 513 ms after launch; startup log reached its message loop at 411 ms. These short, single-run measurements are indicative, not universal guarantees or long-duration memory ceilings. Benchmark runs were separate from those live probes.

First-time table navigation remains a cost: an additional 15-second stress probe while opening new table pages reported p95 471 ms and four one-second timeouts. Controls are then retained for reuse. The first lookup for an unseen domain SID can still block the sampler; cache hits do not. Persistence toggles/shutdown still serialize with an ongoing disk write. Slow external drivers and full machine resource exhaustion are not bounded by this pass.

Validation completed: full `go test ./...`, vet and `build.ps1 -Version 0.9.8`; native rendered-gap/clipping/GDI handle lifetime tests; blocked startup/driver/status-lock tests; mailbox newest-sample and sensor-extrema/recording tests; lazy-page lifecycle/theme/settings/recorder tests; manual navigation across all pages. Windows race testing was not available (no C compiler configured).

Installed 0.9.8 into `C:\Program Files\gotm`, verified identical to the built executable with SHA256 `B93AB0477C7518973C2D5C0A37B338360693D089F8B5C6CBB4AB2D2F6E6E2C49`. Left running on Performance with recording off. Release kit is local under `dist/release/gotm-0.9.8`; changes are not committed or published.

Reproduce a bounded live probe with `go run ./tools/perfprobe -pid <gotm-pid> -seconds 15 -load 48` (reduce worker count on smaller machines). The probe reads main-window message responses and process memory; workers exit when it completes and no priorities or affinity settings are changed.
