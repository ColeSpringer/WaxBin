package identity

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MatchKey normalizes a display string into a dedup key: lowercased, diacritics
// stripped, punctuation/separators folded to single spaces, and surrounding
// space trimmed. It is the join key behind normalized entities (genre, artist,
// release_group), so "Hip-Hop"/"hip hop" and "Beyoncé"/"Beyonce" each resolve to
// one row. Stripping diacritics matches the FTS tokenizer (unicode61
// remove_diacritics 2), so an entity and its search index fold the same way. It
// is deliberately lossy and must never be shown to users; the canonical display
// name is stored alongside.
func MatchKey(s string) string {
	// NFD decomposes accented letters into a base letter plus combining marks, so
	// the marks can be dropped uniformly (precomposed "é" -> "e" + U+0301 -> "e").
	s = norm.NFD.String(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true // leading-space suppression
	for _, r := range s {
		switch {
		case unicode.IsMark(r):
			continue // drop combining marks (diacritics) entirely, no space
		case isWordRune(r):
			b.WriteRune(r)
			lastSpace = false
		default:
			// Punctuation, symbols, separators (any script, incl. fullwidth/CJK)
			// fold to a single space: "AC/DC" -> "ac dc", "東京（Live）" -> "東京 live".
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// isWordRune reports whether r is kept verbatim in a match key: an ASCII letter
// or digit, or any non-ASCII Unicode letter or digit (CJK, Greek, Cyrillic, ...).
// Combining marks are handled (dropped) by the caller before this is consulted.
func isWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r > 0x7f:
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	default:
		return false
	}
}

// genreSplit are the in-tag genre separators. A multi-genre tag like
// "Rock; Pop / Indie" splits into three before normalization.
const genreSplit = ";/\\"

// SplitGenres splits a raw genre tag into trimmed, de-duplicated display names.
// Duplicates are removed by match key, preserving the first-seen display casing.
func SplitGenres(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return strings.ContainsRune(genreSplit, r)
	})
	var out []string
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		name := strings.TrimSpace(f)
		if name == "" {
			continue
		}
		mk := MatchKey(name)
		if mk == "" || seen[mk] {
			continue
		}
		seen[mk] = true
		out = append(out, name)
	}
	return out
}

// creditSplitRe are the UNAMBIGUOUS separators between credited people in one tag
// value: a semicolon, slash, or ampersand ("Gaiman & Pratchett", "A; B", "A / B").
// It deliberately does NOT split on a bare comma (also the "Last, First" name
// format) or the word "and" (common inside a single entity, e.g. a publisher), so a
// single credited person is not shattered into bogus contributor entities.
var creditSplitRe = regexp.MustCompile(`\s*[;/&]\s*`)

