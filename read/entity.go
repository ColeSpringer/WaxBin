package read

import "github.com/colespringer/waxbin/model"

// EntityKind names a shared entity addressable by the entity-info lookup. It is
// its own vocabulary rather than model.MergeEntity because the two answer
// different questions: merge covers what can be collapsed (series cannot), while
// this covers what a consumer can look up by pid, series included. Podcasts stay
// out; they have their own read surface (PodcastByPID).
type EntityKind string

const (
	EntityArtist       EntityKind = "artist"
	EntityReleaseGroup EntityKind = "release_group"
	EntityAlbum        EntityKind = "album"
	EntityGenre        EntityKind = "genre"
	EntitySeries       EntityKind = "series"
)

// Valid reports whether k is a known entity kind.
func (k EntityKind) Valid() bool {
	switch k {
	case EntityArtist, EntityReleaseGroup, EntityAlbum, EntityGenre, EntitySeries:
		return true
	default:
		return false
	}
}

// EntityKinds lists the supported entity kinds (for help text and tests).
func EntityKinds() []EntityKind {
	return []EntityKind{EntityArtist, EntityReleaseGroup, EntityAlbum, EntityGenre, EntitySeries}
}

// EntityInfo is the summary answer for one shared entity: identity, links to its
// parent entities, membership counts, and which libraries its members' files
// live in. It gives a consumer a direct read on an entity it holds a pid for (a
// facet bucket, a query field match, an item view) without a full facet scan.
//
// Counts come from the maintained rollups for artist, release group, and genre
// (track-based for the first two, so a book credits its author's LibraryPIDs
// membership but not the artist's ItemCount; item-based for genre), and from
// live indexed aggregates for album and series, which have no rollup rows.
type EntityInfo struct {
	Kind    EntityKind
	PID     model.PID
	Name    string // artist/genre/series name, or release-group/album title
	SortKey string
	MBID    string // empty for a genre, which carries no external id
	Type    string // release-group primary type (album|ep|single|...); empty otherwise
	Year    int    // album release year; 0 otherwise

	// Parent links, filled where the schema has one: a release group's primary
	// artist and an album's release group.
	ArtistPID       model.PID
	ReleaseGroupPID model.PID

	// ItemCount and TotalDurationMS summarize the entity's member items (see the
	// type comment for which source answers each kind). ReleaseGroupCount is
	// filled for an artist only.
	ItemCount         int
	ReleaseGroupCount int
	TotalDurationMS   int64

	// LibraryPIDs are the distinct libraries holding the member items' primary
	// backing files, in library order. Artist membership follows the artist
	// facet: an item counts under its effective artist (a book under its
	// author). A fileless member, such as an undownloaded episode carrying the
	// genre, contributes nothing.
	LibraryPIDs []model.PID
}
