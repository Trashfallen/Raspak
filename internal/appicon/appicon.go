// Package appicon рисует значки Raspak:
//   - программа: оранжевый квадрат, белая коробка-архив с молнией;
//   - архив (файлы .zip, .rar, .7z...): белый лист с загнутым углом, молния
//     и оранжевый бегунок - чтобы архив не путался с ярлыком программы.
//
// Из них собираются assets/icon.ico, assets/archive.ico (tools/genicon) и
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

// Render - значок программы.
func Render(size int) *image.NRGBA { return render(size, appSample) }

// RenderArchive - значок файлов-архивов.
func RenderArchive(size int) *image.NRGBA { return render(size, archiveSample) }

func appSample(x, y float64) rgba {
	if !inRoundRect(x, y, 0.04, 0.04, 0.96, 0.96, 0.22) {
		return clear
	}
	bg := orange((y - 0.04) / 0.92)
	// крышка и короб
	lid := inRoundRect(x, y, 0.17, 0.25, 0.83, 0.39, 0.045)
	body := inRoundRect(x, y, 0.21, 0.41, 0.79, 0.81, 0.06)
	if !lid && !body {
		return bg
	}
	// молния: зубцы через один слева и справа от оси (как на значке архива) и бегунок
	if y > 0.27 && y < 0.58 {
		row := int((y - 0.27) / 0.052)
		dx := x - 0.5
		if math.Mod(y-0.27, 0.052) < 0.031 && (row%2 == 0 && dx > -0.06 && dx < 0.006 || row%2 == 1 && dx < 0.06 && dx > -0.006) {
			return bg
		}
	}
	if inRoundRect(x, y, 0.44, 0.57, 0.56, 0.75, 0.035) && !inRoundRect(x, y, 0.477, 0.665, 0.523, 0.715, 0.014) {
		return bg
	}
	return white
}

func archiveSample(x, y float64) rgba {
	const l, t, r, b, fold = 0.17, 0.05, 0.83, 0.95, 0.2
	line := hex(0xC6C6C6)
	paper := hex(0xFFFFFF)
	// лист с загнутым правым верхним углом
	inPage := inRoundRect(x, y, l, t, r, b, 0.035) && (x-(r-fold))+(t+fold-y) <= fold+1e-9
	if !inPage {
		return clear
	}
	border := 0.022
	inner := inRoundRect(x, y, l+border, t+border, r-border, b-border, 0.02) &&
		(x-(r-fold))+(t+fold-y) <= fold-border*1.4
	// загиб
	if x > r-fold && y < t+fold && (x-(r-fold))+(t+fold-y) >= fold-border*1.4 {
		if (x-(r-fold))+(t+fold-y) <= fold-border*0.3 {
			return line
		}
		if x-(r-fold) <= y-t {
			return hex(0xE4E4E4)
		}
		return clear
	}
	if !inner {
		return line
	}
	c := paper
	// молния: зубцы через один слева и справа от оси
	if y > 0.08 && y < 0.6 {
		row := int((y - 0.08) / 0.055)
		dx := x - 0.5
		if math.Mod(y-0.08, 0.055) < 0.033 && (row%2 == 0 && dx > -0.065 && dx < 0.005 || row%2 == 1 && dx < 0.065 && dx > -0.005) {
			c = hex(0x5B5B5B)
		}
	}
	// оранжевый бегунок
	if inRoundRect(x, y, 0.41, 0.56, 0.59, 0.82, 0.05) {
		c = orange((y - 0.56) / 0.26)
		if inRoundRect(x, y, 0.465, 0.69, 0.535, 0.76, 0.02) {
			c = paper
		}
	}
	return c
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
