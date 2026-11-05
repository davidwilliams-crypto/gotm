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

// rowIdentifier is optionally implemented by a rowSigner whose rows have a
// stable identity, so a reorder of the visible rows can be announced to
// assistive technology (see announceReorder).
type rowIdentifier interface {
	rowID(row int) uint64
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

	// row identities of the cached window, for reorder announcements
	count, idTop  int
	ids, nextIDs  []uint64
	idsValid      bool
	shown         bool // the list view was visible at the last refresh or mark
	reorderEvents int  // EVENT_OBJECT_REORDER raised (tests)

	// what the last repaint invalidated (tests, profiling)
	lastRows, lastCells, lastBands int
}

// listNotifyDepth counts list-view notifications (and list-view key messages)
// being handled on the UI thread. A repaint started from inside one never
// flushes bands with UpdateWindow: painting re-entrantly inside a list-view
// notification is best avoided.
var listNotifyDepth int

// inListMessage runs f as if inside a list-view notification.
func inListMessage(f func()) {
	listNotifyDepth++
	defer func() { listNotifyDepth-- }()
	f()
}

var sigSeed = maphash.MakeSeed()

// cellSig summarizes a cell's text and colours.
func cellSig(text string, bg, fg walk.Color) uint64 {
	return maphash.String(sigSeed, text) ^ (uint64(bg)<<32|uint64(fg))*0x9E3779B97F4A7C15
}

// forget drops the cache; the next repaint redraws every visible row.
func (d *paintDiff) forget() { d.valid, d.idsValid = false, false }

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

// markPainted records what tv's visible rows show now as painted, without
// invalidating anything: for use right after the whole list was invalidated
// (a palette change) or exposed (its page was just shown). The next repaint
// then invalidates only what changes after this point, rather than every
// visible row a second time.
//
// It relies on the paintDiff invariant: the model has not changed since the
// last repaint, so any paint pending or done since shows exactly the current
// model. When the list view was hidden at the last repaint, it is invalidated
// whole here: free when its exposure paint is still pending (the regions
// merge), and it guarantees the first paint after showing is complete.
func (d *paintDiff) markPainted(tv *walk.TableView, count int, s rowSigner) {
	lv, top, rows, layout, ok := d.window(tv, count)
	if !ok {
		d.valid, d.idsValid = false, false
		return
	}
	visible := win.IsWindowVisible(lv)
	if visible && !d.shown {
		win.InvalidateRect(lv, nil, false)
	}
	d.record(s, top, rows, layout, count)
	d.shown = visible
}

// observe is called just before the model changes. Rows that became visible
// since the last repaint (a scroll, a resize, a page's layout finishing after
// it was shown) were painted by the list view itself from the current model,
// so they are recorded as painted now, while that model is still there to
// describe them; otherwise the repaint that follows the change would see them
// as new to the cache and redraw them whole. Cheap when the visible window is
// unchanged (the common case): no row is signed.
func (d *paintDiff) observe(tv *walk.TableView, count int, s rowSigner) {
	if !d.valid {
		return // the next repaint redraws every visible row anyway
	}
	_, top, rows, layout, ok := d.window(tv, count)
	if !ok || layout != d.layout || top == d.top && rows == d.rows {
		return
	}
	d.record(s, top, rows, layout, count)
}

// window resolves tv's normal list view and the range of rows it shows. ok is
// false when the cache cannot be used (frozen columns, no list view yet).
func (d *paintDiff) window(tv *walk.TableView, count int) (lv win.HWND, top, rows int, layout uint64, ok bool) {
	if tv == nil || tv.IsDisposed() {
		return 0, 0, 0, 0, false
	}
	cols := tv.Columns()
	vis := d.vis[:0]
	layout = uint64(14695981039346656037)
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
	for _, h := range listViews(tv) {
		hdr := win.HWND(win.SendMessage(h, lvmGetHeader, 0, 0))
		if n := int(win.SendMessage(hdr, win.HDM_GETITEMCOUNT, 0, 0)); n > 0 && n == len(vis) {
			lv = h
		}
	}
	if frozen || lv == 0 {
		return 0, 0, 0, layout, false
	}
	top = int(win.SendMessage(lv, win.LVM_GETTOPINDEX, 0, 0))
	per := int(win.SendMessage(lv, win.LVM_GETCOUNTPERPAGE, 0, 0))
	rows = max(min(count, top+per+1)-top, 0)
	return lv, top, rows, layout, true
}

