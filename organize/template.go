package organize

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Canonical buckets for missing required (top-level, non-optional) grouping
// fields, so every layout renders the same placeholder rather than an empty path
// segment. Optional fields and fields inside a conditional group never use a
// bucket; they render empty and let their group drop.
const (
	unknownArtist  = "Unknown Artist"
	unknownAlbum   = "Unknown Album"
	unknownTitle   = "Untitled"
	unknownGenre   = "Unknown Genre"
	unknownPodcast = "Unknown Podcast"
	variousArtists = "Various Artists"
)

// unknownBuckets maps a required string field to its placeholder. A name absent
// here (e.g. ext, subtitle) renders empty when missing.
var unknownBuckets = map[string]string{
	"albumartist": unknownArtist,
	"artist":      unknownArtist,
	"album":       unknownAlbum,
	"title":       unknownTitle,
	"genre":       unknownGenre,
	"authorsort":  unknownArtist,
	"author":      unknownArtist,
	"podcast":     unknownPodcast,
}

// knownFields is the template vocabulary. A template referencing any other field
// is rejected at validation and render time so a typo fails loudly instead of
// silently dropping a segment.
var knownFields = map[string]bool{
	// Music.
	"albumartist": true, "artist": true, "album": true, "title": true,
	"genre": true, "ext": true, "track": true, "disc": true, "year": true,
	"edition": true, "composer": true,
	// Audiobook fields.
	"author": true, "authorsort": true, "series": true, "seq": true,
	"narrator": true, "subtitle": true, "asin": true,
	// Podcast fields.
	"podcast": true, "season": true, "episode": true, "pubdate": true,
}

// fieldVal is a template field that formats either as text or as a zero-paddable
// number.
type fieldVal struct {
	s     string
	n     int
	isNum bool
}

// empty reports whether the field has no value: an empty string, or a zero
// number (so {disc?} and {year?} drop when absent).
func (f fieldVal) empty() bool {
	if f.isNum {
		return f.n == 0
	}
	return f.s == ""
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
// using the profile's template for the item's media kind, sanitizing each path
// segment. Empty segments (a fully dropped conditional group between separators)
// collapse rather than leaving an empty directory.
func RenderRelPath(p Profile, item *model.ItemView) (string, error) {
	tmpl := p.templateFor(item.Kind)
	rendered, err := renderTemplate(tmpl, itemFields(item))
	if err != nil {
		return "", err
	}

	parts := strings.Split(rendered, "/")
	clean := make([]string, 0, len(parts))
	for _, raw := range parts {
		// A genuinely empty segment is a dropped conditional group (or a doubled
		// separator): collapse it rather than create an empty directory. A non-empty
		// segment is kept even if it sanitizes to "_" (e.g. a title of only illegal
		// characters), so the file is never left nameless.
		if raw == "" {
			continue
		}
		clean = append(clean, sanitizeSegment(raw))
	}
	if len(clean) == 0 {
		return "", waxerr.New(waxerr.CodeInvalid, "organize.RenderRelPath", "template produced an empty path")
	}
	return filepath.Join(clean...), nil
}

// itemFields builds the template vocabulary from an item view. String values are
// folded (separators neutralized) but not bucketed here: the renderer applies the
// unknown bucket only to a required top-level field, so optional fields and
// group-internal fields stay genuinely empty and let their group drop.
// Compilations use a literal Various Artists album-artist folder.
func itemFields(item *model.ItemView) map[string]fieldVal {
	// Seed every known field empty so a template for another media kind can still
	// render a music item: unused fields drop their optional groups. A genuine typo
	// is still absent from the map and fails during validation or rendering.
	f := make(map[string]fieldVal, len(knownFields))
	for name := range knownFields {
		f[name] = fieldVal{}
	}

	albumArtist := firstNonEmpty(item.AlbumArtist, item.Artist)
	if item.Compilation {
		albumArtist = variousArtists
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(item.DisplayPath)), ".")
	if ext == "" {
		ext = item.Codec
	}
	f["albumartist"] = fieldVal{s: foldField(albumArtist)}
	f["artist"] = fieldVal{s: foldField(firstNonEmpty(item.Artist, item.AlbumArtist))}
	f["album"] = fieldVal{s: foldField(item.Album)}
	f["title"] = fieldVal{s: foldField(item.Title)}
	f["genre"] = fieldVal{s: foldField(item.Genre)}
	f["ext"] = fieldVal{s: foldField(ext)}
	f["track"] = fieldVal{n: item.TrackNo, isNum: true}
	f["disc"] = fieldVal{n: item.DiscNo, isNum: true}
	f["year"] = fieldVal{n: item.Year, isNum: true}

	// Audiobook tokens. For a book the view's Artist is the author (COALESCE'd in
	// the read view), so author/authorsort derive from it; the rest map directly.
	// These stay empty for a track, dropping their optional groups in any layout.
	f["author"] = fieldVal{s: foldField(firstNonEmpty(item.Artist, item.AlbumArtist))}
	f["authorsort"] = fieldVal{s: foldField(firstNonEmpty(item.AuthorSort, model.SortKey(item.Artist)))}
	f["series"] = fieldVal{s: foldField(item.Series)}
	f["seq"] = fieldVal{s: foldField(item.SeriesSeq)}
	f["narrator"] = fieldVal{s: foldField(item.Narrator)}
	f["subtitle"] = fieldVal{s: foldField(item.Subtitle)}
	f["asin"] = fieldVal{s: foldField(item.ASIN)}
	return f
}

