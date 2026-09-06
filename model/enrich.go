package model

import "strings"

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

// EnrichSweep names which targets a queue walk selects, so one pass can walk the
// entities nothing has asked about yet and, separately, the ones a past lookup found
// nothing for and whose marker has since expired.
type EnrichSweep int

const (
	// SweepFresh selects targets carrying no marker at all. It is the zero value, so a
	// caller that names no sweep keeps the meaning the queue methods always had.
	SweepFresh EnrichSweep = iota
	// SweepRetry selects targets whose marker records a no-match stamped at or before
	// the cutoff. A matched marker is never selected: the provider answered, and
	// re-asking about the slots it left empty would repeat one request per entity per
	// window for roles most providers never serve (Deezer serves an artist front and
	// never a background). Registering a provider that fills those is what a phase-scoped
	// force is for.
	SweepRetry
	// SweepDue is the union of the two, which is what one run's count has to report:
	// everything a full pass will walk, whether it arrives on the fresh sweep or the
	// retry one.
	SweepDue
	// SweepAll selects every target, marker or none. It is the forced run, a scoped
	// run, which implies force, and the phases a phase-scoped force names.
	SweepAll
)

// EnrichPhase names one phase of the enrichment pass, in the run's own order. The keys
// are the vocabulary a phase-scoped force takes, over the proxy and at the CLI, and the
// heartbeat labels a phase by its key too.
type EnrichPhase string

const (
	EnrichPhaseArtist       EnrichPhase = "artist"
	EnrichPhaseReleaseGroup EnrichPhase = "release-group"
	EnrichPhaseAlbumRelease EnrichPhase = "album-release"
	EnrichPhaseAuxArt       EnrichPhase = "aux-art"
	EnrichPhaseArtistArt    EnrichPhase = "artist-art"
	EnrichPhaseAlbumArt     EnrichPhase = "album-art"
	EnrichPhaseBook         EnrichPhase = "book"
	EnrichPhaseLyrics       EnrichPhase = "lyrics"
	EnrichPhaseTrackFields  EnrichPhase = "track-fields"
	EnrichPhaseBookFields   EnrichPhase = "book-fields"
	EnrichPhaseAlbumFields  EnrichPhase = "album-fields"
)

// EnrichPhases returns every phase in run order, for validation and help text.
func EnrichPhases() []EnrichPhase {
	return []EnrichPhase{
		EnrichPhaseArtist,
		EnrichPhaseReleaseGroup,
		EnrichPhaseAlbumRelease,
		EnrichPhaseAuxArt,
		EnrichPhaseArtistArt,
		EnrichPhaseAlbumArt,
		EnrichPhaseBook,
		EnrichPhaseLyrics,
		EnrichPhaseTrackFields,
		EnrichPhaseBookFields,
		EnrichPhaseAlbumFields,
	}
}

// Valid reports whether p names a phase. It is a switch rather than a scan of
// EnrichPhases, which hands out a fresh slice per call so a caller cannot edit the
// vocabulary.
func (p EnrichPhase) Valid() bool {
	switch p {
	case EnrichPhaseArtist, EnrichPhaseReleaseGroup, EnrichPhaseAlbumRelease,
		EnrichPhaseAuxArt, EnrichPhaseArtistArt, EnrichPhaseAlbumArt,
		EnrichPhaseBook, EnrichPhaseLyrics, EnrichPhaseTrackFields,
		EnrichPhaseBookFields, EnrichPhaseAlbumFields:
		return true
	}
	return false
}

// EnrichPhasesOf converts wire or flag keys to phases, unvalidated: the engine refuses
// an unknown one, and both the CLI and the proxy handler take that answer.
func EnrichPhasesOf(keys []string) []EnrichPhase {
	if len(keys) == 0 {
		return nil
	}
	out := make([]EnrichPhase, len(keys))
	for i, k := range keys {
		out[i] = EnrichPhase(k)
	}
	return out
}

// Label renders the phase for a message, the key with its hyphens as spaces.
func (p EnrichPhase) Label() string { return strings.ReplaceAll(string(p), "-", " ") }

