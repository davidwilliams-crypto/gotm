# Native hardware detection and sensor availability

Goal: detect supported Windows hardware across vendors, including server/legacy/virtual adapters, while remaining self-contained. No companion or new kernel driver is installed. Hardware which exposes no sensor must be reported as unavailable rather than measured zero.

- [x] Merge DXGI, graphics-kernel and present-device identities using exact identities; detect this server's Matrox adapter and refresh inventory after device changes.
- [x] Add optional WDDM temperature/fan/clock reads across vendors, preserving valid vendor readings and units.
- [x] Discover physical drives without a contiguous numbering assumption or a 32-drive limit.
- [x] Track GPU utilization/memory availability through snapshots, history, recorder, CSV, process tables and sensor UI; preserve gaps in graphs.
- [x] Keep CPU/board sensor explanations accurate, remove automatic dependence on a companion, and label missing drive readings.
- [x] Regression tests, full build, review, install and live verification on this server. Synthetic coverage for other hardware; no claim of universal physical validation.

Decisions: Windows/installed-driver APIs only. Hardware identity is distinct from telemetry availability. Never match devices by enumeration order or fabricate readings for unsupported sensors. Existing optional SDK and external-source implementation remain compatible but no external poller starts by default.

Verification: `go test ./...` passed; `build.ps1 -Version 0.9.7` passed vet and all package tests and produced the installer/portable release. Additional live native-query smoke test passed with `GOTM_GPU_SMOKE=1`. Independent review led to fixes for ambiguous vendor matching, malformed NVMe descriptors, sensor-view polling and PDH-independent GPU inventory.

Installed 0.9.7 at `C:\Program Files\gotm`; executable SHA256 matches the build: `C27D25A371D19839C640BBD6916B1B46729BB43938171AE7385391D67CE0690C`. Live UI confirmed Matrox G200eR, 32 MB, driver 4.3.1.4, PCI 0A:00.0; unavailable GPU telemetry and CPU die temperature are labelled accurately. DELL PERC H730P Mini shows disk activity and explicit unavailable member-drive temperature/SMART messages. App left running on Performance GPU, recording off.
