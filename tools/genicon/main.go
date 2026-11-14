// genicon writes the gotm application icon, drawn by internal/art, as one PNG
// per icon size into winres/ (icon16.png … icon256.png). go-winres packs them
// into the executable's icon resource, so Windows always finds an image made
// for the size it needs (title bar, taskbar, Alt+Tab, Start, Explorer at any
// scaling) instead of stretching another one.
//
//	go run ./tools/genicon                 # the icon PNGs
//	go run ./tools/genicon -sheet out.png  # also a comparison sheet of the design
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"

	"gotm/internal/art"
)

// Sizes are the ones Windows asks for at 100–250 % scaling, plus 256 for the
// large Explorer views.
var sizes = []int{16, 20, 24, 32, 40, 48, 64, 256}

func write(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

// sheet lays the icon out at every size on the grounds it has to work on:
// gotm's own canvas, a light taskbar and plain white.
func sheet(path string) {
	grounds := []color.NRGBA{{0x07, 0x0b, 0x16, 0xff}, {0xe8, 0xed, 0xf7, 0xff}, {0xff, 0xff, 0xff, 0xff}}
	const pad, row = 24, 256 + 24
	w := pad
	for _, s := range sizes {
		w += s + pad
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, row*len(grounds)))
	for g, ground := range grounds {
		draw.Draw(out, image.Rect(0, g*row, w, (g+1)*row), &image.Uniform{ground}, image.Point{}, draw.Src)
		x := pad
		for i := len(sizes) - 1; i >= 0; i-- {
			s := sizes[i]
			img := art.AppIcon(s)
			y := g*row + (row-s)/2
			draw.Draw(out, image.Rect(x, y, x+s, y+s), img, image.Point{}, draw.Over)
			x += s + pad
		}
	}
	write(path, out)
}

func main() {
	sheetPath := flag.String("sheet", "", "also write a comparison sheet to this PNG")
	dir := flag.String("dir", "winres", "folder for the icon PNGs")
	flag.Parse()
	for _, s := range sizes {
		write(filepath.Join(*dir, fmt.Sprintf("icon%d.png", s)), art.AppIcon(s))
	}
	if *sheetPath != "" {
		sheet(*sheetPath)
	}
}
