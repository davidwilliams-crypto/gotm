//go:build windows

// Package ui is gotm's native Win32 front end, built on the Walk toolkit.
// Navigation, cards and graphs are owner-drawn into back buffers (so refreshes
// never flicker); tables are real list views in the system dark theme.
package ui

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"

	"gotm/internal/config"
	"gotm/internal/metrics"
	"gotm/internal/safepath"
)

// page is one screen in the sidebar.
type page interface {
	build() decl.Widget // the page's root composite
	init()              // after the window exists
	shown()             // became the current page
	tick(s *metrics.Snapshot)
}

type pageDef struct {
	id, title, subtitle, glyph string
	pro                        bool
	impl                       page
	root                       *walk.Composite
	initialized                bool
}

// App owns the main window and everything hanging off it.
type App struct {
	configWrites   *settingsWriter
	settingResults settingsMailbox
	ioClosing      atomic.Bool
	dbgN           int
	version        string
	store          *config.Store
	cfg            config.Config
	sampler        *metrics.Sampler
	host           metrics.HostInfo
	rec            *metrics.Recorder
	sensors        *metrics.SensorHub

	mw    *walk.MainWindow
	right *walk.Composite // header + current page + footer, laid out by relayout()
	tray  *walk.NotifyIcon

	snap *metrics.Snapshot

	pages []*pageDef
	wraps []*walk.Composite // one container per page; only the current one is shown
	cur   int
	side  *canvasView
	heads []*canvasView // one header per page (all paint the same chrome)
	foots []*canvasView

	procs    *procsTab
	settings *settingsTab

	updates    snapshotMailbox
	alertText  string
	alertSince map[int]int64
	alertShown map[int]time.Time
	localIP    string
	ryzenOn    bool   // AMD SDK switch (HKLM, set by an administrator)
	elevatedAs string // set when elevated as an account other than the signed-in user

	endSessionCB, prevMainProc uintptr // main-window hook for WM_ENDSESSION
	pumpDone                   chan struct{}
	startupTab                 int
	opening                    *pageConstruction
	loading                    *canvasView
	pageBuildHooked            bool

	vis              visibilityGate // whether anyone can see the window (visibility.go)
	visRefreshQueued bool
	visNotify        visibilityNotifications
	tip              trayTip // throttled tray tooltip (shell.go)
}

// StartTab selects the page shown at launch (an index into the sidebar order).
var StartTab int

func debugf(format string, a ...any) {
	if os.Getenv("GOTM_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[gotm] "+format+"\n", a...)
	}
}

// Page ids, in sidebar order. Settings is pinned to the bottom of the sidebar.
const (
	pgPerformance = iota
	pgProcesses
	pgStartup
	pgServices
	pgPower
	pgRecorder
	pgConnections
	pgApps
	pgDrivers
	pgDisk
	pgSettings
	pgCount
)

// PageIDs maps command-line names to page indexes.
var PageIDs = map[string]int{
	"performance": pgPerformance, "processes": pgProcesses, "startup": pgStartup, "services": pgServices,
	"power": pgPower, "recorder": pgRecorder, "connections": pgConnections, "apps": pgApps,
	"drivers": pgDrivers, "disk": pgDisk, "settings": pgSettings,
}

