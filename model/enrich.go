package model

// Enrichment types cross the port between the enrich package (which talks to
// MusicBrainz / Cover Art Archive) and store/sqlite (which persists results). The
// enrich pass reads targets, resolves them against a provider, then hands back a
// typed result the store applies atomically, respecting locks and provenance.

// Enrichment entity-type discriminators for EnrichTarget and entity_enrichment.
const (
	EnrichArtistType       = "artist"
	EnrichReleaseGroupType = "release_group"
	EnrichBookType         = "book"
)

// Artist relation kinds stored in artist_relation.
const (
	RelationMemberOf = "member_of"
	RelationAKA      = "aka"
	RelationSimilar  = "similar"
)

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
	Art           *ArtImage
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
