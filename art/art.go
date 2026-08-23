// Package art contains WaxBin's pure-Go image handling for the read-side art
// resolver: content hashing for the content-addressed store, format and dimension
// probing, thumbnail generation (decode, scale to fit, re-encode), and folding a
// caller-supplied format name to the short token the store holds.
// JPEG/PNG/GIF use standard library decoders; WebP, BMP and TIFF use x/image.
// Formats without a registered decoder, such as AVIF or HEIC, are stored and served
// unscaled by the resolver, and Displayable names the formats a resolver may hand back
// as stored rather than re-encoding. No CGO is used.
package art

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"mime"
	"strings"

	_ "golang.org/x/image/bmp" // register the BMP decoder with image.Decode
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // register the TIFF decoder with image.Decode
	_ "golang.org/x/image/webp" // register the WebP decoder with image.Decode

	_ "image/gif" // register the GIF decoder with image.Decode
)

// jpegQuality is the thumbnail JPEG quality. 85 is a standard quality/size
// tradeoff for cover-art thumbnails.
const jpegQuality = 85

// Hash returns the content-address key for image bytes: the hex SHA-256. Two
// files with identical bytes, such as the same cover embedded in every track of
// an album, produce the same hash and are stored once.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Probe reports an image's format and pixel dimensions without decoding the whole
// image. It returns an error for an unrecognized or truncated image.
func Probe(data []byte) (format string, width, height int, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, fmt.Errorf("probing image: %w", err)
	}
	return format, cfg.Width, cfg.Height, nil
}

// SniffExotic recognizes an ISOBMFF-based image WaxBin has no pure-Go decoder for
// (AVIF, HEIC/HEIF) by its `ftyp` brand, returning the short format token. Such an
// image is still stored, deduped, and served, but always at full size, since it cannot
// be thumbnailed. Which route it takes there depends on whether anything else supplied
// its dimensions: with none it never reaches a decoder, while a container that declared
// them (a FLAC picture block, say) sends the first resolve at each box through a decode
// that fails and falls back. It reports false for anything the standard decoders already
// handle or do not recognize.
func SniffExotic(data []byte) (format string, ok bool) {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return "", false
	}
	switch string(data[8:12]) {
	case "avif", "avis":
		return "avif", true
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
		return "heic", true
	}
	return "", false
}

// NormalizeFormat folds a caller-supplied image format to the short token
// ArtImage.Format holds. It accepts the token itself ("jpeg"), a bare extension
// ("jpg"), or a media type from a transport ("image/jpeg; charset=binary"), and falls
// back to an image media type's subtype for a format WaxBin has no decoder for, since
// that is still the only description the stored cover will ever have.
//
// Anything it cannot read as a format normalizes to "", which every caller reads as
// "the caller named nothing" and answers with its own policy. That covers a media type
// naming something other than an image, and any subtype outside the token shape below:
// what reaches here is a header a remote server chose or a flag a person typed, and
// the result is stored, reported over the proxy, and printed, so an unbounded string
// has no business becoming a format. Its own output always normalizes to itself.
func NormalizeFormat(s string) string {
	s = strings.TrimSpace(s)
	if mt, _, err := mime.ParseMediaType(s); err == nil {
		s = mt
	}
	// A media type ParseMediaType refused still arrives with its parameters attached.
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	if typ, sub, ok := strings.Cut(s, "/"); ok {
		// A media type names a format only when it is an image type. text/html and
		// application/octet-stream are what a server hands back an error page or an
		// undeclared blob under, and reading "html" or "octet-stream" as a format would
		// store the very bytes an unrecognized-image refusal exists to turn away.
		if !strings.EqualFold(strings.TrimSpace(typ), "image") {
			return ""
		}
		s = sub
	}
	s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, ".")))
	if !formatToken(s) {
		return ""
	}
	// Fold onto the names image.Decode reports, so one format cannot store under two
	// tokens. ID3v2.2 writes three-character codes, plenty of taggers write JPG, the
	// x- spellings are what older servers send, and pjpeg is progressive JPEG's legacy
	// media type.
	switch s {
	case "jpg", "jpe", "pjpeg", "jfif":
		return "jpeg"
	case "tif":
		return "tiff"
	case "heif":
		return "heic"
	case "x-png":
		return "png"
	case "x-bmp", "x-ms-bmp", "x-windows-bmp":
		return "bmp"
	case "x-tiff":
		return "tiff"
	}
	return s
}

