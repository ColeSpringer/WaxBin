package enrich

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// Provider is the pluggable metadata-provider port. It mirrors source.Provider:
// each provider serves one external service (MusicBrainz, LRCLIB, ListenBrainz, an
// embedder's Discogs/Last.fm/Audnexus), advertises what it can supply, and answers a
// candidate lookup. Implementations depend only on model/identity (no store), so the
// port never pulls persistence into the provider layer, and they are safe for
// concurrent use.
//
// The MusicBrainz + AcoustID identity spine is NOT expressed through this port. It
// resolves the MBID that anchors every entity and is tried first when a contact is
// configured (see Service.Run). This port carries the layerable candidates that fill
// gaps on top of that anchor (genres, cover art, lyrics, book identifiers, and the
// scalar fields the track, book, and album fields walks apply), so an embedder can add
// a provider without touching identity resolution. A provider's own phases run with or
// without a contact, since it brings its own credentials.
//
// Rate limiting is the provider's own responsibility. The Service calls Enrich
// sequentially within a single-goroutine pass, so a provider is never invoked
// concurrently during one run, and it bounds each call with a soft timeout. It does
// not throttle a provider's request rate, since only the provider knows its service's
// limits, which are often multi-dimensional (a per-second and a per-day cap) and vary
// with the API key tier. A provider that makes network calls must enforce its own
// per-host pacing so an application embedding WaxBin is never rate-limited or banned.
// The built-ins get this from WaxBin's internal HTTP client and its per-host minimum
// interval (MusicBrainz at 1 req/s, the key-free built-ins gentler); an injected
// provider supplies its own HTTP client and should pace it the same way, with a
// per-host minimum interval or a token bucket, rather than leaning on the Service to
// space its calls.
//
// Request.Want names the capability whose answer the calling pass will use, so a
// provider advertising several capabilities can skip the work the caller will not
// read: a genres pass on a genres-plus-cover provider need not download the cover.
// Honoring it is an optimization, never an obligation. A provider may keep answering
// with everything it has, since the Service ignores whatever a pass did not ask for,
// and a zero Want means everything (the pre-Want contract, which is what an embedder
// calling Enrich directly gets without changing anything). Check it with
// req.Wants(c) rather than comparing bits.
type Provider interface {
	// Name is the stable id recorded as provenance ("musicbrainz", "lrclib", ...). It
	// is written to entity_enrichment.provider and field_provenance.provider so a
	// consumer can attribute a value and reason about a metadata conflict.
	Name() string
	// Capabilities reports which enrichment kinds the provider supplies, so the
	// Service only calls it for a request it can answer.
	Capabilities() Capability
	// Enrich answers one candidate lookup. A nil candidate with a nil error is a clean
	// no-match (the entity was looked up and nothing was found); an error is a
	// best-effort failure the Service logs and continues past (only the identity spine
	// aborts a run).
	Enrich(ctx context.Context, req Request) (*Candidate, error)
}

// Capability is a bitset of the enrichment kinds a provider can supply. A provider
// advertises the union of what it serves, and the Service dispatches a request only
// to a provider whose capability set covers it.
type Capability uint