// renderTemplate renders a template string against a field set. The grammar:
//
//	{field}        substitute the field (required: an empty value renders its
//	               unknown bucket); {field:0N} zero-pads a number.
//	{field?}       optional: an empty value renders nothing (no bucket).
//	<...>          a conditional group: rendered only when at least one field
//	               token inside resolves to a non-empty value; otherwise dropped.
//	               Groups may nest. Fields inside a group are implicitly optional.
//	\{ \} \< \> \\ a literal '{', '}', '<', '>', or '\'.
func renderTemplate(tmpl string, fields map[string]fieldVal) (string, error) {
	text, _, next, err := renderNodes(tmpl, 0, false, fields)
	if err != nil {
		return "", err
	}
	_ = next
	return text, nil
}

// renderNodes renders from tmpl[i] until end (top level) or the matching '>'
// (inside a group). It returns the rendered text, whether any field token
// contributed a value (which decides a group's survival), and the index after
// the consumed run.
func renderNodes(tmpl string, i int, inGroup bool, fields map[string]fieldVal) (string, bool, int, error) {
	var b strings.Builder
	anyVal := false
	for i < len(tmpl) {
		switch c := tmpl[i]; c {
		case '\\':
			if i+1 < len(tmpl) {
				b.WriteByte(tmpl[i+1])
				i += 2
				continue
			}
			b.WriteByte('\\')
			i++
		case '>':
			if inGroup {
				return b.String(), anyVal, i + 1, nil
			}
			b.WriteByte('>') // a stray '>' at top level is a literal
			i++
		case '<':
			inner, innerHas, ni, err := renderNodes(tmpl, i+1, true, fields)
			if err != nil {
				return "", false, 0, err
			}
			i = ni
			if innerHas {
				b.WriteString(inner)
				anyVal = true
			}
		case '{':
			val, has, ni, err := renderField(tmpl, i, inGroup, fields)
			if err != nil {
				return "", false, 0, err
			}
			b.WriteString(val)
			anyVal = anyVal || has
			i = ni
		default:
			b.WriteByte(c)
			i++
		}
	}
	if inGroup {
		return "", false, 0, waxerr.New(waxerr.CodeInvalid, "organize.render", "unterminated '<' group in template")
	}
	return b.String(), anyVal, i, nil
}

// renderField parses and renders a {field[:spec][?]} token at tmpl[i]=='{'.
func renderField(tmpl string, i int, inGroup bool, fields map[string]fieldVal) (string, bool, int, error) {
	const op = "organize.render"
	rel := strings.IndexByte(tmpl[i:], '}')
	if rel < 0 {
		return "", false, 0, waxerr.New(waxerr.CodeInvalid, op, "unterminated '{' in template")
	}
	token := tmpl[i+1 : i+rel]
	next := i + rel + 1

	optional := strings.HasSuffix(token, "?")
	if optional {
		token = token[:len(token)-1]
	}
	name, spec := token, ""
	if k := strings.IndexByte(token, ':'); k >= 0 {
		name, spec = token[:k], token[k+1:]
	}
	fv, ok := fields[name]
	if !ok {
		return "", false, 0, waxerr.New(waxerr.CodeInvalid, op, "unknown template field {"+name+"}")
	}
	if fv.empty() {
		// Inside a group, or marked optional, an empty field contributes nothing and
		// does not keep its group alive. A required top-level numeric still formats
		// (0 -> "00"); a required top-level string renders its unknown bucket.
		if optional || inGroup {
			return "", false, next, nil
		}
		if fv.isNum {
			return fv.format(spec), false, next, nil
		}
		return unknownBuckets[name], false, next, nil
	}
	return fv.format(spec), true, next, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// validateTemplate checks a template parses (balanced groups/braces) and
// references only known fields. It renders against a vocabulary where every field
// is present, so an unknown-field reference surfaces as an error at profile-load
// time rather than at the first organize.
func validateTemplate(tmpl string) error {
	full := make(map[string]fieldVal, len(knownFields))
	for name := range knownFields {
		full[name] = fieldVal{s: "x"}
	}
	_, err := renderTemplate(tmpl, full)
	return err
}
