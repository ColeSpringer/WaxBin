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
// without a contact, since it brings its own credentials. A pass consults Config.Providers
// ahead of the built-ins, in that order, unless Config.ProviderList answers with another
// list for that pass.
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
// Request.Want names the capabilities whose answers the calling pass will use, usually
// one, so a provider advertising several can skip the work the caller will not read: a
// genres pass on a genres-plus-cover provider need not download the cover.
// Honoring it is an optimization, never an obligation. A provider may keep answering
// with everything it has, since the Service ignores whatever a pass did not ask for,
// and a zero Want means everything (the pre-Want contract, which is what an embedder
// calling Enrich directly gets without changing anything). Check it with
// req.Wants(c) rather than comparing bits.
type Provider interface {
	// Name is the stable id recorded as provenance ("musicbrainz", "lrclib", ...). It
	// is written to entity_enrichment.provider and field_provenance.provider so a
	// consumer can attribute a value and reason about a metadata conflict. It is also
	// the provider's identity within a pass, so an injected provider's name has to be
	// its own: one repeating another's, or taking a built-in's or a marker label (see
	// ProviderMusicBrainz and the constants beside it), is dropped when the Service is
	// built.
	Name() string
	// Capabilities reports which enrichment kinds the provider supplies, so the
	// Service only calls it for a request it can answer. A provider whose capabilities
	// differ by target type also implements TargetCapabilities to say which rung serves
	// which.
	Capabilities() Capability
	// Enrich answers one candidate lookup. A nil candidate with a nil error is a clean
	// no-match (the entity was looked up and nothing was found). An error is a failed
	// lookup, not a miss: the Service logs it, applies what the other providers answered,
	// and records the lookup as owed, asked once more on a later pass after its new
	// targets (only the identity spine aborts a run). A provider that fails three times
	// in a row is left out for the rest of the pass, with the slots it serves recorded as
	// misses on the targets after it. So a plain miss is a nil candidate, and so is an
	// answer that is certain to come back the same for this target, like one too large
	// to store; a response that could equally come from a service that is not answering,
	// a refusal or an error page, is better returned as an error, which costs one more
	// request where a wrong nil would settle a night's targets as misses.
	Enrich(ctx context.Context, req Request) (*Candidate, error)
}

// Capability is a bitset of the enrichment kinds a provider can supply. A provider
// advertises the union of what it serves, and the Service dispatches a request only
// to a provider whose capability set covers it at the request's target type (see
// TargetCapabilities).
type Capability uint

