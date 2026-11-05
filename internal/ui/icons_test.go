//go:build windows

package ui

import (
	"os"
	"testing"

	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

var procImageListCount = windows.NewLazySystemDLL("comctl32.dll").NewProc("ImageList_GetImageCount")

func imageCount(h win.HIMAGELIST) int {
	n, _, _ := procImageListCount.Call(uintptr(h))
	return int(n)
}

func TestIconCacheEvictsLeastRecentlyShownAndReusesSlots(t *testing.T) {
	runNativeTest(t, func() {
		exe, err := os.Executable()
		if err != nil {
			panic(err)
		}
		c := newProcIconCache(3)
		c.extract = func(string) win.HICON { return shellSmallIcon(exe) }
		defer win.ImageList_Destroy(c.imageList())
		icon := func() win.HICON {
			h := shellSmallIcon(exe)
			if h == 0 {
				panic("no test icon")
			}
			return h
		}
		c.touchGeneration()
		for _, p := range []string{"a", "b", "c"} {
			c.store(p, icon())
		}
		if c.slot("a") == 0 || c.slot("b") == 0 || c.slot("c") == 0 {
			t.Error("resolved icons have no slot")
			return
		}
		c.touchGeneration()
		c.slot("a") // a is on screen; b and c are not
		c.store("d", icon())
		if _, ok := c.slots["b"]; ok {
			t.Error("least recently shown path was kept")
		}
		if _, ok := c.slots["a"]; !ok {
			t.Error("a path shown in this paint was evicted")
		}
		c.store("e", icon()) // reuses b's freed slot
		if n := imageCount(c.himl); n > 1+3+1 {
			t.Errorf("image list grew to %d images for capacity 3", n)
		}
		// Every remaining path is on screen: none may lose its slot.
		c.touchGeneration()
		shown := map[string]int32{}
		for p := range c.slots {
			shown[p] = c.slot(p)
		}
		c.store("f", icon())
		for p, s := range shown {
			if el := c.slots[p]; el == nil || el.Value.(*iconEntry).slot != s {
				t.Errorf("visible %s lost or changed its slot", p)
			}
		}
		// A path without an icon keeps the generic one and is not retried.
		c.store("noicon", 0)
		if c.slot("noicon") != 0 {
			t.Error("missing icon got a slot")
		}
		c.mu.Lock()
		retried := c.pend["noicon"]
		c.mu.Unlock()
		if retried {
			t.Error("failed path re-queued on every lookup")
		}
	})
}

func TestIconResultsCoalesceIntoOneRepaint(t *testing.T) {
	runNativeTest(t, func() {
		exe, _ := os.Executable()
		c := newProcIconCache(8)
		defer win.ImageList_Destroy(c.imageList())
		var posted []func()
		repaints := 0
		c.attach(nil, func(f func()) bool { posted = append(posted, f); return true }, func() { repaints++ })
		for _, p := range []string{"x", "y", "z"} {
			c.deliver(iconResult{path: p, icon: shellSmallIcon(exe)})
		}
		if len(posted) != 1 {
			t.Errorf("%d flushes posted for 3 icons, want 1", len(posted))
			return
		}
		posted[0]()
		if repaints != 1 || c.slot("x") == 0 || c.slot("z") == 0 {
			t.Errorf("flush: %d repaints, slots %d %d", repaints, c.slot("x"), c.slot("z"))
		}
		c.deliver(iconResult{path: "w"})
		if len(posted) != 2 {
			t.Error("a later icon was not posted")
		}
	})
}
