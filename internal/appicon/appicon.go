// Package appicon рисует значок Raspak: оранжевый квадрат, стрелка вниз в лоток.
// Им же собираются assets/icon.ico (tools/genicon) и картинки пакета для меню Windows 11.
package appicon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Render рисует значок size x size со сглаживанием (4x4 выборки на пиксель).
func Render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					cr, cg, cb, ca := sample(x, y)
					r += cr * ca
					g += cg * ca
					b += cb * ca
					a += ca
				}
			}
			n := float64(ss * ss)
			if a > 0 {
				img.SetRGBA(px, py, color.RGBA{
					R: uint8(r / n * 255), G: uint8(g / n * 255), B: uint8(b / n * 255), A: uint8(a / n * 255),
				})
			}
		}
	}
	return img
}

// sample - цвет точки (x, y в долях от 0 до 1), премультипликация не нужна.
func sample(x, y float64) (r, g, b, a float64) {
	// фон: скругленный квадрат с вертикальным градиентом
	if !inRoundRect(x, y, 0.04, 0.04, 0.96, 0.96, 0.2) {
		return 0, 0, 0, 0
	}
	t := (y - 0.04) / 0.92
	r = lerp(0xFB, 0xE1, t) / 255 // #FB923C -> #E1470B
	g = lerp(0x92, 0x47, t) / 255
	b = lerp(0x3C, 0x0B, t) / 255
	a = 1
	white := false
	// стрелка: стержень и наконечник
	if inRoundRect(x, y, 0.435, 0.16, 0.565, 0.47, 0.03) {
		white = true
	}
	if inTriangle(x, y, 0.27, 0.42, 0.73, 0.42, 0.5, 0.66) {
		white = true
	}
	// лоток: левая и правая стенки и дно
	if inRoundRect(x, y, 0.19, 0.56, 0.29, 0.83, 0.04) || inRoundRect(x, y, 0.71, 0.56, 0.81, 0.83, 0.04) ||
		inRoundRect(x, y, 0.19, 0.73, 0.81, 0.83, 0.04) {
		white = true
	}
	if white {
		return 1, 1, 1, 1
	}
	return
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func inRoundRect(x, y, x0, y0, x1, y1, rad float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+rad, math.Min(x, x1-rad))
	cy := math.Max(y0+rad, math.Min(y, y1-rad))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= rad*rad
}

func inTriangle(x, y, ax, ay, bx, by, cx, cy float64) bool {
	s := func(px, py, qx, qy, rx, ry float64) float64 { return (px-rx)*(qy-ry) - (qx-rx)*(py-ry) }
	d1, d2, d3 := s(x, y, ax, ay, bx, by), s(x, y, bx, by, cx, cy), s(x, y, cx, cy, ax, ay)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

// PNG - значок size x size в PNG.
func PNG(size int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, Render(size))
	return buf.Bytes()
}

// ICO собирает .ico из PNG нужных размеров (Windows Vista+ читает PNG внутри ico).
func ICO(sizes ...int) []byte {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, PNG(s))
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		buf.Write([]byte{dim, dim, 0, 0})
		binary.Write(&buf, binary.LittleEndian, uint16(1))
		binary.Write(&buf, binary.LittleEndian, uint16(32))
		binary.Write(&buf, binary.LittleEndian, uint32(len(imgs[i])))
		binary.Write(&buf, binary.LittleEndian, uint32(off))
		off += len(imgs[i])
	}
	for _, b := range imgs {
		buf.Write(b)
	}
	return buf.Bytes()
}