const (
	// CapIdentity resolves an entity's external anchor (an MBID/ASIN). Reserved for
	// injected identity providers; the built-in spine is MusicBrainz + AcoustID and is
	// not registered on the port.
	CapIdentity Capability = 1 << iota
	// CapGenres supplies genres/tags for a release group.
	CapGenres
	// CapCover supplies cover-art bytes for a release group or for one release, and
	// gates the front halves of the group-art and album-art backfills. The rung is the
	// request type: a TargetReleaseGroup answer is one edition's art standing in for the
	// whole group, while a TargetRelease answer is the pressing an album actually is,
	// which is what the album backfill asks for. The built-in Cover Art Archive serves
	// both and says so through TargetCapabilities, so both front halves run on a stock
	// install. A provider serving covers for groups alone declares that the same way, so
	// the album front half does not walk every identified album on its account. It is not
	// consulted at the artist rung, which answers to CapArtistFront.
	CapCover
	// CapLyrics supplies a recording's lyrics.
	CapLyrics
	// CapBookMeta supplies an audiobook's identifiers and publisher, and gates the book
	// fields walk. That walk reads Candidate.Fields for the book's fill set alongside
	// the dedicated Publisher/ASIN/ISBN slots, which stay as the shorthand for the three
	// fields every book provider answers.
	CapBookMeta
	// CapAuxArt supplies the auxiliary art roles (back, disc, booklet, background) for
	// a release group or a release, in Candidate.Art. It is separate from CapCover because
	// it gates the auxiliary halves of those two backfills, which consult only the
	// providers advertising this, and keep their answers apart from the front's. The
	// built-in Cover Art Archive serves the front alone and does not advertise it, so an
	// install with no injected provider walks no auxiliary half and pays nothing for it.
	// Like CapCover it is not consulted at the artist rung, which answers to
	// CapArtistAuxArt.
	//
	// A provider that already returns auxiliary roles under CapCover keeps working
	// exactly as before and contributes to the first-pass gather. To join the backfill
	// it advertises this alongside CapCover, and answers a request whose Want includes
	// CapAuxArt with the non-front roles it has. A walk with both halves open asks such a
	// provider once, with both capabilities in Want; one whose front is settled asks under
	// CapAuxArt alone, and a front in that answer is ignored.
	//
	// A provider serving these roles for release groups and not for a release declares
	// the release rung empty through TargetCapabilities, so the album-art backfill's
	// auxiliary half does not walk every identified album for an answer it cannot give.
	//
	// The request carries the group's Title and Artist, with MBID only when the catalog
	// has one, since the walk is keyed on the title rather than the id. A provider keyed
	// on ids alone answers a nil candidate for an id-less request rather than an error:
	// an error defers the target and three in a row retire the provider for the run, so
	// a miss reported as one would re-ask a population that is mostly id-less forever
	// and take the provider out of every pass. The built-in archive already does this
	// (see enrich/coverart.go).
	CapAuxArt
	// CapArtistFront supplies an artist's front, the portrait, and gates the front half of
	// the artist-art backfill. It is its own bit, apart from CapCover, because a cover
	// provider that declares no rungs is taken to serve every rung, and gating the artist
	// front on CapCover would ask such a provider about every artist.
	//
	// The request carries the artist's name in Artist, with MBID only when the catalog
	// has one. The walk is keyed on the name, so a local band or a mis-tagged name is
	// asked about too, and a provider keyed on ids alone answers a nil candidate for an
	// id-less request rather than an error.
	CapArtistFront
	// CapFields supplies scalar metadata fields in Candidate.Fields, and gates the track
	// and album fields walks. The rung is the request type rather than a second bit:
	// TargetRecording asks about one track and its answer lands on that item alone,
	// while TargetRelease asks about an album and its answer lands on the album row and,
	// for year, on every member at once. A provider that only knows one of the two
	// declares the rung it serves through TargetCapabilities; one that declares nothing
	// is asked at both, and its nil answer marks a miss.
	//
	// The engine applies only the keys in the target's fill set (model.EnrichFillFields
	// for an item, model.AlbumFillFields for an album), fill-when-empty, lock-respecting,
	// and stamped with the provider's name; everything else in the map is ignored, so a
	// provider returns what it found rather than pre-filtering.
	CapFields
	// CapArtistAuxArt supplies an artist's auxiliary art, the background, and gates the
	// auxiliary half of the artist-art backfill, the artist rung's CapAuxArt. A provider
	// that does not advertise it is never asked about an artist's background, so one
	// serving fronts alone leaves no background miss for the retry window to re-ask. The
	// request carries what CapArtistFront's does.
	CapArtistAuxArt
)

// CapArtistArt is both artist bits, for a provider serving an artist's front and
// background alike; it is asked once per artist whichever halves are open, and one serving
// a single half advertises that half's bit. CapCover and CapAuxArt are not consulted at
// the artist rung, and the Service warns once about a provider offering them there, one
// declaring no rungs included.
const CapArtistArt = CapArtistFront | CapArtistAuxArt

// TargetCapabilities is the optional half of a provider's declaration, for one whose
// capabilities differ by target type: the Cover Art Archive serves a cover for a release
// group and for a release and nothing for an artist; a fan-art service serves auxiliary
// art for a release group, and artist art, under CapArtistArt, for an artist, and
// nothing for a release. Capabilities stays the union; CapabilitiesAt narrows it to what
// the provider answers for one target type, and the Service consults it wherever it
// dispatches, so a provider is asked only at the rungs it serves and a phase runs only
// when some provider serves its capability at its own rung. A provider that does not
// implement it is taken to serve every capability it advertises at every rung, which is
// what every provider written before it did.
type TargetCapabilities interface {
	CapabilitiesAt(t TargetType) Capability
}

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
	//
	// The request carries whichever of MBID, Barcode and CatalogNumber the catalog holds.
	// A provider keyed on an identifier answers for the pressing; one keyed on a title
	// alone answers nothing here, since the titles of a group's releases are the same.
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
	// Want names the capabilities whose answers this pass will use: one, or an art rung's
	// front and auxiliary pair when one call asks about both halves. Zero means everything
	// (the pre-Want contract, so existing providers and embedders are untouched). A
	// provider may skip work whose results serve only capabilities absent from Want; the
	// Service ignores what else it returns, save the auxiliary art a cover answer carries.
	Want   Capability
	Title  string // artist name | release-group title | track title | book title
	Artist string // disambiguating primary artist (release group / recording / book)
	Album  string // album title, for a recording lyrics lookup
	MBID   string // known identity anchor (artist / release-group / recording MBID)
	ASIN   string
	ISBN   string
	// ISRC is the recording's identifier, carried by the fields walks so a provider
	// keyed on it can answer without a text match. It is empty when the catalog holds
	// none.
	ISRC string
	// Barcode and CatalogNumber are the release's printed identifiers, verbatim as the
	// tags spelled them, so a provider normalizes before comparing (model.NormalizeBarcode
	// is exported for it). Every TargetRelease request carries them, art and fields
	// alike, and each is empty when the catalog holds none.
	Barcode       string
	CatalogNumber string
	// ReleaseGroupMBID is the group a TargetRelease request's release belongs to, when
	// the catalog holds it. GroupFrontHash is the content hash of the group's enrichment
	// front as the catalog holds it now (the group's own on a TargetReleaseGroup
	// request, the parent group's on a TargetRelease one), or empty when that front is
	// absent or was chosen by hand. A provider that knows those bytes are this very
	// release's may answer a cover request with Candidate.FrontIsGroupFront instead of
	// bytes, since the caller can reuse the picture it already holds; one that recorded
	// a validator for them may ask the service conditionally and answer nil when it says
	// they are unchanged, which at the group rung keeps the cover in place.
	ReleaseGroupMBID string
	GroupFrontHash   string
	DurationSec      int // track duration, for a duration-disambiguated lyrics match
}

