// Package meta reads catalog metadata and cheap audio properties without
// decoding PCM. The default reader parses ID3v2 for MP3 and Vorbis comments plus
// STREAMINFO for FLAC. Other formats still get a filename-derived title so
// scanning can catalog them deterministically.
package meta

import (
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// TagReader reads tags and cheap audio properties from a file. It must never
// decode PCM (scanning is I/O-bound by contract).
type TagReader interface {
	ReadTags(path string) (*model.Tags, error)
}

// DefaultReader is the built-in pure-Go reader.
type DefaultReader struct{}

// NewReader returns the default metadata reader.
func NewReader() *DefaultReader { return &DefaultReader{} }

// ReadTags dispatches on file extension, always returning a usable *Tags (a
// filename-derived title at minimum) even when no tags are present.
func (DefaultReader) ReadTags(path string) (*model.Tags, error) {
	ext := strings.ToLower(filepath.Ext(path))
	t := &model.Tags{Container: containerForExt(ext), Codec: codecForExt(ext)}

	switch ext {
	case ".mp3":
		_ = readID3v2(path, t) // best-effort; absent/garbled tags just leave fields empty
	case ".flac":
		_ = readFLAC(path, t)
	case ".wav", ".wave":
		_ = readWAV(path, t) // header-only: audio properties + duration, no PCM
	}

	if strings.TrimSpace(t.Title) == "" {
		t.Title = titleFromPath(path)
	}
	return t, nil
}

// titleFromPath derives a display title from the filename (extension stripped).
func titleFromPath(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "Untitled"
	}
	return base
}

func containerForExt(ext string) string {
	switch ext {
	case ".mp3":
		return "mpeg"
	case ".flac":
		return "flac"
	case ".wav":
		return "wav"
	case ".ogg", ".oga":
		return "ogg"
	case ".m4a", ".m4b", ".mp4", ".aac":
		return "mp4"
	case ".opus":
		return "ogg"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

func codecForExt(ext string) string {
	switch ext {
	case ".mp3":
		return "mp3"
	case ".flac":
		return "flac"
	case ".wav":
		return "pcm"
	case ".ogg", ".oga":
		return "vorbis"
	case ".opus":
		return "opus"
	case ".m4a", ".m4b", ".mp4", ".aac":
		return "aac"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

// parseLeadingInt parses the leading integer of values like "3", "3/12", or
// "2024-05" and returns 0 when there is no leading digit.
func parseLeadingInt(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	got := false
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		got = true
	}
	if !got {
		return 0
	}
	return n
}
