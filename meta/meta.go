// Package meta reads catalog metadata, cheap audio properties, and the
// tag-independent audio-essence hash without decoding PCM. It adapts WaxLabel,
// which parses tags, artwork, chapters, lyrics, and essence digests for supported
// containers. PCM decoding belongs to the analyze pass.
package meta

import (
	"context"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// FileMeta is everything the scanner needs from one parse: the tag/property set
// plus the tag-independent essence hash. EssenceHash is empty when the container
// carries no hashable audio essence (e.g. a malformed file); the scanner then
// falls back to the content hash so the file is still cataloged.
type FileMeta struct {
	Tags        model.Tags
	EssenceHash string
}

// Reader reads tags, properties, and the essence hash from a file. It must never
// decode PCM (scanning is I/O-bound by contract).
type Reader interface {
	Read(ctx context.Context, path string) (*FileMeta, error)
}

// normalizeCodec folds WaxLabel's canonical codec name to WaxBin's lowercase
// registry key. WaxLabel reports names such as "PCM", "MP3", and "Vorbis"; the
// analyze registry binds "pcm", "mp3", and "vorbis".
//
// container disambiguates PCM. The pure-Go "pcm" decoder reads RIFF/WAVE only.
// PCM in another container, notably AIFF, must use its container key so analysis
// routes to ffmpeg or skips it cleanly when ffmpeg is absent.
func normalizeCodec(c, container string) string {
	s := strings.ToLower(strings.TrimSpace(c))
	switch s {
	case "mpeg audio", "mpeg", "mp2": // pre-frame-sync fallback labeling
		return "mp3"
	case "pcm":
		switch strings.ToLower(strings.TrimSpace(container)) {
		case "", "wav", "wave", "riff":
			return "pcm"
		default:
			return strings.ToLower(strings.TrimSpace(container)) // e.g. "aiff" -> ffmpeg path
		}
	default:
		return s
	}
}

// firstYear returns the leading 4-digit year of the first non-empty date string,
// or 0 when none parse. WaxLabel dates are ISO-8601 partials ("2024",
// "2024-05", "2024-05-03").
func firstYear(dates ...string) int {
	for _, d := range dates {
		if y := leadingYear(d); y > 0 {
			return y
		}
	}
	return 0
}

// leadingYear parses a 4-digit year prefix, requiring exactly four digits before
// any separator so a bare "24" is not misread as the year 24.
func leadingYear(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	y := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		y = y*10 + int(c-'0')
	}
	// A fifth digit means this is not a year (e.g. a raw timestamp).
	if len(s) > 4 && s[4] >= '0' && s[4] <= '9' {
		return 0
	}
	return y
}

// first returns the first element of s, or "" when empty.
func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
