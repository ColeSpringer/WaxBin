package model

// MergeEntity names an entity type that supports the merge primitive. Two rows
// that heuristic identity kept apart (say "Beatles" vs "The Beatles", or two
// release-groups that late enrichment resolves to one MBID) collapse onto a
// single survivor, which keeps its PID (identity is assigned once; see the
// identity subsystem). The survivor's children (tracks, albums, books, item_genre
// links, contributor credits) are re-pointed onto it, so their play_state and
// provenance ride along untouched. Merge unions state by re-parenting the items
// that carry it, never by duplicating an item.
type MergeEntity string

const (
	MergeArtist       MergeEntity = "artist"
	MergeReleaseGroup MergeEntity = "release_group"
	MergeAlbum        MergeEntity = "album"
	MergeGenre        MergeEntity = "genre"
	MergeSeries       MergeEntity = "series"
)

// Valid reports whether m is a mergeable entity type.
func (m MergeEntity) Valid() bool {
	switch m {
	case MergeArtist, MergeReleaseGroup, MergeAlbum, MergeGenre, MergeSeries:
		return true
	default:
		return false
	}
}

// Starrable reports whether the entity type carries per-user star and rating state.
// It is the narrower vocabulary the entity play-state surfaces speak: series merges
// like the rest but holds no art, curation, or state of its own, and nothing asks to
// star one. Widen it if a consumer appears.
func (m MergeEntity) Starrable() bool {
	switch m {
	case MergeArtist, MergeReleaseGroup, MergeAlbum, MergeGenre:
		return true
	default:
		return false
	}
}

// HasMBID reports whether the entity type carries an mbid column the merge
// unions onto the survivor (genre and series have none).
func (m MergeEntity) HasMBID() bool {
	return m == MergeArtist || m == MergeReleaseGroup || m == MergeAlbum
}

// MergeReport summarizes a completed entity merge.
type MergeReport struct {
	EntityType MergeEntity
	Survivor   PID
	Loser      PID
	// Children is the number of first-class child rows (tracks, albums, books, or
	// item_genre links) re-pointed from the loser onto the survivor.
	Children int
}
