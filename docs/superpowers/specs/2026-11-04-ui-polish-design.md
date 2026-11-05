# GOTM UI polish — proposed design

The goal is to help someone answer “what is busy, and what can I do about it?” at a glance, including when the server itself is busy. Keep the native Windows controls, all eleven pages, every existing action, and GOTM's compact footprint.

## Proposed direction

Use a refined Windows-native look: a graphite background, distinct navigation and content surfaces, legible secondary text, one blue interaction accent, and the existing consistent colors for CPU, memory, GPU, disk and network. Reserve amber/red for warnings and destructive or recording states. The alternative operations-console direction would put more simultaneous readings on screen; the spacious direction would favor larger controls at the cost of visible table rows. The recommendation is the native direction, retaining useful density without asking users to relearn the application.

## What changes on screen

- Navigation: keep the same order and shortcuts. Use the existing app mark alongside the wordmark, clear section labels, and distinct current, hover, focus and pending states. Keep Settings pinned. Avoid another row of toolbar chrome.
- Page headings: use consistent title and subtitle typography, actionable page-specific help, and a compact status area. Keep alarms readable and separate from descriptive text. Adapt or omit decorative detail before truncating essential content.
- Performance: give the selected metric, device name, current value and time span an obvious reading order. Make the left resource rail responsive so graph/detail content remains available on narrower windows; avoid truncating important readings to make room for a sparkline. Keep graph hover, all adapters, CPU core detail and unavailable-sensor semantics. Label missing readings “Unavailable” or “n/a”, never as zero.
- Tables and actions: maintain native sortable, resizable tables, selection, filtering, keyboard control and the complete accessibility tree. Strengthen heading and selected-row contrast. Make filter cues, command hover/press/focus states, disabled actions and destructive actions easy to distinguish. Reuse shared styling so changes reach Processes, Startup, Services, Connections, Installed Apps, Drivers and Disk Space consistently.
- Recording and status: make Live, recording off/on, export in progress and persistence warnings distinguishable by text as well as color. Preserve click targets and accessible names at narrow widths.

## Acceptance

Inspect a wide and a constrained layout, navigation to all types of page, keyboard focus/activation, native selection, unavailable GPU, recording off, and the high-contrast palette. Verify no regressions to recording, sorting, selection or asynchronous page loading. Run all tests and vet; rebuild and update the local installation. Reuse the existing renderer and cached fonts/brushes: no web runtime, animation timers, polling loops or permanent bitmap caches. Compare native navigation and rendering measurements with those from the responsiveness pass.
