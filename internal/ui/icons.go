//go:build windows

package ui

import (
	"container/list"
	"runtime"
	"sync"
	"unsafe"

	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// procIconCache turns process image paths into small icons on a background
// thread and keeps them in one native image list shared by the process
// tables.
//
// A file path handed to Walk's table goes through SHGetFileInfo ON THE UI
// THREAD — not just the first time (cold shell icon cache, icon-handler
// extensions loading, antivirus scanning each read: seconds across a whole
// process list) but even later, because a cache hit still parses the file.
// On a sleeping disk that "warm" call blocks for seconds and Windows flags
// the window "not responding". Rows therefore show the generic program icon
// (slot 0) until a worker thread has extracted the real one. The UI thread
// performs no file or shell access for icons, cold or warm.
//
// The image list holds at most iconCapacity program icons. The least
// recently displayed path gives up its slot, which is then reused, so a long
// session that sees many short-lived programs does not grow GDI memory
// without bound. A slot used in the current paint generation (a row that may
// be on screen) is never reused; the cache grows past its capacity instead.
//
// The image list and the slot map belong to the UI thread; only the request
// queue and the finished results cross threads, under mu.
type procIconCache struct {
	capacity int

	// UI thread only
	himl  win.HIMAGELIST
	slots map[string]*list.Element // path -> *iconEntry (in lru)
	lru   *list.List               // front = most recently used
	free  []int32                  // reusable slots
	gen   uint64                   // paint generation, see touchGeneration
	views []win.HWND               // list views showing the image list

	mu       sync.Mutex
	pend     map[string]bool
	done     []iconResult
	queued   bool              // a flush is posted
	post     func(func()) bool // runs a func on the UI thread; nil until a table attaches
	resolved func()            // UI-thread repaint after new icons arrive
	ch       chan string
	start    sync.Once
	extract  func(path string) win.HICON
}

type iconEntry struct {
	path string
	slot int32 // 0: no icon of its own, the generic one is shown
	used uint64
}

type iconResult struct {
	path string
	icon win.HICON
}

const iconCapacity = 512

var procIcons = newProcIconCache(iconCapacity)

func newProcIconCache(capacity int) *procIconCache {
	return &procIconCache{
		capacity: capacity,
		slots:    map[string]*list.Element{},
		lru:      list.New(),
		pend:     map[string]bool{},
		ch:       make(chan string, 1024),
		extract:  shellSmallIcon,
	}
}

// imageList creates the shared list on first use, with the stock
// application icon in slot 0.
func (c *procIconCache) imageList() win.HIMAGELIST {
	if c.himl == 0 {
		cx := win.GetSystemMetrics(win.SM_CXSMICON)
		cy := win.GetSystemMetrics(win.SM_CYSMICON)
		c.himl = win.ImageList_Create(cx, cy, win.ILC_COLOR32|win.ILC_MASK, 32, 32)
		// The small stock application icon (a shared icon: never destroyed).
		generic := win.HICON(win.LoadImage(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION), win.IMAGE_ICON, cx, cy, win.LR_SHARED))
		if generic == 0 {
			generic = win.LoadIcon(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION))
		}
		win.ImageList_ReplaceIcon(c.himl, -1, generic)
	}
	return c.himl
}

// attach shows the shared image list in a table's native list views. post
// marshals a function onto the UI thread; resolved repaints changed rows.
func (c *procIconCache) attach(lvs []win.HWND, post func(func()) bool, resolved func()) {
	h := c.imageList()
	for _, lv := range lvs {
		// The list must outlive any one table: without this style a list view
		// destroys its image list when the view is destroyed.
		style := win.GetWindowLong(lv, win.GWL_STYLE)
		if style&win.LVS_SHAREIMAGELISTS == 0 {
			win.SetWindowLong(lv, win.GWL_STYLE, style|win.LVS_SHAREIMAGELISTS)
		}
		win.SendMessage(lv, win.LVM_SETIMAGELIST, win.LVSIL_SMALL, uintptr(h))
		c.views = append(c.views, lv)
	}
	c.mu.Lock()
	c.post, c.resolved = post, resolved
	pending := len(c.done) > 0 && !c.queued && post != nil
	if pending {
		c.queued = true
	}
	c.mu.Unlock()
	if pending && !post(c.flush) {
		c.mu.Lock()
		c.queued = false
		c.mu.Unlock()
	}
}

// touchGeneration starts a new paint generation; slots looked up during it
// count as possibly visible and are never reused before the next one.
func (c *procIconCache) touchGeneration() { c.gen++ }

