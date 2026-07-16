package art

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{200, 100, 50, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestProbe(t *testing.T) {
	format, w, h, err := Probe(makePNG(t, 64, 48))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if format != "png" || w != 64 || h != 48 {
		t.Errorf("probe = %s %dx%d, want png 64x48", format, w, h)
	}
	if _, _, _, err := Probe([]byte("not an image")); err == nil {
		t.Error("probe of garbage should error")
	}
}

func TestHashStable(t *testing.T) {
	a := makePNG(t, 10, 10)
	if Hash(a) != Hash(a) {
		t.Error("hash is not stable")
	}
	if Hash(a) == Hash(makePNG(t, 11, 10)) {
		t.Error("different images hashed equal")
	}
}

func TestThumbnailDownscalesPreservingAspect(t *testing.T) {
	src := makePNG(t, 200, 100) // 2:1
	out, format, w, h, err := Thumbnail(src, 50)
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if format != "png" {
		t.Errorf("format = %s, want png (source had alpha channel)", format)
	}
	// 200x100 fits in a 50px box as 50x25 (aspect preserved, long side == 50).
	if w != 50 || h != 25 {
		t.Errorf("thumb dims = %dx%d, want 50x25", w, h)
	}
	gotFormat, gw, gh, err := Probe(out)
	if err != nil || gotFormat != "png" || gw != 50 || gh != 25 {
		t.Errorf("encoded thumb probes as %s %dx%d (err %v), want png 50x25", gotFormat, gw, gh, err)
	}
}

func TestThumbnailJPEGStaysJPEG(t *testing.T) {
	_, format, _, _, err := Thumbnail(makeJPEG(t, 300, 300), 64)
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %s, want jpeg for a jpeg source", format)
	}
}

func TestThumbnailNeverUpscales(t *testing.T) {
	_, _, w, h, err := Thumbnail(makePNG(t, 30, 20), 100) // box larger than source
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if w != 30 || h != 20 {
		t.Errorf("thumb dims = %dx%d, want the original 30x20 (no upscaling)", w, h)
	}
}

func TestSniffExotic(t *testing.T) {
	avif := append([]byte{0, 0, 0, 0x20}, []byte("ftypavif")...)
	if f, ok := SniffExotic(avif); !ok || f != "avif" {
		t.Errorf("avif sniff = %q,%v, want avif,true", f, ok)
	}
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	if f, ok := SniffExotic(heic); !ok || f != "heic" {
		t.Errorf("heic sniff = %q,%v, want heic,true", f, ok)
	}
	if _, ok := SniffExotic([]byte("\xff\xd8\xff\xe0 jpeg")); ok {
		t.Error("jpeg bytes should not sniff as exotic")
	}
	if _, ok := SniffExotic([]byte("short")); ok {
		t.Error("short input should not sniff as exotic")
	}
}
