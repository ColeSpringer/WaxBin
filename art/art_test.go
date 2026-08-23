package art

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
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

// TestDescribe covers the three answers the one describer gives its four callers: a
// decodable image, an exotic one recognized by magic with no dimensions, and bytes
// nothing recognizes, which still get a content address.
func TestDescribe(t *testing.T) {
	data := makePNG(t, 64, 48)
	got := Describe(data)
	if got.Format != "png" || got.Width != 64 || got.Height != 48 {
		t.Errorf("describe = %s %dx%d, want png 64x48", got.Format, got.Width, got.Height)
	}
	if got.Hash != Hash(data) {
		t.Errorf("describe hash = %q, want the content address", got.Hash)
	}

	avif := append([]byte{0, 0, 0, 0x20}, []byte("ftypavif")...)
	exotic := Describe(avif)
	if exotic.Format != "avif" || exotic.Width != 0 || exotic.Height != 0 {
		t.Errorf("exotic describe = %s %dx%d, want avif with unknown dimensions",
			exotic.Format, exotic.Width, exotic.Height)
	}

	junk := Describe([]byte("not an image"))
	if junk.Format != "" {
		t.Errorf("junk describe format = %q, want empty (the neither-decoded-nor-recognized signal)", junk.Format)
	}
	if junk.Hash != Hash([]byte("not an image")) {
		t.Error("junk describe dropped the content address")
	}
}

// TestNormalizeFormat covers the three shapes a caller names a format in, the folds
// that keep one format from storing under two tokens, and everything that names no
// image and so must not become one.
func TestNormalizeFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"jpeg", "jpeg"},
		{"JPEG", "jpeg"},
		{"  png  ", "png"},
		{"jpg", "jpeg"},   // ID3v2.2's three-character code
		{"jpe", "jpeg"},   //
		{".JPG", "jpeg"},  // a filename extension
		{".tiff", "tiff"}, //
		{"tif", "tiff"},   // one format, one token
		{"image/jpeg", "jpeg"},
		{"image/jpg", "jpeg"},
		{"image/jpeg; charset=binary", "jpeg"},
		{"image/jpeg; charset", "jpeg"}, // parameters ParseMediaType refuses
		{"IMAGE/PNG", "png"},
		{"image/x-png", "png"},         // the spellings older servers send
		{"image/x-ms-bmp", "bmp"},      //
		{"image/x-windows-bmp", "bmp"}, //
		{"image/pjpeg", "jpeg"},        // progressive JPEG's legacy media type
		{"pjpeg", "jpeg"},              //
		{"jfif", "jpeg"},               // an extension that reaches here as a bare token
		{".jfif", "jpeg"},              //
		{"image/heif", "heic"},         // one ISOBMFF format, one token
		{"heif", "heic"},
		{"image/bmp", "bmp"},
		{"image/tiff", "tiff"},
		{"image/jxl", "jxl"}, // no decoder, still the only name it will have
		{"image/svg+xml", "svg+xml"},
		{"", ""},
		{"   ", ""},
		{"application/octet-stream", ""}, // an undeclared blob names no format
		{"text/html", ""},                // nor does an error page
		{"image/*", ""},                  // nor does the Accept header echoed back
		{"image/jpeg, image/jpeg", ""},   // nor does a doubled header
		{"-->", ""},                      // ID3v2's "the body is a URL" sentinel
		{"image/<script>alert(1)</script>", ""},
		{"image/" + strings.Repeat("a", maxFormatToken+1), ""},
	}
	for _, c := range cases {
		got := NormalizeFormat(c.in)
		if got != c.want {
			t.Errorf("NormalizeFormat(%q) = %q, want %q", c.in, got, c.want)
		}
		// The store documents its format as already normalized, which is only worth
		// anything if normalizing twice is normalizing once.
		if again := NormalizeFormat(got); again != got {
			t.Errorf("NormalizeFormat is not idempotent on %q: %q then %q", c.in, got, again)
		}
	}
}

// TestProbeDecodesBMPAndTIFF pins the reason the format hint is a rescue and not the
// main path: x/image ships both decoders, so the two formats a cover most often arrives
// in without one now decode here, with the dimensions a thumbnail needs.
func TestProbeDecodesBMPAndTIFF(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 37, 23))
	src.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})

	var b bytes.Buffer
	if err := bmp.Encode(&b, src); err != nil {
		t.Fatalf("encode bmp: %v", err)
	}
	if got := Describe(b.Bytes()); got.Format != "bmp" || got.Width != 37 || got.Height != 23 {
		t.Errorf("bmp describe = %s %dx%d, want bmp 37x23", got.Format, got.Width, got.Height)
	}
	if _, _, w, h, err := Thumbnail(b.Bytes(), 16); err != nil || w != 16 || h != 10 {
		t.Errorf("bmp thumbnail = %dx%d (err %v), want 16x10", w, h, err)
	}

	var tb bytes.Buffer
	if err := tiff.Encode(&tb, src, nil); err != nil {
		t.Fatalf("encode tiff: %v", err)
	}
	if got := Describe(tb.Bytes()); got.Format != "tiff" || got.Width != 37 || got.Height != 23 {
		t.Errorf("tiff describe = %s %dx%d, want tiff 37x23", got.Format, got.Width, got.Height)
	}

	// A BMP signature over bytes that are not a BMP stays unrecognized, which is what
	// keeps the refusal a fact about the bytes rather than about the first two of them.
	junk := append([]byte("BM"), make([]byte, 60)...)
	if got := Describe(junk); got.Format != "" {
		t.Errorf("junk with a BM prefix described as %q, want unrecognized", got.Format)
	}
}