// EnrichQueueOptions selects what one queue walk (or the count that mirrors it) should
// return. MissCutoff is a unix-ns instant: a no-match marker stamped at or before it has
// expired and is walked again. Zero means no miss ever expires, so SweepRetry selects
// nothing and SweepDue equals SweepFresh.
type EnrichQueueOptions struct {
	Sweep      EnrichSweep
	MissCutoff int64
}

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
	// artist_art, album_art, fields, or fields_album for the passes that borrow the
	// marker table for their own granularity.
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
	// MBID because MBID names the target's own id. The release-match queue leaves that
	// empty by construction, since an album carrying one is not queued there; the art
	// queue fills both, the release id included, because a provider needs it to know
	// whose pressing it is being asked about.
	Barcode          string
	CatalogNumber    string
	Media            string
	Country          string
	ReleaseGroupMBID string
	// HasArt reports whether the target already holds a front image, and the walks that
	// set it are the artist identity queue, the artist-art backfill, and the album-art
	// backfill. What counts as held differs by rung: an album consumes the art fallback
	// chain, so a member track's embedded cover answers its front, while an artist is a
	// source in that chain and only its own row counts. A pass asks for a front only
	// when this is false, so a library whose rips carry embedded art spends no
	// rate-limited requests on covers the store would refuse to fill anyway.
	HasArt bool
	// ArtLocked reports whether the entity's whole "art" lock stands, the one that gates
	// the front cover and every auxiliary role alike. The artist identity queue and the
	// release-group queue set it for the same reason: the store refuses the write, so
	// fetching first would spend a rate-limited request on every locked cover, every
	// forced run. A per-role lock is not a reason to skip the fetch, so it is checked at
	// apply instead. The art backfill queues carry the whole-entity lock in their
	// predicates rather than here, since a locked entity has nothing for them to do.
	ArtLocked bool
	// GroupFrontHash is the content hash of the release group's enrichment front, set by
	// the album-art queue (the parent group's) and the release-group queue (the group's
	// own), and empty when that front is absent or was chosen by hand, so a provider can
	// be offered the reuse of bytes it recognizes, or ask about them conditionally.
	// ReleaseGroupMBID rides beside it for the album ask.
	GroupFrontHash string
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
	Identity  bool
	Albums    bool // albums needing a release match
	AuxArt    bool // release groups needing an auxiliary-art backfill
	ArtistArt bool // artists needing an art backfill
	// AlbumArt counts albums needing an art backfill, per askable slot. The zero value
	// counts none, mirroring a run whose providers gate the phase off entirely.
	AlbumArt    AlbumArtSlots
	Lyrics      bool // tracks needing a lyrics lookup
	TrackFields bool // tracks needing a scalar-fields lookup
	BookFields  bool // books needing a scalar-fields lookup
	AlbumFields bool // albums needing a scalar-fields lookup
	// Forced names the phases a phase-scoped force walks under SweepAll, so their count
	// takes every target while the rest are counted under the run's own sweep.
	Forced []EnrichPhase
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
// reviewable, and undoable afterwards. It carries no art: the album-art backfill keys
// on the stored identifiers, so the pressing's own cover is fetched there instead of
// riding the moment an id lands.
type AlbumReleaseMatch struct {
	AlbumID  int64
	PID      PID
	Matched  bool
	MBID     string
	Reason   string
	Provider string
}

// AlbumArtBackfill is the art one album-art backfill pass gathered, the album twin of
// ArtistArtBackfill. It is keyed on the album's printed identifiers rather than on a
// name, because the releases of one group share a title and the wrong edition's picture
// is the failure this rung exists to avoid.
//
// It carries a front for the reason the artist backfill does: the album rung has no
// other producer, so an album whose members carry no embedded cover has nothing at all
// there, and the group's cover standing in for every edition is what a per-release ask
// replaces. Art is nil when the album already resolves a front or nothing offered one.
//
// Matched=false records a completed lookup nothing answered, so the album is not
// re-asked every run. Provider names who supplied the first image, and the marker
// carries it; the store substitutes its own label when there is none, since the column
// is NOT NULL.
type AlbumArtBackfill struct {
	AlbumID  int64
	PID      PID
	Matched  bool
	Provider string
	Art      *ArtImage
	AuxArt   map[ArtRole]*ArtImage
	// FrontFromGroup says the group's enrichment front, the row whose source hash is
	// GroupFrontHash, is this pressing's own per the provider that fetched it, so the
	// store attaches that picture at the album rung from the row it already holds. Art is
	// nil then. A copy that finds no such row (the group's front moved between the queue
	// page and the write) leaves the marker unmatched, since the album is still vacant
	// and a durable match would never ask again.
	FrontFromGroup bool
	GroupFrontHash string
}

// AlbumArtSlots names which album art vacancies a walk may ask about. Front needs a
// provider advertising CapCover, which the built-in Cover Art Archive does at the
// release rung, so a stock install asks it; Aux needs one advertising CapAuxArt, which
// no built-in does. A slot no registered provider can fill is left out of the vacancy
// test, so a stock install never marks an album for a vacancy nothing could have
// answered. Both false means the phase does not run at all.
type AlbumArtSlots struct {
	Front bool
	Aux   bool
}

// Any reports whether either slot is askable, which is the phase's own gate.
func (s AlbumArtSlots) Any() bool { return s.Front || s.Aux }

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
