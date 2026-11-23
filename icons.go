package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// Home-screen icons, drawn at startup instead of shipping image files: the same house
// outline as the favicon, on a full-bleed background so Android's "maskable" crop works.
var (
	iconMu    sync.Mutex
	iconCache = map[int][]byte{}
)

func iconPNG(size int) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[size]; ok {
		return b
	}
	bg := color.RGBA{0x11, 0x14, 0x18, 0xff}
	fg := color.RGBA{0x5a, 0xa9, 0xff, 0xff}
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// House path in a 32-unit box (matches the SVG favicon), shrunk into the maskable
	// safe zone (centre 60%).
	pts := [][2]float64{{6, 16}, {16, 7}, {26, 16}, {26, 26}, {6, 26}, {6, 16}}
	s := float64(size)
	scale, off := s*0.6/20, s*0.2-6*s*0.6/20 // map x 6..26 -> 20%..80%
	yoff := s*0.2 - 7*scale + (s*0.6-19*scale)/2
	for i := range pts {
		pts[i] = [2]float64{pts[i][0]*scale + off, pts[i][1]*scale + yoff}
	}
	half := 1.6 * scale // stroke half-width

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			d := math.Inf(1)
			for i := 0; i+1 < len(pts); i++ {
				d = math.Min(d, segDist(px, py, pts[i], pts[i+1]))
			}
			a := math.Max(0, math.Min(1, half-d+0.5)) // 1px antialiasing
			img.SetRGBA(x, y, color.RGBA{
				uint8(float64(bg.R)*(1-a) + float64(fg.R)*a),
				uint8(float64(bg.G)*(1-a) + float64(fg.G)*a),
				uint8(float64(bg.B)*(1-a) + float64(fg.B)*a), 0xff})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	iconCache[size] = buf.Bytes()
	return iconCache[size]
}

func segDist(px, py float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := ((px-a[0])*dx + (py-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(a[0]+t*dx), py-(a[1]+t*dy))
}
