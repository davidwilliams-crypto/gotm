//go:build windows

package ui

import (
	"testing"
	"time"
	"unsafe"

	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"

	"gotm/internal/config"
	"gotm/internal/metrics"
)

// tickPage counts presentation ticks and remembers the snapshot it was given.
type tickPage struct {
	ticks int
	last  *metrics.Snapshot
}

func (p *tickPage) build() decl.Widget       { return decl.Composite{} }
func (p *tickPage) init()                    {}
func (p *tickPage) shown()                   {}
func (p *tickPage) tick(s *metrics.Snapshot) { p.ticks++; p.last = s }

// windowlessApp is an App with every page slot filled and the current page
// initialized, but no native windows: enough to drive onSnapshot.
func windowlessApp() (*App, *tickPage) {
	a := &App{pages: make([]*pageDef, pgCount), cur: pgProcesses}
	var cur *tickPage
	for i := range a.pages {
		p := &tickPage{}
		a.pages[i] = &pageDef{impl: p}
		if i == a.cur {
			cur = p
			a.pages[i].initialized = true
		}
	}
	return a, cur
}

func sizeMsg(a *App, kind uintptr) { a.observeVisibility(0, win.WM_SIZE, kind, 0) }

func TestRestoreAfterMinimizeTicksOnceWithNewestSnapshot(t *testing.T) {
	a, p := windowlessApp()
	a.onSnapshot(&metrics.Snapshot{T: 1000}, nil)
	if p.ticks != 1 {
		t.Fatalf("visible tick count = %d", p.ticks)
	}
	sizeMsg(a, sizeMinimized)
	if a.vis.visible() || !stallHidden.Load() {
		t.Fatal("minimize not tracked")
	}
	newest := &metrics.Snapshot{T: 3000}
	a.onSnapshot(&metrics.Snapshot{T: 2000}, nil)
	a.onSnapshot(newest, nil)
	if p.ticks != 1 {
		t.Fatalf("page ticked while minimized: %d", p.ticks)
	}
	if a.snap != newest {
		t.Fatal("newest snapshot not retained while hidden")
	}
	sizeMsg(a, sizeRestored)
	if p.ticks != 2 || p.last != newest {
		t.Fatalf("restore did not tick immediately with newest snapshot: ticks=%d", p.ticks)
	}
	if stallHidden.Load() {
		t.Fatal("watchdog still backed off after restore")
	}
	sizeMsg(a, sizeMaximized) // already visible and current: no extra work
	if p.ticks != 2 {
		t.Fatalf("redundant refresh: %d", p.ticks)
	}
	a.onSnapshot(&metrics.Snapshot{T: 4000}, nil)
	if p.ticks != 3 {
		t.Fatalf("normal ticking did not resume: %d", p.ticks)
	}
}

func TestMinimizeAndRestoreWithoutSkippedWorkDoesNotRefresh(t *testing.T) {
	a, p := windowlessApp()
	a.snap = &metrics.Snapshot{}
	sizeMsg(a, sizeMinimized)
	sizeMsg(a, sizeRestored)
	if p.ticks != 0 {
		t.Fatalf("refreshed although nothing was skipped: %d", p.ticks)
	}
}

func TestAlertsEvaluatedWhileHidden(t *testing.T) {
	a, p := windowlessApp()
	a.cfg.Alerts = []config.Alert{{Metric: "cpu", Above: 50, ForSeconds: 2, Enabled: true}}
	a.observeVisibility(0, wmWTSSessionChange, wtsSessionLock, 0)
	if a.vis.visible() {
		t.Fatal("lock not tracked")
	}
	for i := int64(0); i < 4; i++ {
		a.onSnapshot(&metrics.Snapshot{T: 10000 + i*1000, CPU: 90}, nil)
	}
	if a.alertText == "" {
		t.Fatal("alert not evaluated while the session was locked")
	}
	if p.ticks != 0 {
		t.Fatalf("page ticked while locked: %d", p.ticks)
	}
	// The alert text changed while hidden; unlocking repaints and ticks once.
	a.observeVisibility(0, wmWTSSessionChange, wtsSessionUnlock, 0)
	if p.ticks != 1 || !a.vis.visible() || a.vis.stale {
		t.Fatalf("unlock did not refresh: ticks=%d stale=%t", p.ticks, a.vis.stale)
	}
}

