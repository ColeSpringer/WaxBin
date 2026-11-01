// Package testimage builds pictures for tests.
package testimage

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

// Textured is an opaque w x h picture with a photograph's texture, a gradient under
// faint noise, distinct per seed. The noise is what makes a scaled copy smaller as a JPEG,
// which the cover bound wants before it keeps one; a plain gradient packs so well that a
// short one is kept as it arrived. Keep the short side small: the bound reads the long
// side, and a test pays for every pixel several times.
func Textured(w, h int, seed uint32) *image.RGBA {
	if seed == 0 {
		seed = 88172645 // xorshift never leaves zero
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x*255/w) + uint8(seed&7)
			img.Pix[i+1] = uint8(y*255/h) + uint8((seed>>8)&7)
			img.Pix[i+2] = 96 + uint8((seed>>16)&7)
			img.Pix[i+3] = 255
		}
	}
	return img
}

// TexturedPNG is Textured encoded as a PNG.
func TexturedPNG(tb testing.TB, w, h int, seed uint32) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, Textured(w, h, seed)); err != nil {
		tb.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}
