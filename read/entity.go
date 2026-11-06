package read

import "github.com/colespringer/waxbin/model"

// EntityKind names a shared entity addressable by the entity-info lookup. It is its
// own vocabulary rather than model.MergeEntity because the two answer different
// questions: merge covers what can be collapsed, this covers what a consumer can look
// up by pid. The two memberships happen to coincide now that series merges, but the
// read side lives in read and takes no position on collapsibility, so they stay apart.
// Podcasts stay out; they have their own read surface (PodcastByPID).
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
// (track-based for the first two, so a book or a credit places an artist in
// LibraryPIDs without raising its ItemCount; item-based for genre), and from
// live indexed aggregates for album and series, which have no rollup rows. While a scan
// runs, the rollup counts, an album's Year and the sort spellings the albums, release
// groups and artists derive from their members (so their SortKey) trail its writes until
// its next settle, every few thousand files and at its end.
type EntityInfo struct {
	Kind    EntityKind
	PID     model.PID
	Name    string // artist/genre/series name, or release-group/album title
	SortKey string
	// MBID is empty for a genre and for a series, neither of which carries an
	// external id: a genre is a normalized local label, and book enrichment
	// resolves a release from a tagged release id without ever reaching the
	// series above it.
	MBID string
	Type string // release-group primary type (album|ep|single|...); empty otherwise
	Year int    // album release year; 0 otherwise

	// The album's other release identifiers and the two columns describing its
	// edition, filled by scan from tags and writable through entity edit. Album only,
	// empty for every other kind, as Type and Year are.
	//
	// The two writers normalize differently by policy: an edit runs Barcode and Country
	// through their normalizers, a scan stores the tag verbatim (the track.mbid rule), so
	// normalize before comparing. Country shows it sharpest, since a scan can store the
	// "US & Europe" an edit refuses. Media has no normalizer at all.
	Barcode       string
	Label         string
	CatalogNumber string
	Media         string
	Country       string

	// Parent links, filled where the schema has one: a release group's primary
	// artist and an album's release group.
	ArtistPID       model.PID
	ReleaseGroupPID model.PID

	// ItemCount and TotalDurationMS summarize the entity's live member items (see the
	// type comment for which source answers each kind): a trashed member is in neither,
	// while a missing one, whose file row stays, is in both. ReleaseGroupCount is filled
	// for an artist only.
	ItemCount         int
	ReleaseGroupCount int
	TotalDurationMS   int64

	// LibraryPIDs are the distinct libraries holding the member items' files, an
	// alternate copy's included, in library order. Artist membership is every way an
	// item files under the artist: as its performing or album artist, as a book's
	// author, or through any credit on it, so an artist reachable from any artist
	// dimension's bucket (creditArtist, author, narrator included) reports the
	// libraries of the items crediting it. A fileless member, such as an undownloaded
	// episode carrying the genre, contributes nothing.
	LibraryPIDs []model.PID

	// PrimaryLibraryPIDs are the libraries among LibraryPIDs holding a member item's
	// primary file, the libraries its members are placed in (ItemView.LibraryPID), in
	// the same order; a library holding only copies of the members is in LibraryPIDs
	// alone. A host serving whatever a library can play gates an entity on LibraryPIDs,
	// and one granting a reader a library by placement gates it on this list, so an
	// artist whose only presence in a library is a copy is not shown there with every
	// item filtered out.
	PrimaryLibraryPIDs []model.PID

	// Aliases are an artist's other names, by name: a merged loser's, a name a locked
	// rename left, those enrichment found. Empty for every other kind.
	Aliases []string
}

// EntityPage is one keyset-paginated window of entity summaries in collation order
// (the generated sort_key, then pid as a tiebreak), the entity-side twin of
// read.Page. It enumerates the entities that exist rather than the entities matching
// a query: a filtered, counted answer is what Facet gives.
//
// It is the primitive behind an alphabetical index over a large dimension. A facet
// cannot be that: paging one would re-run the whole GROUP BY per page, where this
// walks the entity table's sort_key index and costs O(page).
//
// Drain it on HasMore, not on a non-empty Entities. A page enumerates its entities
// and hydrates them separately, so one deleted in between is counted toward the page
// but absent from Entities, and a page can be short (even empty) with more to come.
// Nothing is skipped or repeated by that; Next still resumes after the last
// enumerated entity.
//
// Two things to know before comparing its numbers to a facet's. First, EntityInfo
// counts come from the maintained rollups, which are track-based for artist and
// release group, so an artist's ItemCount differs from the artist facet's bucket
// count, which COALESCEs a book under its author. Both are correct answers to
// different questions. Second, EntityGenre enumerates both the genre and mood facets
// of the genre table, matching EntityByPID.
type EntityPage struct {
	Kind     EntityKind
	Entities []*EntityInfo
	Next     Cursor
	HasMore  bool
}
