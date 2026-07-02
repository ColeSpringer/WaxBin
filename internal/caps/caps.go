// Package caps detects optional external helpers (ffmpeg, fpcalc) used by the
// analyze pass. The pure-Go baseline always works; helpers are capability-
// detected, reported by doctor, and never required for core use.
package caps

import (
	"os/exec"
	"strings"
	"sync"
)

// Caps reports which optional helpers are available on PATH. The pure-Go
// decoders and fingerprint do not appear here; they are always present.
type Caps struct {
	FFmpeg     bool   // ffmpeg: PCM decode for AAC/ALAC/Opus + ebur128 (analysis)
	FFmpegPath string // resolved path when FFmpeg is true
	Fpcalc     bool   // fpcalc: Chromaprint fingerprint (preferred grouping; AcoustID)
	FpcalcPath string
}

var (
	once   sync.Once
	cached Caps
)

// Detect probes PATH for the optional helpers once and caches the result. The
// look-ups are cheap, and a helper appearing/disappearing mid-process is not a
// case worth re-probing on every analyze file.
func Detect() Caps {
	once.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			cached.FFmpeg, cached.FFmpegPath = true, p
		}
		if p, err := exec.LookPath("fpcalc"); err == nil {
			cached.Fpcalc, cached.FpcalcPath = true, p
		}
	})
	return cached
}

// ImageCaps reports which exotic image formats an external helper can thumbnail.
// AVIF and HEIC are detected INDEPENDENTLY: ffmpeg on PATH does not imply HEIC
// support (HEVC is often omitted for patent reasons; AVIF's AV1 is royalty-free and
// far more common), so each is gated on its own codec.
type ImageCaps struct {
	FFmpegPath string
	AVIF       bool // ffmpeg can decode AVIF (has an av1 decoder)
	HEIC       bool // ffmpeg can decode HEIC/HEIF (has an hevc decoder)
}

var (
	imgOnce   sync.Once
	imgCached ImageCaps
)

// ImageSupport reports the external thumbnailing support for exotic image formats,
// probed once and cached. It inspects ffmpeg's decoder list (a cheap proxy for a
// tiny-sample decode) for the av1 (AVIF) and hevc (HEIC) decoders. It is best-effort
// and never required; the resolver serves such an image unscaled when unsupported.
func ImageSupport() ImageCaps {
	imgOnce.Do(func() {
		c := Detect()
		if !c.FFmpeg {
			return
		}
		imgCached.FFmpegPath = c.FFmpegPath
		out, err := exec.Command(c.FFmpegPath, "-hide_banner", "-decoders").Output()
		if err != nil {
			return
		}
		imgCached.AVIF, imgCached.HEIC = parseImageDecoders(out)
	})
	return imgCached
}

// parseImageDecoders scans `ffmpeg -decoders` output for the codecs AVIF and HEIC
// depend on. It matches decoder-name VARIANTS, not just the native names: AV1 is
// often exposed only as libdav1d/libaom-av1 (native "av1" arrived ~FFmpeg 5.1) and
// HEVC as libde265. Those substrings are distinctive enough not to false-match.
func parseImageDecoders(out []byte) (avif, heic bool) {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[1]
		if strings.Contains(name, "av1") { // av1, libdav1d, libaom-av1
			avif = true
		}
		if strings.Contains(name, "hevc") || strings.Contains(name, "de265") { // hevc, libde265
			heic = true
		}
	}
	return avif, heic
}