// Run builds the window and blocks until it is closed.
func Run(version string, store *config.Store, sampler *metrics.Sampler) error {
	// Diagnostic log of start-up phases and any ≥1.5 s UI-thread stall (with
	// goroutine stacks): <settings>\stall.log. Refuses redirected folders
	// while elevated, like every other settings write.
	if dir := config.Dir(); dir != "" && !safepath.Redirected(dir) {
		InitStallLog(dir)
	} else {
		InitStallLog("")
	}
	// The UI thread is a COM single-threaded apartment: UI Automation (uia.go)
	// marshals screen-reader calls to it, and Walk's own annotations need it.
	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	applyTheme() // gotm's dark palette, or the system colours in high-contrast mode
	stallPhase("theme applied")
	a := &App{
		version: version, store: store, cfg: store.Get(), sampler: sampler,
		host: metrics.HostInfo{NumCPU: sampler.NumCPU(), Elevated: metrics.IsElevated(), Self: os.Getpid()}, alertSince: map[int]int64{}, alertShown: map[int]time.Time{},
		sensors: metrics.NewSensorHubDeferred(),
	}
	a.rec = metrics.NewRecorder(3*time.Hour, 2*time.Second, store.File("recorder"))
	a.ryzenOn = metrics.RyzenSDKEnabled()
	startup := startStartup(metrics.ReadHostInfo, a.sensors.Initialize, func() (string, string) {
		ip := firstLocalIP()
		account, other := metrics.ElevatedAs()
		if !other {
			account = ""
		}
		return ip, account
	})
	a.setRyzenSDK(a.ryzenOn && a.host.Elevated)

	a.procs = newProcsTab(a)
	a.settings = newSettingsTab(a)
	perf := newPerfPage(a)
	a.pages = make([]*pageDef, pgCount)
	a.pages[pgPerformance] = &pageDef{id: "performance", title: "Performance", subtitle: "Select a resource to inspect its activity and history.", glyph: "", impl: perf}
	a.pages[pgProcesses] = &pageDef{id: "processes", title: "Processes", subtitle: "Find a process, inspect its resource use or end a task.", glyph: "", impl: a.procs}
	a.pages[pgStartup] = &pageDef{id: "startup", title: "Startup apps", subtitle: "Choose which apps launch at sign-in.", glyph: "", impl: newStartupTab(a)}
	a.pages[pgServices] = &pageDef{id: "services", title: "Services", subtitle: "Inspect service status, start type and available actions.", glyph: "", impl: newServicesTab(a)}
	a.pages[pgPower] = &pageDef{id: "power", title: "Power & Freq", subtitle: "Inspect sensor readings, temperatures and recorded extremes.", glyph: "", pro: true, impl: newPowerPage(a)}
	a.pages[pgRecorder] = &pageDef{id: "recorder", title: "Flight Recorder", subtitle: "Record activity and select a moment to investigate a spike.", glyph: "", pro: true, impl: newRecorderPage(a)}
	a.pages[pgConnections] = &pageDef{id: "connections", title: "Connections", subtitle: "Trace a TCP or UDP endpoint to its process.", glyph: "", pro: true, impl: newConnsPage(a)}
	a.pages[pgApps] = &pageDef{id: "apps", title: "Installed Apps", subtitle: "Find installed software and open its installation folder.", glyph: "", pro: true, impl: newAppsPage(a)}
	a.pages[pgDrivers] = &pageDef{id: "drivers", title: "Drivers", subtitle: "Inspect driver status, publisher and version.", glyph: "", pro: true, impl: newDriversPage(a)}
	a.pages[pgDisk] = &pageDef{id: "disk", title: "Disk Space", subtitle: "Scan a folder to find its largest files and subfolders.", glyph: "", pro: true, impl: newDiskPage(a)}
	a.pages[pgSettings] = &pageDef{id: "settings", title: "Settings", subtitle: fmt.Sprintf("gotm %s", version), glyph: "", impl: a.settings}

	a.side = newCanvasView(a.paintSidebar)
	a.side.bgSide = true
	a.side.keyboard, a.side.keyName = true, "Pages. Up and Down choose a page, Enter opens it. Ctrl+1 to Ctrl+0 open pages directly."
	a.side.acc, a.side.accName, a.side.accCtl = true, "Pages", ctlList
	a.side.onClick = func(id string, _ walk.MouseButton) { a.onSideClick(id) }
	a.heads = make([]*canvasView, pgCount)
	a.foots = make([]*canvasView, pgCount)
	for i := range a.heads {
		i := i
		a.heads[i] = newCanvasView(func(cv *walk.Canvas, w, h int) { a.paintHeader(cv, w, h, i) })
		a.heads[i].acc, a.heads[i].accName, a.heads[i].accCtl = true, a.pages[i].title+" page header", ctlPane
		a.foots[i] = newCanvasView(func(cv *walk.Canvas, w, h int) { a.paintFooter(cv, w, h, i) })
		a.foots[i].bgSide = true
		a.foots[i].keyboard, a.foots[i].keyName = true, "Status bar. Left and Right move between the flight recorder switch and the administrator button, Enter activates."
		a.foots[i].acc, a.foots[i].accName, a.foots[i].accCtl = true, "Status bar", ctlStatusBar
		a.foots[i].onClick = func(id string, _ walk.MouseButton) {
			switch id {
			case "admin":
				if !a.host.Elevated {
					a.elevate()
				}
			case "rec":
				a.rec.SetEnabled(!a.rec.Enabled())
				if p, ok := a.pages[pgRecorder].impl.(*recorderPage); ok {
					p.sync()
				}
				a.invalidateChrome()
			}
		}
	}

	start := StartTab
	if start < 0 || start >= pgCount {
		start = pgPerformance
		if n, ok := PageIDs[a.cfg.LastPage]; ok {
			start = n
		}
	}
	// The shell paints immediately; even a direct table-page launch is staged.
	// Page models remain available to settings, recording and sensor accounting.
	a.wraps = make([]*walk.Composite, pgCount)
	contents := []decl.Widget{a.initialPageWidget(start)}

	enableDarkMode() // dark context menus and list-view headers (before any window exists)
	icon := appIcon()
	initialSize := a.initialSize()
	sideW := sidebarWidth(initialSize.Width)
	err := decl.MainWindow{
		AssignTo:   &a.mw,
		Title:      a.windowTitle(),
		Font:       decl.Font{Family: uiFamily, PointSize: 10},
		Icon:       icon,
		Background: decl.SolidColorBrush{Color: T.Bg},
		MinSize:    a.minSize(),
		Size:       initialSize,
		Layout:     decl.HBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			a.side.declare(sideW, 0, sideW, 0, 0),
			decl.Composite{
				AssignTo:      &a.right,
				StretchFactor: bodyStretch,
				Background:    decl.SolidColorBrush{Color: T.Bg},
				Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
				Children:      contents,
			},
		},
	}.Create()
	debugf("window created, err=%v", err)
	stallPhase("main window created")
	if err != nil {
		return err
	}
	a.mw.SetSize(walk.Size{Width: initialSize.Width, Height: initialSize.Height})
	if tb := a.mw.ToolBar(); tb != nil {
		tb.SetVisible(false)
	}
	if sb := a.mw.StatusBar(); sb != nil {
		sb.SetVisible(false)
	}
	allowDark(win.HWND(a.mw.Handle()))
	titleBar(win.HWND(a.mw.Handle()))

	a.mw.SizeChanged().Attach(func() {
		w := sidebarWidth(a.mw.ClientBounds().Width)
		if w != sideW {
			sideW = w
			size := walk.Size{Width: w}
			_ = a.side.cw.SetMinMaxSize(size, size)
		}
	})
	a.side.attachWheel()
	a.pages[pgRecorder].impl.(*recorderPage).startBackground()
	stallPhase("shell initialized")
	a.setupShortcuts()
	a.setupTray(icon)
	// pages may have trimmed their minimum width during init: apply the real size now
	a.mw.SetSize(walk.Size{Width: initialSize.Width, Height: initialSize.Height})

	a.watchSystem() // sign-out/shutdown save the window state; colour changes re-theme
	stallWindow.Store(uintptr(a.mw.Handle()))
	defer stallWindow.Store(0)
	var exitRyzen *metrics.RyzenSDK // stopped at close, awaited (bounded) after the loop
	a.mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		a.saveWindowState()
		a.ioClosing.Store(true)
		if r := a.sensors.Ryzen(); r != nil {
			exitRyzen = r
		}
		a.setRyzenSDK(false) // Stop returns at once; the SDK is released on its own thread
		a.rec.SetPersist(false)
		if p, ok := a.pages[pgDisk].impl.(*diskPage); ok && p.cancel != nil {
			p.cancel()
		}
		if a.tray != nil {
			a.tray.Dispose()
		}
	})

	a.navigate(start)
	stallPhase("navigation queued; entering message loop")
	a.pumpDone = make(chan struct{})
	go a.pump(startup, a.host)
	walk.App().Run()
	close(a.pumpDone) // pump unsubscribes and exits instead of blocking forever
	a.finishFileWork()
	if exitRyzen != nil {
		// Let the worker finish UnInit so the crash marker is cleared, without
		// letting a stuck driver hold up exit.
		exitRyzen.Wait(1500 * time.Millisecond)
	}
	return nil
}

