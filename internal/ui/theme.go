//go:build windows

package ui

import (
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/tailscale/walk"
	"github.com/tailscale/win"
)

// palette holds every colour gotm paints with. Channel colours identify a
// resource on every page (a card, its graph and its legend always share one
// hue). There are two palettes: gotm's own dark theme, and one built from the
// system colours while Windows' high-contrast mode is on.
type palette struct {
	Bg, Side, Card, CardHi, CardSel, Stroke, Grid walk.Color
	Text, Muted, Faint, Accent, AccentText        walk.Color
	Good, Warn, Bad                               walk.Color
	CPU, Mem, GPU, Disk, Net, Power, Thermal      walk.Color
	Clock                                         walk.Color // a calm sky blue: clocks are information, not a warning
	RowAlt, RowSel, RowSelText, TableBG           walk.Color
}

var darkPalette = palette{
	Bg:         walk.RGB(0x18, 0x1A, 0x1D),
	Side:       walk.RGB(0x10, 0x12, 0x14),
	Card:       walk.RGB(0x20, 0x23, 0x27),
	CardHi:     walk.RGB(0x29, 0x2E, 0x34),
	CardSel:    walk.RGB(0x22, 0x36, 0x4B),
	Stroke:     walk.RGB(0x36, 0x3B, 0x42),
	Grid:       walk.RGB(0x32, 0x38, 0x3E),
	Text:       walk.RGB(0xF0, 0xF2, 0xF4),
	Muted:      walk.RGB(0xBA, 0xC1, 0xC9),
	Faint:      walk.RGB(0x96, 0x9F, 0xA9),
	Accent:     walk.RGB(0x82, 0xB7, 0xFF),
	AccentText: walk.RGB(0x10, 0x18, 0x21),
	Good:       walk.RGB(0x77, 0xCE, 0xA0),
	Warn:       walk.RGB(0xF2, 0xBF, 0x70),
	Bad:        walk.RGB(0xFF, 0x92, 0x94),
	CPU:        walk.RGB(0x83, 0xB5, 0xFF),
	Mem:        walk.RGB(0xC4, 0x9C, 0xF0),
	GPU:        walk.RGB(0x6A, 0xD5, 0xC5),
	Disk:       walk.RGB(0x80, 0xCA, 0x94),
	Net:        walk.RGB(0xE5, 0xBB, 0x83),
	Power:      walk.RGB(0xE9, 0xCF, 0x86),
	Thermal:    walk.RGB(0xED, 0xA7, 0x85),
	Clock:      walk.RGB(0x93, 0xCD, 0xE4),
	RowAlt:     walk.RGB(0x1D, 0x20, 0x24),
	RowSel:     walk.RGB(0x31, 0x4F, 0x70),
	RowSelText: walk.RGB(0xFF, 0xFF, 0xFF),
	TableBG:    walk.RGB(0x19, 0x1B, 0x1E),
}

// T is the palette in use. Everything reads it at paint time, so swapping it
// and repainting is enough to re-theme the owner-drawn surfaces.
var T = darkPalette

// highContrast is true while the high-contrast palette is active.
var highContrast bool

// highContrastOn asks Windows whether a high-contrast theme is active.
// GOTM_HIGH_CONTRAST=1 forces the system-colour palette for testing.
func highContrastOn() bool {
	if os.Getenv("GOTM_HIGH_CONTRAST") != "" {
		return true
	}
	var hc struct {
		Size   uint32
		Flags  uint32
		Scheme *uint16
	}
	hc.Size = uint32(unsafe.Sizeof(hc))
	if r, _, _ := procSystemParametersInfo.Call(win.SPI_GETHIGHCONTRAST, uintptr(hc.Size), uintptr(unsafe.Pointer(&hc)), 0); r == 0 {
		return false
	}
	return hc.Flags&1 != 0 // HCF_HIGHCONTRASTON
}

// applyTheme selects the palette for the current system state and reports
// whether the mode changed. It runs before the first window is built and
// again on WM_SYSCOLORCHANGE / WM_THEMECHANGED.
func applyTheme() bool {
	hc := highContrastOn()
	if hc {
		T = highContrastPalette()
	} else {
		T = darkPalette
	}
	changed := hc != highContrast
	highContrast = hc
	return changed
}

const (
	colorWindow        = 5
	colorWindowText    = 8
	colorHighlight     = 13
	colorHighlightText = 14
	colorBtnFace       = 15
	colorGrayText      = 17
)

func sysColor(i int) walk.Color {
	c := win.GetSysColor(i)
	return walk.RGB(byte(c), byte(c>>8), byte(c>>16))
}

// luma is the relative brightness of a colour, 0 (black) to 1 (white).
func luma(c walk.Color) float64 {
	return (0.2126*float64(c.R()) + 0.7152*float64(c.G()) + 0.0722*float64(c.B())) / 255
}