func TestEveryReasonMustClearBeforeRefresh(t *testing.T) {
	a, p := windowlessApp()
	sizeMsg(a, sizeMinimized)
	a.observeVisibility(0, wmWTSSessionChange, wtsRemoteDisconnect, 0)
	a.onSnapshot(&metrics.Snapshot{T: 1}, nil)
	sizeMsg(a, sizeRestored)
	if p.ticks != 0 || a.vis.visible() {
		t.Fatal("refreshed while the session was still disconnected")
	}
	a.observeVisibility(0, wmWTSSessionChange, wtsRemoteConnect, 0)
	if p.ticks != 1 {
		t.Fatalf("reconnect did not refresh: %d", p.ticks)
	}
}

func TestUserPresenceClearsSessionReasonsOnly(t *testing.T) {
	a, _ := windowlessApp()
	sizeMsg(a, sizeMinimized)
	a.observeVisibility(0, wmWTSSessionChange, wtsSessionLock, 0)
	a.setHidden(hideDisplayOff, hideDisplayOff)
	a.userPresent()
	if a.vis.hidden != hideMinimized {
		t.Fatalf("hidden = %b, want only minimized", a.vis.hidden)
	}
	a.observeVisibility(0, win.WM_ACTIVATE, 0, 0) // WA_INACTIVE: not presence
	a.observeVisibility(0, wmWTSSessionChange, wtsSessionLock, 0)
	a.observeVisibility(0, win.WM_ACTIVATE, 1, 0)
	if a.vis.hidden != hideMinimized {
		t.Fatal("activation did not clear the lock")
	}
}

func TestShowHideNotifications(t *testing.T) {
	a, p := windowlessApp()
	pos := win.WINDOWPOS{Flags: swpHideWindow}
	a.observeVisibility(0, win.WM_WINDOWPOSCHANGED, 0, uintptr(unsafe.Pointer(&pos)))
	if a.vis.hidden != hideHidden {
		t.Fatalf("hide not tracked: %b", a.vis.hidden)
	}
	a.onSnapshot(&metrics.Snapshot{T: 1}, nil)
	pos.Flags = win.SWP_NOSIZE | win.SWP_NOMOVE // an ordinary move: no change
	a.observeVisibility(0, win.WM_WINDOWPOSCHANGED, 0, uintptr(unsafe.Pointer(&pos)))
	if a.vis.visible() {
		t.Fatal("unrelated position change unhid the window")
	}
	pos.Flags = swpShowWindow
	a.observeVisibility(0, win.WM_WINDOWPOSCHANGED, 0, uintptr(unsafe.Pointer(&pos)))
	if !a.vis.visible() || p.ticks != 1 {
		t.Fatalf("show did not refresh: visible=%t ticks=%d", a.vis.visible(), p.ticks)
	}
}

func TestChromeInvalidatedWhileHiddenRefreshesOnShow(t *testing.T) {
	a, p := windowlessApp()
	a.snap = &metrics.Snapshot{T: 5}
	a.setHidden(hideCloaked, hideCloaked)
	a.invalidateChrome() // e.g. navigation or an alert while on another desktop
	a.setHidden(hideCloaked, 0)
	if p.ticks != 1 || p.last != a.snap {
		t.Fatalf("uncloak did not refresh: %d", p.ticks)
	}
}

func TestFirstVisibleSnapshotAfterMissedNotificationRefreshes(t *testing.T) {
	a, p := windowlessApp()
	a.vis.stale = true // work was skipped; the "visible" notification never came
	a.onSnapshot(&metrics.Snapshot{T: 9}, nil)
	if p.ticks != 1 || a.vis.stale {
		t.Fatalf("ticks=%d stale=%t", p.ticks, a.vis.stale)
	}
}