// slot returns the image-list index for path: its own icon, or 0 (generic)
// while it is being extracted or when it has none. A miss queues extraction.
// UI thread only.
func (c *procIconCache) slot(p string) int32 {
	if p == "" {
		return 0
	}
	if el := c.slots[p]; el != nil {
		e := el.Value.(*iconEntry)
		e.used = c.gen
		c.lru.MoveToFront(el)
		return e.slot
	}
	c.enqueue(p)
	return 0
}

// enqueue asks the worker to resolve p. Cheap and idempotent; duplicates and
// overflow collapse (a later lookup re-enqueues).
func (c *procIconCache) enqueue(p string) {
	c.start.Do(func() { go c.worker() })
	c.mu.Lock()
	if c.pend[p] {
		c.mu.Unlock()
		return
	}
	c.pend[p] = true
	c.mu.Unlock()
	select {
	case c.ch <- p:
	default:
		c.mu.Lock()
		delete(c.pend, p)
		c.mu.Unlock()
	}
}

// worker extracts icons on one locked, COM-initialized thread: the shell
// wants an STA thread, and a pathological path (a dead network share, a
// sleeping disk spinning up, an antivirus filtering every read) must block
// only this goroutine, never the UI. Results are handed to the UI thread in
// batches: one posted flush, however many icons finish meanwhile.
func (c *procIconCache) worker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	for p := range c.ch {
		c.deliver(iconResult{path: p, icon: c.extract(p)})
	}
}

func (c *procIconCache) deliver(r iconResult) {
	c.mu.Lock()
	c.done = append(c.done, r)
	post := c.post
	schedule := !c.queued && post != nil
	if schedule {
		c.queued = true
	}
	c.mu.Unlock()
	if schedule && !post(c.flush) {
		c.mu.Lock()
		c.queued = false
		c.mu.Unlock()
	}
}

func shellSmallIcon(p string) win.HICON {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0
	}
	var shfi win.SHFILEINFO
	if win.SHGetFileInfo(name, 0, &shfi, uint32(unsafe.Sizeof(shfi)), win.SHGFI_ICON|win.SHGFI_SMALLICON) == 0 {
		return 0
	}
	return shfi.HIcon
}

// flush moves finished icons into the image list (UI thread) and asks the
// table to repaint the rows whose icon changed.
func (c *procIconCache) flush() {
	c.mu.Lock()
	done := c.done
	c.done = nil
	c.queued = false
	resolved := c.resolved
	for _, r := range done {
		delete(c.pend, r.path)
	}
	c.mu.Unlock()
	if len(done) == 0 {
		return
	}
	for _, r := range done {
		c.store(r.path, r.icon)
	}
	if resolved != nil {
		resolved()
	}
}

// store records path's icon (0: none, keep the generic one), copying it into
// the image list and releasing the extracted HICON.
func (c *procIconCache) store(p string, icon win.HICON) {
	slot := int32(0)
	if icon != 0 {
		h := c.imageList()
		if n := len(c.free); n > 0 {
			slot = c.free[n-1]
			c.free = c.free[:n-1]
			if win.ImageList_ReplaceIcon(h, slot, icon) < 0 {
				c.free = append(c.free, slot)
				slot = 0
			}
		} else if slot = win.ImageList_ReplaceIcon(h, -1, icon); slot < 0 {
			slot = 0
		}
		win.DestroyIcon(icon) // the image list keeps its own copy
	}
	if el := c.slots[p]; el != nil { // re-extracted after a lost request
		e := el.Value.(*iconEntry)
		if e.slot != 0 {
			c.free = append(c.free, e.slot)
		}
		e.slot = slot
		c.lru.MoveToFront(el)
	} else {
		c.slots[p] = c.lru.PushFront(&iconEntry{path: p, slot: slot, used: c.gen})
	}
	c.evict()
}

// evict forgets least recently displayed paths beyond capacity, freeing
// their slots for reuse. Paths used in the current paint generation stay.
func (c *procIconCache) evict() {
	for c.lru.Len() > c.capacity {
		el := c.lru.Back()
		e := el.Value.(*iconEntry)
		if e.used == c.gen {
			return // possibly on screen right now
		}
		c.lru.Remove(el)
		delete(c.slots, e.path)
		if e.slot != 0 {
			c.free = append(c.free, e.slot)
		}
	}
}

// release detaches the image list from a list view about to be destroyed.
func (c *procIconCache) release(lv win.HWND) {
	for i, v := range c.views {
		if v == lv {
			win.SendMessage(lv, win.LVM_SETIMAGELIST, win.LVSIL_SMALL, 0)
			c.views = append(c.views[:i], c.views[i+1:]...)
			return
		}
	}
}