// Wants reports whether this request's pass will use an answer for c. Capability.Has
// is any-overlap, so a want naming an art rung's front and auxiliary pair reports each.
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
	// FrontIsGroupFront says the release group's front cover the caller holds (the bytes
	// behind Request.GroupFrontHash) is this release's own. It answers a TargetRelease
	// request with Cover left nil: the caller attaches the group's picture at the release
	// rung without a second download. Bytes offered for the front alongside it are
	// dropped, since the two answer the same slot; the auxiliary roles are unaffected.
	FrontIsGroupFront bool

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

// The built-in providers' names, which are also the provenance ids they stamp. A
// ProviderList hook finds a built-in in the fixed list it is handed by one of these, and
// a consumer reads them back off provenance rows. An injected provider supplies its own.
const (
	// ProviderMusicBrainz names the identity spine, and in the provider list the entry
	// that ranks the spine's own release-group genres in the genre merge.
	ProviderMusicBrainz  = "musicbrainz"
	ProviderCoverArt     = "coverartarchive"
	ProviderListenBrainz = "listenbrainz"
	ProviderLRCLIB       = "lrclib"
)

// providerMBEdition marks an album whose release the edition tier decided rather than a
// printed identifier. Distinct on purpose: that tier is not immune to MusicBrainz
// coverage gaps (see release.go), so its writes must stay findable and reviewable. It is
// a marker value the store records, not a provider. providerNone is the store's label
// for a marker no provider answered (store/sqlite's enrichProviderNone).
const (
	providerMBEdition = "musicbrainz:edition"
	providerNone      = "none"
)

// reservedProviderName reports whether name belongs to a built-in or labels a marker,
// so an injected provider taking it would write values nobody could tell from the
// built-in's, or markers that read as some other outcome.
func reservedProviderName(name string) bool {
	switch name {
	case ProviderMusicBrainz, ProviderCoverArt, ProviderListenBrainz, ProviderLRCLIB,
		providerMBEdition, providerNone:
		return true
	}
	return false
}

// Mock is a scriptable Provider for tests and for standing in for an injected
// provider (Discogs, Last.fm, ...) without any network. Set ProviderName + Caps and
// either EnrichFunc for full control or the simple Ret/Err fields for the common
// case. It never touches the network.
type Mock struct {
	ProviderName string
	Caps         Capability
	// CapsAt, when set, is the mock's per-target declaration (TargetCapabilities): a
	// target type it names serves that entry, and one it leaves out serves nothing. Nil
	// serves Caps at every target type.
	CapsAt map[TargetType]Capability

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

// CapabilitiesAt reports CapsAt's entry for t, or Caps when CapsAt is nil.
func (m *Mock) CapabilitiesAt(t TargetType) Capability {
	if m.CapsAt == nil {
		return m.Caps
	}
	return m.CapsAt[t]
}

// Enrich returns the scripted hook result, or the simple-mode Ret/Err.
func (m *Mock) Enrich(ctx context.Context, req Request) (*Candidate, error) {
	if m.EnrichFunc != nil {
		return m.EnrichFunc(ctx, req)
	}
	return m.Ret, m.Err
}
