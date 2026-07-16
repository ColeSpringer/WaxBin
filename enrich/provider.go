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
// resolves the MBID that anchors every entity and is always tried first (see
// Service.Run). This port carries the layerable candidates that fill gaps on top of
// that anchor (genres, cover art, lyrics, book identifiers, and generic curated
// fields), so an embedder can add a provider without touching identity resolution.
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
	// CapBookMeta supplies an audiobook's identifiers and publisher.
	CapBookMeta
)

// Has reports whether c advertises want.
func (c Capability) Has(want Capability) bool { return c&want != 0 }

// TargetType selects which entity a Request concerns, so a provider can key its
// lookup and refuse a target it does not serve.
type TargetType string

const (
	TargetArtist       TargetType = "artist"        // one artist
	TargetReleaseGroup TargetType = "release_group" // one album/release group (genres, cover)
	TargetBook         TargetType = "book"          // one audiobook (identifiers, publisher)
	TargetRecording    TargetType = "recording"     // one track (lyrics)
)

// Request is a provider lookup input. The Service fills the identity hints it has;
// a provider uses whichever it needs (LRCLIB keys on Title+Artist+Album+DurationSec,
// the Cover Art Archive on MBID). Force asks a caching provider to bypass its cache.
type Request struct {
	Type        TargetType
	Force       bool
	Title       string // artist name | release-group title | track title | book title
	Artist      string // disambiguating primary artist (release group / recording / book)
	Album       string // album title, for a recording lyrics lookup
	MBID        string // known identity anchor (artist / release-group / recording MBID)
	ASIN        string
	ISBN        string
	DurationSec int // track duration, for a duration-disambiguated lyrics match
}

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
	Cover  *model.ArtImage

	// Book fields.
	Publisher string

	// Recording fields.
	Lyrics *model.Lyrics

	// Fields carries generic curated values keyed by the metadata vocabulary
	// (model.MetadataFields), for a provider that supplies a field with no dedicated
	// slot above. Reserved for injected providers; the built-ins leave it nil.
	Fields map[string]string
}

// Provider-name constants used as provenance ids. The built-ins are fixed; an
// injected provider supplies its own Name().
const (
	providerMusicBrainz  = "musicbrainz"
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
