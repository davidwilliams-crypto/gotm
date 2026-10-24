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
	"slices"
	"syscall"
	"time"
	"unsafe"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"

	"gotm/internal/config"
	"gotm/internal/metrics"
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
}

// App owns the main window and everything hanging off it.
type App struct {
	dbgN    int
	version string
	store   *config.Store
	cfg     config.Config
	sampler *metrics.Sampler
	host    metrics.HostInfo
	rec     *metrics.Recorder
	sensors *metrics.SensorHub

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

	scheduled  chan struct{}
	alertText  string
	alertSince map[int]int64
	alertShown map[int]time.Time
	localIP    string
	ryzenOn    bool   // AMD SDK switch (HKLM, set by an administrator)
	elevatedAs string // set when elevated as an account other than the signed-in user

	endSessionCB, prevMainProc uintptr // main-window hook for WM_ENDSESSION
	startupTab                 int
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
	// The UI thread is a COM single-threaded apartment: UI Automation (uia.go)
	// marshals screen-reader calls to it, and Walk's own annotations need it.
	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	applyTheme() // gotm's dark palette, or the system colours in high-contrast mode
	a := &App{
		version: version, store: store, cfg: store.Get(), sampler: sampler,
		host: metrics.ReadHostInfo(), alertSince: map[int]int64{}, alertShown: map[int]time.Time{},
		scheduled: make(chan struct{}, 1), sensors: metrics.NewSensorHub(),
		localIP: firstLocalIP(),
	}
	a.rec = metrics.NewRecorder(3*time.Hour, 2*time.Second, store.File("recorder"))
	a.ryzenOn = metrics.RyzenSDKEnabled()
	if acct, other := metrics.ElevatedAs(); other {
		a.elevatedAs = acct
	}
	a.setRyzenSDK(a.ryzenOn && a.host.Elevated)

	a.procs = newProcsTab(a)
	a.settings = newSettingsTab(a)
	perf := newPerfPage(a)
	a.pages = make([]*pageDef, pgCount)
	a.pages[pgPerformance] = &pageDef{id: "performance", title: "Performance", subtitle: "Live CPU, memory, GPU, disk and network", glyph: "", impl: perf}
	a.pages[pgProcesses] = &pageDef{id: "processes", title: "Processes", subtitle: "Everything that is running, in a tree or a flat list", glyph: "", impl: a.procs}
	a.pages[pgStartup] = &pageDef{id: "startup", title: "Startup apps", subtitle: "What launches at sign-in, and what you can switch off", glyph: "", impl: newStartupTab(a)}
	a.pages[pgServices] = &pageDef{id: "services", title: "Services", subtitle: "Windows services: state, start type and control", glyph: "", impl: newServicesTab(a)}
	a.pages[pgPower] = &pageDef{id: "power", title: "Power & Freq", subtitle: "Clocks, temperatures, drive health and power state", glyph: "", pro: true, impl: newPowerPage(a)}
	a.pages[pgRecorder] = &pageDef{id: "recorder", title: "Flight Recorder", subtitle: "Scrub back in time to see what caused a spike", glyph: "", pro: true, impl: newRecorderPage(a)}
	a.pages[pgConnections] = &pageDef{id: "connections", title: "Connections", subtitle: "Every TCP/UDP endpoint and the process that owns it", glyph: "", pro: true, impl: newConnsPage(a)}
	a.pages[pgApps] = &pageDef{id: "apps", title: "Installed Apps", subtitle: "Programs registered with Windows", glyph: "", pro: true, impl: newAppsPage(a)}
	a.pages[pgDrivers] = &pageDef{id: "drivers", title: "Drivers", subtitle: "Kernel and file-system drivers, with publisher and version", glyph: "", pro: true, impl: newDriversPage(a)}
	a.pages[pgDisk] = &pageDef{id: "disk", title: "Disk Space", subtitle: "Where the space went: folders and largest files", glyph: "", pro: true, impl: newDiskPage(a)}
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
	// Every page starts hidden except the first one shown. Hidden pages take no
	// space, so the window is sized for one page rather than all eleven.
	a.wraps = make([]*walk.Composite, pgCount)
	var contents []decl.Widget
	for i, p := range a.pages {
		contents = append(contents, decl.Composite{
			AssignTo:      &a.wraps[i],
			Visible:       i == start,
			StretchFactor: bodyStretch,
			Background:    decl.SolidColorBrush{Color: T.Bg},
			Layout:        decl.VBox{MarginsZero: true, SpacingZero: true},
			Children:      []decl.Widget{p.impl.build()},
		})
	}

	enableDarkMode() // dark context menus and list-view headers (before any window exists)
	icon := appIcon()
	err := decl.MainWindow{
		AssignTo:   &a.mw,
		Title:      a.windowTitle(),
		Font:       decl.Font{Family: uiFamily, PointSize: 10},
		Icon:       icon,
		Background: decl.SolidColorBrush{Color: T.Bg},
		MinSize:    a.minSize(),
		Size:       a.initialSize(),
		Layout:     decl.HBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			a.side.declare(236, 0, 236, 0, 0),
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
	if err != nil {
		return err
	}
	// Declarative Visible is not honoured at creation, so every page was shown
	// and the layout sized the window for all of them. Hide the others now and
	// give the window its real size explicitly.
	for k, w := range a.wraps {
		if k != start {
			w.SetVisible(false)
		}
	}
	a.mw.SetSize(walk.Size{Width: a.initialSize().Width, Height: a.initialSize().Height})
	if tb := a.mw.ToolBar(); tb != nil {
		tb.SetVisible(false)
	}
	if sb := a.mw.StatusBar(); sb != nil {
		sb.SetVisible(false)
	}
	allowDark(win.HWND(a.mw.Handle()))
	titleBar(win.HWND(a.mw.Handle()))

	a.side.attachWheel()
	for _, p := range a.pages {
		p.impl.init()
	}
	a.setupShortcuts()
	a.setupTray(icon)
	// pages may have trimmed their minimum width during init: apply the real size now
	a.mw.SetSize(walk.Size{Width: a.initialSize().Width, Height: a.initialSize().Height})

	a.watchSystem() // sign-out/shutdown save the window state; colour changes re-theme
	a.mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		a.saveWindowState()
		a.setRyzenSDK(false) // releases the AMD SDK before the process exits
		a.rec.SetPersist(false)
		if p, ok := a.pages[pgDisk].impl.(*diskPage); ok && p.cancel != nil {
			p.cancel()
		}
		if a.tray != nil {
			a.tray.Dispose()
		}
	})

	a.navigate(start)
	go a.pump()
	walk.App().Run()
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