// TestDisplayable pins the membership of the conservative floor, that a media type
// folds to the same answer as its token, and the two exclusions that carry weight:
// TIFF, which decodes here and so is the format a sized resolve re-encodes, and AVIF,
// which older WebViews and Safari before 16 cannot paint.
func TestDisplayable(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"jpeg", true},
		{"png", true},
		{"gif", true},
		{"webp", true},
		{"bmp", true},
		{"image/jpeg", true},                 // a Content-Type folds like its token
		{"image/jpeg; charset=binary", true}, //
		{"jpg", true},                        // and so does a bare extension
		{"image/x-ms-bmp", true},             //
		{"tiff", false},                      // decodes here, outside the floor
		{"image/tiff", false},                //
		{"avif", false},                      // deliberately out: no decoder, and older clients cannot paint it
		{"heic", false},                      //
		{"svg+xml", false},
		{"jxl", false},
		{"", false},          // the caller named nothing
		{"text/html", false}, // and neither does an error page
		{"application/octet-stream", false},
	}
	for _, c := range cases {
		if got := Displayable(c.in); got != c.want {
			t.Errorf("Displayable(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestThumbnailOutputIsDisplayable is the invariant the sized resolve rests on. It
// re-encodes a source a client cannot paint, so if Thumbnail could ever emit a format
// outside the floor the replacement bytes would be as unpaintable as the ones they
// replaced.
func TestThumbnailOutputIsDisplayable(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 20, 12))
	src.Set(0, 0, color.RGBA{R: 9, G: 8, B: 7, A: 255})

	var g bytes.Buffer
	if err := gif.Encode(&g, src, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	var b bytes.Buffer
	if err := bmp.Encode(&b, src); err != nil {
		t.Fatalf("encode bmp: %v", err)
	}
	var tb bytes.Buffer
	if err := tiff.Encode(&tb, src, nil); err != nil {
		t.Fatalf("encode tiff: %v", err)
	}

	sources := map[string][]byte{
		"jpeg": makeJPEG(t, 20, 12),
		"png":  makePNG(t, 20, 12),
		"gif":  g.Bytes(),
		"bmp":  b.Bytes(),
		"tiff": tb.Bytes(),
	}
	for name, data := range sources {
		for _, box := range []int{8, 20, 200} {
			_, format, _, _, err := Thumbnail(data, box)
			if err != nil {
				t.Errorf("Thumbnail(%s, %d): %v", name, box, err)
				continue
			}
			if !Displayable(format) {
				t.Errorf("Thumbnail(%s, %d) emitted %q, which is outside the displayable floor", name, box, format)
			}
		}
	}
}

func TestRung(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 0},   // no box asked for: nothing to round
		{-1, 0},  //
		{1, 32},  // under the floor: the smallest rung is what gets generated
		{31, 32}, //
		{32, 32}, // an exact rung answers as itself
		{192, 192},
		{2048, 2048},
		{33, 48},     // just past a rung climbs to the next
		{129, 192},   // worst over-delivery in a 1.5 step, 1.488x
		{1025, 1536}, // and the worst anywhere on the ladder, 1.499x, in the hero range
		{187, 192},   // the responsive-layout box the cache used to key on
		{193, 256},   // and its neighbour, one rung up
		{2049, 2049}, // past the top: bespoke, so served as asked rather than rounded
		{100000, 100000},
	}
	for _, c := range cases {
		if got := Rung(c.in); got != c.want {
			t.Errorf("Rung(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestRungsAreOrderedAndRoundTrip pins the ladder itself rather than one walk of it:
// every rung must round to itself, and the list must ascend, since Rung answers by
// climbing it.
func TestRungsAreOrderedAndRoundTrip(t *testing.T) {
	rungs := Rungs()
	if len(rungs) == 0 {
		t.Fatal("Rungs() is empty")
	}
	for i, r := range rungs {
		if r <= 0 {
			t.Errorf("Rungs()[%d] = %d, want a positive box", i, r)
		}
		if i > 0 && r <= rungs[i-1] {
			t.Errorf("Rungs()[%d] = %d does not ascend past %d", i, r, rungs[i-1])
		}
		if got := Rung(r); got != r {
			t.Errorf("Rung(%d) = %d, want the rung itself", r, got)
		}
	}
}

// TestRungsIsACopy keeps a caller from editing the ladder out from under the resolver.
func TestRungsIsACopy(t *testing.T) {
	first := Rungs()
	first[0] = 9999
	if second := Rungs(); second[0] == 9999 {
		t.Error("Rungs() handed out the backing array; a caller can rewrite the ladder")
	}
}