// highContrastPalette builds gotm's palette from the user's high-contrast
// theme: their window, text, selection and disabled-text colours carry the
// meaning they have everywhere else, outlines use the text colour so every
// card and separator is visible, and the resource hues are darkened or
// lightened until they stand out from the chosen background.
func highContrastPalette() palette {
	bg := sysColor(colorWindow)
	txt := sysColor(colorWindowText)
	hi := sysColor(colorHighlight)
	hiT := sysColor(colorHighlightText)
	gray := sysColor(colorGrayText)
	side := sysColor(colorBtnFace)
	// Resource colours also carry small numeric labels. Keep their hues
	// identifiable while giving each a readable contrast on the user's window.
	bgLum := luminance(bg)
	toward := walk.RGB(0, 0, 0)
	if bgLum < 0.18 {
		toward = walk.RGB(255, 255, 255)
	}
	vivid := func(c walk.Color) walk.Color {
		for i := 0; i < 24; i++ {
			ink := luminance(c)
			if (max(ink, bgLum)+0.05)/(min(ink, bgLum)+0.05) >= 4.5 {
				break
			}
			c = blend(toward, c, 0.16)
		}
		return c
	}
	p := darkPalette
	p.Bg, p.Side, p.Card = bg, side, bg
	p.CardHi, p.CardSel = blend(hi, bg, 0.22), blend(hi, bg, 0.45)
	p.Stroke, p.Grid = txt, gray
	p.Text, p.Muted, p.Faint = txt, txt, gray
	p.Accent, p.AccentText = hi, hiT
	p.Good, p.Warn, p.Bad = vivid(darkPalette.Good), vivid(darkPalette.Warn), vivid(darkPalette.Bad)
	p.CPU, p.Mem, p.GPU = vivid(darkPalette.CPU), vivid(darkPalette.Mem), vivid(darkPalette.GPU)
	p.Disk, p.Net = vivid(darkPalette.Disk), vivid(darkPalette.Net)
	p.Power, p.Thermal, p.Clock = vivid(darkPalette.Power), vivid(darkPalette.Thermal), vivid(darkPalette.Clock)
	p.RowAlt = blend(txt, bg, 0.06)
	p.RowSel, p.RowSelText, p.TableBG = hi, hiT, bg
	return p
}

func luminance(c walk.Color) float64 {
	linear := func(channel byte) float64 {
		v := float64(channel) / 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.R()) + 0.7152*linear(c.G()) + 0.0722*linear(c.B())
}

// blend mixes fg over bg at alpha a (0..1); GDI has no alpha, so tints are
// pre-composited against the known surface colour.
func blend(fg, bg walk.Color, a float64) walk.Color {
	m := func(f, b byte) byte { return byte(float64(b) + (float64(f)-float64(b))*a + 0.5) }
	return walk.RGB(m(fg.R(), bg.R()), m(fg.G(), bg.G()), m(fg.B(), bg.B()))
}

// ---- fonts ------------------------------------------------------------------

type fontKey struct {
	family string
	pt     int
	style  walk.FontStyle
}

var (
	fontMu    sync.Mutex
	fontCache = map[fontKey]*walk.Font{}
)

const uiFamily = "Segoe UI"

func fontOf(family string, pt int, style walk.FontStyle) *walk.Font {
	k := fontKey{family, pt, style}
	fontMu.Lock()
	defer fontMu.Unlock()
	if f, ok := fontCache[k]; ok {
		return f
	}
	f, err := walk.NewFont(family, pt, style)
	if err != nil {
		f, _ = walk.NewFont(uiFamily, pt, style)
	}
	fontCache[k] = f
	return f
}

func uiFont(pt int) *walk.Font     { return fontOf(uiFamily, pt, 0) }
func uiSemibold(pt int) *walk.Font { return fontOf("Segoe UI Semibold", pt, 0) }
func uiBold(pt int) *walk.Font     { return fontOf(uiFamily, pt, walk.FontBold) }
func uiLight(pt int) *walk.Font    { return fontOf("Segoe UI Light", pt, 0) }
func iconFont(pt int) *walk.Font   { return fontOf(iconFamily(), pt, 0) }
func monoFont(pt int) *walk.Font   { return fontOf("Cascadia Mono", pt, 0) }

var (
	iconOnce sync.Once
	iconFam  string
)

// iconFamily prefers Segoe Fluent Icons (Windows 11), falling back to MDL2.
func iconFamily() string {
	iconOnce.Do(func() {
		iconFam = "Segoe MDL2 Assets"
		if f, err := walk.NewFont("Segoe Fluent Icons", 12, 0); err == nil && f != nil {
			// NewFont succeeds for missing families, so probe the registry key list instead
			if fontInstalled("Segoe Fluent Icons") {
				iconFam = "Segoe Fluent Icons"
			}
			f.Dispose() // probe only; one font, once
		}
	})
	return iconFam
}

// ---- title bar and dark mode plumbing -------------------------------------------

const (
	dwmaUseImmersiveDarkMode = 20
	dwmaCaptionColor         = 35
	dwmaTextColor            = 36
	dwmaColorDefault         = 0xFFFFFFFF
)

// titleBar asks DWM for a caption that matches the palette: dark to match the
// sidebar, or the system's own caption in high-contrast mode.
func titleBar(hwnd win.HWND) {
	on := int32(0)
	if luma(T.Side) < 0.5 {
		on = 1
	}
	win.DwmSetWindowAttribute(hwnd, win.DWMWINDOWATTRIBUTE(dwmaUseImmersiveDarkMode), unsafe.Pointer(&on), 4)
	col, txt := uint32(dwmaColorDefault), uint32(dwmaColorDefault)
	if !highContrast {
		col = uint32(T.Side.R()) | uint32(T.Side.G())<<8 | uint32(T.Side.B())<<16
		txt = uint32(T.Text.R()) | uint32(T.Text.G())<<8 | uint32(T.Text.B())<<16
	}
	win.DwmSetWindowAttribute(hwnd, win.DWMWINDOWATTRIBUTE(dwmaCaptionColor), unsafe.Pointer(&col), 4)
	win.DwmSetWindowAttribute(hwnd, win.DWMWINDOWATTRIBUTE(dwmaTextColor), unsafe.Pointer(&txt), 4)
}

// darkControl switches a native control (edit, list view, scrollbar…) to the
// system dark visual style. In high-contrast mode the system style stays.
func darkControl(hwnd win.HWND, class string) {
	if highContrast {
		return
	}
	if class == "" {
		class = "DarkMode_Explorer"
	}
	p, err := syscallUTF16(class)
	if err != nil {
		return
	}
	win.SetWindowTheme(hwnd, p, nil)
}
