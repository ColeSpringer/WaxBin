package model

import (
	"slices"
	"strings"
)

// This file defines the vocabulary for custom (non-standard) item tags: the tag frames
// a file carries that WaxBin's typed model does not map to a column, plus the tags a
// user sets directly. They are stored in the item_tag table and lockable under a
// namespaced "tag.<KEY>" field in field_provenance (the same shape as "credit.<role>").

// reservedTagKeys are the canonical tag keys WaxBin already maps into its own model,
// owns through another surface (credits, identifiers, sort names, acquisition, book
// fields, lyrics), or manages as file-own-audio/internal state. A custom tag may never
// use one of these keys: the scalar/credit/identifier edit APIs are the single source
// of truth for them, and preserving them here would double-store or shadow a modeled
// value. Everything else a file carries is a custom tag. Keys are canonical uppercase.
var reservedTagKeys = map[string]bool{
	// Scalar model fields and their write-back keys.
	"TITLE": true, "ARTIST": true, "ALBUM": true, "ALBUMARTIST": true, "COMPOSER": true,
	"GENRE": true, "COMMENT": true, "COMPILATION": true,
	"TRACKNUMBER": true, "TRACKTOTAL": true, "DISCNUMBER": true, "DISCTOTAL": true,
	"RECORDINGDATE": true, "RELEASEDATE": true, "ORIGINALDATE": true, "DATE": true,
	// Identifiers.
	"ISRC": true, "BARCODE": true, "CATALOGNUMBER": true, "LABEL": true,
	"MUSICBRAINZ_TRACKID": true, "MUSICBRAINZ_ALBUMID": true, "MUSICBRAINZ_RELEASEGROUPID": true,
	"MUSICBRAINZ_ARTISTID": true, "MUSICBRAINZ_ALBUMARTISTID": true,
	// Edition description, owned by the album entity's media/country columns. Only the
	// canonical spellings appear: WaxLabel folds Picard's ID3 and MP4 variants onto
	// RELEASECOUNTRY, so the mixed-case name never reaches a tag set. Newly reserving a
	// key costs a catalog that stored one two things on the next rescan: the item_tag
	// rows go, so a `tag.MEDIA` predicate stops matching, and a locked curated row goes
	// with its lock and provenance. On-disk data is safe (TagEdit is per-key
	// set-or-clear), and the album column is where the value now surfaces.
	"MEDIA": true, "RELEASECOUNTRY": true,
	// Sort names. COMPOSERSORT is reserved globally, so an audiobook file carrying
	// it loses the frame as a custom tag even though books do not consume the
	// field (m4b narrator conventionally rides COMPOSER, not its sort). The
	// scalar composer_sort surface owns the key for every kind.
	"ARTISTSORT": true, "ALBUMSORT": true, "ALBUMARTISTSORT": true, "COMPOSERSORT": true,
	// Contributor roles (owned by the credit surface / item_contributor).
	"LYRICIST": true, "CONDUCTOR": true, "PERFORMER": true, "REMIXER": true, "PRODUCER": true,
	"ENGINEER": true, "MIXER": true, "ARRANGER": true, "WRITER": true, "DJMIXER": true,
	// Audiobook fields and lyrics.
	"NARRATOR": true, "GROUPING": true, "DESCRIPTION": true, "LONGDESCRIPTION": true,
	"MEDIATYPE": true, "LYRICS": true,
	// Acquisition provenance.
	"SOURCE_URL": true, "SOURCE_ID": true, "ACQUISITION_DATE": true,
	// File-own-audio and per-user playback state WaxBin manages elsewhere.
	"REPLAYGAIN_TRACK_GAIN": true, "REPLAYGAIN_TRACK_PEAK": true,
	"REPLAYGAIN_ALBUM_GAIN": true, "REPLAYGAIN_ALBUM_PEAK": true,
	"ENCODER": true, "ENCODEDBY": true, "ENCODING_HISTORY": true, "ACOUSTID_FINGERPRINT": true,
	"RATING": true, "PLAYCOUNT": true,
	// Wire-format frame spellings the tag library folds onto the canonical keys above
	// on every format's read path. It folded them for Matroska alone until v1.5.0,
	// which added the ID3 TXXX, MP4 freeform, APEv2, and Vorbis read folds plus global
	// edit aliases, so a catalog scanned before the bump holds them as custom tags from
	// one of those four. Reserving them retires those rows on each item's next scan and
	// stops an edit under the spelling, which the alias would now write onto the
	// canonical frame while the catalog kept believing it stored a custom tag. Most
	// fold onto projected track fields and re-surface there. Three do not: their
	// canonical keys are reserved for owners that give a plain track no surface
	// (CONTENT_GROUP onto book-owned GROUPING, REMIXED_BY onto credit-owned REMIXER,
	// ENCODED_BY onto deliberately dropped ENCODEDBY), so on a plain track those
	// spellings stop surfacing anywhere, the same cost COMPOSERSORT documents above.
	// Spellings the library aliases for edits but does not fold on every read path
	// (YEAR, ALBUM_ARTIST, UNSYNCEDLYRICS and kin) stay deliberately unreserved:
	// reserving one would drop its value entirely on the formats that read it
	// verbatim, so it keeps surfacing as a custom tag there instead.
	"PART_NUMBER": true, "TOTAL_PARTS": true, "TOTAL_DISCS": true, "LEAD_PERFORMER": true,
	"DATE_RECORDED": true, "DATE_RELEASED": true, "DATE_RELEASE": true,
	"DATE_ORIGINAL": true, "ORIGINAL_DATE": true, "ENCODED_BY": true,
	"CATALOG_NUMBER": true, "PUBLISHER": true, "REMIXED_BY": true, "CONTENT_GROUP": true,
	// Tempo, now the track.bpm column. BPM is the canonical key and TBPM the ID3 frame
	// spelling folded onto it. Reserving them costs a catalog that stored either the
	// same two things newly reserving MEDIA cost: the item_tag rows go on the next
	// scan, taking any lock and provenance with them, and a stored rule naming
	// tag.BPM starts erroring instead of quietly matching nothing. The value itself
	// re-enters through the column on that same scan.
	"BPM": true, "TBPM": true,
	// Internal rebuild hint.
	TagWaxbinItemPID: true,
}

