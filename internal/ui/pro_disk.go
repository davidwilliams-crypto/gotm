//go:build windows

package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tailscale/walk"
	decl "github.com/tailscale/walk/declarative"

	"gotm/internal/metrics"
)

// diskPage shows drive capacity and a folder-size breakdown for any folder.
type diskPage struct {
	*gridPage
	strip      *canvasView
	drives     []metrics.Drive
	root       string
	history    []string
	res        *metrics.ScanResult
	prog       *metrics.ScanProgress
	cancel     context.CancelFunc
	scan       int // scan generation, so a stale result is ignored
	files      bool
	bScan      *button
	bUp        *button
	bStop      *button
	bBrowse    *button
	bFiles     *button
	bOpen      *button
	lastDr     time.Time
	drivesBusy bool // ReadDrives is running in the background
}

func newDiskPage(a *App) *diskPage {
	g := newGridPage(a, pgDisk, []decl.TableViewColumn{
		col("Name", 420), colR("Size", 110), col("Share", 190), colR("Files", 90), col("Type", 90),
	}, 2)
	p := &diskPage{gridPage: g}
	g.filterPlaceholder = "Search this folder"
	g.onFilter = p.render
	g.activate = p.drill
	g.onSelect = p.updateButtons
	g.model.keyCol = 5

	p.strip = newCanvasView(p.paintStrip)
	p.strip.keyboard, p.strip.keyName = true, "Drives. Left and Right choose a drive, Enter measures it."
	p.strip.acc, p.strip.accName, p.strip.accCtl = true, "Drives", ctlList
	p.bScan = newButton("Scan", "", 86, func() { p.startScan(p.root, false) })
	p.bUp = newButton("Up", "", 72, p.up)
	p.bStop = newButton("Stop", "", 80, func() {
		if p.cancel != nil {
			p.cancel()
		}
	})
	p.bBrowse = newButton("Browse…", "", 110, p.browse)
	p.bFiles = newButton("Largest files", "", 130, func() { p.files = !p.files; p.bFiles.set(true, p.files); p.render() })
	p.bFiles.toggle = true
	p.bOpen = newButton("Open folder", "", 124, p.openExplorer)
	p.bOpen.tip = "Show the selection or scanned folder in Explorer"
	g.left = []decl.Widget{p.bScan.widget(), p.bUp.widget(), p.bBrowse.widget(), p.bStop.widget(),
		p.bFiles.widget(), p.bOpen.widget()}
	g.top = []decl.Widget{p.strip.declare(0, 96, 0, 96, 0)}

	p.strip.onClick = func(id string, _ walk.MouseButton) {
		var n int
		if _, err := fmt.Sscanf(id, "drv:%d", &n); err == nil && n < len(p.drives) {
			p.history = nil
			p.startScan(p.drives[n].Letter+`\`, false)
		}
	}
	return p
}

func (p *diskPage) build() decl.Widget { return p.gridPage.build() }
func (p *diskPage) init() {
	p.gridPage.init()
	p.updateButtons()
}

func (p *diskPage) shown() {
	p.refreshDrives()
	if p.res == nil && p.root == "" {
		// scanning a whole drive is I/O heavy, so wait for the user to choose
		p.setStatus("Pick a drive above, or Browse… to a folder, to measure where the space went.")
	}
}

func (p *diskPage) tick(*metrics.Snapshot) {
	if time.Since(p.lastDr) > 5*time.Second {
		p.refreshDrives()
	}
	if p.cancel != nil && p.prog != nil {
		p.setStatus(fmt.Sprintf("Scanning %s … %s files · %s · %d folders",
			p.root, commaInt(p.prog.Files.Load()), fmtBytes(uint64(p.prog.Bytes.Load())), p.prog.Dirs.Load()))
	}
}

func (p *diskPage) refreshDrives() {
	if p.drivesBusy {
		return // the previous read is still waiting on a slow drive
	}
	p.drivesBusy, p.lastDr = true, time.Now()
	go func() {
		d := metrics.ReadDrives()
		p.app.mw.Synchronize(func() {
			p.drives, p.drivesBusy = d, false
			p.strip.invalidate()
		})
	}()
}

func (p *diskPage) paintStrip(cv *walk.Canvas, w, h int) {
	// Cards narrow to fit every drive; past ~8 drives the rest are summarised
	// (they stay reachable through Browse…).
	n := len(p.drives)
	cw := 232
	if n > 0 && n*(cw+12) > w {
		cw = max(150, (w-(n-1)*12)/n)
	}
	fit := max(1, (w+12)/(cw+12))
	if n > fit {
		fit = max(1, fit-1) // leave the last slot for the "more" note
	}
	x := 0
	for i, d := range p.drives {
		if i >= fit {
			more := fmt.Sprintf("+%d more drives: use Browse…", n-fit)
			drawText(cv, more, uiFont(9), T.Muted,
				rect{x, 4, max(120, w-x), h - 8}, walk.TextLeft|walk.TextVCenter|walk.TextWordbreak)
			p.strip.addText(rect{x, 4, max(120, w-x), h - 8}, more)
			break
		}
		r := rect{x, 4, cw, h - 8}
		x += cw + 12
		cur := strings.EqualFold(filepath.VolumeName(p.root)+`\`, d.Letter+`\`)
		fill := T.Card
		if cur {
			fill = T.CardSel
		} else if p.strip.hover == fmt.Sprintf("drv:%d", i) {
			fill = T.CardHi
		}
		fillRound(cv, r, 10, fill)
		strokeRound(cv, r, 10, T.Stroke)
		drawText(cv, d.Letter, uiBold(14), T.Text, rect{r.X + 14, r.Y + 8, 60, 26}, taLeft)
		used := d.Total - d.Free
		frac := 0.0
		if d.Total > 0 {
			frac = float64(used) / float64(d.Total)
		}
		drawText(cv, fmt.Sprintf("%s · %.0f%% used", d.Kind, frac*100), uiFont(8), T.Faint, rect{r.X + 14, r.Y + 32, 140, 16}, taLeft)
		barC := T.Disk
		switch {
		case frac > 0.9:
			barC = T.Bad
		case frac > 0.75:
			barC = T.Warn
		}
		bar := rect{r.X + 14, r.Y + r.H - 24, r.W - 28, 8}
		fillRound(cv, bar, 4, T.Grid)
		fillRound(cv, rect{bar.X, bar.Y, int(float64(bar.W) * frac), bar.H}, 4, barC)
		drawText(cv, fmt.Sprintf("%s free of %s", fmtBytes(d.Free), fmtBytes(d.Total)), uiFont(9), T.Muted,
			rect{r.X + 80, r.Y + 8, r.W - 92, 40}, walk.TextRight|walk.TextVCenter|walk.TextSingleLine)
		p.strip.addItem(r, fmt.Sprintf("drv:%d", i),
			fmt.Sprintf("Drive %s, %s, %.0f%% used, %s free of %s", d.Letter, d.Kind, frac*100, fmtBytes(d.Free), fmtBytes(d.Total)),
			ctlListItem, cur)
	}
}

func commaInt(n int64) string {
	s := fmt.Sprint(n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// startScan measures root in the background and renders the result.
func (p *diskPage) startScan(root string, pushHistory bool) {
	if root == "" {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if pushHistory && p.root != "" && p.root != root {
		p.history = append(p.history, p.root)
	}
	p.root = root
	p.scan++
	gen := p.scan
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.prog = &metrics.ScanProgress{}
	prog := p.prog
	p.res = nil
	p.model.set(nil)
	p.updateButtons()
	p.strip.invalidate()
	p.setStatus("Scanning " + root + " …")

	go func() {
		res := metrics.ScanDir(ctx, root, prog)
		p.app.mw.Synchronize(func() {
			if gen != p.scan {
				return
			}
			p.cancel = nil
			p.res = &res
			if res.Err != nil {
				p.updateButtons()
				p.setStatus("Cannot scan " + root + ": " + res.Err.Error())
				return
			}
			p.render()
			p.updateButtons()
		})
	}()
}

func (p *diskPage) render() {
	if p.res == nil {
		return
	}
	if p.res.Err != nil {
		p.setStatus("Cannot scan " + p.root + ": " + p.res.Err.Error())
		return
	}
	q := p.filterText()
	var rows [][]string
	total := float64(p.res.Total)
	list := p.res.Entries
	if p.files {
		list = p.res.TopFiles
	}
	for _, e := range list {
		name := e.Name
		if p.files {
			name = e.Path
		}
		if !matchesAny(q, name) {
			continue
		}
		share := 0.0
		if total > 0 {
			share = float64(e.Size) / total
		}
		kind := "File"
		files := ""
		if e.IsDir {
			kind = "Folder"
			files = commaInt(e.Files)
		}
		bar := strings.Repeat("█", int(share*14+0.5))
		rows = append(rows, []string{name, fmtBytes(uint64(e.Size)), fmt.Sprintf("%.1f%%  %s", share*100, bar),
			files, kind, e.Path})
	}
	p.model.set(rows)
	p.updateButtons()
	extra := ""
	if p.res.Denied > 0 {
		extra = fmt.Sprintf(" · %d folders not readable", p.res.Denied)
	}
	if p.res.Cancelled {
		extra += " · stopped early, sizes are partial"
	}
	p.setResultStatus(fmt.Sprintf("%s · %d shown · %s in %s files · %.1fs%s", p.root, len(rows), fmtBytes(uint64(p.res.Total)), commaInt(p.res.Files), p.res.Elapsed.Seconds(), extra))
}

func (p *diskPage) selectedEntry() (metrics.ScanEntry, bool) {
	if p.res == nil {
		return metrics.ScanEntry{}, false
	}
	r := p.model.selectedRow()
	if r == nil {
		return metrics.ScanEntry{}, false
	}
	list := p.res.Entries
	if p.files {
		list = p.res.TopFiles
	}
	for _, e := range list {
		if e.Path == cell(r, 5) {
			return e, true
		}
	}
	return metrics.ScanEntry{}, false
}

func (p *diskPage) drill() {
	if e, ok := p.selectedEntry(); ok && e.IsDir {
		p.startScan(e.Path, true)
	}
}

func (p *diskPage) up() {
	if n := len(p.history); n > 0 {
		prev := p.history[n-1]
		p.history = p.history[:n-1]
		p.startScan(prev, false)
		return
	}
	if parent := filepath.Dir(filepath.Clean(p.root)); parent != "" && parent != filepath.Clean(p.root) && parent != "." {
		p.startScan(parent, false)
	}
}

func (p *diskPage) browse() {
	dlg := walk.FileDialog{Title: "Choose a folder to measure"}
	if ok, err := dlg.ShowBrowseFolder(p.app.mw); err != nil {
		p.app.errBox("Choose folder", err)
	} else if ok && dlg.FilePath != "" {
		p.startScan(dlg.FilePath, true)
	}
}

func (p *diskPage) openExplorer() {
	target := p.root
	if e, ok := p.selectedEntry(); ok {
		target = e.Path
	}
	openInExplorer(target)
}

func (p *diskPage) updateButtons() {
	scanning := p.cancel != nil
	p.bStop.set(scanning, false)
	p.bScan.set(p.root != "" && !scanning, false)
	p.bUp.set(p.root != "" && !scanning && (len(p.history) > 0 || filepath.Dir(filepath.Clean(p.root)) != filepath.Clean(p.root)), false)
	p.bFiles.set(p.res != nil && (p.files || len(p.res.TopFiles) > 0) && !scanning, p.files)
	p.bOpen.set(p.root != "", false)
}

func (p *diskPage) reload() {
	if p.root != "" && p.cancel == nil {
		p.startScan(p.root, false)
	} else {
		p.refreshDrives()
	}
}
