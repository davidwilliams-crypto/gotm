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

// palette holds every colour gotm paints with. The values are the dark theme
// of the Meridian design system; each field names the Meridian token it
// carries. There are two palettes: Meridian dark, and one built from the
// system colours while Windows' high-contrast mode is on.
//
// The resource channel colours are gotm's own extension: Meridian has a single
// accent, but a card, its graph and its legend must share one hue per resource
// and the recorder plots several series at once. CPU, the lead resource, takes
// the accent; the others are calm hues of similar lightness.
type palette struct {
	Bg, Side                                 walk.Color // canvas: the page ground and the sidebar
	Card                                     walk.Color // surface: every card and control ground
	Sunken                                   walk.Color // surface-sunken: inset tracks, unselected segments
	CardHi                                   walk.Color // hover on a surface (an app token: ink at 6% over surface)
	Tint                                     walk.Color // surface-tint: selected rows and cards, progress tracks
	Edge                                     walk.Color // shadow-card in dark: the faint outline of a resting card
	Line                                     walk.Color // border: decorative hairlines only
	Stroke                                   walk.Color // border-strong: control outlines that must be seen
	Grid                                     walk.Color // graph grid lines (border)
	Text, Muted                              walk.Color // ink, ink-muted
	Disabled                                 walk.Color // ink-muted at 45% over surface
	Accent                                   walk.Color // accent: fills only
	AccentHover                              walk.Color // accent-hover
	OnAccent                                 walk.Color // on-accent: text and icons on an accent fill
	AccentInk                                walk.Color // accent-text: accent-coloured text and glyphs
	Focus                                    walk.Color // focus: the keyboard focus ring
	Good, Warn, Bad                          walk.Color // success, warning, danger
	GoodTint, WarnTint, BadTint              walk.Color // their badge grounds
	CPU, Mem, GPU, Disk, Net, Power, Thermal walk.Color
	Clock                                    walk.Color // a calm sky blue: clocks are information, not a warning
	RowAlt, RowSel, RowSelText, TableBG      walk.Color
}

func hex(v uint32) walk.Color { return walk.RGB(byte(v>>16), byte(v>>8), byte(v)) }

var darkPalette = meridianDark()

func meridianDark() palette {
	canvas, surface := hex(0x070b16), hex(0x10172a)
	ink, muted, accent := hex(0xeef2fb), hex(0x9aa6c1), hex(0x7da2ff)
	tint := hex(0x1a2c5e)
	return palette{
		Bg:          canvas,
		Side:        canvas,
		Card:        surface,
		Sunken:      hex(0x0a101f),
		CardHi:      blend(ink, surface, 0.06),
		Tint:        tint,
		Edge:        blend(hex(0xffffff), surface, 0.05),
		Line:        hex(0x222b45),
		Stroke:      hex(0x66749a),
		Grid:        hex(0x222b45),
		Text:        ink,
		Muted:       muted,
		Disabled:    blend(muted, surface, 0.45),
		Accent:      accent,
		AccentHover: hex(0x9bb8ff),
		OnAccent:    hex(0x06102b),
		AccentInk:   hex(0x9bb8ff),
		Focus:       hex(0x9bb8ff),
		Good:        hex(0x4fd1a5),
		Warn:        hex(0xf2b84b),
		Bad:         hex(0xff8a73),
		GoodTint:    hex(0x0f2f29),
		WarnTint:    hex(0x33270d),
		BadTint:     hex(0x3a1712),
		CPU:         accent,
		Mem:         hex(0xc49cf0),
		GPU:         hex(0x6ad5c5),
		Disk:        hex(0x80ca94),
		Net:         hex(0xe5bb83),
		Power:       hex(0xe9cf86),
		Thermal:     hex(0xeda785),
		Clock:       hex(0x93cde4),
		RowAlt:      surface, // Meridian tables have no zebra stripes
		RowSel:      tint,
		RowSelText:  ink,
		TableBG:     surface,
	}
}

// Meridian radii, in design units. fillRound and strokeRound cap a radius at
// half the shape's shorter side, so rFull always gives a pill.
const (
	rSm   = 8   // radius-sm: chips, segments, small buttons
	rMd   = 12  // radius-md: buttons, inputs, list rows
	rLg   = 20  // radius-lg: cards
	rFull = 999 // radius-full: badges, toggles, progress bars
)

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
	p.Bg, p.Side, p.Card, p.Sunken = bg, side, bg, bg
	p.CardHi, p.Tint = blend(hi, bg, 0.22), blend(hi, bg, 0.45)
	p.Edge, p.Line, p.Stroke, p.Grid = txt, txt, txt, gray
	p.Text, p.Muted, p.Disabled = txt, txt, gray
	p.Accent, p.AccentHover, p.OnAccent = hi, hi, hiT
	p.AccentInk, p.Focus = txt, hi
	p.GoodTint, p.WarnTint, p.BadTint = bg, bg, bg
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

func fontOf(family string, pt int, style walk.FontStyle) *walk.Font {
	k := fontKey{family, pt, style}
	fontMu.Lock()
	defer fontMu.Unlock()
	if f, ok := fontCache[k]; ok {
		return f
	}
	f, err := walk.NewFont(family, pt, style)
	if err != nil {
		f, _ = walk.NewFont("Segoe UI", pt, style)
	}
	fontCache[k] = f
	return f
}

func uiFont(pt int) *walk.Font     { return fontOf(uiFamily(), pt, 0) }
func uiSemibold(pt int) *walk.Font { return fontOf(uiStrongFamily(), pt, 0) }
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