const (
	// CapIdentity resolves an entity's external anchor (an MBID/ASIN). Reserved for
	// injected identity providers; the built-in spine is MusicBrainz + AcoustID and is
	// not registered on the port.
	CapIdentity Capability = 1 << iota
	// CapGenres supplies genres/tags for a release group.
	CapGenres
	// CapCover supplies release-group cover-art bytes.
	CapCover
	// CapLyrics supplies a recording's lyrics.
	CapLyrics
	// CapBookMeta supplies an audiobook's identifiers and publisher, and gates the book
	// fields walk. That walk reads Candidate.Fields for the book's fill set alongside
	// the dedicated Publisher/ASIN/ISBN slots, which stay as the shorthand for the three
	// fields every book provider answers.
	CapBookMeta
	// CapAuxArt supplies the auxiliary art roles (back, disc, booklet, background) for
	// a release group, in Candidate.Art. It is separate from CapCover because it gates
	// its own pass: the auxiliary backfill re-asks about groups whose front cover is
	// already settled, which the cover-fetching passes never do, and it consults only
	// the providers advertising this. The built-in Cover Art Archive serves the front
	// alone and does not advertise it, so an install with no injected provider runs the
	// backfill not at all and pays nothing for it.
	//
	// A provider that already returns auxiliary roles under CapCover keeps working
	// exactly as before and contributes to the first-pass gather. To join the backfill
	// it advertises this alongside CapCover, and answers a request whose Want is
	// CapAuxArt with the non-front roles it has (the front is ignored there; the
	// release-group pass owns that slot).
	//
	// The request carries the group's Title and Artist, with MBID only when the catalog
	// has one, since the walk is keyed on the title rather than the id. A provider keyed
	// on ids alone answers a nil candidate for an id-less request rather than an error:
	// callProvider logs an error once per entity per run, so a miss reported as one is
	// noise on a population that is mostly id-less. The built-in archive already does
	// this (see enrich/coverart.go).
	CapAuxArt
	// CapArtistArt supplies art for an artist, front and auxiliary roles alike, and gates
	// the artist backfill. It is its own bit because the Cover Art Archive advertises
	// CapCover and answers nothing for an artist: gating there would walk every artist on
	// a stock install and mark each a permanent no-match, which is the bug the backfill
	// exists to remove. Like CapAuxArt, a provider written before it advertises CapCover
	// alone and has to add this to be queued.
	//
	// The request carries the artist's name in Artist, with MBID only when the catalog
	// has one. The walk is keyed on the name, so a local band or a mis-tagged name is
	// asked about too, and a provider keyed on ids alone answers a nil candidate for an
	// id-less request rather than an error.
	CapArtistArt
	// CapFields supplies scalar metadata fields in Candidate.Fields, and gates the track
	// and album fields walks. The rung is the request type rather than a second bit:
	// TargetRecording asks about one track and its answer lands on that item alone,
	// while TargetRelease asks about an album and its answer lands on the album row and,
	// for year, on every member at once. A provider that only knows one of the two
	// answers nothing for the other.
	//
	// The engine applies only the keys in the target's fill set (model.EnrichFillFields
	// for an item, model.AlbumFillFields for an album), fill-when-empty, lock-respecting,
	// and stamped with the provider's name; everything else in the map is ignored, so a
	// provider returns what it found rather than pre-filtering.
	CapFields
)

// Has reports whether c advertises want.
func (c Capability) Has(want Capability) bool { return c&want != 0 }

// TargetType selects which entity a Request concerns, so a provider can key its
// lookup and refuse a target it does not serve.
type TargetType string

const (
	TargetArtist       TargetType = "artist"        // one artist
	TargetReleaseGroup TargetType = "release_group" // one album/release group (genres, cover)
	// TargetRelease is one specific release (edition) of a group, for the cover of the
	// pressing an album actually is. It is separate from TargetReleaseGroup because the
	// group's cover is one edition's art standing in for all of them, and a provider that
	// only knows groups should answer nothing rather than the wrong picture.
	TargetRelease   TargetType = "release"
	TargetBook      TargetType = "book"      // one audiobook (identifiers, publisher)
	TargetRecording TargetType = "recording" // one track (lyrics)
)

// Request is a provider lookup input. The Service fills the identity hints it has;
// a provider uses whichever it needs (LRCLIB keys on Title+Artist+Album+DurationSec,
// the Cover Art Archive on MBID). Force asks a caching provider to bypass its cache.
type Request struct {
	Type  TargetType
	Force bool
	// Want is the capability whose answer this pass will use; zero means everything
	// (the pre-Want contract, so existing providers and embedders are untouched). A
	// provider may skip work whose results serve only capabilities absent from Want;
	// anything extra it returns is ignored by the Service.
	Want   Capability
	Title  string // artist name | release-group title | track title | book title
	Artist string // disambiguating primary artist (release group / recording / book)
	Album  string // album title, for a recording lyrics lookup
	MBID   string // known identity anchor (artist / release-group / recording MBID)
	ASIN   string
	ISBN   string
	// ISRC is the recording's identifier and Barcode the release's, carried by the
	// fields walks so a provider keyed on one can answer without a text match. Each is
	// empty when the catalog holds none.
	ISRC        string
	Barcode     string
	DurationSec int // track duration, for a duration-disambiguated lyrics match
}

