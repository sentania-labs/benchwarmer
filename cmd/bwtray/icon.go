package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"

	"github.com/sentania-labs/benchwarmer/internal/state"
)

// iconFor returns an ICO for a condition: a filled circle for Available, a
// ring for Yielding, a dimmed circle with a bar for Unavailable, so the state
// is readable without relying on color alone.
func iconFor(c state.Condition, reachable bool) []byte {
	const size = 32
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	var fill color.NRGBA
	shape := "disc"
	switch {
	case !reachable:
		fill, shape = color.NRGBA{120, 120, 120, 255}, "bar"
	case c == state.Available:
		fill = color.NRGBA{34, 160, 90, 255}
	case c == state.Yielding:
		fill, shape = color.NRGBA{230, 150, 20, 255}, "ring"
	default:
		fill, shape = color.NRGBA{110, 110, 130, 255}, "bar"
	}
	cx, cy, r := 15.5, 15.5, 13.0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			in := d <= r
			switch shape {
			case "ring":
				in = d <= r && d >= r-5
			case "bar":
				if in && y >= 13 && y <= 18 && x >= 7 && x <= 24 {
					img.Set(x, y, color.NRGBA{255, 255, 255, 255})
					continue
				}
			}
			if in {
				img.Set(x, y, fill)
			}
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	return pngToICO(pngBuf.Bytes(), size)
}

// pngToICO wraps one PNG image in an ICO container (supported since Vista).
func pngToICO(p []byte, size int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w(uint16(0))   // reserved
	w(uint16(1))   // type: icon
	w(uint16(1))   // count
	w(uint8(size)) // width
	w(uint8(size)) // height
	w(uint8(0))    // palette
	w(uint8(0))    // reserved
	w(uint16(1))   // planes
	w(uint16(32))  // bpp
	w(uint32(len(p)))
	w(uint32(6 + 16)) // offset of image data
	b.Write(p)
	return b.Bytes()
}
