package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/internal/caps"
	"github.com/colespringer/waxbin/waxerr"
)

// firstLine returns the first line of s, so a multi-line ffmpeg diagnostic collapses
// to one actionable message in the wrapped error.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// exoticSupported reports whether an external helper can thumbnail this format
// (AVIF/HEIC), detected per-format because ffmpeg on PATH does not imply HEIC support.
func exoticSupported(format string) bool {
	if !art.IsExoticFormat(format) {
		return false
	}
	ic := caps.ImageSupport()
	if ic.FFmpegPath == "" {
		return false
	}
	switch format {
	case "avif":
		return ic.AVIF
	case "heic", "heif":
		return ic.HEIC
	}
	return false
}

// exoticThumbnail decodes and scales an AVIF/HEIC image to a PNG thumbnail via
// ffmpeg. The source is written to a temp file because ISOBMFF needs a seekable
// input, and a single scaled frame is read back as PNG. It is bounded and
// best-effort; its output is cached by the resolver so ffmpeg is shelled once per
// image, never per browse request.
func (s *Store) exoticThumbnail(ctx context.Context, srcData []byte, size int) ([]byte, string, int, int, error) {
	const op = "store.ResolveArt"
	ic := caps.ImageSupport()
	if ic.FFmpegPath == "" {
		return nil, "", 0, 0, waxerr.New(waxerr.CodeUnsupported, op, "no image thumbnail helper available")
	}
	tmp, err := os.CreateTemp("", "waxart-*")
	if err != nil {
		return nil, "", 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(srcData); err != nil {
		tmp.Close()
		return nil, "", 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	tmp.Close()

	vf := fmt.Sprintf("scale='min(iw,%d)':'min(ih,%d)':force_original_aspect_ratio=decrease", size, size)
	cmd := exec.CommandContext(ctx, ic.FFmpegPath,
		"-hide_banner", "-loglevel", "error", "-i", tmp.Name(),
		"-vf", vf, "-frames:v", "1", "-f", "image2", "-c:v", "png", "pipe:1")
	var buf, stderr bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Include ffmpeg's diagnostics (unsupported codec, bad input) so a decode
		// failure is troubleshootable rather than a bare "exit status 1".
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, "", 0, 0, waxerr.Wrapf(waxerr.CodeIO, op, err, "ffmpeg: %s", firstLine(msg))
		}
		return nil, "", 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	format, w, h, err := art.Probe(buf.Bytes())
	if err != nil {
		return nil, "", 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return buf.Bytes(), format, w, h, nil
}