// SplitCredits splits a combined credit string (authors or narrators) into trimmed
// individual names, dropping empties. It is the canonical splitter shared by the
// metadata adapter, the scanner, and the field-edit path, so every path that turns a
// credit string into contributor entities splits it the same way.
func SplitCredits(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := creditSplitRe.Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ArtistKey is the entity-identity key for an artist: MBID when known, else the
// normalized name. Matches the track/release-group MBID-first convention.
func ArtistKey(mbid, name string) string {
	if m := strings.TrimSpace(mbid); m != "" {
		return "mbid:" + strings.ToLower(m)
	}
	return "name:" + MatchKey(name)
}

// ReleaseGroupKey is the entity-identity key for a release group: MBID when
// known, else (primary-artist match key, normalized title). Returns "" when
// there is no title to key on (a non-album single is not grouped).
func ReleaseGroupKey(mbid, artistMatchKey, title string) string {
	if m := strings.TrimSpace(mbid); m != "" {
		return "mbid:" + strings.ToLower(m)
	}
	t := MatchKey(title)
	if t == "" {
		return ""
	}
	return "rg:" + artistMatchKey + "\x1f" + t
}

// AlbumKey is the entity-identity key for a specific release/edition: MBID when
// known, else (release-group key, year, disc total, folder). The folder
// disambiguates same-titled editions that share a release group. Returns "" when
// the release-group key is empty.
func AlbumKey(mbid, releaseGroupKey string, year, discTotal int, folder string) string {
	if m := strings.TrimSpace(mbid); m != "" {
		return "mbid:" + strings.ToLower(m)
	}
	if releaseGroupKey == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("al:")
	b.WriteString(releaseGroupKey)
	b.WriteByte(0x1f)
	b.WriteString(numOrEmpty(year))
	b.WriteByte(0x1f)
	b.WriteString(numOrEmpty(discTotal))
	b.WriteByte(0x1f)
	b.WriteString(MatchKey(folder))
	return b.String()
}

// numOrEmpty renders n for a key segment, treating 0 (an unknown year or disc
// count) as empty so it does not falsely distinguish two otherwise-equal keys.
func numOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// BookKey is the entity-identity key for an audiobook: ASIN when known, else
// ISBN, else (primary-author match key, normalized title, normalized edition).
// The edition segment keeps an abridged release distinct from the same title's
// unabridged one. It returns "" when there is no id and no title to key on, so a
// fully untitled book stays ungrouped (its files would otherwise all collapse
// together). Files sharing the key are the parts of one book.
func BookKey(asin, isbn, author, title, edition string) string {
	if a := strings.TrimSpace(asin); a != "" {
		return "asin:" + strings.ToLower(a)
	}
	if i := normalizeISBN(isbn); i != "" {
		return "isbn:" + i
	}
	t := MatchKey(title)
	if t == "" {
		return ""
	}
	return "book:" + MatchKey(author) + "\x1f" + t + "\x1f" + MatchKey(edition)
}

// SeriesKey is the entity-identity key for a series: MBID when known, else the
// normalized name. Returns "" when there is no name.
func SeriesKey(mbid, name string) string {
	if m := strings.TrimSpace(mbid); m != "" {
		return "mbid:" + strings.ToLower(m)
	}
	n := MatchKey(name)
	if n == "" {
		return ""
	}
	return "series:" + n
}

// PodcastKey is the entity-identity key for a podcast feed: Podcasting 2.0
// <podcast:guid> when present, otherwise the normalized feed URL. The title is not
// part of identity, so a retitled show keeps its pid and episodes. Returns "" when
// neither input is usable.
func PodcastKey(guid, feedURL string) string {
	if g := strings.TrimSpace(guid); g != "" {
		return "pguid:" + strings.ToLower(g)
	}
	u := normalizeFeedURL(feedURL)
	if u == "" {
		return ""
	}
	return "feed:" + u
}

// EpisodeKey is the entity-identity key for one episode, scoped to its podcast so
// reused bare GUIDs cannot collide across feeds. Within a feed it prefers GUID,
// then enclosure URL, then title. Scoping by podcastKey keeps episode identity
// stable when a feed URL changes. Returns "" when there is nothing to key on.
func EpisodeKey(podcastKey, guid, enclosureURL, title string) string {
	if podcastKey == "" {
		return ""
	}
	var id string
	switch {
	case strings.TrimSpace(guid) != "":
		// A feed <guid> is opaque and case-sensitive per the RSS spec, so it is only
		// trimmed, not lowercased: two distinct case-differing guids must not merge.
		id = "g:" + strings.TrimSpace(guid)
	case normalizeFeedURL(enclosureURL) != "":
		id = "e:" + normalizeFeedURL(enclosureURL)
	default:
		t := MatchKey(title)
		if t == "" {
			return ""
		}
		id = "t:" + t
	}
	return "ep:" + podcastKey + "\x1f" + id
}

// normalizeFeedURL lowercases the scheme/host and trims a trailing slash so http
// and https, or a present/absent trailing slash, key the same. It preserves path
// and query case because paths can be case-sensitive and private-feed tokens can
// live in the query. Returns "" for a value with no usable characters.
func normalizeFeedURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	scheme := ""
	if i := strings.Index(s, "://"); i >= 0 {
		scheme = strings.ToLower(s[:i])
		s = s[i+3:]
	}
	// Lowercase the authority (host[:port]) up to the first path/query/fragment.
	rest := ""
	if j := strings.IndexAny(s, "/?#"); j >= 0 {
		s, rest = strings.ToLower(s[:j]), s[j:]
	} else {
		s = strings.ToLower(s)
	}
	// http and https share an identity (a feed served over both is one feed); other
	// schemes keep theirs.
	if scheme == "https" {
		scheme = "http"
	}
	out := s + rest
	if scheme != "" {
		out = scheme + "://" + out
	}
	return strings.TrimRight(out, "/")
}

// VirtualTrackKey is the entity-identity key for one virtual track carved out of a
// single-file album rip by a .cue sheet. It anchors on the backing file's essence
// hash plus the cue TRACK number and the track's start offset, deliberately NOT its
// title, so retagging one track's name in the cue does not fork its identity across
// a rescan (offset-anchored, not title-anchored). The essence hash keeps two
// distinct rips from colliding, and the "vtrack:" namespace keeps a virtual track
// from ever sharing a key with a whole-file track keyed on the same essence.
func VirtualTrackKey(fileEssence string, trackNumber int, startMS int64) string {
	return "vtrack:" + fileEssence + "\x1f" + strconv.Itoa(trackNumber) + "\x1f" + strconv.FormatInt(startMS, 10)
}

// normalizeISBN keeps only the digits and the ISBN-10 check character 'X' (folded
// to lowercase), so "978-0-13-468599-1" and "9780134685991" key the same. It
// returns "" for a value with no usable characters.
func normalizeISBN(isbn string) string {
	var b strings.Builder
	for _, r := range isbn {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'x' || r == 'X':
			b.WriteByte('x')
		}
	}
	return b.String()
}
