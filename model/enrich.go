package model

// Enrichment types cross the port between the enrich package (which talks to
// MusicBrainz / Cover Art Archive) and store/sqlite (which persists results). The
// enrich pass reads targets, resolves them against a provider, then hands back a
// typed result the store applies atomically, respecting locks and provenance.

// Enrichment entity-type discriminators for EnrichTarget and entity_enrichment.
const (
	EnrichArtistType       = "artist"
	EnrichReleaseGroupType = "release_group"
	EnrichAlbumType        = "album"
	EnrichBookType         = "book"
)

// Artist relation kinds stored in artist_relation.
const (
	RelationMemberOf = "member_of"
	RelationAKA      = "aka"
	RelationSimilar  = "similar"
)

// EnrichScope narrows one enrichment pass to explicit targets, per phase. The id
// slices are internal store rowids (the same currency the port queries iterate),
// resolved from a public pid by the store's EnrichScopeForItem and
// EnrichScopeForEntity. A nil scope is the full pass; a phase whose id list is
// empty is skipped entirely. A scoped run implies force: the caller pointed at
// these targets deliberately, so a previously-missed lookup is retried instead
// of being skipped by its marker.
type EnrichScope struct {
	ArtistIDs       []int64
	ReleaseGroupIDs []int64
	AlbumIDs        []int64
	BookItemIDs     []int64
	LyricsItemIDs   []int64
}

// EnrichTarget is one entity the enrichment pass should look up. Type selects the
// provider query; MBID (when already known) is the fast path; Name/ArtistName/Year
// disambiguate a text search when there is no MBID. IDs are internal store rowids
// (the enrich Store port is implemented only by store/sqlite, so it exchanges
// rowids like the analyze port does).
type EnrichTarget struct {
	Type       string // EnrichArtistType | EnrichReleaseGroupType | EnrichBookType
	ID         int64
	PID        PID
	Name       string // artist name / release-group title / book/track title
	MBID       string // existing MBID, when known
	ArtistName string // release-group primary artist / book author / track artist, for disambiguation
	Album      string // album title, for a per-track lyrics lookup
	// FilePath and DurationSec back the optional AcoustID fallback: a representative
	// audio file for a release group with no MBID, fingerprinted to resolve one. They
	// are populated only when the store is asked to include the representative file.
	// DurationSec also disambiguates a per-track lyrics lookup.
	FilePath    string
	DurationSec int

	// The album release match keys on these. Barcode and CatalogNumber are the
	// identifiers it searches by, verbatim as a scan stored them, so a consumer
	// normalizes before comparing. Media and Country describe the edition without
	// naming it and feed the weaker third tier, also verbatim and in the tags' own
	// vocabulary ("2xCD", "US & Europe"), so that tier does its own folding.
	// ReleaseGroupMBID is the group the answer must belong to, and is separate from
	// MBID because MBID names the target's own id, which for an album target is empty
	// by construction: an album that already has one is not queued.
	Barcode          string
	CatalogNumber    string
	Media            string
	Country          string
	ReleaseGroupMBID string
	// HasArt reports whether the album already resolves a front cover, its own or one
	// derived from a member track's. The release match fetches a matched pressing's cover
	// only when it does not, so a library whose rips carry embedded art spends no
	// rate-limited requests on covers the store would refuse to fill anyway.
	HasArt bool
	// ArtLocked reports whether the entity's cover is locked. It serves the release-group
	// pass what HasArt serves the album pass: the store refuses the write, so fetching
	// first would spend a rate-limited request on every locked cover, every forced run.
	ArtLocked bool
}

// ArtistEnrichment is the resolved data for one artist, applied in a single
// transaction. Matched=false records a completed no-result lookup so the artist is
// not retried on the next run.
type ArtistEnrichment struct {
	ArtistID int64
	PID      PID
	Matched  bool
	MBID     string
	SortName string // MusicBrainz sort-name, stored as a primary alias
	Aliases  []string
	// Relations link this artist to OTHER artists, identified by their MBID. The
	// store resolves each target MBID to an existing catalog artist and skips the
	// ones not present (no stub artists are created).
	Relations []ArtistRelationInput
}

// ArtistRelationInput is one directed artist relation to persist. Inbound reverses
// the edge: normally the enriched artist is the source and TargetMBID the
// destination, but when Inbound is set the target is the source (so a "member of
// band" relation is always stored member -> band regardless of which end was
// enriched, since MusicBrainz reports it from both directions).
type ArtistRelationInput struct {
	TargetMBID string
	Kind       string // RelationMemberOf | RelationAKA | RelationSimilar
	Inbound    bool
}

// ReleaseGroupEnrichment is the resolved data for one release group. Genres are
// added to member items that carry no genre yet (never overwriting a tagged or
// locked genre); Art is the release-group front cover from the Cover Art Archive.
type ReleaseGroupEnrichment struct {
	ReleaseGroupID int64
	PID            PID
	Matched        bool
	MBID           string
	Type           string // album|ep|single|compilation
	Genres         []string
	// GenreProvider is the provider that supplied the display-primary genre, recorded
	// as field_provenance.provider for the genre field. Empty when no genre was found
	// (or the provider is untracked); "musicbrainz" when the genre came from the
	// identity spine's own release-group genres.
	GenreProvider string
	// Art keeps meaning the front cover. AuxArt carries the role-tagged images
	// excluding front, applied fill-when-empty per role at this entity's own rung and
	// skipped entirely under the entity's art lock.
	Art    *ArtImage
	AuxArt map[ArtRole]*ArtImage
}

// LyricsEnrichment is the resolved lyrics for one recording (track). Lyrics are
// filled only when the item has none, so a sidecar/embedded copy is never overwritten.
// Matched=false records a completed no-match so the track is not re-queried each run.
type LyricsEnrichment struct {
	ItemID   int64
	PID      PID
	Matched  bool
	Lyrics   *Lyrics
	Provider string // the provider that supplied the lyrics ("lrclib", ...)
}

// AlbumReleaseMatch is the release one album was matched to, applied fill-when-empty
// like every other entity MBID. Matched=false records a completed no-match so the
// album is not re-searched every run. Reason names the evidence that decided it
// ("barcode", "catalog number", "medium and country", ...), for the change log and for
// a human reading a log line; nothing branches on it.
//
// Provider is the enrichment marker's provider string, and it carries meaning: the
// weaker edition tier records its own value so an edition match stays findable,
// reviewable, and undoable afterwards. Art is that specific pressing's front cover,
// which only a matched release has (the release-group pass fetches the group's).
type AlbumReleaseMatch struct {
	AlbumID  int64
	PID      PID
	Matched  bool
	MBID     string
	Reason   string
	Provider string
	// Art keeps meaning the front cover. AuxArt carries the role-tagged images
	// excluding front, applied fill-when-empty per role at the album's own rung and
	// skipped entirely under the album's art lock.
	Art    *ArtImage
	AuxArt map[ArtRole]*ArtImage
}

// BookEnrichment is the resolved data for one audiobook: external identifiers and
// the publisher, filled only when the corresponding field is currently empty so a
// tagged value is never overwritten.
type BookEnrichment struct {
	BookItemID int64
	PID        PID
	Matched    bool
	MBID       string
	ASIN       string
	ISBN       string
	Publisher  string
}

// EnrichmentCoverage reports how many entities of each type have been enriched,
// for doctor and audit.
type EnrichmentCoverage struct {
	Artists       int
	ReleaseGroups int
	Books         int
	Matched       int // rows where a provider returned a usable match
}
