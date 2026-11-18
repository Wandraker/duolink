// Package icon draws the Duolink mark: two strands merging into one.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"golang.org/x/image/vector"
)

var (
	tile  = color.NRGBA{0x1A, 0x1F, 0x29, 0xFF}
	blue  = color.NRGBA{0x5B, 0x8C, 0xFF, 0xFF}
	amber = color.NRGBA{0xFF, 0xB5, 0x47, 0xFF}
	white = color.NRGBA{0xF2, 0xF5, 0xFA, 0xFF}
	grey  = color.NRGBA{0x6B, 0x74, 0x86, 0xFF}
)

type pt struct{ x, y float64 }

func cubic(a, b, c, d pt, t float64) pt {
	u := 1 - t
	return pt{
		u*u*u*a.x + 3*u*u*t*b.x + 3*u*t*t*c.x + t*t*t*d.x,
		u*u*u*a.y + 3*u*u*t*b.y + 3*u*t*t*c.y + t*t*t*d.y,
	}
}

// stroke fills a thick round-capped curve through pts onto dst.
func stroke(dst *image.NRGBA, pts []pt, w float64, col color.NRGBA) {
	r := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	h := w / 2
	left := make([]pt, len(pts))
	right := make([]pt, len(pts))
	for i := range pts {
		a, b := pts[max(i-1, 0)], pts[min(i+1, len(pts)-1)]
		dx, dy := b.x-a.x, b.y-a.y
		l := math.Hypot(dx, dy)
		if l == 0 {
			l = 1
		}
		nx, ny := -dy/l*h, dx/l*h
		left[i] = pt{pts[i].x + nx, pts[i].y + ny}
		right[i] = pt{pts[i].x - nx, pts[i].y - ny}
	}
	r.MoveTo(float32(left[0].x), float32(left[0].y))
	for _, p := range left[1:] {
		r.LineTo(float32(p.x), float32(p.y))
	}
	for i := len(right) - 1; i >= 0; i-- {
		r.LineTo(float32(right[i].x), float32(right[i].y))
	}
	r.ClosePath()
	r.Draw(dst, dst.Bounds(), image.NewUniform(col), image.Point{})
	// Round caps, each drawn on its own so opposite windings don't cancel.
	for _, c := range []pt{pts[0], pts[len(pts)-1]} {
		cr := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
		circle(cr, c, h)
		cr.Draw(dst, dst.Bounds(), image.NewUniform(col), image.Point{})
	}
}

func circle(r *vector.Rasterizer, c pt, rad float64) {
	const k = 0.5522847498 * 1 // cubic circle constant
	x, y, d := c.x, c.y, rad*k
	f := func(v float64) float32 { return float32(v) }
	r.MoveTo(f(x+rad), f(y))
	r.CubeTo(f(x+rad), f(y+d), f(x+d), f(y+rad), f(x), f(y+rad))
	r.CubeTo(f(x-d), f(y+rad), f(x-rad), f(y+d), f(x-rad), f(y))
	r.CubeTo(f(x-rad), f(y-d), f(x-d), f(y-rad), f(x), f(y-rad))
	r.CubeTo(f(x+d), f(y-rad), f(x+rad), f(y-d), f(x+rad), f(y))
	r.ClosePath()
}

func roundRect(dst *image.NRGBA, s, rad float64, col color.NRGBA) {
	r := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	k := rad * 0.5522847498
	f := func(v float64) float32 { return float32(v) }
	r.MoveTo(f(rad), 0)
	r.LineTo(f(s-rad), 0)
	r.CubeTo(f(s-rad+k), 0, f(s), f(rad-k), f(s), f(rad))
	r.LineTo(f(s), f(s-rad))
	r.CubeTo(f(s), f(s-rad+k), f(s-rad+k), f(s), f(s-rad), f(s))
	r.LineTo(f(rad), f(s))
	r.CubeTo(f(rad-k), f(s), 0, f(s-rad+k), 0, f(s-rad))
	r.LineTo(0, f(rad))
	r.CubeTo(0, f(rad-k), f(rad-k), 0, f(rad), 0)
	r.ClosePath()
	r.Draw(dst, dst.Bounds(), image.NewUniform(col), image.Point{})
}

func sample(a, b, c, d pt, n int, s float64) []pt {
	out := make([]pt, n+1)
	for i := 0; i <= n; i++ {
		p := cubic(a, b, c, d, float64(i)/float64(n))
		out[i] = pt{p.x * s, p.y * s}
	}
	return out
}

// Render draws the mark at size×size. active=false greys the two strands
// (combining switched off).
func Render(size int, active bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)
	s := float64(size)
	small := size <= 20
	if !small {
		roundRect(img, s, s*0.22, tile)
	}
	// Geometry in a 0..1 box.
	w := 0.105
	if small {
		w = 0.16
	}
	ca, cb := blue, amber
	if !active {
		ca, cb = grey, grey
	}
	m := pt{0.60, 0.5}
	top := sample(pt{0.18, 0.27}, pt{0.38, 0.27}, pt{0.44, 0.5}, m, 40, s)
	bot := sample(pt{0.18, 0.73}, pt{0.38, 0.73}, pt{0.44, 0.5}, m, 40, s)
	if small {
		top = sample(pt{0.08, 0.2}, pt{0.34, 0.2}, pt{0.42, 0.5}, pt{0.6, 0.5}, 40, s)
		bot = sample(pt{0.08, 0.8}, pt{0.34, 0.8}, pt{0.42, 0.5}, pt{0.6, 0.5}, 40, s)
	}
	stroke(img, top, w*s, ca)
	stroke(img, bot, w*s, cb)
	end := 0.84
	if small {
		end = 0.92
	}
	trunk := []pt{{m.x * s, 0.5 * s}, {end * s, 0.5 * s}}
	tw := w * 1.4
	if !active {
		tw = w
	}
	stroke(img, trunk, tw*s, white)
	return img
}

// ICO returns a .ico file with PNG-encoded images of the given sizes.
func ICO(active bool, sizes ...int) []byte {
	var imgs [][]byte
	for _, sz := range sizes {
		var b bytes.Buffer
		png.Encode(&b, Render(sz, active))
		imgs = append(imgs, b.Bytes())
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, sz := range sizes {
		d := byte(sz)
		if sz >= 256 {
			d = 0
		}
		out.Write([]byte{d, d, 0, 0})
		binary.Write(&out, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&out, binary.LittleEndian, [2]uint32{uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, b := range imgs {
		out.Write(b)
	}
	return out.Bytes()
}
