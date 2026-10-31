// Set-of-Marks: numbered boxes over interactive elements, drawn with a
// built-in 3x5 digit font (the stdlib has no fonts).
package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
)

var digitFont = [10]uint16{ // 3x5, row-major, bit 14 = top-left
	0b111_101_101_101_111, 0b010_110_010_010_111, 0b111_001_111_100_111, 0b111_001_111_001_111,
	0b101_101_111_001_001, 0b111_100_111_001_111, 0b111_100_111_101_111, 0b111_001_001_001_001,
	0b111_101_111_101_111, 0b111_101_111_001_111,
}

var markColors = []color.RGBA{{255, 214, 0, 255}, {255, 64, 200, 255}, {0, 230, 255, 255}, {255, 128, 0, 255}, {120, 255, 80, 255}}

const glyphScale = 2

type mark struct {
	N    int
	Rect image.Rectangle // in the image's pixels
}

func drawMarks(pngData []byte, marks []mark) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(src.Bounds())
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	for i, m := range marks {
		c := image.NewUniform(markColors[i%len(markColors)])
		r := m.Rect.Intersect(img.Bounds())
		for _, edge := range []image.Rectangle{ // 2 px outline
			{r.Min, image.Pt(r.Max.X, r.Min.Y+2)}, {image.Pt(r.Min.X, r.Max.Y-2), r.Max},
			{r.Min, image.Pt(r.Min.X+2, r.Max.Y)}, {image.Pt(r.Max.X-2, r.Min.Y), r.Max},
		} {
			draw.Draw(img, edge.Intersect(r), c, image.Point{}, draw.Src)
		}
		label := strconv.Itoa(m.N)
		w, h := len(label)*4*glyphScale+glyphScale, 5*glyphScale+2*glyphScale
		at := r.Min
		if at.X+w > img.Bounds().Max.X {
			at.X = img.Bounds().Max.X - w
		}
		draw.Draw(img, image.Rect(at.X, at.Y, at.X+w, at.Y+h).Intersect(img.Bounds()), c, image.Point{}, draw.Src)
		for k, ch := range label {
			drawDigit(img, at.X+glyphScale+k*4*glyphScale, at.Y+glyphScale, digitFont[ch-'0'])
		}
	}
	var out bytes.Buffer
	err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&out, img)
	return out.Bytes(), err
}

func drawDigit(img *image.RGBA, x, y int, bits uint16) {
	for row := range 5 {
		for col := range 3 {
			if bits&(1<<(14-(row*3+col))) == 0 {
				continue
			}
			px := image.Rect(x+col*glyphScale, y+row*glyphScale, x+(col+1)*glyphScale, y+(row+1)*glyphScale)
			draw.Draw(img, px.Intersect(img.Bounds()), image.Black, image.Point{}, draw.Src)
		}
	}
}

// pngSize reads width and height from the IHDR chunk.
func pngSize(b []byte) (int, int, bool) {
	if len(b) < 24 || string(b[1:4]) != "PNG" {
		return 0, 0, false
	}
	be := func(o int) int { return int(b[o])<<24 | int(b[o+1])<<16 | int(b[o+2])<<8 | int(b[o+3]) }
	return be(16), be(20), true
}
