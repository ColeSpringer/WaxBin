// Package read holds WaxBin's canonical read/consumer API primitives: faceted
// aggregation, collation-correct keyset pagination, and the canonical "unknown"
// buckets. The types live here; the SQL that produces them lives in
// store/sqlite, which depends on this package. The boundary rule: WaxBin returns
// clean, canonical, counted results; consumers decide pixels/UX.
package read

// GroupBy is a faceting dimension. The set is enumerated and individually
// tested so every consumer (CLI --group-by, OpenSubsonic getGenres/getArtists,
// stats) groups the same way.
type GroupBy string

const (
	GroupGenre       GroupBy = "genre"
	GroupArtist      GroupBy = "artist"
	GroupAlbumArtist GroupBy = "albumArtist"
	GroupAlbum       GroupBy = "album"
	GroupYear        GroupBy = "year"
	GroupKind        GroupBy = "kind"
	// GroupLibrary buckets items by their primary backing file's library (key =
	// library pid, display = the display root). A fileless item, such as an
	// undownloaded episode, lands in the NoFile unknown bucket.
	GroupLibrary GroupBy = "library"
)

// Valid reports whether g is a known faceting dimension: one of the fixed dimensions
// or a well-formed custom-tag dimension ("tag.<KEY>" for a canonical, non-reserved key).
func (g GroupBy) Valid() bool {
	switch g {
	case GroupGenre, GroupArtist, GroupAlbumArtist, GroupAlbum, GroupYear, GroupKind, GroupLibrary:
		return true
	default:
		_, ok := TagGroupKey(g)
		return ok
	}
}

// GroupBys lists the supported faceting dimensions (for help text and tests).
func GroupBys() []GroupBy {
	return []GroupBy{GroupGenre, GroupArtist, GroupAlbumArtist, GroupAlbum, GroupYear, GroupKind, GroupLibrary}
}

// Canonical "unknown" bucket display labels. A consumer rendering a facet or a
// browse list shows these verbatim for the absent dimension, so every client
// renders the same sentinel rather than inventing its own.
const (
	UnknownArtist = "[Unknown Artist]"
	NoGenre       = "[No Genre]"
	UnknownYear   = "[Unknown Year]"
	NonAlbum      = "[Non-Album]"
	NoFile        = "[No File]"
)
