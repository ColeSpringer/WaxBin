package organize

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Canonical buckets for missing grouping fields, so every layout renders the
// same placeholder rather than an empty path segment.
const (
	unknownArtist = "Unknown Artist"
	unknownAlbum  = "Unknown Album"
	unknownTitle  = "Untitled"
	unknownGenre  = "Unknown Genre"
)

// fieldVal is a template field that formats either as text or as a zero-paddable
// number.
type fieldVal struct {
	s     string
	n     int
	isNum bool
}

func (f fieldVal) format(spec string) string {
	if !f.isNum {
		return f.s
	}
	if len(spec) > 0 && spec[0] == '0' {
		if width, err := strconv.Atoi(spec); err == nil {
			return fmt.Sprintf("%0*d", width, f.n)
		}
	}
	return strconv.Itoa(f.n)
}

// RenderRelPath renders an item's destination path relative to the library root
// using the profile's track template, sanitizing each path segment.
func RenderRelPath(p Profile, item *model.ItemView) (string, error) {
	fields := itemFields(item)
	rendered, err := renderTemplate(p.TrackTemplate, fields)
	if err != nil {
		return "", err
	}

	parts := strings.Split(rendered, "/")
	clean := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = sanitizeSegment(seg)
		if seg != "" {
			clean = append(clean, seg)
		}
	}
	if len(clean) == 0 {
		return "", waxerr.New(waxerr.CodeInvalid, "organize.RenderRelPath", "template produced an empty path")
	}
	return filepath.Join(clean...), nil
}

func itemFields(item *model.ItemView) map[string]fieldVal {
	albumArtist := firstNonEmpty(item.AlbumArtist, item.Artist, unknownArtist)
	artist := firstNonEmpty(item.Artist, item.AlbumArtist, unknownArtist)
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(item.DisplayPath)), ".")
	if ext == "" {
		ext = item.Codec
	}
	// Sanitize each text value here, before substitution, so a path separator
	// inside a field (e.g. an artist literally named "AC/DC") becomes "AC_DC"
	// rather than splitting into nested directories. The only separators that
	// survive into the rendered path are the template's own structural ones.
	return map[string]fieldVal{
		"albumartist": {s: sanitizeSegment(albumArtist)},
		"artist":      {s: sanitizeSegment(artist)},
		"album":       {s: sanitizeSegment(firstNonEmpty(item.Album, unknownAlbum))},
		"title":       {s: sanitizeSegment(firstNonEmpty(item.Title, unknownTitle))},
		"genre":       {s: sanitizeSegment(firstNonEmpty(item.Genre, unknownGenre))},
		"ext":         {s: sanitizeSegment(ext)},
		"track":       {n: item.TrackNo, isNum: true},
		"disc":        {n: item.DiscNo, isNum: true},
		"year":        {n: item.Year, isNum: true},
	}
}

func renderTemplate(tmpl string, fields map[string]fieldVal) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		rel := strings.IndexByte(tmpl[i:], '}')
		if rel < 0 {
			return "", waxerr.New(waxerr.CodeInvalid, "organize.render", "unterminated '{' in template")
		}
		token := tmpl[i+1 : i+rel]
		i += rel + 1

		name, spec := token, ""
		if k := strings.IndexByte(token, ':'); k >= 0 {
			name, spec = token[:k], token[k+1:]
		}
		fv, ok := fields[name]
		if !ok {
			return "", waxerr.New(waxerr.CodeInvalid, "organize.render", "unknown template field {"+name+"}")
		}
		b.WriteString(fv.format(spec))
	}
	return b.String(), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sanitizeSegment makes one path segment filesystem-safe: illegal characters
// become '_', and leading/trailing spaces and dots are trimmed. Reserved device
// names and long-path rules still need platform-specific handling.
func sanitizeSegment(seg string) string {
	seg = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20: // control characters
			return '_'
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		default:
			return r
		}
	}, seg)
	seg = strings.Trim(seg, " ")
	seg = strings.TrimRight(seg, ".")
	if seg == "" {
		return "_"
	}
	return seg
}
