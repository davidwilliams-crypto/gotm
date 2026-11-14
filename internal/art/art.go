// Package art draws gotm's own artwork: the application icon and the small
// placeholder shown for programs that have no icon of their own.
//
// Everything is described as geometry and rendered separately for each pixel
// size, never scaled from another size: edges are anti-aliased from exact
// distances, tiles are snapped to whole pixels, and small sizes get a simpler
// drawing with heavier strokes instead of a blurred copy of the large one.
// Colours are tokens of the Meridian design system, flat: no gradients, no
// glows.
package art

import (
	"image"
	"image/color"
	"math"
)

// Meridian tokens used by the artwork.
var (
	accent     = rgb(0x2152e5) // accent (light theme): reads on dark and on light grounds
	accentSoft = rgb(0x7da2ff) // accent (dark theme): the quiet area under the line
	white      = rgb(0xffffff) // on-accent
)

type rgbf struct{ r, g, b float64 }

func rgb(v uint32) rgbf {
	return rgbf{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

type pt struct{ x, y float64 }

// canvas holds premultiplied float RGBA.
type canvas struct {
	w, h int
	pix  []float64
}

func newCanvas(w, h int) *canvas { return &canvas{w: w, h: h, pix: make([]float64, w*h*4)} }

// paint composites colour c over the canvas wherever cover (0..1 per pixel
// centre) is positive. alpha scales the colour's opacity.
func (c *canvas) paint(col rgbf, alpha float64, cover func(x, y float64) float64) {
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			a := cover(float64(x)+0.5, float64(y)+0.5)
			if a <= 0 {
				continue
			}
			a = math.Min(a, 1) * alpha
			p := c.pix[(y*c.w+x)*4:]
			p[0] = col.r*a + p[0]*(1-a)
			p[1] = col.g*a + p[1]*(1-a)
			p[2] = col.b*a + p[2]*(1-a)
			p[3] = a + p[3]*(1-a)
		}
	}
}

// image converts to straight-alpha 8-bit pixels.
func (c *canvas) image() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
	for i := 0; i < c.w*c.h; i++ {
		p := c.pix[i*4:]
		a := p[3]
		if a <= 0 {
			continue
		}
		q := func(v float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, v/a)) * 255)) }
		img.Pix[i*4+0], img.Pix[i*4+1], img.Pix[i*4+2] = q(p[0]), q(p[1]), q(p[2])
		img.Pix[i*4+3] = uint8(math.Round(math.Min(1, a) * 255))
	}
	return img
}

// edge turns a signed distance (negative inside, in pixels) into coverage.
func edge(d float64) float64 { return math.Max(0, math.Min(1, 0.5-d)) }

// sdRound is the signed distance to a rounded rectangle.
func sdRound(x, y, x0, y0, x1, y1, r float64) float64 {
	r = math.Min(r, math.Min(x1-x0, y1-y0)/2)
	cx, cy := (x0+x1)/2, (y0+y1)/2
	qx, qy := math.Abs(x-cx)-(x1-x0)/2+r, math.Abs(y-cy)-(y1-y0)/2+r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

// sdSegment is the distance from a point to a line segment.
func sdSegment(x, y float64, a, b pt) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	t := 0.0
	if l := dx*dx + dy*dy; l > 0 {
		t = math.Max(0, math.Min(1, ((x-a.x)*dx+(y-a.y)*dy)/l))
	}
	return math.Hypot(x-a.x-t*dx, y-a.y-t*dy)
}

// polyline is a stroked path with round caps and joins.
type polyline []pt

func (p polyline) dist(x, y float64) float64 {
	d := math.Inf(1)
	for i := 1; i < len(p); i++ {
		d = math.Min(d, sdSegment(x, y, p[i-1], p[i]))
	}
	return d
}

func scale(ps []pt, x0, y0, s float64) []pt {
	out := make([]pt, len(ps))
	for i, p := range ps {
		out[i] = pt{x0 + p.x*s, y0 + p.y*s}
	}
	return out
}

