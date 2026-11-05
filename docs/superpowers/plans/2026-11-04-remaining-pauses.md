# Remaining UI and I/O pauses

User request: fix pauses on first opening table pages under load and prevent slow disk writes or account lookups from blocking GOTM. Preserve all features, lazy startup, configured sampling rates, in-memory retention, and self-contained hardware monitoring.

Confirmed causes:
- First navigation constructs and styles the entire native widget tree in one UI callback. Unloaded measurements: Processes creation 45–76 ms plus initialization/style 25–42 ms; contention amplifies those uninterrupted callbacks.
- Recorder Add performs filesystem I/O in the sample-consumer pipeline. Persistence toggles acquire the same I/O lock. A blocked-writer regression failed both capture and toggle responsiveness before the fix.
- Configuration saves (including fsync) and CSV exports run directly on the UI thread.
- An unseen process SID performs a synchronous account lookup, serialized behind a resolver lock, on the sampling path.

Implementation:
1. Stage native table/page construction across actual window messages, including columns. Keep the current page usable, preserve initialized page state, and safely cancel partial construction. Remove redundant model/font work supported by profiling.
2. One recorder disk worker with up to 256 pending samples. Add, toggles, status, and memory reads never wait for disk I/O. Drain accepted work in order; explicit bounded Flush/Close for non-UI shutdown. Overflow keeps samples in normal memory retention and reports the number missing from disk until persistence is restarted.
3. Serialize settings mutations in a bounded background queue and apply successful saves on the UI thread. Preserve failed-save behavior and sequential toggle semantics. Export one CSV at a time in the background with visible progress/error state.
4. Resolve owned SID strings using two fixed workers, a 64-item queue, and a 256-entry cache. Return SID text immediately; refresh displayed names after completion. Preserve PID/creation-time metadata identity and cache retry intervals.

Validation: deliberately blocked writes and lookups; overflow, ordering, retry, and shutdown tests; real native message-pump navigation/cancellation tests; full tests and vet; build; bounded 48-worker stress probes with startup and table navigation; live native UI checks. Record measured results and limitations below after verification. No commit or GitHub publication requested in this turn.

## Verified results

- Full Windows tests and vet passed, including blocked recording writes and close/re-enable, sticky overflow, account queue saturation/retry, failed settings writes, and single-flight export. CSV exports freeze their sample window and replace an existing destination only after closing the completed temporary file. A Windows file-sharing regression verifies the previous export survives a refused replacement.
- Native Processes initialization/style fell from 25–42 ms to about 2 ms without changing columns or row semantics. With 2,000 process rows under shared host load, full staged opening took 390–497 ms, dispatching 124–146 probe messages in between; the longest individual callback was 77–129 ms. Direct launch builds the shell first. On this machine with 48 busy workers, Processes first became visible at 236 ms versus 572 ms for 0.9.8, with first response at 314 ms versus 718 ms. These are single runs, not fixed upper bounds.
- Exercised Processes, Services, Drivers and Connections on the installed app. Continuous external UI Automation inspection is a separate limit: equivalent 45-second navigation probes with the inspector attached timed out 34 times on **both** builds. A bounded ETW trace identified the inspecting client and repeated UIA/MSAA traversal, and thread samples showed Windows list-view property requests. Ending the inspection session produced a 6-second Connections run with zero timeouts in 117 responses (p95 0.511 ms). Other foreground runs still saw inspection-related timeouts. The changes remove the uninterrupted GOTM construction callback, not the cost Windows incurs serving an external full-tree scan.
- Shutdown allows up to 1.5 seconds after the message loop exits to drain file work. A permanently blocked disk can exceed this budget; accepted queued writes cannot be guaranteed durable if the process then exits. This preserves bounded exit time. Native accessibility, including access to offscreen rows, remains enabled.
- Locally installed and validated 0.9.9, SHA256 `47EBA4322498130B7A4CE020D1AB4D6E11FEBCDDF7B090520267748C2D8F0268`, before the separately requested UI polish. No GitHub release was published.
- Final native test under an explicit 48-worker load: 2,000-row Processes opened in 510 ms, serviced 155 posted probes while building, longest callback 129 ms; cancel/reopen test passed in the same run. The installed Performance page had zero one-second timeouts in 261 responses during that run. The separate cold Processes stress runs still had 12 and 11 timeouts respectively, so the faster first response does not establish a general steady-state latency improvement for externally inspected tables.
