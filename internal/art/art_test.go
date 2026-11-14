package art

import (
	"image/color"
	"testing"
)

// Every icon size is drawn for itself: the tile must be solid accent near
// the top, transparent in the corners, and carry the white bar.
func TestAppIconAtEverySize(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32, 40, 48, 64, 256} {
		img := AppIcon(size)
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Fatalf("size %d: bounds %v", size, b)
		}
		if a := img.NRGBAAt(0, 0).A; a != 0 {
			t.Errorf("size %d: corner alpha %d, want a rounded (transparent) corner", size, a)
		}
		top := img.NRGBAAt(size/2, size/8+1)
		if top != (color.NRGBA{0x21, 0x52, 0xe5, 0xff}) {
			t.Errorf("size %d: tile colour %v, want the Meridian accent", size, top)
		}
		whites := 0
		for i := 0; i < len(img.Pix); i += 4 {
			if img.Pix[i] == 255 && img.Pix[i+1] == 255 && img.Pix[i+2] == 255 && img.Pix[i+3] == 255 {
				whites++
			}
		}
		if whites < size/2 {
			t.Errorf("size %d: only %d fully white pixels, the white bar is missing or washed out", size, whites)
		}
	}
}

func TestGenericAppUsesTheInk(t *testing.T) {
	ink := color.NRGBA{0x9a, 0xa6, 0xc1, 0xff}
	for _, size := range []int{16, 20, 24, 32} {
		img := GenericApp(size, ink)
		if img.NRGBAAt(size/2, size/2).A != 0 {
			t.Errorf("size %d: the window body should be empty", size)
		}
		solid := 0
		for i := 0; i < len(img.Pix); i += 4 {
			if img.Pix[i+3] == 255 {
				if (color.NRGBA{img.Pix[i], img.Pix[i+1], img.Pix[i+2], 255}) != ink {
					t.Fatalf("size %d: opaque pixel is not the ink", size)
				}
				solid++
			}
		}
		if solid < size*2 {
			t.Errorf("size %d: only %d opaque pixels", size, solid)
		}
	}
}
