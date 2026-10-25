// Package appicon рисует значок Raspak - оранжевую папку с молнией, как
// сжатые папки Windows. Он один и для программы, и для файлов-архивов
// (.zip, .rar, .7z...).
//
// Из него собираются assets/icon.ico, assets/archive.ico (tools/genicon) и
// картинки пакета меню Windows 11.
package appicon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

type rgba struct{ r, g, b, a float64 }

var (
	white  = rgba{1, 1, 1, 1}
	clear  = rgba{}
	orange = func(t float64) rgba { // #FB923C -> #E1470B сверху вниз
		return rgba{lerp(0xFB, 0xE1, t) / 255, lerp(0x92, 0x47, t) / 255, lerp(0x3C, 0x0B, t) / 255, 1}
	}
)

func hex(c uint32) rgba {
	return rgba{float64(c>>16&0xff) / 255, float64(c>>8&0xff) / 255, float64(c&0xff) / 255, 1}
}

// render рисует картинку со сглаживанием: 4x4 выборки на пиксель.
func render(size int, sample func(x, y float64) rgba) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := sample((float64(px)+(float64(sx)+0.5)/ss)/float64(size), (float64(py)+(float64(sy)+0.5)/ss)/float64(size))
					r += c.r * c.a
					g += c.g * c.a
					b += c.b * c.a
					a += c.a
				}
			}
			if a > 0 {
				n := float64(ss * ss)
				img.SetNRGBA(px, py, color.NRGBA{
					R: uint8(r / a * 255), G: uint8(g / a * 255), B: uint8(b / a * 255), A: uint8(a / n * 255),
				})
			}
		}
	}
	return img
}

// Render - значок программы: та же оранжевая папка с молнией, что у архивов,
// чтобы ярлык программы и файлы-архивы выглядели одинаково.
func Render(size int) *image.NRGBA { return render(size, folderSample) }

// RenderArchive - значок файлов-архивов.
func RenderArchive(size int) *image.NRGBA { return render(size, folderSample) }

// folderSample - значок архива: оранжевая папка (задняя стенка с ушком,
// выглядывающий лист, передняя стенка) и молния с бегунком на передней стенке.
func folderSample(x, y float64) rgba {
	front := inRoundRect(x, y, 0.06, 0.34, 0.94, 0.88, 0.07)
	if front {
		bg := orange((y - 0.34) / 0.54)
		// молния: белые зубцы через один слева и справа от оси, бегунок
		if y > 0.34 && y < 0.62 {
			row := int((y - 0.34) / 0.05)
			dx := x - 0.5
			if math.Mod(y-0.34, 0.05) < 0.03 && (row%2 == 0 && dx > -0.058 && dx < 0.004 || row%2 == 1 && dx < 0.058 && dx > -0.004) {
				return white
			}
		}
		if inRoundRect(x, y, 0.435, 0.6, 0.565, 0.79, 0.035) && !inRoundRect(x, y, 0.474, 0.7, 0.526, 0.755, 0.014) {
			return white
		}
		return bg
	}
	if inRoundRect(x, y, 0.13, 0.24, 0.87, 0.5, 0.03) {
		return hex(0xFFF4EC) // лист внутри папки
	}
	back := inRoundRect(x, y, 0.06, 0.19, 0.94, 0.8, 0.07) || inRoundRect(x, y, 0.06, 0.12, 0.44, 0.3, 0.06)
	if back {
		t := (y - 0.12) / 0.6
		return rgba{lerp(0xD9, 0xB4, t) / 255, lerp(0x5A, 0x3A, t) / 255, lerp(0x14, 0x08, t) / 255, 1} // темнее передней стенки
	}
	return clear
}

func lerp(a, b, t float64) float64 { return a + (b-a)*math.Max(0, math.Min(1, t)) }

func inRoundRect(x, y, x0, y0, x1, y1, rad float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+rad, math.Min(x, x1-rad))
	cy := math.Max(y0+rad, math.Min(y, y1-rad))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= rad*rad
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// PNG - значок программы size x size.
func PNG(size int) []byte { return encodePNG(Render(size)) }

// ArchivePNG - значок архива size x size.
func ArchivePNG(size int) []byte { return encodePNG(RenderArchive(size)) }

// ICO собирает .ico программы из PNG нужных размеров (Windows Vista+ читает PNG в ico).
func ICO(sizes ...int) []byte { return ico(PNG, sizes) }

// ArchiveICO - .ico для файлов-архивов.
func ArchiveICO(sizes ...int) []byte { return ico(ArchivePNG, sizes) }

func ico(draw func(int) []byte, sizes []int) []byte {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, draw(s))
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