// IsReservedTagKey reports whether key (canonical uppercase) is one WaxBin owns through
// another surface, so it may not be used as a custom tag. It is the single source of
// truth shared by the scan-time custom-tag collector (which skips these) and the
// SetItemTag edit (which rejects them).
func IsReservedTagKey(key string) bool { return reservedTagKeys[key] }

// ReservedTagKeys returns the reserved keys in sorted order, for the callers that
// need the whole set rather than one lookup: the store names it in SQL to sweep the
// "tag.<KEY>" provenance rows a newly reserved key stranded.
func ReservedTagKeys() []string {
	out := make([]string, 0, len(reservedTagKeys))
	for k := range reservedTagKeys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// CanonicalTagKey normalizes a tag key to its canonical uppercase-ASCII form and
// reports whether it is valid, mirroring the tag library's key rules so a key that
// passes here also survives an on-disk write. It trims surrounding whitespace, rejects
// empty or non-ASCII input, uppercases (so "key" and "KEY" dedup), and rejects a byte
// outside 0x20-0x7D or a literal '=' (which the tag wire format reserves). The range
// stops one short of printable ASCII because the Vorbis comment specification does,
// so a '~' would write everywhere except there; meta pins the two rules together.
func CanonicalTagKey(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	b := []byte(s)
	for i := range b {
		c := b[i]
		if c >= 0x80 {
			return "", false // non-ASCII
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A' // uppercase
			b[i] = c
		}
		if c < 0x20 || c > 0x7D || c == '=' {
			return "", false
		}
	}
	return string(b), true
}

// CutTagPrefix returns the key portion of a "tag.<KEY>" field and whether the prefix
// was present with a non-empty key. It is the custom-tag analogue of CutCreditPrefix.
func CutTagPrefix(field string) (string, bool) {
	const p = "tag."
	if len(field) > len(p) && field[:len(p)] == p {
		return field[len(p):], true
	}
	return "", false
}

// TagLockField returns the field_provenance field name a custom tag's lock uses:
// "tag.<KEY>" (for example "tag.MOOD"). It keeps custom-tag locks in the item-scoped
// field_provenance table alongside the scalar and credit fields, namespaced so they
// never collide.
func TagLockField(key string) string { return "tag." + key }

// ItemTag is one custom tag on an item: a canonical key and its ordered values.
type ItemTag struct {
	Key    string
	Values []string
}