// AppIcon draws the gotm application icon at size x size pixels: an accent
// tile carrying three rising bars, the tallest in white (the one reading that
// matters) and the two before it in the quiet accent.
func AppIcon(size int) *image.NRGBA {
	s := float64(size)
	c := newCanvas(size, size)

	// The tile is snapped to whole pixels so its straight edges are crisp.
	inset := math.Round(s * 0.035)
	if size < 32 {
		inset = 0
	}
	x0, y0, x1, y1 := inset, inset, s-inset, s-inset
	side := x1 - x0
	c.paint(accent, 1, func(x, y float64) float64 {
		return edge(sdRound(x, y, x0, y0, x1, y1, side*0.225))
	})

	// Bars and gaps are whole pixels wide and stand on a whole-pixel
	// baseline, so they stay sharp down to 16 pixels.
	const n = 3
	gap := side * 0.085
	bw := math.Max(2, math.Round((side*0.60-gap*(n-1))/n))
	gap = math.Max(1, math.Round(gap))
	left := math.Round(x0 + (side-(bw*n+gap*(n-1)))/2)
	base := math.Round(y0 + side*0.76)
	for i, height := range [n]float64{0.24, 0.38, 0.54} {
		bx := left + float64(i)*(bw+gap)
		top := math.Round(base - side*height)
		col, alpha := accentSoft, 0.95
		if i == n-1 {
			col, alpha = white, 1
		}
		c.paint(col, alpha, func(x, y float64) float64 {
			return edge(sdRound(x, y, bx, top, bx+bw, base, bw*0.32))
		})
	}
	return c.image()
}

// GenericApp draws the placeholder for a program without an icon of its own:
// a small window outline with a title strip, in the given ink. It is drawn
// for the exact pixel size it is shown at.
func GenericApp(size int, ink color.NRGBA) *image.NRGBA {
	s := float64(size)
	c := newCanvas(size, size)
	col := rgbf{float64(ink.R) / 255, float64(ink.G) / 255, float64(ink.B) / 255}
	alpha := float64(ink.A) / 255

	stroke := math.Max(1, math.Round(s/14))
	m := math.Round(s * 0.11)
	x0, x1 := m, s-m
	h := math.Round(s * 0.66)
	y0 := math.Round((s - h) / 2)
	y1 := y0 + h
	r := math.Max(1.5, s*0.14)
	frame := func(x, y float64) float64 { return sdRound(x, y, x0, y0, x1, y1, r) }
	// outline: the band between the frame and the frame shrunk by the stroke
	c.paint(col, alpha, func(x, y float64) float64 {
		return math.Min(edge(frame(x, y)), edge(-(frame(x, y) + stroke)))
	})
	// title strip: the top of the frame, filled
	bar := y0 + math.Max(stroke+1, math.Round(h*0.30))
	c.paint(col, alpha, func(x, y float64) float64 {
		return math.Min(edge(frame(x, y)), edge(y-bar))
	})
	return c.image()
}

// Checkbox draws a Meridian check box for a table row at size x size pixels.
// Unchecked it is an outline in box (border-strong); checked it is filled
// with fill (accent) and carries a check mark in mark (on-accent).
func Checkbox(size int, checked bool, box, fill, mark color.NRGBA) *image.NRGBA {
	s := float64(size)
	c := newCanvas(size, size)
	f := func(n color.NRGBA) rgbf { return rgbf{float64(n.R) / 255, float64(n.G) / 255, float64(n.B) / 255} }
	m := math.Max(1, math.Round(s*0.10))
	x0, y0, x1, y1 := m, m, s-m, s-m
	side := x1 - x0
	r := side * 0.28
	shape := func(x, y float64) float64 { return sdRound(x, y, x0, y0, x1, y1, r) }
	if !checked {
		stroke := math.Max(1, math.Round(s/12))
		c.paint(f(box), 1, func(x, y float64) float64 {
			return math.Min(edge(shape(x, y)), edge(-(shape(x, y) + stroke)))
		})
		return c.image()
	}
	c.paint(f(fill), 1, func(x, y float64) float64 { return edge(shape(x, y)) })
	tick := polyline(scale([]pt{{0.27, 0.53}, {0.44, 0.69}, {0.74, 0.34}}, x0, y0, side))
	w := math.Max(1.4, side*0.13)
	c.paint(f(mark), 1, func(x, y float64) float64 { return edge(tick.dist(x, y) - w/2) })
	return c.image()
}