// setRyzenSDK starts or stops the optional AMD Monitoring SDK reader. A crash
// marker next to config.json keeps a bad start-up from repeating on every launch.
func (a *App) setRyzenSDK(on bool) {
	cur := a.sensors.Ryzen()
	switch {
	case on && cur == nil:
		r := metrics.NewRyzenSDK()
		a.sensors.SetRyzen(r)
		r.Start()
	case !on && cur != nil:
		cur.Stop()
		a.sensors.SetRyzen(nil)
	}
}

func firstLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, ad := range addrs {
		if ipn, ok := ad.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil &&
			!ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

func (a *App) pageWidget(i int) decl.Composite {
	return decl.Composite{
		AssignTo: &a.wraps[i], StretchFactor: bodyStretch,
		Background: decl.SolidColorBrush{Color: T.Bg},
		Layout:     decl.VBox{MarginsZero: true, SpacingZero: true},
		Children:   []decl.Widget{a.pages[i].impl.build()},
	}
}

// ensurePage creates and initializes a page once, on the UI thread. Subsequent
// visits reuse its controls, model, selection and scroll state.
func (a *App) ensurePage(i int) error {
	p := a.pages[i]
	if p.initialized {
		return nil
	}
	if a.wraps[i] == nil {
		a.mw.SetSuspended(true)
		defer a.mw.SetSuspended(false)
		if err := a.pageWidget(i).Create(decl.NewBuilder(a.right)); err != nil {
			a.wraps[i] = nil
			return err
		}
		a.wraps[i].SetVisible(false)
	}
	p.impl.init()
	p.initialized = true
	return nil
}

// navigate shows page i and hides the others.
func (a *App) navigate(i int) {
	if i < 0 || i >= len(a.pages) {
		return
	}
	if !a.pages[i].initialized {
		if a.opening == nil || a.opening.id != i {
			a.startPageBuild(i)
		}
		return
	}
	a.cancelPageBuild()
	a.showPage(i)
}

func (a *App) showPage(i int) {
	if a.loading != nil && a.loading.cw != nil {
		a.loading.cw.SetVisible(false)
	}
	a.cur = i
	for k, w := range a.wraps {
		if w != nil && w.Visible() != (k == i) {
			w.SetVisible(k == i)
		}
	}
	a.mw.RequestImmediateLayout()
	a.side.keyHit = fmt.Sprintf("nav:%d", i)
	a.side.invalidate()
	a.invalidateChrome()
	metrics.SetGPUSensorsWanted(a.gpuSensorsWanted())
	a.sensors.SetDetailWanted(i == pgPower)
	a.pages[i].impl.shown()
	if a.snap != nil {
		a.pages[i].impl.tick(a.snap)
	}
	a.focusPage()
}

func (a *App) invalidateChrome() {
	// Hidden pages repaint when shown. Avoid invalidating every native window
	// on each change to the current page's status.
	if !a.vis.visible() {
		a.vis.stale = true // repainted by refreshVisible when the window is seen again
		return
	}
	if a.cur >= 0 && a.cur < len(a.heads) {
		a.heads[a.cur].invalidate()
	}
	if a.cur >= 0 && a.cur < len(a.foots) {
		a.foots[a.cur].invalidate()
	}
}

func (a *App) onSideClick(id string) {
	var n int
	if _, err := fmt.Sscanf(id, "nav:%d", &n); err == nil {
		a.navigate(n)
	}
}

// pump forwards each sampler tick to the UI thread, coalescing bursts so a
// busy UI never builds up a backlog. It returns when the window closes.
type startupInfo struct {
	host                metrics.HostInfo
	localIP, elevatedAs string
}

// One bounded startup worker publishes CPU/host details before potentially slow
// network/account lookups. Both sends are buffered even if the window closes.
func startStartup(readHost func() metrics.HostInfo, initialize func(), extra func() (string, string)) <-chan startupInfo {
	out := make(chan startupInfo, 2)
	go func() {
		defer close(out)
		info := startupInfo{host: readHost()}
		initialize()
		out <- info
		info.localIP, info.elevatedAs = extra()
		out <- info
	}()
	return out
}

type snapshotUpdate struct {
	snap *metrics.Snapshot
	rows []metrics.SensorRow
}

// snapshotMailbox retains only the newest immutable UI payload while the UI is
// busy. Recording and sensor min/max run before this presentation-only gate.
type snapshotMailbox struct {
	mu      sync.Mutex
	pending *snapshotUpdate
	queued  bool
}

func (m *snapshotMailbox) offer(v *snapshotUpdate) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = v
	if m.queued {
		return false
	}
	m.queued = true
	return true
}
func (m *snapshotMailbox) take() *snapshotUpdate {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.pending
	m.pending = nil
	m.queued = false
	return v
}

