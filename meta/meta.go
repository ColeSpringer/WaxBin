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
	// Lyrics is the file's embedded lyrics (unsynced USLT and/or synced SYLT), or
	// nil when it carries none. A sibling .lrc sidecar, parsed by the scanner, takes
	// precedence over this.
	Lyrics *model.Lyrics
	// CoverArt is the file's embedded front-cover image (raw bytes + format), or nil
	// when it embeds none. The scanner finalizes its hash and dimensions and falls
	// back to a directory cover image when this is absent.
	CoverArt *model.ArtImage
	// ItemPIDHint is the value of the file's WAXBIN_ITEM_PID tag, if present. It is a
	// rebuild-only hint for restoring the backing item's original PID; identity stays
	// essence-first (the tag is copyable), so the store adopts it only when unambiguous.
	ItemPIDHint string
	// Diagnostics are observations about the file worth persisting (an unsupported
	// container, truncated audio, a legacy-only tag fallback). They are born here,
	// like Lyrics, rather than in the scanner from an os.Stat like an AuxObservation,
	// so they share only the model-input-to-store leg of the route with those.
	//
	// FilePID and DisplayPath are left unset: the reader is given a path, not a
	// catalog identity. The store fills them.
	Diagnostics []model.FileDiagnostic
}

// Reader reads tags, properties, and the essence hash from a file. It must never
// decode PCM (scanning is I/O-bound by contract).
type Reader interface {
	Read(ctx context.Context, path string) (*FileMeta, error)
}

// normalizeCodec folds WaxLabel's canonical codec name to WaxBin's lowercase
// catalog key. WaxLabel reports names such as "PCM", "MP3", and "Vorbis"; the
// catalog stores "pcm", "mp3", and "vorbis".
//
// PCM keeps the plain "pcm" key regardless of container. Nothing routes on the
// codec any more (WaxFlow sniffs the container's content and decodes it directly),
// so PCM in AIFF or MP4 is "pcm" and decodes like any other input. Non-WAV PCM was
// previously keyed by container to steer it to the ffmpeg subprocess; that, and the
// bug where PCM-in-MP4 keyed to an un-analyzable "mp4", are both gone.
func normalizeCodec(c string) string {
	s := strings.ToLower(strings.TrimSpace(c))
	switch s {
	case "mpeg audio", "mpeg", "mp2": // pre-frame-sync fallback labeling
		return "mp3"
	case "pcm":
		return "pcm"
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

// firstNonEmpty returns the first argument that is non-empty after trimming, with
// surrounding whitespace removed so it does not leak into stored values. It matches
// the same helper in scan and organize.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
