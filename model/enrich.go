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
	// FieldsItemIDs are the items the fields walks should ask about. Tracks and books
	// share the item id space, so one list covers both walks.
	FieldsItemIDs []int64
}

// EnrichTarget is one entity the enrichment pass should look up. Type selects the
// provider query; MBID (when already known) is the fast path; Name/ArtistName/Year
// disambiguate a text search when there is no MBID. IDs are internal store rowids
// (the enrich Store port is implemented only by store/sqlite, so it exchanges
// rowids like the analyze port does).
type EnrichTarget struct {
	// Type is the entity type or the pass marker this target belongs to: artist,
	// release_group, album, or book for the identity phases, and lyrics, aux_art,
	// artist_art, fields, or fields_album for the passes that borrow the marker table
	// for their own granularity.
	Type       string
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

	// The fields walks carry whichever identifiers the item already holds, so a
	// provider keyed on one can use it instead of a text match. ISRC is a recording's,
	// ASIN and ISBN a book's; each is empty when the catalog has none.
	ISRC string
	ASIN string
	ISBN string

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
	// ArtLocked reports whether the entity's whole "art" lock stands, the one that gates
	// the front cover and every auxiliary role alike. The release-group pass and the
	// album release match both read it for the same reason: the store refuses the write,
	// so fetching first would spend a rate-limited request on every locked cover, every
	// forced run. A per-role lock is not a reason to skip the fetch, so it is checked at
	// apply instead.
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
	// Art is the artist's front image and AuxArt its role-tagged others, background
	// most of all, which is where artist imagery lands. Both are applied fill-when-empty
	// per role at the artist's own rung and skipped entirely under its art lock, exactly
	// as at the release-group rung. No built-in provider answers either: the Cover Art
	// Archive is release-group keyed, so these fill only for an injected provider.
	Art    *ArtImage
	AuxArt map[ArtRole]*ArtImage
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

// ReleaseGroupAuxArt is the auxiliary-role backfill for one release group: the
// images an aux-capable provider offered for the roles beside the front. It is
// separate from ReleaseGroupEnrichment because the two passes ask different
// questions. That one resolves identity and fetches art on the way past, keyed on
// the front; this one asks only about the empty aux slots of a group whose front is
// already settled, which is the case the front-keyed pre-guards can never reach.
//
// Matched=false records a completed lookup nothing answered, so the group is not
// re-asked every run. Provider names who supplied the first image, and the marker
// carries it; the store substitutes its own label when there is none, since the
// column is NOT NULL. AuxArt never carries the front role: the release-group pass
// owns that slot.
type ReleaseGroupAuxArt struct {
	ReleaseGroupID int64
	PID            PID
	Matched        bool
	Provider       string
	AuxArt         map[ArtRole]*ArtImage
}

// EnrichCountOptions selects the optional phases a heartbeat denominator should count.
// Each flag mirrors whether the run actually runs that phase (a toggle for the album
// release match, a registered capability for the rest), because a denominator counting
// work the run will not do reports a ratio that never reaches one.
type EnrichCountOptions struct {
	// Identity covers the MusicBrainz-backed phases (artist, release group, book).
	// They run only with a contact configured, so a contact-less run counts none of
	// them; Albums is the release match, which needs the toggle as well.
	Identity    bool
	Albums      bool // albums needing a release match
	AuxArt      bool // release groups needing an auxiliary-art backfill
	ArtistArt   bool // artists needing an art backfill
	Lyrics      bool // tracks needing a lyrics lookup
	TrackFields bool // tracks needing a scalar-fields lookup
	BookFields  bool // books needing a scalar-fields lookup
	AlbumFields bool // albums needing a scalar-fields lookup
}

// ArtistArtBackfill is the art one artist-art backfill pass gathered. It is the artist
// twin of ReleaseGroupAuxArt, and separate from ArtistEnrichment for the same reason:
// that one resolves identity and fetches art on the way past, so an artist it has
// already marked never gets asked again, while this one asks only about the empty slots
// of an artist whose identity is settled.
//
// Unlike the release-group backfill it does carry a front. Artist-rung art is fetched
// inside the identity pass, so an already-marked artist has no picture at all, and the
// front is the usual gap rather than the settled slot. Art is nil when the front is
// already held or nothing offered one.
//
// Matched=false records a completed lookup nothing answered, so the artist is not
// re-asked every run. Provider names who supplied the first image, and the marker
// carries it; the store substitutes its own label when there is none, since the column
// is NOT NULL.
type ArtistArtBackfill struct {
	ArtistID int64
	PID      PID
	Matched  bool
	Provider string
	Art      *ArtImage
	AuxArt   map[ArtRole]*ArtImage
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

// ItemFieldsEnrichment is the scalar fields a provider supplied for one item, track or
// book. Fields carries the metadata vocabulary keyed the way an edit does; the store
// keeps only the keys in the kind's EnrichFillFields set that are currently empty and
// unlocked, so a provider returns everything it found and the store decides what lands.
// Matched=false records a completed lookup nothing answered, so the item is not re-asked
// every run.
type ItemFieldsEnrichment struct {
	ItemID  int64
	PID     PID
	Matched bool
	// Provider names the marker's provider: who answered at all. Providers names the
	// provider per field, since two of them commonly split a set (one knows the bpm,
	// another the isrc) and the provenance row is where a consumer attributes a value.
	// A field absent from Providers falls back to Provider.
	Provider  string
	Providers map[string]string
	Fields    map[string]string
}

// AlbumFieldsEnrichment is the scalar fields a provider supplied for one album, the
// entity rung of the same walk. Only AlbumFillFields keys are applied: label lands on
// the album row itself, year on every member at once through the uniform whole-album
// edit, since year participates in the album identity key and a per-member write would
// fork the album.
type AlbumFieldsEnrichment struct {
	AlbumID int64
	PID     PID
	Matched bool
	// Provider and Providers split the same way ItemFieldsEnrichment's do: the marker's
	// provider, and the provider behind each field for its curation row.
	Provider  string
	Providers map[string]string
	Fields    map[string]string
}

// EnrichFillFields is the set of scalar fields an item-rung enrichment walk may fill for
// a kind, and the port's answer to which keys in a Candidate.Fields map are applied.
//
// The rule is the kind's editable scalar fields minus everything a provider's guess has
// no business deciding: the identity keys (a per-member write forks the entity off its
// album or its author), the title, genre (CapGenres owns that), the recording MBID
// (identity), the positions and flags a file's own tags settle (track_no, disc_no,
// compilation), the derived sort fields (an edit of the display field regenerates them,
// so filling one would be undone and could restore a locked value), and comment, which
// is the listener's own note rather than a fact about the recording.
//
// A kind with no fields walk returns nil.
func EnrichFillFields(kind Kind) map[string]bool {
	switch kind {
	case KindTrack:
		return map[string]bool{"bpm": true, "isrc": true, "composer": true}
	case KindBook:
		return map[string]bool{
			"publisher": true, "year": true, "description": true, "narrator": true,
			"subtitle": true, "edition": true, "asin": true, "isbn": true,
		}
	}
	return nil
}

// AlbumFillFields is the entity-rung twin of EnrichFillFields: the album fields a
// provider may fill. The release identifiers (barcode, catalog number, media, country)
// are refused on purpose, since they are the evidence the MusicBrainz release matcher
// searches by and a provider's guess must not drive it.
func AlbumFillFields() map[string]bool {
	return map[string]bool{"label": true, "year": true}
}

// EnrichmentCoverage reports how many entities of each type have been enriched,
// for doctor and audit.
type EnrichmentCoverage struct {
	Artists       int
	ReleaseGroups int
	Books         int
	Matched       int // rows where a provider returned a usable match
}
