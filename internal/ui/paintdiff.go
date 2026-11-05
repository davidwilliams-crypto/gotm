//go:build windows

package ui

import (
	"hash/maphash"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

// Table pages refresh every tick, but most visible cells show the same text
// in the same colours as before. Repainting every visible row each tick made
// the native list view (on software GDI) the largest CPU cost of an open
// table. paintDiff remembers a signature of every cell it last asked the list
// view to show and invalidates only rows or cells whose signature changed.
//
// Correctness rests on one invariant: between two repaints the model does not
// change, so any paint the list view performs on its own (scrolling, exposure,
// hover) shows exactly what the cached signatures describe. Rows the cache
// has not seen (scrolled into view, new rows, a new column layout) are
// repainted whole.

// rowSigner describes what a table row displays.
type rowSigner interface {
	// rowSigs fills out[0] with a signature of the row as a whole (selection,
	// icon, indent) and out[1+k] with one of the cell in walk column vis[k].
	rowSigs(row int, vis []int, out []uint64)
}

// paintOp asks for one row (sub < 0) or one cell (native sub-item) to repaint.
type paintOp struct{ row, sub int32 }

type paintDiff struct {
	top, rows, width int
	layout           uint64
	valid            bool
	sigs, next       []uint64
	vis              []int
	ops              []paintOp

	// what the last repaint invalidated (tests, profiling)
	lastRows, lastCells, lastBands int
}

var sigSeed = maphash.MakeSeed()

// cellSig summarizes a cell's text and colours.
func cellSig(text string, bg, fg walk.Color) uint64 {
	return maphash.String(sigSeed, text) ^ (uint64(bg)<<32|uint64(fg))*0x9E3779B97F4A7C15
}

// forget drops the cache; the next repaint redraws every visible row.
func (d *paintDiff) forget() { d.valid = false }

// repaint compares what tv's visible rows show now with what they showed at
// the previous repaint and invalidates only what changed. It never paints
// synchronously.
func (d *paintDiff) repaint(tv *walk.TableView, count int, s rowSigner) {
	d.refresh(tv, count, s, true)
}

// repaintAsync is repaint without flushing separated bands, for use inside
// list-view notifications (selection changes), where painting re-entrantly is
// best avoided.
func (d *paintDiff) repaintAsync(tv *walk.TableView, count int, s rowSigner) {
	d.refresh(tv, count, s, false)
}

func (d *paintDiff) refresh(tv *walk.TableView, count int, s rowSigner, bands bool) {
	d.lastRows, d.lastCells, d.lastBands = 0, 0, 0
	if tv == nil || tv.IsDisposed() {
		return
	}
	cols := tv.Columns()
	vis := d.vis[:0]
	layout := uint64(14695981039346656037)
	frozen := false
	for i := 0; i < cols.Len(); i++ {
		c := cols.At(i)
		if !c.Visible() {
			continue
		}
		frozen = frozen || c.Frozen()
		vis = append(vis, i)
		layout = (layout ^ uint64(i+1)) * 1099511628211
	}
	d.vis = vis
	// The normal list view holds every visible column unless some are frozen.
	var lv win.HWND
	for _, h := range listViews(tv) {
		hdr := win.HWND(win.SendMessage(h, lvmGetHeader, 0, 0))
		if n := int(win.SendMessage(hdr, win.HDM_GETITEMCOUNT, 0, 0)); n > 0 && n == len(vis) {
			lv = h
		}
	}
	if frozen || lv == 0 {
		d.valid = false
		redrawVisible(tv)
		return
	}
	top := int(win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0))
	per := int(win.SendMessage(lv, win.LVM_GETCOUNTPERPAGE, 0, 0))
	rows := min(count, top+per+1) - top
	if rows < 0 {
		rows = 0
	}
	width := len(vis) + 1
	next := growSigs(d.next, rows*width)
	for r := 0; r < rows; r++ {
		s.rowSigs(top+r, vis, next[r*width:(r+1)*width])
	}
	old := d.sigs
	if !d.valid || d.layout != layout || d.width != width {
		old = nil
	}
	d.ops = diffPaint(d.ops[:0], d.top, d.rows, old, top, rows, next, width)
	d.apply(lv, d.ops, bands)
	d.sigs, d.next = next, d.sigs
	d.top, d.rows, d.width, d.layout, d.valid = top, rows, width, layout, true
}

func growSigs(s []uint64, n int) []uint64 {
	if cap(s) < n {
		return make([]uint64, n, n+n/2)
	}
	return s[:n]
}

// diffPaint lists what to repaint when the window of rows [top, top+rows)
// now shows cur, given that [oldTop, oldTop+oldRows) showed old (nil: not
// known). width is the number of signatures per row: the row's own, then
// one per native sub-item. A row is repainted whole when it is new to the
// cache, when its own signature or its first cell (which carries icon and
// indent) changed, or when most of its cells changed; otherwise only its
// changed cells are.
func diffPaint(ops []paintOp, oldTop, oldRows int, old []uint64, top, rows int, cur []uint64, width int) []paintOp {
	for r := 0; r < rows; r++ {
		row := top + r
		c := cur[r*width : (r+1)*width]
		o := row - oldTop
		if old == nil || o < 0 || o >= oldRows {
			ops = append(ops, paintOp{int32(row), -1})
			continue
		}
		p := old[o*width : (o+1)*width]
		if c[0] != p[0] || c[1] != p[1] {
			ops = append(ops, paintOp{int32(row), -1})
			continue
		}
		changed := 0
		for k := 2; k < width; k++ {
			if c[k] != p[k] {
				changed++
			}
		}
		switch {
		case changed == 0:
		case changed*2 > width-1:
			ops = append(ops, paintOp{int32(row), -1})
		default:
			for k := 2; k < width; k++ {
				if c[k] != p[k] {
					ops = append(ops, paintOp{int32(row), int32(k - 1)})
				}
			}
		}
	}
	return ops
}

// bandGap is how many unchanged rows may separate two changed rows before
// they are painted separately.
const bandGap = 2

// apply invalidates the listed rows (batched into runs) and cells.
//
// The list view paints every item that intersects the bounding box of its
// update region, not just the invalid parts: changed cells in rows 2 and 14
// would repaint rows 2 through 14, all columns. So separated bands of changes
// are painted one at a time: each band but the last is flushed with
// UpdateWindow before the next is invalidated. The last band (the only band,
// commonly) is left to the normal, low-priority WM_PAINT.
func (d *paintDiff) apply(lv win.HWND, ops []paintOp, bands bool) {
	for i := 0; i < len(ops); {
		op := ops[i]
		if bands && i > 0 && op.row-ops[i-1].row > bandGap {
			win.UpdateWindow(lv)
			d.lastBands++
		}
		if op.sub < 0 {
			j := i
			for j+1 < len(ops) && ops[j+1].sub < 0 && ops[j+1].row == ops[j].row+1 {
				j++
			}
			win.SendMessage(lv, win.LVM_REDRAWITEMS, uintptr(op.row), uintptr(ops[j].row))
			d.lastRows += j - i + 1
			i = j + 1
			continue
		}
		rc := win.RECT{Top: op.sub, Left: win.LVIR_BOUNDS}
		if win.SendMessage(lv, win.LVM_GETSUBITEMRECT, uintptr(op.row), uintptr(unsafe.Pointer(&rc))) != 0 {
			win.InvalidateRect(lv, &rc, false)
		} else {
			win.SendMessage(lv, win.LVM_REDRAWITEMS, uintptr(op.row), uintptr(op.row))
		}
		d.lastCells++
		i++
	}
}