func (a *App) pump(startup <-chan startupInfo, initialHost metrics.HostInfo) {
	ch, cancel := a.sampler.Subscribe()
	defer cancel()
	pumpSnapshots(ch, startup, a.pumpDone, initialHost, a.publishStartup, func(snap *metrics.Snapshot, host metrics.HostInfo) {
		// Record and account for extrema even when a previous UI update is queued.
		update := a.prepareSnapshot(snap, host)
		if !a.updates.offer(update) {
			return
		}
		a.mw.Synchronize(func() {
			select {
			case <-a.pumpDone:
				return
			default:
			}
			if update := a.updates.take(); update != nil {
				a.onSnapshot(update.snap, update.rows)
			}
		})
	})
}

// Startup discovery must never gate live samples. The initial host is copied
// on the UI thread; this worker only reads its own copy while discovery runs.
func pumpSnapshots(samples <-chan *metrics.Snapshot, startup <-chan startupInfo, done <-chan struct{}, host metrics.HostInfo,
	publishStartup func(startupInfo), consume func(*metrics.Snapshot, metrics.HostInfo)) {
	for {
		select {
		case <-done:
			return
		case info, ok := <-startup:
			if !ok {
				startup = nil
				continue
			}
			host = info.host
			publishStartup(info)
		case snap, ok := <-samples:
			if !ok {
				return
			}
			consume(snap, host)
		}
	}
}

