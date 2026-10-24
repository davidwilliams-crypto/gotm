// genicon writes the gotm application icon (a rounded tile with a usage graph)
// to winres/icon.png and winres/icon16.png. Run: go run ./tools/genicon
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
)

func render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	bg := color.RGBA{0x1d, 0x4e, 0xd8, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}
	soft := color.RGBA{0xff, 0xff, 0xff, 0x55}

	// rounded square, antialiased by 4x supersampling
	r := s * 0.2
	inside := func(x, y float64) bool {
		cx := math.Min(math.Max(x, r), s-r)
		cy := math.Min(math.Max(y, r), s-r)
		return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			n := 0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					if inside(float64(x)+(float64(sx)+0.5)/4, float64(y)+(float64(sy)+0.5)/4) {
						n++
					}
				}
			}
			if n > 0 {
				c := bg
				c.A = uint8(255 * n / 16)
				img.SetRGBA(x, y, c)
			}
		}
	}

	// usage curve
	pts := [][2]float64{{0.14, 0.70}, {0.30, 0.52}, {0.42, 0.62}, {0.58, 0.30}, {0.72, 0.46}, {0.86, 0.24}}
	thick := math.Max(1.2, s*0.065)
	line := func(a, b [2]float64, c color.RGBA, t float64) {
		ax, ay, bx, by := a[0]*s, a[1]*s, b[0]*s, b[1]*s
		steps := int(math.Hypot(bx-ax, by-ay)*2) + 1
		for i := 0; i <= steps; i++ {
			f := float64(i) / float64(steps)
			px, py := ax+(bx-ax)*f, ay+(by-ay)*f
			for dy := -t; dy <= t; dy++ {
				for dx := -t; dx <= t; dx++ {
					if dx*dx+dy*dy <= t*t {
						ix, iy := int(px+dx), int(py+dy)
						if ix >= 0 && iy >= 0 && ix < size && iy < size && img.RGBAAt(ix, iy).A > 0 {
							draw.Draw(img, image.Rect(ix, iy, ix+1, iy+1), &image.Uniform{c}, image.Point{}, draw.Over)
						}
					}
				}
			}
		}
	}
	// baseline hint, then the curve
	line([2]float64{0.14, 0.82}, [2]float64{0.86, 0.82}, soft, math.Max(0.6, thick/3))
	for i := 1; i < len(pts); i++ {
		line(pts[i-1], pts[i], fg, thick/2)
	}
	return img
}

func write(path string, size int) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, render(size)); err != nil {
		panic(err)
	}
}

func main() {
	write("winres/icon.png", 256)
	write("winres/icon16.png", 16)
}
