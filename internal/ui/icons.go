//go:build windows

package ui

import (
	"container/list"
	"image"
	"image/color"
	"runtime"
	"sync"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"

	"gotm/internal/art"
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
		// Slot 0 is gotm's own placeholder, drawn for exactly this size in
		// the muted ink. Windows' stock application icon (a 1990s window)
		// is only the fallback if the bitmap cannot be made.
		if hbm := genericAppBitmap(int(cx), int(cy)); hbm != 0 {
			added := win.ImageList_Add(c.himl, hbm, 0)
			win.DeleteObject(win.HGDIOBJ(hbm)) // the image list keeps its own copy
			if added == 0 {
				return c.himl
			}
		}
		generic := win.HICON(win.LoadImage(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION), win.IMAGE_ICON, cx, cy, win.LR_SHARED))
		if generic == 0 {
			generic = win.LoadIcon(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION))
		}
		win.ImageList_ReplaceIcon(c.himl, -1, generic)
	}
	return c.himl
}

var (
	procImageListReplace     = windows.NewLazySystemDLL("comctl32.dll").NewProc("ImageList_Replace")
	procImageListGetIconSize = windows.NewLazySystemDLL("comctl32.dll").NewProc("ImageList_GetIconSize")
)

// retheme redraws the placeholder in the current palette's ink (the muted
// ink of the dark theme is too faint on a high-contrast white window).
func (c *procIconCache) retheme() {
	if c.himl == 0 {
		return
	}
	var cx, cy int32
	if r, _, _ := procImageListGetIconSize.Call(uintptr(c.himl), uintptr(unsafe.Pointer(&cx)), uintptr(unsafe.Pointer(&cy))); r == 0 {
		return
	}
	if hbm := genericAppBitmap(int(cx), int(cy)); hbm != 0 {
		procImageListReplace.Call(uintptr(c.himl), 0, uintptr(hbm), 0)
		win.DeleteObject(win.HGDIOBJ(hbm))
	}
}

// genericAppBitmap renders the placeholder icon in the muted ink. The caller
// deletes it.
func genericAppBitmap(w, h int) win.HBITMAP {
	if w <= 0 || h <= 0 || w != h {
		return 0
	}
	return rgbaBitmap(art.GenericApp(w, nrgba(T.Muted)))
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

// deliver hands a worker result to the UI thread. When no table can receive
// it (none attached, or the window is gone and the flush cannot be posted),
// the result is dropped and its HICON destroyed at once rather than held
// until the process exits; a later lookup simply asks again.
func (c *procIconCache) deliver(r iconResult) {
	c.mu.Lock()
	post := c.post
	if post == nil {
		delete(c.pend, r.path)
		c.mu.Unlock()
		destroyResults([]iconResult{r})
		return
	}
	c.done = append(c.done, r)
	schedule := !c.queued
	if schedule {
		c.queued = true
	}
	c.mu.Unlock()
	if schedule && !post(c.flush) {
		destroyResults(c.drop())
	}
}

// drop takes every result waiting for a flush and forgets that a flush is
// posted, under mu. The caller destroys their icons.
func (c *procIconCache) drop() []iconResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	done := c.done
	c.done = nil
	c.queued = false
	for _, r := range done {
		delete(c.pend, r.path)
	}
	return done
}

