package art

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// transparentPNG is a textured picture with a see-through corner, which a JPEG cannot
// hold.
func transparentPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(362436069)
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			a := uint8(255)
			if x < w/4 && y < h/4 {
				a = 0
			}
			img.Set(x, y, color.NRGBA{uint8(x*255/w) + uint8(seed&7), uint8(y*255/h) + uint8((seed>>8)&7), 64, a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestBoundKeepsAnImageWithinTheLimit: a picture whose longest side is at the limit is
// kept as it arrived, byte for byte.
func TestBoundKeepsAnImageWithinTheLimit(t *testing.T) {
	t.Parallel()
	src := makePNG(t, MaxSourceDim, 100)
	out, bounded, err := Bound(src)
	if err != nil || bounded || !bytes.Equal(out, src) {
		t.Fatalf("Bound(%dx100): bounded %t, err %v, same bytes %t; want the input untouched",
			MaxSourceDim, bounded, err, bytes.Equal(out, src))
	}
}

// TestBoundScalesAnOversizedImage: a picture over the limit comes back fitted to it with
// its aspect kept. Which encoding it gets is TestBoundPicksTheSmallerEncoding's concern.
func TestBoundScalesAnOversizedImage(t *testing.T) {
	t.Parallel()
	out, bounded, err := Bound(makePNG(t, MaxSourceDim+200, (MaxSourceDim+200)/2))
	if err != nil || !bounded {
		t.Fatalf("Bound: bounded %t, err %v; want a scaled copy", bounded, err)
	}
	format, w, h, err := Probe(out)
	if err != nil || w != MaxSourceDim || h != MaxSourceDim/2 {
		t.Errorf("bounded image probes as %s %dx%d (err %v), want %dx%d",
			format, w, h, err, MaxSourceDim, MaxSourceDim/2)
	}
}

// TestBoundKeepsTransparencyAsPNG: a see-through picture over the limit stays a PNG,
// since a JPEG would paint its clear corner in.
func TestBoundKeepsTransparencyAsPNG(t *testing.T) {
	t.Parallel()
	out, bounded, err := Bound(transparentPNG(t, MaxSourceDim+200, 400))
	if err != nil || !bounded {
		t.Fatalf("Bound: bounded %t, err %v; want a scaled copy", bounded, err)
	}
	format, w, _, err := Probe(out)
	if err != nil || format != "png" || w != MaxSourceDim {
		t.Fatalf("bounded image probes as %s, %d wide (err %v), want png %d", format, w, err, MaxSourceDim)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(10, 10).RGBA(); a != 0 {
		t.Errorf("the clear corner has alpha %d after bounding, want 0", a)
	}
}

// TestBoundLeavesBytesItCannotDecode: what no decoder reads is kept as it arrived; the
// resolver already serves such a source unscaled.
func TestBoundLeavesBytesItCannotDecode(t *testing.T) {
	t.Parallel()
	src := []byte("not a picture at all")
	out, bounded, err := Bound(src)
	if err != nil || bounded || !bytes.Equal(out, src) {
		t.Errorf("Bound(garbage): bounded %t, err %v, same bytes %t; want it untouched",
			bounded, err, bytes.Equal(out, src))
	}
}

// texturedPNG is a photograph's worth of trouble for a lossless encoder: a gradient with
// faint noise on it, which deflate cannot squeeze and a JPEG quantizes away.
func texturedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodePNG(t, texture(w, h))
}

func texture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(2463534242)
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x*255/w) + uint8(seed&7)
			img.Pix[i+1] = uint8(y*255/h) + uint8((seed>>8)&7)
			img.Pix[i+2] = 128 + uint8((seed>>16)&7)
			img.Pix[i+3] = 255
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// hugeHeaderPNG claims a frame of w x h pixels and carries no readable image data, so a
// decode of it can only fail, while its header probes fine.
func hugeHeaderPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	chunk := func(typ string, data []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(data)))
		buf.Write(n[:])
		body := append([]byte(typ), data...)
		buf.Write(body)
		binary.BigEndian.PutUint32(n[:], crc32.ChecksumIEEE(body))
		buf.Write(n[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 2 // 8 bits a sample, RGB
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte("this is not a deflate stream"))
	chunk("IEND", nil)
	return buf.Bytes()
}

