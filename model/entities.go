package model

// GenreFacet distinguishes the two uses of the shared genre entity. Mood uses
// the same table shape as genre, even when a catalog only contains genre rows.
type GenreFacet string

const (
	FacetGenre GenreFacet = "genre"
	FacetMood  GenreFacet = "mood"
)

// Artist is a normalized performer entity. Artists are resolved by MatchKey
// unless metadata supplies stronger external identifiers.
type Artist struct {
	ID       int64
	PID      PID
	Name     string // canonical display casing
	SortKey  string
	MatchKey string
	MBID     string
}

// ReleaseGroup is the album abstraction for browse: it groups the editions of
// one work above the concrete album/release.
type ReleaseGroup struct {
	ID              int64
	PID             PID
	Title           string
	SortKey         string
	PrimaryArtistID int64
	MBID            string
	Type            string // album|ep|single|compilation|audiobook
	MatchKey        string
}

// Album is a concrete release/edition under a ReleaseGroup.
type Album struct {
	ID             int64
	PID            PID
	ReleaseGroupID int64
	Title          string
	SortKey        string
	Year           int
	DiscTotal      int
	MBID           string
	Edition        string
	// Release identifiers, populated from tags at album creation and editable
	// through the entity-curation surface.
	Barcode       string // release barcode (UPC/EAN)
	Label         string // record label
	CatalogNumber string // label catalog number
	MatchKey      string
}

// Genre is a normalized genre-or-mood entity. The Facet field selects which;
// MatchKey (not Name) carries uniqueness within a facet.
type Genre struct {
	ID       int64
	PID      PID
	Facet    GenreFacet
	Name     string // canonical display, e.g. "Hip-Hop"
	MatchKey string // normalized, e.g. "hip hop"
	SortKey  string
}

// ArtistRollup is the maintained catalog-structural summary for an artist.
type ArtistRollup struct {
	ArtistID          int64
	ReleaseGroupCount int
	TrackCount        int
	TotalDurationMS   int64
	UpdatedAt         int64
}