func TestHiddenSnapshotDoesNotAllocate(t *testing.T) {
	a, _ := windowlessApp()
	a.cfg.Alerts = []config.Alert{{Metric: "cpu", Above: 50, ForSeconds: 60, Enabled: true}, {Metric: "mem", Above: 90, Enabled: true}}
	sizeMsg(a, sizeMinimized)
	s := &metrics.Snapshot{T: 1000, CPU: 80}
	if n := testing.AllocsPerRun(100, func() { a.onSnapshot(s, nil) }); n != 0 {
		t.Fatalf("hidden tick allocates %v times", n)
	}
}

func TestCheckAlertsReusesState(t *testing.T) {
	a, _ := windowlessApp()
	a.cfg.Alerts = []config.Alert{{Metric: "cpu", Above: 50, ForSeconds: 600, Enabled: true}, {Metric: "disk", Above: 10, Enabled: true}}
	s := &metrics.Snapshot{T: 1000, CPU: 80}
	a.checkAlerts(s)
	since := a.alertSince
	if n := testing.AllocsPerRun(100, func() { a.checkAlerts(s) }); n != 0 {
		t.Fatalf("checkAlerts allocates %v times per tick when nothing fires", n)
	}
	if len(a.alertSince) != 1 || a.alertSince[0] != 1000 {
		t.Fatalf("alert state = %v", a.alertSince)
	}
	a.checkAlerts(&metrics.Snapshot{T: 2000, CPU: 10})
	if len(a.alertSince) != 0 {
		t.Fatal("cleared rule kept its start time")
	}
	a.checkAlerts(s)
	if len(since) != 1 {
		t.Fatal("alert state map was replaced")
	}
}

func TestCheckAlertsReportsFirstAndCount(t *testing.T) {
	a, _ := windowlessApp()
	a.cfg.Alerts = []config.Alert{
		{Metric: "mem", Above: 50, Enabled: true},
		{Metric: "cpu", Above: 50, Enabled: true},
		{Metric: "gpu", Above: 1, Enabled: true}, // GPU not OK: never fires
	}
	a.checkAlerts(&metrics.Snapshot{T: 1000, CPU: 70, Mem: metrics.MemInfo{Percent: 60}, GPU: metrics.GPUInfo{Percent: 99}})
	want := "Memory above 50% for 0 s (now 60%)  (+1 more)"
	if a.alertText != want {
		t.Fatalf("alert text = %q, want %q", a.alertText, want)
	}
}

func TestTrayTipThrottlesAndConverges(t *testing.T) {
	var tip trayTip
	t0 := time.Unix(1000, 0)
	if !tip.offer(10.2, 40, t0) || tip.text() != "gotm — CPU 10% · RAM 40%" {
		t.Fatalf("first tooltip not sent: %q", tip.text())
	}
	if tip.offer(9.6, 40.4, t0.Add(3*time.Second)) {
		t.Fatal("unchanged rounded text was sent again")
	}
	if tip.offer(55, 41, t0.Add(3500*time.Millisecond)) != true {
		t.Fatal("change after the throttle window was not sent")
	}
	sent := t0.Add(3500 * time.Millisecond)
	if tip.offer(60, 41, sent.Add(500*time.Millisecond)) || tip.offer(70, 42, sent.Add(time.Second)) {
		t.Fatal("tooltip updated inside the throttle window")
	}
	// No new readings arrive; the pending latest value is flushed once the window passes.
	if tip.flush(sent.Add(1900 * time.Millisecond)) {
		t.Fatal("flushed early")
	}
	if !tip.flush(sent.Add(2*time.Second)) || tip.text() != "gotm — CPU 70% · RAM 42%" {
		t.Fatalf("did not converge on newest value: %q", tip.text())
	}
	if tip.flush(sent.Add(10 * time.Second)) {
		t.Fatal("converged value sent twice")
	}
	// A change that reverts inside the window leaves nothing pending.
	tip.offer(71, 42, sent.Add(2500*time.Millisecond))
	tip.offer(70, 42, sent.Add(2600*time.Millisecond))
	if tip.flush(sent.Add(time.Minute)) {
		t.Fatal("reverted change was sent")
	}
}