// TestBoundPicksTheSmallerEncoding: a scaled copy is stored in whichever of JPEG and PNG
// is smaller, so a photograph becomes a JPEG and a flat graphic stays a PNG.
func TestBoundPicksTheSmallerEncoding(t *testing.T) {
	t.Parallel()
	w, h := MaxSourceDim+200, (MaxSourceDim+200)/2
	for _, tc := range []struct {
		name, want string
		src        []byte
	}{
		{"textured", "jpeg", texturedPNG(t, w, h)},
		{"flat gradient", "png", makePNG(t, w, h)},
	} {
		out, bounded, err := Bound(tc.src)
		if err != nil || !bounded {
			t.Fatalf("%s: bounded %t, err %v; want a scaled copy", tc.name, bounded, err)
		}
		format, gw, _, err := Probe(out)
		if err != nil || format != tc.want || gw != MaxSourceDim {
			t.Errorf("%s: scaled copy probes as %s, %d wide (err %v); want %s %d wide", tc.name, format, gw, err, tc.want, MaxSourceDim)
		}
		if len(out) >= len(tc.src) {
			t.Errorf("%s: scaled copy is %d bytes against %d arriving; want it smaller", tc.name, len(out), len(tc.src))
		}
	}
}

// TestBoundKeepsAnArrivalItCannotShrink: a JPEG just over the limit, encoded leaner than
// the copy would be, is kept as it arrived, since the bound is about bytes.
func TestBoundKeepsAnArrivalItCannotShrink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, texture(MaxSourceDim+100, MaxSourceDim+100), &jpeg.Options{Quality: 60}); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	out, bounded, err := Bound(src)
	if err != nil || bounded || !bytes.Equal(out, src) {
		t.Errorf("Bound(lean jpeg): bounded %t, err %v, same bytes %t; want the arrival kept", bounded, err, bytes.Equal(out, src))
	}
}

// TestBoundRefusesToDecodeAHugeFrame: a small file claiming a frame past MaxDecodePixels
// is kept as it arrived without a decode, which would allocate the whole frame.
func TestBoundRefusesToDecodeAHugeFrame(t *testing.T) {
	t.Parallel()
	src := hugeHeaderPNG(t, 9000, 9000)
	if _, w, h, err := Probe(src); err != nil || w*h <= MaxDecodePixels {
		t.Fatalf("fixture probes as %dx%d (err %v), want a frame past the ceiling %d", w, h, err, MaxDecodePixels)
	}
	out, bounded, err := Bound(src)
	if err != nil || bounded || !bytes.Equal(out, src) {
		t.Errorf("Bound(huge header): bounded %t, err %v, same bytes %t; want the arrival kept untouched", bounded, err, bytes.Equal(out, src))
	}
	if _, _, _, _, err := Thumbnail(src, 64); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("Thumbnail(huge header) err = %v, want a refusal naming the ceiling", err)
	}
}

// withOrientation returns jpg with an EXIF APP1 segment after its start marker that
// says the picture is to be shown at orientation o.
func withOrientation(t *testing.T, jpg []byte, o int) []byte {
	t.Helper()
	if len(jpg) < 2 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatal("fixture is not a JPEG")
	}
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01" + // big-endian, magic, IFD0 at 8, one entry
		"\x01\x12\x00\x03\x00\x00\x00\x01" + string([]byte{0, byte(o), 0, 0}) + // Orientation, SHORT, count 1, value
		"\x00\x00\x00\x00") // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	out := append([]byte{0xFF, 0xD8}, seg...)
	return append(out, jpg[2:]...)
}

