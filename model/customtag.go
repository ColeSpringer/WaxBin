package model

import "strings"

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
	// Internal rebuild hint.
	TagWaxbinItemPID: true,
}

// IsReservedTagKey reports whether key (canonical uppercase) is one WaxBin owns through
// another surface, so it may not be used as a custom tag. It is the single source of
// truth shared by the scan-time custom-tag collector (which skips these) and the
// SetItemTag edit (which rejects them).
func IsReservedTagKey(key string) bool { return reservedTagKeys[key] }

// CanonicalTagKey normalizes a tag key to its canonical uppercase-ASCII form and
// reports whether it is valid, mirroring the tag library's key rules so a key that
// passes here also survives an on-disk write. It trims surrounding whitespace, rejects
// empty or non-ASCII input, uppercases (so "bpm" and "BPM" dedup), and rejects a byte
// outside printable ASCII or a literal '=' (which the tag wire format reserves).
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
		if c < 0x20 || c > 0x7E || c == '=' {
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