func TestTrayTipRoundingMatchesFormat(t *testing.T) {
	var tip trayTip
	tip.offer(2.5, 99.5, time.Unix(0, 0))
	if got := tip.text(); got != "gotm — CPU 2% · RAM 100%" {
		t.Fatalf("tooltip %q differs from %%.0f formatting", got)
	}
}

func TestSessionAndDisplayDecoding(t *testing.T) {
	for code, want := range map[uintptr]struct {
		mask, value hideReason
		ok          bool
	}{
		wtsSessionLock:       {hideLocked, hideLocked, true},
		wtsSessionUnlock:     {hideLocked, 0, true},
		wtsConsoleDisconnect: {hideDisconnected, hideDisconnected, true},
		wtsRemoteConnect:     {hideDisconnected | hideDisplayOff, 0, true},
		0x5:                  {0, 0, false}, // logon
	} {
		m, v, ok := sessionChange(code)
		if m != want.mask || v != want.value || ok != want.ok {
			t.Errorf("session code %#x = %b,%b,%t", code, m, v, ok)
		}
	}
	for state, off := range map[uint32]bool{0: true, 1: false, 2: false} {
		p := powerBroadcastSetting{guid: guidConsoleDisplayState, size: 4, state: state}
		if got, ok := displayOff(uintptr(unsafe.Pointer(&p))); !ok || got != off {
			t.Errorf("display state %d: off=%t ok=%t", state, got, ok)
		}
	}
	other := powerBroadcastSetting{size: 4}
	if _, ok := decodeDisplayState(&other); ok {
		t.Error("foreign power setting accepted")
	}
	defer func(old func() bool) { isRemoteSession = old }(isRemoteSession)
	remote := false
	isRemoteSession = func() bool { return remote }
	a, _ := windowlessApp()
	p := powerBroadcastSetting{guid: guidConsoleDisplayState, size: 4, state: 0}
	a.observeVisibility(0, wmPowerBroadcast, pbtPowerSettingChange, uintptr(unsafe.Pointer(&p)))
	if a.vis.hidden != hideDisplayOff {
		t.Fatal("display off not tracked")
	}
	p.state = 1
	a.observeVisibility(0, wmPowerBroadcast, pbtPowerSettingChange, uintptr(unsafe.Pointer(&p)))
	if !a.vis.visible() {
		t.Fatal("display on not tracked")
	}
	// A remote desktop user does not look at the physical console display.
	remote, p.state = true, 0
	a.observeVisibility(0, wmPowerBroadcast, pbtPowerSettingChange, uintptr(unsafe.Pointer(&p)))
	if !a.vis.visible() {
		t.Fatal("console display off hid a remote session")
	}
	// Reconnecting remotely clears a console display-off seen earlier.
	remote = false
	a.observeVisibility(0, wmPowerBroadcast, pbtPowerSettingChange, uintptr(unsafe.Pointer(&p)))
	a.observeVisibility(0, wmWTSSessionChange, wtsRemoteConnect, 0)
	if !a.vis.visible() {
		t.Fatal("remote connect kept the console display-off state")
	}
}

func TestStallWatchdogBacksOffWhileHidden(t *testing.T) {
	if stallPollPeriod(true) <= stallPollPeriod(false) || stallQuietLimit(true) <= stallQuietLimit(false) {
		t.Fatal("watchdog does not back off while hidden")
	}
	if stallQuietLimit(false) != 1500*time.Millisecond {
		t.Fatal("visible stall threshold changed")
	}
	a, _ := windowlessApp()
	a.setHidden(hideMinimized, hideMinimized)
	if !stallHidden.Load() {
		t.Fatal("hidden state not published to the watchdog")
	}
	a.setHidden(hideMinimized, 0)
	if stallHidden.Load() {
		t.Fatal("visible state not published to the watchdog")
	}
}