// record computes the signatures (and identities) of rows [top, top+rows)
// into d.next and makes them the cache.
func (d *paintDiff) record(s rowSigner, top, rows int, layout uint64, count int) {
	width := len(d.vis) + 1
	next := growSigs(d.next, rows*width)
	for r := 0; r < rows; r++ {
		s.rowSigs(top+r, d.vis, next[r*width:(r+1)*width])
	}
	d.sigs, d.next = next, d.sigs
	d.top, d.rows, d.width, d.layout, d.valid = top, rows, width, layout, true
	d.recordIDs(s, top, rows, count)
}

// recordIDs caches the identities of the window's rows and reports whether a
// row at a position both windows show now has a different identity: the
// visible order changed (or a visible row was replaced) while the row count
// stayed the same.
func (d *paintDiff) recordIDs(s rowSigner, top, rows, count int) (reordered bool) {
	ider, ok := s.(rowIdentifier)
	if !ok {
		d.idsValid = false
		return false
	}
	ids := growSigs(d.nextIDs, rows)
	for r := range ids {
		ids[r] = ider.rowID(top + r)
	}
	if d.idsValid && count == d.count {
		lo, hi := max(top, d.idTop), min(top+rows, d.idTop+len(d.ids))
		for row := lo; row < hi; row++ {
			if ids[row-top] != d.ids[row-d.idTop] {
				reordered = true
				break
			}
		}
	}
	d.ids, d.nextIDs = ids, d.ids
	d.idTop, d.count, d.idsValid = top, count, true
	return reordered
}

func (d *paintDiff) refresh(tv *walk.TableView, count int, s rowSigner, bands bool) {
	d.lastRows, d.lastCells, d.lastBands = 0, 0, 0
	if tv == nil || tv.IsDisposed() {
		return
	}
	bands = bands && listNotifyDepth == 0
	lv, top, rows, layout, ok := d.window(tv, count)
	if !ok {
		d.valid, d.idsValid = false, false
		redrawVisible(tv)
		return
	}
	width := len(d.vis) + 1
	next := growSigs(d.next, rows*width)
	for r := 0; r < rows; r++ {
		s.rowSigs(top+r, d.vis, next[r*width:(r+1)*width])
	}
	old := d.sigs
	if !d.valid || d.layout != layout || d.width != width {
		old = nil
	}
	d.ops = diffPaint(d.ops[:0], d.top, d.rows, old, top, rows, next, width)
	d.apply(lv, d.ops, bands)
	d.sigs, d.next = next, d.sigs
	d.top, d.rows, d.width, d.layout, d.valid = top, rows, width, layout, true
	visible := win.IsWindowVisible(lv)
	if d.recordIDs(s, top, rows, count) && visible && d.shown {
		d.announceReorder(lv)
	}
	d.shown = visible
}

// announceReorder tells assistive technology that the list's visible rows
// changed order. Without a model reset the list view raises no event of its
// own for a reorder: LVM_REDRAWITEMS and InvalidateRect only paint, while
// LVM_SETITEMCOUNT (a reset, or rows added or removed) raises object
// destroy/create on the client, which screen readers take as "re-read".
// EVENT_OBJECT_REORDER is what the list view itself raises after sorting its
// items; UI Automation maps it to a StructureChanged (children reordered)
// event. Raised at most once per repaint, only when the order changed.
func (d *paintDiff) announceReorder(lv win.HWND) {
	win.NotifyWinEvent(win.EVENT_OBJECT_REORDER, lv, win.OBJID_CLIENT, win.CHILDID_SELF)
	d.reorderEvents++
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
			// The cell's bottom edge is the next row's top edge, and the list
			// view repaints every item its update region touches: without
			// this, each changed cell also repainted the whole row below it.
			rc.Bottom--
			win.InvalidateRect(lv, &rc, false)
		} else {
			win.SendMessage(lv, win.LVM_REDRAWITEMS, uintptr(op.row), uintptr(op.row))
		}
		d.lastCells++
		i++
	}
}
