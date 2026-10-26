package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	"fyne.io/fyne/v2"
)

// makeIcon draws the app icon: two arrows chasing each other round a circle.
func makeIcon(bg color.NRGBA) fyne.Resource {
	const n = 128
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	c := float64(n) / 2
	white := color.NRGBA{255, 255, 255, 255}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			r := math.Hypot(dx, dy)
			if r > c-2 {
				continue
			}
			img.SetNRGBA(x, y, bg)
			ang := math.Atan2(dy, dx) * 180 / math.Pi
			if ang < 0 {
				ang += 360
			}
			inArc := math.Mod(ang, 180) < 140 // two arcs with gaps
			if r > c*0.42 && r < c*0.62 && inArc {
				img.SetNRGBA(x, y, white)
			}
		}
	}
	// arrow heads at the end of each arc
	for _, base := range []float64{140, 320} {
		a := base * math.Pi / 180
		tip := [2]float64{c + c*0.52*math.Cos(a+0.35), c + c*0.52*math.Sin(a+0.35)}
		l := [2]float64{c + c*0.30*math.Cos(a), c + c*0.30*math.Sin(a)}
		r := [2]float64{c + c*0.76*math.Cos(a), c + c*0.76*math.Sin(a)}
		fillTriangle(img, tip, l, r, white)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return fyne.NewStaticResource("claudesync.png", buf.Bytes())
}

func fillTriangle(img *image.NRGBA, a, b, c [2]float64, col color.NRGBA) {
	minX, maxX := math.Min(a[0], math.Min(b[0], c[0])), math.Max(a[0], math.Max(b[0], c[0]))
	minY, maxY := math.Min(a[1], math.Min(b[1], c[1])), math.Max(a[1], math.Max(b[1], c[1]))
	sign := func(p, q, r [2]float64) float64 { return (p[0]-r[0])*(q[1]-r[1]) - (q[0]-r[0])*(p[1]-r[1]) }
	for y := int(minY); y <= int(maxY); y++ {
		for x := int(minX); x <= int(maxX); x++ {
			p := [2]float64{float64(x) + 0.5, float64(y) + 0.5}
			d1, d2, d3 := sign(p, a, b), sign(p, b, c), sign(p, c, a)
			neg, pos := d1 < 0 || d2 < 0 || d3 < 0, d1 > 0 || d2 > 0 || d3 > 0
			if !(neg && pos) {
				img.SetNRGBA(x, y, col)
			}
		}
	}
}

var (
	iconIdle = makeIcon(color.NRGBA{0xC9, 0x6A, 0x42, 0xFF})
	iconBusy = makeIcon(color.NRGBA{0x3B, 0x82, 0xC4, 0xFF})
	iconWarn = makeIcon(color.NRGBA{0xB4, 0x3C, 0x3C, 0xFF})
)