// Displayable reports whether a format is one every mainstream client, browser and
// native toolkit alike, has decoded for years. A positive size in a resolve consults it
// before answering with the stored source unscaled, so a cover that fits the requested
// box but is held in a format outside the set is re-encoded at its own size rather than
// handed back as stored.
//
// The set is a conservative floor rather than an exhaustive claim about what any given
// client can paint. A consumer with a wider decoder is free to ignore it and read
// ArtBlob.Format instead. TIFF is the one format WaxBin decodes today that is outside
// it, which makes it the case a sized resolve re-encodes; a cover in a format nothing
// here decodes cannot be re-encoded at all, so it is served as stored and the consumer
// decides what to draw with it. That residual is why this is exported rather than kept
// private to the store: the consumer holding it should not have to keep a second list
// beside this one.
//
// It folds its argument first, so a caller holding a transport's Content-Type gets the
// same answer as one holding a stored token.
func Displayable(format string) bool {
	switch NormalizeFormat(format) {
	case "jpeg", "png", "gif", "webp", "bmp":
		return true
	}
	return false
}

// maxFormatToken bounds a format at more than any real one needs and far less than a
// header or a flag can carry.
const maxFormatToken = 32

// formatToken reports whether s has the shape of a media type's subtype: lowercase
// letters, digits, and the three punctuation marks the real ones use (svg+xml,
// x-icon, jpeg2000.1). It is what turns "*", "jpeg, image/jpeg", an HTML fragment, and
// ID3v2's "-->" URL sentinel into "the caller named nothing".
func formatToken(s string) bool {
	if s == "" || len(s) > maxFormatToken {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// Info is what the store needs to know about an image it is about to hold: the content
// address, the format it was recognized as, and its pixel dimensions.
type Info struct {
	Hash   string
	Format string
	Width  int
	Height int
}

// Describe reports an image's Info: the content address always, and the format and
// dimensions when the bytes decode, or the magic-sniffed format alone for an exotic
// AVIF/HEIC image whose dimensions stay unknown. It never fails, because an
// unrecognized image still has a content address and the resolver already serves such
// a source unscaled. An empty Format is the single signal that the bytes neither
// decoded nor were recognized, which each caller answers with its own policy.
func Describe(data []byte) Info {
	info := Info{Hash: Hash(data)}
	format, w, h, err := Probe(data)
	if err != nil {
		if f, ok := SniffExotic(data); ok {
			info.Format = f
		}
		return info
	}
	info.Format, info.Width, info.Height = format, w, h
	return info
}

// Thumbnail decodes src and produces a thumbnail scaled to fit within a
// maxDim x maxDim box, preserving aspect ratio. It never upscales: a source
// already within the box is returned re-encoded at its own size. The output is
// JPEG for a JPEG source and PNG otherwise, so PNG/GIF/WebP transparency survives.
// It returns the encoded bytes, the output format, and the output dimensions.
func Thumbnail(src []byte, maxDim int) (out []byte, format string, w, h int, err error) {
	if maxDim <= 0 {
		return nil, "", 0, 0, fmt.Errorf("thumbnail: non-positive max dimension %d", maxDim)
	}
	img, srcFormat, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decoding image: %w", err)
	}
	b := img.Bounds()
	tw, th := fitDimensions(b.Dx(), b.Dy(), maxDim)

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	if tw == b.Dx() && th == b.Dy() {
		// Same size in and out, which is every box at or above the source's own longest
		// side. Kernel.Scale has no equal-size case and would run a full resample whose
		// weights all resolve to identity, so copy instead.
		xdraw.Copy(dst, image.Point{}, img, b, xdraw.Src, nil)
	} else {
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	}

	var buf bytes.Buffer
	outFormat := "png"
	if srcFormat == "jpeg" {
		outFormat = "jpeg"
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", 0, 0, fmt.Errorf("encoding jpeg thumbnail: %w", err)
		}
	} else {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, "", 0, 0, fmt.Errorf("encoding png thumbnail: %w", err)
		}
	}
	return buf.Bytes(), outFormat, tw, th, nil
}

// fitDimensions returns the largest width x height that fits in a maxDim box while
// preserving the source aspect ratio, never exceeding the source size (no
// upscaling) and never collapsing a non-empty image to a zero dimension.
func fitDimensions(w, h, maxDim int) (int, int) {
	if w <= 0 || h <= 0 {
		return 1, 1
	}
	long := w
	if h > w {
		long = h
	}
	if long <= maxDim {
		return w, h // already within the box: do not upscale
	}
	scale := float64(maxDim) / float64(long)
	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}