func (a *App) prepareSnapshot(s *metrics.Snapshot, host metrics.HostInfo) *snapshotUpdate {
	a.rec.Add(s)
	return &snapshotUpdate{snap: s, rows: a.sensors.Update(s, host)}
}

func (a *App) publishStartup(info startupInfo) {
	a.mw.Synchronize(func() {
		select {
		case <-a.pumpDone:
			return
		default:
		}
		a.host, a.localIP, a.elevatedAs = info.host, info.localIP, info.elevatedAs
		_ = a.mw.SetTitle(a.windowTitle())
		a.invalidateChrome()
	})
}

func (a *App) onSnapshot(s *metrics.Snapshot, rows []metrics.SensorRow) {
	a.snap = s
	if p, ok := a.pages[pgPower].impl.(*powerPage); ok {
		p.rows = rows
	}
	// Alerts and the tray are features of their own: they run even while the
	// window cannot be seen. Recording and sensor extrema already ran on the
	// pump goroutine (prepareSnapshot), and the sampler keeps filling history.
	a.updateTray(s)
	a.checkAlerts(s)
	if a.skipPresentation() {
		return // nobody can see it: refreshVisible catches up when they can
	}
	// The title and instruction are static between alert and navigation changes.
	if a.cur >= 0 && a.cur < len(a.foots) {
		a.foots[a.cur].invalidate()
	}
	if a.pages[a.cur].initialized {
		a.pages[a.cur].impl.tick(s)
	}
}

// ---- admin ---------------------------------------------------------------------

func (a *App) windowTitle() string {
	t := "gotm"
	if a.host.Hostname != "" {
		t += " — " + a.host.Hostname
	}
	if a.host.Elevated {
		t += " [Administrator]"
	}
	return t
}

func (a *App) elevate() {
	if exe, err := os.Executable(); err == nil && metrics.UserWritable(filepath.Dir(exe)) {
		if !a.confirm("Run as administrator",
			"gotm is running from a folder that ordinary programs can change:\n"+filepath.Dir(exe)+
				"\n\nAnything that replaced gotm.exe there would get administrator rights too. "+
				"Install gotm with its setup program (into Program Files) to elevate safely.\n\nElevate this copy anyway?") {
			return
		}
	}
	// Hand the single-instance slot to the elevated replacement before it is
	// launched: it reaches the guard while this instance is still closing,
	// and would otherwise treat itself as a second copy and exit right after
	// UAC was accepted.
	ReleaseSingleInstance()
	if err := metrics.RelaunchElevated("--tab " + a.pages[a.cur].id); err != nil {
		GuardSingleInstance() // UAC cancelled/denied: this instance keeps running
		walk.MsgBox(a.mw, "Run as administrator",
			"The elevated copy could not be started (UAC was cancelled or denied).\n\n"+err.Error(),
			walk.MsgBoxIconWarning)
		return
	}
	a.mw.Close()
}

// ---- shared helpers -------------------------------------------------------------

