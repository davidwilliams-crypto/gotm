//go:build windows

package ui

import (
	"testing"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
	"golang.org/x/sys/windows"
)

// The embedded Geist fonts must register, GDI must hand back those faces and
// not a substitute, and every digit must be the same width in both weights.
func TestGeistLoadsWithTabularFigures(t *testing.T) {
	runNativeTest(t, func() {
		if uiFamily() != "Geist" || uiStrongFamily() != "Geist SemiBold" {
			t.Fatalf("families = %q, %q: the embedded fonts did not register", uiFamily(), uiStrongFamily())
		}
		bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 200, Height: 40}, 96)
		if err != nil {
			panic(err)
		}
		defer bmp.Dispose()
		cv, err := walk.NewCanvasFromImage(bmp)
		if err != nil {
			panic(err)
		}
		defer cv.Dispose()
		getTextFace := windows.NewLazySystemDLL("gdi32.dll").NewProc("GetTextFaceW")
		for _, f := range []*walk.Font{uiFont(10), uiSemibold(10)} {
			hf := measureFont(f, 96)
			old := win.SelectObject(cv.HDC(), win.HGDIOBJ(hf))
			var name [64]uint16
			getTextFace.Call(uintptr(cv.HDC()), 64, uintptr(unsafe.Pointer(&name[0])))
			win.SelectObject(cv.HDC(), old)
			if got := windows.UTF16ToString(name[:]); got != f.Family() {
				t.Errorf("GDI selected %q for %q", got, f.Family())
			}
			want := textWidth(cv, "0000", f)
			for _, s := range []string{"1111", "4444", "7777", "9999"} {
				if got := textWidth(cv, s, f); got != want {
					t.Errorf("%s: %q is %d wide, %q is %d", f.Family(), s, got, "0000", want)
				}
			}
		}
	})
}