// Wants reports whether this request's pass will use an answer for c. Capability.Has
// is any-overlap, which is exact for the single-bit wants the Service stamps.
func (r Request) Wants(c Capability) bool { return r.Want == 0 || r.Want.Has(c) }

// Candidate is a provider's proposed enrichment for one request. The Service applies
// it fill-when-empty and lock-respecting, so a provider returns everything it found
// and the store decides what actually lands. A nil *Candidate is a clean no-match.
// Confidence is advisory (0..1); the Service currently orders by provider priority,
// not score.
type Candidate struct {
	Confidence float64

	// Identity anchors an injected identity provider may resolve.
	MBID string
	ASIN string
	ISBN string

	// ReleaseGroup fields.
	Type   string   // album|ep|single|compilation|audiobook
	Genres []string // display names, provider-ordered (highest confidence first)
	// Cover stays as the front alias. Art carries role-tagged images (back, disc,
	// booklet, background, and optionally front); the effective front is
	// Art[ArtRoleFront] when present, else Cover. Non-front roles apply
	// fill-when-empty at the target entity's own level.
	Cover *model.ArtImage
	Art   map[model.ArtRole]*model.ArtImage

	// Book fields.
	Publisher string

	// Recording fields.
	Lyrics *model.Lyrics

	// Fields carries scalar values keyed by the metadata vocabulary
	// (model.MetadataFields), for a provider that supplies a field with no dedicated
	// slot above. The engine applies only the keys in the target's fill set
	// (model.EnrichFillFields for a recording or a book, model.AlbumFillFields for a
	// release) and ignores the rest, so a provider fills in what it found without
	// knowing which fields the catalog will take. Reserved for injected providers; the
	// built-ins leave it nil.
	Fields map[string]string
}

// Provider-name constants used as provenance ids. The built-ins are fixed; an
// injected provider supplies its own Name().
const (
	providerMusicBrainz = "musicbrainz"
	// providerMBEdition marks an album whose release the edition tier decided rather than
	// a printed identifier. Distinct on purpose: that tier is not immune to MusicBrainz
	// coverage gaps (see release.go), so its writes must stay findable and reviewable.
	providerMBEdition    = "musicbrainz:edition"
	providerCoverArt     = "coverartarchive"
	providerListenBrainz = "listenbrainz"
	providerLRCLIB       = "lrclib"
)

// Mock is a scriptable Provider for tests and for standing in for an injected
// provider (Discogs, Last.fm, ...) without any network. Set ProviderName + Caps and
// either EnrichFunc for full control or the simple Ret/Err fields for the common
// case. It never touches the network.
type Mock struct {
	ProviderName string
	Caps         Capability

	// Simple mode: Enrich returns Ret, Err.
	Ret *Candidate
	Err error

	// Hook mode overrides simple mode when set.
	EnrichFunc func(ctx context.Context, req Request) (*Candidate, error)
}

// Name reports the mock's configured provider id.
func (m *Mock) Name() string { return m.ProviderName }

// Capabilities reports the mock's configured capability set.
func (m *Mock) Capabilities() Capability { return m.Caps }

// Enrich returns the scripted hook result, or the simple-mode Ret/Err.
func (m *Mock) Enrich(ctx context.Context, req Request) (*Candidate, error) {
	if m.EnrichFunc != nil {
		return m.EnrichFunc(ctx, req)
	}
	return m.Ret, m.Err
}
