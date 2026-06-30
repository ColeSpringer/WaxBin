// Package art contains WaxBin's pure-Go image handling for the read-side art
// resolver: content hashing for the content-addressed store, format and dimension
// probing, and thumbnail generation (decode, scale to fit, re-encode).
// JPEG/PNG/GIF use standard library decoders; WebP uses x/image. Formats without
// a registered decoder, such as AVIF or HEIC, are stored and served unscaled by
// the resolver. No CGO is used.
package art

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
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
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)

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