// copyRows copies the selected row of the current page's table to the
// clipboard (visible columns, tab-separated, header first). Pages without a
// table do nothing.
func (a *App) copyRows() {
	if !a.pages[a.cur].initialized {
		return
	}
	if c, ok := a.pages[a.cur].impl.(interface{ copyRow() }); ok {
		c.copyRow()
	}
}

// saveConfig enqueues a change. True means accepted; configuration is applied
// only after a successful background save.
func (a *App) saveConfig(mutate func(*config.Config)) bool {
	return a.queueSettings(mutate, false)
}
func (a *App) errBox(title string, err error) {
	walk.MsgBox(a.mw, title, err.Error(), walk.MsgBoxIconWarning)
}

func (a *App) confirm(title, text string) bool {
	return walk.MsgBox(a.mw, title, text, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion|walk.MsgBoxDefButton2) == walk.DlgCmdYes
}

func trimSpace(s string) string {
	b := []rune(s)
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && b[0] == ' ' {
		b = b[1:]
	}
	return string(b)
}

// appIcon loads the embedded application icon, or the stock one if the
// executable was built without resources.
func appIcon() *walk.Icon {
	if ic, err := walk.NewIconFromResource("APP"); err == nil {
		return ic
	}
	return walk.IconApplication()
}

// pageRoot lets each page register its root composite with the shell.
func (a *App) registerRoot(id int, root *walk.Composite) { a.pages[id].root = root }

// pagePadding is the content margin used by every page body.
var padBody = decl.Margins{Left: 24, Top: 8, Right: 24, Bottom: 10}

// initialSize restores the saved window size, clamped so a stale or inflated
// value can never open a window larger than the screen.
// minSize keeps the window usable but never larger than the screen's work
// area: 1366x768 at 125% leaves only about 1090x580 units above the taskbar.
func (a *App) minSize() decl.Size {
	w, h := 900, 540
	var wa win.RECT
	const spiGetWorkArea = 0x0030
	if r, _, _ := procSystemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0); r != 0 {
		dpi := screenDPI()
		if aw := int(wa.Right-wa.Left) * 96 / dpi; aw < w+40 {
			w = max(640, aw-40)
		}
		if ah := int(wa.Bottom-wa.Top) * 96 / dpi; ah < h+40 {
			h = max(420, ah-40)
		}
	}
	return decl.Size{Width: w, Height: h}
}

var procSystemParametersInfo = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")

func screenDPI() int {
	dpi := 96
	if hdc := win.GetDC(0); hdc != 0 {
		dpi = int(win.GetDeviceCaps(hdc, win.LOGPIXELSX))
		win.ReleaseDC(0, hdc)
	}
	if dpi <= 0 {
		dpi = 96
	}
	return dpi
}

func (a *App) initialSize() decl.Size {
	w, h := a.cfg.WindowW, a.cfg.WindowH
	sw := int(win.GetSystemMetrics(win.SM_CXSCREEN))
	sh := int(win.GetSystemMetrics(win.SM_CYSCREEN))
	dpi := 96
	if hdc := win.GetDC(0); hdc != 0 {
		dpi = int(win.GetDeviceCaps(hdc, win.LOGPIXELSX))
		win.ReleaseDC(0, hdc)
	}
	if dpi <= 0 {
		dpi = 96
	}
	maxW, maxH := sw*96/dpi*9/10, sh*96/dpi*9/10 // 90% of the screen, in 1/96 inch units
	if w > maxW && maxW > 0 {
		w = maxW
	}
	if h > maxH && maxH > 0 {
		h = maxH
	}
	return decl.Size{Width: w, Height: h}
}

// bodyStretch is the stretch factor of a page body. Walk's box layout hands
// out spare space in order of minimum size and proportionally to stretch, so
// the fixed-height header and footer would otherwise waste their share.
const bodyStretch = 1000

// saveWindowState stores the restored window size (not a minimized or
// maximized one), layout, sort and current page.
func (a *App) saveWindowState() {
	var width, height int
	var wp win.WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	if win.GetWindowPlacement(win.HWND(a.mw.Handle()), &wp) {
		dpi := screenDPI()
		if w := int(wp.RcNormalPosition.Right-wp.RcNormalPosition.Left) * 96 / dpi; w >= 640 {
			width = w
		}
		if h := int(wp.RcNormalPosition.Bottom-wp.RcNormalPosition.Top) * 96 / dpi; h >= 420 {
			height = h
		}
	}
	page := a.pages[a.cur].id
	// Compose with queued settings rather than replacing them with an older UI
	// snapshot. The worker reserves a slot for this final state update.
	a.queueSettings(func(c *config.Config) {
		if width != 0 {
			c.WindowW = width
		}
		if height != 0 {
			c.WindowH = height
		}
		c.LastPage = page
	}, true)
}

