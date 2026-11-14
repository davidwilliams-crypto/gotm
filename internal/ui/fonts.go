//go:build windows

package ui

import (
	_ "embed"
	"sync"
	"unsafe"
)

// Meridian sets everything in Geist, at weights 400 and 600. Geist is not a
// Windows font, so both weights are embedded (internal/ui/fonts, SIL OFL 1.1)
// and registered for this process only: nothing is installed on the PC and no
// other program sees them. If Windows refuses a font, the type falls back to
// Segoe UI, the next family in Meridian's own font stack.

//go:embed fonts/Geist-Regular.ttf
var geistRegular []byte

//go:embed fonts/Geist-SemiBold.ttf
var geistSemiBold []byte

var procAddFontMemResourceEx = gdi32.NewProc("AddFontMemResourceEx")

var (
	familyOnce               sync.Once
	familyText, familyStrong string
)

// addMemoryFont registers a font file held in memory with GDI for this
// process. The handle is kept for the life of the process.
func addMemoryFont(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var count uint32
	h, _, _ := procAddFontMemResourceEx.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0, uintptr(unsafe.Pointer(&count)))
	return h != 0 && count > 0
}

func loadFamilies() {
	familyOnce.Do(func() {
		familyText, familyStrong = "Segoe UI", "Segoe UI Semibold"
		if addMemoryFont(geistRegular) && addMemoryFont(geistSemiBold) {
			familyText, familyStrong = "Geist", "Geist SemiBold"
		}
	})
}

// uiFamily is the family of body text (weight 400).
func uiFamily() string { loadFamilies(); return familyText }

// uiStrongFamily is the family of titles, labels and numerals (weight 600).
func uiStrongFamily() string { loadFamilies(); return familyStrong }
