// Package caps detects optional external helpers (ffmpeg, fpcalc) used by the
// analyze pass. The pure-Go baseline always works; helpers are capability-
// detected, reported by doctor, and never required for core use.
package caps

import (
	"os/exec"
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