// watchSystem hooks the main window for two things Walk does not surface:
// WM_ENDSESSION (sign-out and shutdown must save the window state) and the
// system colour / theme change messages (high-contrast mode may have been
// switched on or off, so the palette is re-applied once Walk has processed
// the message itself).
func (a *App) watchSystem() {
	const spiSetHighContrast = 0x0043
	activation := activationMsg()
	a.endSessionCB = syscall.NewCallback(func(hwnd win.HWND, msg uint32, wp, lp uintptr) uintptr {
		stallBeat.Store(time.Now().UnixMilli()) // the UI thread is alive
		if activation != 0 && msg == activation {
			// a second launch asked this instance to show itself
			a.userPresent()
			activateMainWindow(hwnd)
			return 0
		}
		a.observeVisibility(hwnd, msg, wp, lp) // minimize, hide, lock, display off
		switch msg {
		case win.WM_ENDSESSION:
			if wp != 0 {
				a.saveWindowState()
			}
		case win.WM_SYSCOLORCHANGE, win.WM_THEMECHANGED:
			a.mw.Synchronize(a.retheme)
		case win.WM_SETTINGCHANGE:
			if wp == spiSetHighContrast {
				a.mw.Synchronize(a.retheme)
			}
		}
		return win.CallWindowProc(a.prevMainProc, hwnd, msg, wp, lp)
	})
	a.prevMainProc = win.SetWindowLongPtr(win.HWND(a.mw.Handle()), win.GWLP_WNDPROC, a.endSessionCB)
	a.registerVisibility(win.HWND(a.mw.Handle()))
	signalActivationReady() // second launches can stop knocking now
}

// retheme re-applies the palette after a system colour change. Owner-drawn
// surfaces read T when they paint; native widgets are told their new colours.
func (a *App) retheme() {
	changed := applyTheme()
	if !changed && !highContrast {
		return // still gotm's own dark theme: nothing to do
	}
	hwnd := win.HWND(a.mw.Handle())
	titleBar(hwnd)
	setBackground(a.mw, T.Bg)
	rethemeChildren(a.mw)
	recolorTables()
	const rdwInvalidate, rdwErase, rdwAllChildren = 0x0001, 0x0004, 0x0080
	procRedrawWindow.Call(uintptr(hwnd), 0, 0, rdwInvalidate|rdwErase|rdwAllChildren)
}

var procRedrawWindow = windows.NewLazySystemDLL("user32.dll").NewProc("RedrawWindow")

// rethemeChildren walks the widget tree and hands native widgets the palette.
func rethemeChildren(c walk.Container) {
	kids := c.Children()
	for i := 0; i < kids.Len(); i++ {
		switch w := kids.At(i).(type) {
		case *walk.Composite:
			setBackground(w, T.Bg)
			rethemeChildren(w)
		case *walk.Label:
			w.SetTextColor(T.Muted)
		case *walk.LineEdit:
			setBackground(w, T.Card)
			w.SetTextColor(T.Text)
		}
	}
}

// setBackground swaps a window's brush and frees the old one (each widget
// was built with a brush of its own, so nothing else refers to it).
func setBackground(w walk.Window, c walk.Color) {
	old := w.Background()
	w.SetBackground(solid(c))
	if b, ok := old.(*walk.SolidColorBrush); ok && b != nil {
		b.Dispose()
	}
}

// gpuSensorsWanted derives demand from the visible page so a hidden Performance
// repaint cannot turn off the sensors used by Power & Freq.
func (a *App) gpuSensorsWanted() bool {
	if a.cur == pgPower {
		return true
	}
	if a.cur != pgPerformance || len(a.pages) <= pgPerformance || a.pages[pgPerformance] == nil {
		return false
	}
	p, ok := a.pages[pgPerformance].impl.(*perfPage)
	if !ok {
		return false
	}
	items := p.items(a.snap)
	return items[clampI(p.sel, 0, len(items)-1)].kind == resGPU
}