// markedJPEG is a w by h blue picture with a red block in its top left corner.
func markedJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{0, 0, 255, 255}
			if x < w/10 && y < h/5 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func redness(t *testing.T, data []byte, x, y int) (red bool) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, b, _ := img.At(x, y).RGBA()
	return r > b
}

// TestOrientation reads the tag in both byte orders and answers 1 for everything else.
func TestOrientation(t *testing.T) {
	t.Parallel()
	plain := markedJPEG(t, 40, 20, 80)
	if o := orientation(plain); o != 1 {
		t.Errorf("orientation(no exif) = %d, want 1", o)
	}
	if o := orientation(withOrientation(t, plain, 6)); o != 6 {
		t.Errorf("orientation(exif 6) = %d, want 6", o)
	}
	little := []byte("Exif\x00\x00II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00")
	if o := exifOrientation(little); o != 8 {
		t.Errorf("little-endian orientation = %d, want 8", o)
	}
	if o := orientation([]byte("not a jpeg")); o != 1 {
		t.Errorf("orientation(garbage) = %d, want 1", o)
	}
}

// TestFitTurnsAJPEGUpright: a picture tagged to be shown turned is scaled turned, so a
// phone photograph of a sleeve is stored and thumbnailed the way the phone shows it.
func TestFitTurnsAJPEGUpright(t *testing.T) {
	t.Parallel()
	src := withOrientation(t, markedJPEG(t, 300, 100, 90), 6) // the 0th row is the visual right side
	out, _, w, h, err := Thumbnail(src, 300)
	if err != nil || w != 100 || h != 300 {
		t.Fatalf("thumbnail = %dx%d (err %v), want the picture turned to 100x300", w, h, err)
	}
	// Turned a quarter clockwise, the red top-left block sits top-right.
	if !redness(t, out, 100-5, 5) || redness(t, out, 5, 5) {
		t.Errorf("the red block did not move to the top right corner")
	}
	bounded, scaled, err := Bound(withOrientation(t, texturedJPEG(t, MaxSourceDim+200, 600), 6))
	if err != nil || !scaled {
		t.Fatalf("Bound: scaled %t, err %v; want a turned, scaled copy", scaled, err)
	}
	if _, bw, bh, err := Probe(bounded); err != nil || bh != MaxSourceDim || bw >= bh {
		t.Errorf("bounded copy is %dx%d (err %v), want it turned, %d tall", bw, bh, err, MaxSourceDim)
	}
}

func texturedJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, texture(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestBoxReduceAverages: each block becomes the mean of its pixels, edges included.
func TestBoxReduceAverages(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 5, 4))
	for y := range 4 {
		for x := range 5 {
			v := uint8(0)
			if x%2 == 1 {
				v = 200
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	out := boxReduce(img, 2)
	if out.Rect.Dx() != 3 || out.Rect.Dy() != 2 {
		t.Fatalf("reduced to %dx%d, want 3x2 with the odd column kept as its own block", out.Rect.Dx(), out.Rect.Dy())
	}
	r, _, _, _ := out.At(0, 0).RGBA()
	if got := r >> 8; got != 100 {
		t.Errorf("a black and white block averaged to %d, want 100", got)
	}
	r, _, _, _ = out.At(2, 0).RGBA()
	if got := r >> 8; got != 0 {
		t.Errorf("the lone black edge column averaged to %d, want 0", got)
	}
}

// TestReduceFactor pins the block size against the box.
func TestReduceFactor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ long, box, want int }{
		{1000, 2000, 1}, {4000, 2000, 1}, {4001, 2000, 2}, {8000, 2000, 2}, {8001, 2000, 3}, {2000, 64, 16},
	} {
		if got := reduceFactor(tc.long, tc.box); got != tc.want {
			t.Errorf("reduceFactor(%d, %d) = %d, want %d", tc.long, tc.box, got, tc.want)
		}
	}
}