// destroyResults releases the HICONs of results that will never be stored.
func destroyResults(rs []iconResult) {
	for _, r := range rs {
		if r.icon != 0 {
			win.DestroyIcon(r.icon)
		}
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
	// A program without an icon of its own gets the shell's stock "program"
	// picture. gotm shows its own placeholder for those instead.
	if generic := genericExeIndex(); generic >= 0 && shfi.IIcon == generic {
		if shfi.HIcon != 0 {
			win.DestroyIcon(shfi.HIcon)
		}
		return 0
	}
	return shfi.HIcon
}

// genericExeIndex is the system image list index of the icon the shell gives
// to an .exe that has none (asked by file type, so no file is touched), or -1.
var genericExeIndex = sync.OnceValue(func() int32 {
	name, err := windows.UTF16PtrFromString("program.exe")
	if err != nil {
		return -1
	}
	const fileAttributeNormal = 0x80
	var shfi win.SHFILEINFO
	if win.SHGetFileInfo(name, fileAttributeNormal, &shfi, uint32(unsafe.Sizeof(shfi)),
		win.SHGFI_SYSICONINDEX|win.SHGFI_SMALLICON|win.SHGFI_USEFILEATTRIBUTES) == 0 {
		return -1
	}
	return shfi.IIcon
})

// rgbaBitmap copies an image into a 32-bit premultiplied DIB, the form an
// ILC_COLOR32 image list takes without a mask. The caller deletes it.
func rgbaBitmap(img *image.NRGBA) win.HBITMAP {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	bi := win.BITMAPINFOHEADER{BiWidth: int32(w), BiHeight: -int32(h), BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	bi.BiSize = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	hbm := win.CreateDIBSection(0, &bi, win.DIB_RGB_COLORS, &bits, 0, 0)
	if hbm == 0 || bits == nil {
		return 0
	}
	dst := unsafe.Slice((*byte)(bits), w*h*4)
	for i := 0; i < w*h; i++ {
		r, g, b, a := uint32(img.Pix[i*4]), uint32(img.Pix[i*4+1]), uint32(img.Pix[i*4+2]), uint32(img.Pix[i*4+3])
		dst[i*4+0] = byte(b * a / 255)
		dst[i*4+1] = byte(g * a / 255)
		dst[i*4+2] = byte(r * a / 255)
		dst[i*4+3] = byte(a)
	}
	return hbm
}

func nrgba(c walk.Color) color.NRGBA { return color.NRGBA{c.R(), c.G(), c.B(), 0xff} }

// styleChecks replaces a list view's stock check boxes (white squares on any
// theme) with Meridian ones in the current palette. The list view owns the
// new image list; the one it had is destroyed here.
func styleChecks(lv win.HWND) {
	if win.SendMessage(lv, win.LVM_GETEXTENDEDLISTVIEWSTYLE, 0, 0)&win.LVS_EX_CHECKBOXES == 0 {
		return
	}
	cx, cy := win.GetSystemMetrics(win.SM_CXSMICON), win.GetSystemMetrics(win.SM_CYSMICON)
	if old := win.SendMessage(lv, 0x1002 /* LVM_GETIMAGELIST */, win.LVSIL_STATE, 0); old != 0 {
		procImageListGetIconSize.Call(old, uintptr(unsafe.Pointer(&cx)), uintptr(unsafe.Pointer(&cy)))
	}
	if cx <= 0 || cx != cy {
		return
	}
	himl := win.ImageList_Create(cx, cy, win.ILC_COLOR32, 2, 0)
	if himl == 0 {
		return
	}
	for _, checked := range []bool{false, true} {
		hbm := rgbaBitmap(art.Checkbox(int(cx), checked, nrgba(T.Stroke), nrgba(T.Accent), nrgba(T.OnAccent)))
		added := int32(-1)
		if hbm != 0 {
			added = win.ImageList_Add(himl, hbm, 0)
			win.DeleteObject(win.HGDIOBJ(hbm))
		}
		if added < 0 {
			win.ImageList_Destroy(himl)
			return
		}
	}
	if old := win.SendMessage(lv, win.LVM_SETIMAGELIST, win.LVSIL_STATE, uintptr(himl)); old != 0 {
		win.ImageList_Destroy(win.HIMAGELIST(old))
	}
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
// When the last one goes (the window is closing), the cache also stops
// accepting results: a flush posted to the closing window may never run, so
// the results waiting for it, and any that arrive later, have their HICONs
// destroyed instead of being held until the process exits.
func (c *procIconCache) release(lv win.HWND) {
	for i, v := range c.views {
		if v == lv {
			win.SendMessage(lv, win.LVM_SETIMAGELIST, win.LVSIL_SMALL, 0)
			c.views = append(c.views[:i], c.views[i+1:]...)
			if len(c.views) == 0 {
				c.mu.Lock()
				c.post, c.resolved = nil, nil
				c.mu.Unlock()
				destroyResults(c.drop())
			}
			return
		}
	}
}
