//go:build windows

package ui

import (
	"runtime"
	"sync"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// procIconCache turns process image paths into walk Icons on a background
// thread.
//
// A file path handed to Walk's table goes through SHGetFileInfo ON THE UI
// THREAD — not just the first time (cold shell icon cache, icon-handler
// extensions loading, antivirus scanning each read: seconds across a whole
// process list) but even later, because a cache hit still parses the file.
// On a sleeping disk that "warm" call blocks for seconds and Windows flags
// the window "not responding". Rows therefore show the generic program icon
// until a worker thread has extracted the real one; Image() then returns a
// *walk.Icon, which Walk maps through its own in-memory image list. The UI
// thread performs no file or shell access for icons, cold or warm.
type procIconCache struct {
	mu    sync.RWMutex
	icons map[string]*walk.Icon
	pend  map[string]bool
	ch    chan string
	start sync.Once
}

var procIcons = &procIconCache{
	icons: map[string]*walk.Icon{},
	pend:  map[string]bool{},
	ch:    make(chan string, 1024),
}

// icon returns the resolved icon for p, or nil while it is still pending.
func (c *procIconCache) icon(p string) *walk.Icon {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.icons[p]
}

// enqueue asks the worker to resolve p. Cheap and idempotent; duplicates and
// overflow collapse (a later refresh re-enqueues).
func (c *procIconCache) enqueue(p string) {
	c.start.Do(func() { go c.worker() })
	c.mu.Lock()
	if _, done := c.icons[p]; done || c.pend[p] {
		c.mu.Unlock()
		return
	}
	c.pend[p] = true
	c.mu.Unlock()
	select {
	case c.ch <- p:
	default:
		c.mu.Lock()
		c.pend[p] = false
		c.mu.Unlock()
	}
}

// worker extracts icons on one locked, COM-initialized thread: the shell
// wants an STA thread, and a pathological path (a dead network share, a
// sleeping disk spinning up, an antivirus filtering every read) must block
// only this goroutine, never the UI.
func (c *procIconCache) worker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	for p := range c.ch {
		var shfi win.SHFILEINFO
		icon := (*walk.Icon)(nil)
		if name, err := windows.UTF16PtrFromString(p); err == nil {
			if win.SHGetFileInfo(name, 0, &shfi, uint32(unsafe.Sizeof(shfi)),
				win.SHGFI_ICON|win.SHGFI_SMALLICON) != 0 && shfi.HIcon != 0 {
				if ic, err := walk.NewIconFromHICON(shfi.HIcon); err == nil {
					icon = ic // walk's Icon takes ownership of the HICON; at
					// 96 dpi (this build) the table needs no scaled variant
				} else {
					win.DestroyIcon(shfi.HIcon)
				}
			}
		}
		c.mu.Lock()
		c.icons[p] = icon // nil on failure: keep the generic icon for this path
		c.mu.Unlock()
	}
}