// navigate shows page i and hides the others.
func (a *App) navigate(i int) {
	if i < 0 || i >= len(a.pages) {
		return
	}
	a.cur = i
	for k, w := range a.wraps {
		if w != nil && w.Visible() != (k == i) {
			w.SetVisible(k == i)
		}
	}
	a.mw.RequestImmediateLayout()
	a.side.invalidate()
	a.invalidateChrome()
	metrics.SetGPUSensorsWanted(false) // the Performance page turns it back on for a GPU view
	a.sensors.SetDetailWanted(i == pgPower)
	a.pages[i].impl.shown()
	if a.snap != nil {
		a.pages[i].impl.tick(a.snap)
	}
	a.focusPage()
}

func (a *App) invalidateChrome() {
	for _, v := range a.heads {
		v.invalidate()
	}
	for _, v := range a.foots {
		v.invalidate()
	}
}

func (a *App) onSideClick(id string) {
	var n int
	if _, err := fmt.Sscanf(id, "nav:%d", &n); err == nil {
		a.navigate(n)
	}
}

// pump forwards each sampler tick to the UI thread, coalescing bursts so a
// busy UI never builds up a backlog.
func (a *App) pump() {
	ch, cancel := a.sampler.Subscribe()
	defer cancel()
	for snap := range ch {
		s := snap
		select {
		case a.scheduled <- struct{}{}:
		default:
			continue // a UI update is already queued
		}
		a.mw.Synchronize(func() {
			<-a.scheduled
			a.onSnapshot(s)
		})
	}
}

func (a *App) onSnapshot(s *metrics.Snapshot) {
	a.snap = s
	if p, ok := a.pages[pgPower].impl.(*powerPage); ok {
		p.rows = a.sensors.Update(s, a.host)
	}
	a.rec.Add(s)
	a.updateTray(s)
	a.checkAlerts(s)
	a.invalidateChrome()
	a.pages[a.cur].impl.tick(s)
}

// ---- admin ---------------------------------------------------------------------

func (a *App) windowTitle() string {
	t := fmt.Sprintf("gotm — %s", a.host.Hostname)
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
	if err := metrics.RelaunchElevated("--tab " + a.pages[a.cur].id); err != nil {
		walk.MsgBox(a.mw, "Run as administrator",
			"The elevated copy could not be started (UAC was cancelled or denied).\n\n"+err.Error(),
			walk.MsgBoxIconWarning)
		return
	}
	a.mw.Close()
}

// ---- shared helpers -------------------------------------------------------------

func (a *App) saveConfig(mutate func(*config.Config)) bool {
	c := a.store.Get()
	// Header sorting is kept live until shutdown; other settings must retain it.
	c.SortKey, c.SortDesc = a.cfg.SortKey, a.cfg.SortDesc
	mutate(&c)
	saved, err := a.store.Set(c)
	if err != nil {
		walk.MsgBox(a.mw, "Settings", "Could not save settings: "+err.Error(), walk.MsgBoxIconError)
		return false
	}
	alertsChanged := !slices.Equal(a.cfg.Alerts, saved.Alerts)
	a.cfg = saved
	a.sampler.SetInterval(saved.RefreshMs)
	a.sampler.SetHistorySeconds(saved.HistorySeconds)
	if alertsChanged {
		a.alertSince = map[int]int64{}
		a.alertShown = map[int]time.Time{}
	}
	return true
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
	c := a.store.Get()
	var wp win.WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	if win.GetWindowPlacement(win.HWND(a.mw.Handle()), &wp) {
		dpi := screenDPI()
		if w := int(wp.RcNormalPosition.Right-wp.RcNormalPosition.Left) * 96 / dpi; w >= 640 {
			c.WindowW = w
		}
		if h := int(wp.RcNormalPosition.Bottom-wp.RcNormalPosition.Top) * 96 / dpi; h >= 420 {
			c.WindowH = h
		}
	}
	c.TreeView, c.GroupByName = a.cfg.TreeView, a.cfg.GroupByName
	c.SortKey, c.SortDesc = a.cfg.SortKey, a.cfg.SortDesc
	c.LastPage = a.pages[a.cur].id
	_, _ = a.store.Set(c)
}

// watchSystem hooks the main window for two things Walk does not surface:
// WM_ENDSESSION (sign-out and shutdown must save the window state) and the
// system colour / theme change messages (high-contrast mode may have been
// switched on or off, so the palette is re-applied once Walk has processed
// the message itself).
func (a *App) watchSystem() {
	const spiSetHighContrast = 0x0043
	a.endSessionCB = syscall.NewCallback(func(hwnd win.HWND, msg uint32, wp, lp uintptr) uintptr {
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
