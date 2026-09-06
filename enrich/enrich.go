// Package enrich populates catalog entities from external metadata providers:
// MusicBrainz (release-group type, artist aliases/relations, genres, and the MBIDs
// that anchor identity) and the Cover Art Archive (release-group cover art), with
// an optional AcoustID fingerprint fallback for release groups that text search
// cannot resolve. Enrichment is MBID-first, provenance-aware, and lock-respecting:
// it never overwrites a tagged or user-locked field, only fills gaps and adds
// entity data. Responses are cached so a re-run, or an offline run, reuses prior
// answers instead of re-hitting a rate-limited API. It requires no bundled dataset
// and degrades gracefully when a provider is unreachable.
//
// Beside the MusicBrainz spine sit the port phases, each gated by a registered
// provider's capability and each running with or without a MusicBrainz contact: the
// three art backfills (release-group auxiliary, artist, album), lyrics, and the fields
// walks that fill a track's, a book's, or an album's empty scalar fields from
// Candidate.Fields. The album one is the phase a stock install still runs, since the
// Cover Art Archive answers at the release rung.
//
// It is the "metadata brain" enrichment half; the WaxLabel tag adapter lives in
// package meta. This package defines its own Store port (implemented by
// store/sqlite) so it depends on the domain model, not on SQLite.
package enrich

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/caps"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the enrichment pass needs, satisfied by store/sqlite.
// The needing-enrichment queries are keyset-paginated by entity id (afterID) so a
// forced re-run, which rewrites the marker rather than removing the entity from the
// set, still advances and terminates. Each takes an optional ids list (nil = the
// full pass) that scopes the walk to explicit rowids, keeping the keyset shape.
//
// Every queue takes model.EnrichQueueOptions, which names the sweep it should walk:
// the targets no marker covers (SweepFresh, the zero value and the ordinary run), the
// ones whose no-match marker has expired against MissCutoff (SweepRetry), their union
// (SweepDue, which only the count asks for), or everything (SweepAll, the forced run).
// A run walks its whole phase list fresh, then walks it again for the expired misses,
// so a capped run reaches the new files of every phase before it re-asks about
// anything. A phase-scoped force walks SweepAll for its phases and the ordinary sweeps
// for the rest.
type Store interface {
	ArtistsNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ReleaseGroupsNeedingEnrichment populates each target's representative file only
	// when includeRepFile is set (the AcoustID fallback needs it), so the correlated
	// lookup is skipped on the common path where AcoustID is off.
	ReleaseGroupsNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, includeRepFile bool, ids []int64) ([]model.EnrichTarget, error)
	// AlbumsNeedingReleaseMatch returns the next keyset page of albums that carry some
	// matchable evidence (a release identifier, or a medium or country) but no release
	// MBID, under a release group that has one.
	AlbumsNeedingReleaseMatch(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ReleaseGroupsNeedingAuxArt returns the next keyset page of release groups the
	// auxiliary-art backfill should ask about: they carry a title, no whole-entity art
	// lock, and at least one empty auxiliary slot. Their front covers are deliberately
	// not consulted, since a settled front is the population the phase exists for. An
	// MBID rides along when the catalog has one but does not gate the walk.
	ReleaseGroupsNeedingAuxArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	BooksNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ItemsNeedingLyrics returns the next keyset page of tracks that carry no lyrics
	// yet and that the sweep selects, each with the title, artist, album, and duration
	// a lyrics provider keys on.
	ItemsNeedingLyrics(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ArtistsNeedingArtBackfill returns the next keyset page of artists with an empty
	// art slot, front or auxiliary. Unlike ReleaseGroupsNeedingAuxArt it does consult
	// the front, because artist art is fetched inside the identity pass and an already
	// marked artist has none at all, which is the gap this phase exists for. It walks by
	// name, so an artist MusicBrainz never matched is asked about too.
	ArtistsNeedingArtBackfill(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// AlbumsNeedingArt returns the next keyset page of albums with an empty art slot the
	// slots argument names as askable. Unlike the two backfills above it walks by
	// identifier rather than by name: the releases of one group share a title, so an
	// album carrying no release mbid, barcode or catalog number is skipped rather than
	// asked about with a title that can only return the wrong edition.
	AlbumsNeedingArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.AlbumArtSlots, ids []int64) ([]model.EnrichTarget, error)
	// ItemsNeedingFields returns the next keyset page of items of one kind whose fill
	// set (model.EnrichFillFields) still has a gap, each carrying the title, credit,
	// duration, and identifiers a provider keys on.
	ItemsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, kind model.Kind, ids []int64) ([]model.EnrichTarget, error)
	// AlbumsNeedingFields returns the next keyset page of albums missing a label or a
	// year, the entity rung of the same walk.
	AlbumsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// CountEntitiesNeedingEnrichment mirrors the phases a run would execute: a nil
	// scope counts everything, a scoped count covers only the scoped ids, and a
	// phase the scoped run skips (an empty id list) contributes zero. Each optional
	// phase's flag must mirror whether the run actually runs it, or the ratio drifts.
	// It is asked for SweepDue when the run walks two sweeps, so the denominator covers
	// both of them, and a phase a phase-scoped force names is counted under SweepAll.
	CountEntitiesNeedingEnrichment(ctx context.Context, q model.EnrichQueueOptions, opts model.EnrichCountOptions, scope *model.EnrichScope) (int, error)

	// ApplyItemFields writes the scalar fields a provider supplied for one item and
	// records the fields marker. Only the keys in the kind's fill set that are empty and
	// unlocked land, each stamped with the provider's name; a value that fails
	// validation is skipped rather than failing the pass, and nothing surviving writes
	// the marker alone.
	ApplyItemFields(ctx context.Context, in model.ItemFieldsEnrichment) error
	// ApplyAlbumFields writes an album's label on the album row and its year across
	// every member at once, both fill-when-empty and lock-respecting, and records the
	// album fields marker. The year fill is vetoed unless the album has no year and
	// every member is present, year-less, and unlocked, since it moves the album
	// identity key.
	ApplyAlbumFields(ctx context.Context, in model.AlbumFieldsEnrichment) error

	ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error
	ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error
	// ApplyReleaseGroupAuxArt fills a release group's empty auxiliary art roles
	// (fill-when-empty, lock-respecting per role) and records the backfill marker
	// whether or not anything was found, so a group no provider serves is not re-asked
	// every run.
	ApplyReleaseGroupAuxArt(ctx context.Context, in model.ReleaseGroupAuxArt) error
	// ApplyArtistArtBackfill fills an artist's empty art roles, front and auxiliary
	// (fill-when-empty, lock-respecting per role), and records the backfill marker
	// whether or not anything was found.
	ApplyArtistArtBackfill(ctx context.Context, in model.ArtistArtBackfill) error
	// ApplyAlbumArtBackfill is the album twin: it fills the album's own art rung,
	// fill-when-empty against what the art chain already resolves, and records the
	// marker whether or not anything was found. An album that has vanished since the
	// queue page takes neither the fill nor the marker.
	ApplyAlbumArtBackfill(ctx context.Context, in model.AlbumArtBackfill) error
	// ApplyAlbumReleaseMatch fills an album's release MBID when it has none and records
	// the marker either way (under the deciding tier's provider) so a no-match is not
	// re-searched every run. It writes no art: the album-art phase keys on the stored
	// identifiers instead.
	ApplyAlbumReleaseMatch(ctx context.Context, in model.AlbumReleaseMatch) error
	ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error
	// ApplyLyricsEnrichment attaches a track's resolved lyrics, only when it has none
	// (fill-when-empty), and records the per-recording enrichment marker.
	ApplyLyricsEnrichment(ctx context.Context, in model.LyricsEnrichment) error

	// ExpiredMissesExist reports whether any no-match marker predates cutoff, so a run
	// can skip the retry sweep entirely rather than have every phase re-query its base
	// table for a population that is usually empty.
	ExpiredMissesExist(ctx context.Context, cutoff int64) (bool, error)

	EnrichmentCacheGet(ctx context.Context, key string) ([]byte, bool, error)
	EnrichmentCachePut(ctx context.Context, key string, payload []byte) error
	EnrichmentCoverage(ctx context.Context) (model.EnrichmentCoverage, error)
}

// Config tunes the enrichment service: the MusicBrainz contact, the network policy,
// provider endpoints (overridable for tests), the optional AcoustID key, and toggles.
// The contact gates the MusicBrainz spine and the key-free built-ins, which are public
// services that require an identifying User-Agent; an injected provider brings its own
// and runs without one.
type Config struct {
	// Contact is the operator contact (email or URL) folded into the User-Agent, as
	// MusicBrainz requires. When empty (and UserAgent is empty) the identity phases and
	// the key-free built-ins do not run; a registered provider's own phases still do.
	Contact string
	// UserAgent overrides the full User-Agent string; when empty one is built from
	// the app name and Contact.
	UserAgent string
	// AcoustIDKey enables the AcoustID fingerprint fallback (requires fpcalc). Empty
	// disables it.
	AcoustIDKey string
	// FetchCoverArt enables Cover Art Archive lookups (default enabled when a contact
	// is set; the facade sets it explicitly). One switch gates both rungs, the
	// release-group cover and the per-release one. The per-release half reuses the
	// group's picture when the archive served that very release's, so a separate key
	// would buy little; add one if the halves ever need to part.
	FetchCoverArt bool
	// FetchLyrics enables the LRCLIB lyrics provider (default enabled when a contact
	// is set; the facade sets it explicitly). Lyrics are filled only for a track that
	// has none.
	FetchLyrics bool
	// FetchCommunityGenres enables the ListenBrainz community-genre provider (default
	// enabled when a contact is set; the facade sets it explicitly). MusicBrainz genres
	// always flow through the identity spine regardless of this toggle.
	FetchCommunityGenres bool
	// MatchReleases enables the album release match: resolving which release of a
	// group an album is, from a barcode, a catalog number, or the medium and country
	// it already carries. An identifier costs one search per qualifying album, which is
	// the trade for searching by the identifier instead of browsing the group. Falling
	// through to the medium/country tier costs a whole-group browse, but the projected
	// result is cached per group, so a group's second album is free.
	MatchReleases bool
	// RetryMissesAfter is how old a no-match marker has to be before the pass asks
	// about that target again. Zero never retries, which is what a caller building this
	// struct directly gets; the facade resolves the config key to 30 days by default,
	// the way it supplies FetchCoverArt. A matched marker is durable regardless: the
	// provider answered, and a provider registered later is reached with
	// RunOptions.ForcePhases.
	RetryMissesAfter time.Duration

	// Providers are injected candidate providers supplied by an embedder (Discogs,
	// Last.fm, Audnexus, ...). They take priority over the built-in field/genre/cover/
	// lyrics providers for a value conflict; the MusicBrainz identity spine still
	// resolves the anchoring MBID first regardless. The default CLI build injects none.
	Providers []Provider

	// Network policy applied to the shared netsafe client.
	BlockPrivateIPs bool
	Timeout         time.Duration
	// MinRequestInterval is the per-host spacing (MusicBrainz requires >= 1s). Zero
	// takes the 1s default; tests set a tiny value. The key-free built-ins (LRCLIB,
	// ListenBrainz) pace at this interval too when set, else a gentler default.
	MinRequestInterval time.Duration

	// Endpoint overrides. Empty fields default to the public services.
	MusicBrainzBaseURL  string
	CoverArtBaseURL     string
	AcoustIDBaseURL     string
	ListenBrainzBaseURL string
	LRCLibBaseURL       string
}

const (
	defaultUserAgentBase = "WaxBin/1.0 (+https://github.com/colespringer/waxbin)"
	defaultMBBaseURL     = "https://musicbrainz.org/ws/2"
	defaultCAABaseURL    = "https://coverartarchive.org"
	defaultAcoustBaseURL = "https://api.acoustid.org"
	defaultLBBaseURL     = "https://api.listenbrainz.org"
	defaultLRCLibBaseURL = "https://lrclib.net"
	defaultMBInterval    = time.Second // MusicBrainz: at most 1 request/second
	// defaultBuiltinInterval paces the key-free built-ins (LRCLIB, ListenBrainz) when
	// no explicit interval is configured. They publish rate limits and return 429/503
	// under load, so a gentle default keeps a large pass from being throttled.
	defaultBuiltinInterval = 500 * time.Millisecond
	// providerTimeout bounds one candidate-provider call so a slow optional provider
	// cannot stall the identity/genre loop; it never aborts the pass, only that lookup.
	providerTimeout      = 15 * time.Second
	maxEnrichGenres      = 6 // cap on non-MusicBrainz (injected/community) genres added to an item
	enrichBatch          = 100
	defaultEnrichTimeout = 30 * time.Second
	// acoustFingerprintMaxDur bounds how much audio fpcalc analyzes for an AcoustID
	// lookup. Zero (a time.Duration) fingerprints the whole file, which AcoustID
	// matches most accurately.
	acoustFingerprintMaxDur time.Duration = 0
)

// Service enriches catalog entities. It is safe for concurrent use, though the
// pass itself is single-goroutine (network-bound and rate-limited).
type Service struct {
	store Store
	cfg   Config
	log   *slog.Logger
	caps  caps.Caps

	// mb + aid are the identity spine: MusicBrainz resolves the anchoring MBID (and,
	// for a release group, its type and its own genres) and AcoustID is the internal
	// fingerprint fallback that feeds MBIDs back to MusicBrainz. Neither is a port
	// Provider; they always run first.
	mb  *musicBrainz
	aid *acoustID

	// providers are the layerable candidate providers (genres, cover, lyrics, book
	// meta), in priority order: the injected providers first (indices [0:numInjected]),
	// then the key-free built-ins. First non-nil wins for a single-value candidate
	// (cover, lyrics); genres merge as a union with the MusicBrainz baseline spliced
	// between the injected and built-in groups.
	providers   []Provider
	numInjected int
}

// New builds an enrichment service from cfg, constructing the shared netsafe client
// with the contact User-Agent and MusicBrainz pacing, then registering the injected
// providers ahead of the key-free built-ins (Cover Art Archive cover, ListenBrainz
// genres, LRCLIB lyrics). Each rate-limited built-in gets its own paced client.
func New(store Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgentBase
		if cfg.Contact != "" {
			ua = "WaxBin/1.0 (" + cfg.Contact + ")"
		}
	}
	interval := cfg.MinRequestInterval
	if interval == 0 {
		interval = defaultMBInterval
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultEnrichTimeout
	}
	client := netsafe.New(netsafe.Policy{
		UserAgent:       ua,
		Timeout:         timeout,
		BlockPrivateIPs: cfg.BlockPrivateIPs,
		MinHostInterval: interval,
	})
	c := cache{store: store}
	s := &Service{
		store: store,
		cfg:   cfg,
		log:   log,
		caps:  caps.Detect(),
		mb:    &musicBrainz{client: client, baseURL: baseOr(cfg.MusicBrainzBaseURL, defaultMBBaseURL), cache: c},
		aid:   &acoustID{client: client, baseURL: baseOr(cfg.AcoustIDBaseURL, defaultAcoustBaseURL), key: cfg.AcoustIDKey},
	}

	// Injected providers rank first; record the boundary so the genre merge can splice
	// the MusicBrainz baseline in after them but before the built-ins.
	//
	// A provider's name is the provenance mark stamped on everything it supplies, and the
	// store refuses an enrichment value that names no provider. Dropping a nameless one
	// here keeps that refusal from aborting the whole pass for every item it answers,
	// which would leave the catalog retrying forever with nothing to show for it.
	for _, p := range cfg.Providers {
		if p.Name() == "" {
			log.Warn("enrichment: dropping an injected provider with no name; its values could carry no provenance")
			continue
		}
		s.providers = append(s.providers, p)
	}
	s.numInjected = len(s.providers)

	// The built-ins are public services that demand an identifying User-Agent, and the
	// contact is what supplies it, so they are registered only when one is configured.
	// The guard is load-bearing rather than tidiness: enrichConfig defaults cover art,
	// lyrics, and community genres on regardless of the contact, so without it a
	// contact-less run would walk every track against LRCLIB under the default
	// User-Agent, which is exactly what those services ask callers not to do.
	if !s.spineEnabled() {
		return s
	}

	// The key-free built-ins. The Cover Art Archive shares the MusicBrainz client (a
	// different host, so its pacing is independent anyway); the rate-limited lyrics/
	// genre built-ins each get their own paced client.
	builtinInterval := cfg.MinRequestInterval
	if builtinInterval == 0 {
		builtinInterval = defaultBuiltinInterval
	}
	builtinPolicy := netsafe.Policy{UserAgent: ua, Timeout: timeout, BlockPrivateIPs: cfg.BlockPrivateIPs, MinHostInterval: builtinInterval}
	if cfg.FetchCoverArt {
		s.providers = append(s.providers, &caaProvider{
			caa: &coverArt{client: client, baseURL: baseOr(cfg.CoverArtBaseURL, defaultCAABaseURL), cache: c},
			log: log,
		})
	}
	if cfg.FetchCommunityGenres {
		s.providers = append(s.providers, &listenBrainz{
			client: netsafe.New(builtinPolicy), baseURL: baseOr(cfg.ListenBrainzBaseURL, defaultLBBaseURL),
		})
	}
	if cfg.FetchLyrics {
		s.providers = append(s.providers, &lrclib{
			client: netsafe.New(builtinPolicy), baseURL: baseOr(cfg.LRCLibBaseURL, defaultLRCLibBaseURL),
		})
	}
	return s
}

// baseOr returns v (its trailing slashes trimmed so a configured base URL with a
// trailing "/" does not produce a double slash when a path is appended) or def when
// v is empty.
func baseOr(v, def string) string {
	if v == "" {
		return def
	}
	return strings.TrimRight(v, "/")
}

// spineEnabled reports whether the MusicBrainz identity spine and the key-free
// built-ins may run. Those are public services that require an identifying contact in
// the User-Agent, so the contact is the operator's consent to talk to them at all.
func (s *Service) spineEnabled() bool {
	return s.cfg.Contact != "" || s.cfg.UserAgent != ""
}

// enrichDisabledMessage names both routes to a runnable pass. A contact enables the
// MusicBrainz identity phases and the key-free built-ins; an injected provider enables
// the phase its capability gates, without one.
const enrichDisabledMessage = "enrichment needs a MusicBrainz contact " +
	"(set enrichment.contact) or an injected provider"

// Enabled reports whether any phase can run: the spine is configured, or a registered
// provider advertises a capability that gates a phase of its own. An injected provider
// brings its own credentials and its own service agreement, so a catalog with one and no
// contact still has work to do.
func (s *Service) Enabled() bool {
	if s.spineEnabled() {
		return true
	}
	// CapCover gates a phase of its own now (the album-art backfill), so an injected
	// cover-only provider with no contact configured is a runnable install.
	for _, c := range []Capability{CapCover, CapAuxArt, CapArtistArt, CapLyrics, CapFields, CapBookMeta} {
		if s.hasCapability(c) {
			return true
		}
	}
	return false
}

// acoustEnabled reports whether the AcoustID fingerprint fallback is usable: a key
// is set and fpcalc is present to produce a Chromaprint fingerprint.
func (s *Service) acoustEnabled() bool { return s.cfg.AcoustIDKey != "" && s.caps.Fpcalc }

// RunOptions controls one enrichment pass.
type RunOptions struct {
	Force bool // re-enrich already-enriched entities
	Limit int  // cap on entities processed (0 = all needing enrichment)
	// ForcePhases forces the named phases alone: each walks every target it could serve,
	// marker or none, asked the way a forced run asks (cache bypassed, Request.Force set),
	// while every other phase walks its ordinary sweeps. It is how a provider registered
	// after the markers settled is asked about them without re-resolving the whole
	// catalog against MusicBrainz. Exclusive with Force and with Scope, which already
	// force every phase they walk; a phase this install does not run is refused.
	ForcePhases []model.EnrichPhase
	// Scope narrows the pass to explicit targets (nil = the full catalog walk).
	// A scoped run implies Force: pointing at a target is an explicit gesture, so
	// a previously-missed lookup is retried (markers and cached responses are
	// bypassed) rather than skipped; the MusicBrainz pacing bounds the cost. A
	// phase whose scope list is empty is skipped entirely. The fill-when-empty
	// invariants are unchanged: a scoped lyrics or identifier fill still applies
	// only where the field is empty and unlocked.
	Scope *model.EnrichScope
}

// Result tallies an enrichment run.
type Result struct {
	ArtistsEnriched       int
	ArtistsMatched        int
	ReleaseGroupsEnriched int
	ReleaseGroupsMatched  int
	AlbumsSearched        int
	AlbumsMatched         int
	BooksEnriched         int
	BooksMatched          int
	LyricsEnriched        int
	LyricsMatched         int
	// AuxArtEnriched and AuxArtMatched count release groups the auxiliary-art backfill
	// phase walked, and the ones some provider answered for: entities, like every other
	// Enriched/Matched pair here. They are not a finer reading of AuxArtFetched below,
	// which counts images across every pass that gathers them, this one included. A run
	// can report AuxArtEnriched=40 with AuxArtFetched=6.
	AuxArtEnriched int
	AuxArtMatched  int
	// ArtistArtEnriched and ArtistArtMatched are the same pair for the artist-art
	// backfill, counting artists walked and artists some provider answered for. The
	// images themselves land in ArtFetched and AuxArtFetched with every other pass's.
	ArtistArtEnriched int
	ArtistArtMatched  int
	// AlbumArtEnriched and AlbumArtMatched are the same pair for the album-art backfill,
	// counting albums walked and albums some provider answered for.
	AlbumArtEnriched int
	AlbumArtMatched  int
	// The three fields walks, each counting the targets it walked and the ones some
	// provider answered for. TrackFields and BookFields are the item rung, one per kind
	// because each is gated by a different capability; AlbumFields is the entity rung.
	TrackFieldsEnriched int
	TrackFieldsMatched  int
	BookFieldsEnriched  int
	BookFieldsMatched   int
	AlbumFieldsEnriched int
	AlbumFieldsMatched  int
	// Retried counts the targets the retry sweep walked: earlier misses whose marker
	// had expired against the configured window. They are counted in the phase totals
	// beside it too, since a re-ask spends the same budget as a first ask; this says how
	// much of the run was re-asking. A forced run walks one sweep and leaves it zero.
	Retried int

	// ArtFetched counts front covers gathered and handed to the store, not covers
	// actually applied (the store's fill-when-empty and lock guards decide that);
	// AuxArtFetched counts the non-front role images the same way, in whichever pass
	// gathered them. Counting true applications would need the Store apply methods to
	// return reports, interface churn the tally is not worth.
	ArtFetched    int
	AuxArtFetched int
	// ArtReused counts album fronts the pass handed to the store as the group's picture
	// rather than as bytes, on the provider's word that those bytes are that pressing's
	// own. It counts what was handed over, not what the store attached, the rule
	// ArtFetched states above and for the same reason. Kept apart from ArtFetched so that
	// figure stays a download count: a reuse spends no request at all.
	ArtReused int

	// On-disk write-back tallies, all zero unless the run wrote tags. They live here
	// rather than on the facade's wrapper because a background job serializes this
	// struct alone, and a run where every write failed must not read the same as one
	// with nothing to write. Unrepresented counts files whose value cannot land and is
	// not retried: a lossy write (a key the format could not store, or content the
	// rewrite dropped), a file the tag library refuses to write, or a shared file
	// refused for an album label. Failed counts files the next pass that writes tags
	// retries. Skipped counts parts left unwritten because their book's primary part
	// could not be written.
	TagsWritten       int
	TagsFailed        int
	TagsUnrepresented int
	TagsSkipped       int

	// Reach is every target the run looked up, by phase, in the scope shape. A
	// limited run's tag write-back bounds itself to it, so --limit caps that work
	// too. It is this run's bookkeeping, not part of the serialized result.
	Reach *model.EnrichScope `json:"-"`
}

// total counts the entities a run has processed, one term per phase. Every phase that
// can run must appear, since this is both the --limit tally and the heartbeat's
// numerator against CountEntitiesNeedingEnrichment.
func (r *Result) total() int {
	return r.ArtistsEnriched + r.ReleaseGroupsEnriched + r.AlbumsSearched +
		r.AuxArtEnriched + r.ArtistArtEnriched + r.AlbumArtEnriched + r.BooksEnriched +
		r.LyricsEnriched + r.TrackFieldsEnriched + r.BookFieldsEnriched + r.AlbumFieldsEnriched
}

// Heartbeat reports progress; it may be nil.
type Heartbeat func(progress float64, msg string) error

// Run enriches artists, then release groups, then books, until each set is
// exhausted or the limit is reached. It is resumable: each entity is committed
// independently and marked, so an interrupted run resumes where it left off. A
// per-entity miss marks the entity looked-up-with-no-match and continues; a network
// failure (offline, cancellation) aborts with the underlying error rather than
// hammering an unreachable service. A scoped run (RunOptions.Scope) walks only the
// scoped targets through the same pipeline, provenance and markers included, and
// implies force.
func (s *Service) Run(ctx context.Context, opts RunOptions, hb Heartbeat) (*Result, error) {
	const op = "enrich.Run"
	res := &Result{}
	if !s.Enabled() {
		return res, waxerr.New(waxerr.CodeUnsupported, op, enrichDisabledMessage)
	}
	// A scoped run implies force: the caller pointed at these targets, so markers
	// and cached provider responses are bypassed and the lookup actually re-runs.
	scope := opts.Scope
	if len(opts.ForcePhases) > 0 {
		if opts.Force || scope != nil {
			return res, waxerr.New(waxerr.CodeInvalid, op, "a phase-scoped force cannot combine with --force or a scope, which already force every phase they walk")
		}
		for _, p := range opts.ForcePhases {
			if !p.Valid() {
				return res, waxerr.New(waxerr.CodeInvalid, op, "unknown enrichment phase "+strconv.Quote(string(p)))
			}
		}
	}
	st := &runState{
		force:           opts.Force || scope != nil,
		forcedPhases:    map[model.EnrichPhase]bool{},
		browsedGroups:   map[string]bool{},
		refreshedGroups: map[string]bool{},
	}
	for _, p := range opts.ForcePhases {
		st.forcedPhases[p] = true
	}
	// The retry window, resolved once so every phase measures against the same instant.
	// A forced run has one sweep that takes everything, so no cutoff applies; otherwise
	// the fresh targets are walked first and the expired misses after them.
	countSweep := model.SweepAll
	st.sweeps = []model.EnrichSweep{model.SweepAll}
	if !st.force {
		st.sweeps = []model.EnrichSweep{model.SweepFresh}
		countSweep = model.SweepFresh
		if s.cfg.RetryMissesAfter > 0 {
			st.missCutoff = time.Now().Add(-s.cfg.RetryMissesAfter).UnixNano()
			// One scan of the marker table decides whether the second sweep is worth
			// walking. Most nights nothing is due, and adding the sweep regardless makes
			// every phase re-query its own base table for an empty answer.
			due, err := s.store.ExpiredMissesExist(ctx, st.missCutoff)
			if err != nil {
				return res, err
			}
			if due {
				st.sweeps = append(st.sweeps, model.SweepRetry)
				countSweep = model.SweepDue
			}
		}
	}
	countQuery := model.EnrichQueueOptions{Sweep: countSweep, MissCutoff: st.missCutoff}
	res.Reach = &model.EnrichScope{}
	phases := s.phases(st, res, scope)
	if err := checkPhasesBuilt(phases, opts.ForcePhases, op); err != nil {
		return res, err
	}

	// The total is only needed to report a heartbeat ratio, so skip the three
	// counting queries entirely when there is no heartbeat.
	var total int
	if hb != nil {
		// The flags and the scope must match the phase list above, which adds the
		// identity phases only with a MusicBrainz contact, the album phase only when the
		// toggle is on too, the art-backfill and lyrics phases only when a capable
		// provider is registered, and skips a phase the scope leaves empty, so the
		// denominator counts exactly the work that will run.
		n, err := s.store.CountEntitiesNeedingEnrichment(ctx, countQuery, model.EnrichCountOptions{
			Identity:    s.spineEnabled(),
			Albums:      s.cfg.MatchReleases && s.spineEnabled(),
			AuxArt:      s.hasCapability(CapAuxArt),
			ArtistArt:   s.hasCapability(CapArtistArt),
			AlbumArt:    s.albumArtSlots(),
			Lyrics:      s.hasCapability(CapLyrics),
			TrackFields: s.hasCapability(CapFields),
			BookFields:  s.hasCapability(CapBookMeta),
			AlbumFields: s.hasCapability(CapFields),
			Forced:      opts.ForcePhases,
		}, scope)
		if err != nil {
			return res, err
		}
		total = n
	}
	progress := func() float64 {
		if total <= 0 || res.total() >= total {
			return 1
		}
		return float64(res.total()) / float64(total)
	}
	beat := func(msg string) error {
		if hb == nil {
			return nil
		}
		return hb(progress(), msg)
	}
	remaining := func() int {
		if opts.Limit <= 0 {
			return enrichBatch
		}
		if r := opts.Limit - res.total(); r < enrichBatch {
			return r
		}
		return enrichBatch
	}
	limitReached := func() bool { return opts.Limit > 0 && res.total() >= opts.Limit }

	// Sweeps outside phases, not inside: --limit is one budget for the run, and a capped
	// nightly pass has to reach the files nothing has looked at yet in EVERY phase
	// before it spends anything on re-asking. Nesting the sweeps inside a phase would
	// let one phase's expired population starve every phase after it, night after night.
	//
	// The price is one night of convergence in a narrow case. The phase order is
	// load-bearing (an album needs its group's mbid, and the art rungs read the ids the
	// phase above just landed), and that still holds within each sweep; what no longer
	// holds is across them, so an entity whose identity lands on a retry leaves the next
	// phase's FRESH queue unserved until the following run.
	for _, sweep := range st.sweeps {
		st.retrying = sweep == model.SweepRetry
		for i := range phases {
			q := model.EnrichQueueOptions{Sweep: sweep, MissCutoff: st.missCutoff}
			st.forcing = st.forcedPhases[phases[i].key]
			if st.forcing {
				// The first sweep took everything, so the retry sweep has nothing left.
				if st.retrying {
					continue
				}
				q.Sweep = model.SweepAll
			}
			if err := s.runSweep(ctx, st, phases[i], res, q, beat, remaining, limitReached); err != nil {
				return res, err
			}
		}
		st.retrying = false
	}
	st.forcing = false
	_ = beat("enriched " + strconv.Itoa(res.total()) + " entities")
	return res, nil
}

// phases builds the run's phase list, in run order, from what this install can do and
// what the scope names. It is separate from Run so a test can pin the keys against
// model.EnrichPhases(); the phases take the addresses of res.Reach's lists, so Reach
// must already be attached.
func (s *Service) phases(st *runState, res *Result, scope *model.EnrichScope) []phase {
	var artistIDs, rgIDs, albumIDs, bookIDs, lyricsIDs, fieldsIDs []int64
	if scope != nil {
		artistIDs, rgIDs, albumIDs = scope.ArtistIDs, scope.ReleaseGroupIDs, scope.AlbumIDs
		bookIDs, lyricsIDs, fieldsIDs = scope.BookItemIDs, scope.LyricsItemIDs, scope.FieldsItemIDs
	}

	// A phase runs when the pass is unscoped or the scope names targets for it; a
	// scoped phase with nothing to do is skipped outright (no fetch, no count).
	phaseRuns := func(ids []int64) bool { return scope == nil || len(ids) > 0 }

	// The identity phases talk to MusicBrainz, so they run only with a contact. The
	// port phases below answer to their providers instead and run either way, which is
	// what lets an install with an injected provider and no contact enrich anything at
	// all.
	spine := s.spineEnabled()
	identityRuns := func(ids []int64) bool { return spine && phaseRuns(ids) }

	// Artists first: a release group's artist credit is more useful once its primary
	// artist carries an MBID.
	var phases []phase
	if identityRuns(artistIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseArtist, enriched: &res.ArtistsEnriched, matched: &res.ArtistsMatched, reach: &res.Reach.ArtistIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ArtistsNeedingEnrichment(ctx, q, after, lim, artistIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichArtist(ctx, st, res, t)
			},
		})
	}
	if identityRuns(rgIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseReleaseGroup, enriched: &res.ReleaseGroupsEnriched, matched: &res.ReleaseGroupsMatched, reach: &res.Reach.ReleaseGroupIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingEnrichment(ctx, q, after, lim, s.acoustEnabled(), rgIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichReleaseGroup(ctx, st, res, t)
			},
		})
	}
	// Albums come after release groups, and the ordering is load-bearing: the album
	// query requires a non-empty release_group.mbid, and the phase above is what fills
	// it. Running them the other way round would leave a freshly-enriched group's
	// albums unqueued until the next pass.
	if s.cfg.MatchReleases && identityRuns(albumIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAlbumRelease, enriched: &res.AlbumsSearched, matched: &res.AlbumsMatched, reach: &res.Reach.AlbumIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.AlbumsNeedingReleaseMatch(ctx, q, after, lim, albumIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichAlbumRelease(ctx, st, t)
			},
		})
	}
	// The auxiliary-art backfill: release groups whose front is settled but whose back,
	// disc, booklet, or background slots are empty. The release-group pass above never
	// re-asks about those, because it pre-guards on the front, and the album-art phase
	// below fills only the album's own rung. It runs only when some provider advertises
	// CapAuxArt, which the built-in Cover Art Archive does not, so a stock install walks
	// nothing and writes no markers.
	//
	// Coming after the release-group phase means an id that phase just filled rides
	// along with the request, since the queue reads release_group.mbid live. Nothing
	// gates on it any more, so this is an ordering preference rather than a requirement:
	// a group with no id is asked about by title and primary-artist name.
	//
	// Sitting after "album release" and therefore before the book and lyrics phases
	// keeps the art-fetching phases together and nothing else depends on it.
	if s.hasCapability(CapAuxArt) && phaseRuns(rgIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAuxArt, enriched: &res.AuxArtEnriched, matched: &res.AuxArtMatched, reach: &res.Reach.ReleaseGroupIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingAuxArt(ctx, q, after, lim, rgIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichAuxArt(ctx, st, res, t)
			},
		})
	}
	// The artist-art backfill: artists whose front or auxiliary slots are empty. The
	// identity phase above fetches artist art on the way past, so an artist it has
	// already marked never gets asked again, which is the gap this closes without a
	// --force run that re-searches MusicBrainz for every artist. It runs only when some
	// provider advertises CapArtistArt, which neither built-in does, so a stock install
	// walks nothing and writes no markers.
	//
	// After the identity phase for the reason the aux backfill is after the release-group
	// one: the queue reads artist.mbid live, so an id that phase just filled rides along
	// with the request. The walk is keyed on the name, so an unmatched artist is reached
	// either way. Its place among the art phases is otherwise free.
	if s.hasCapability(CapArtistArt) && phaseRuns(artistIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseArtistArt, enriched: &res.ArtistArtEnriched, matched: &res.ArtistArtMatched, reach: &res.Reach.ArtistIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ArtistsNeedingArtBackfill(ctx, q, after, lim, artistIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichArtistArt(ctx, st, res, t)
			},
		})
	}
	// The album-art backfill: albums with an empty front or auxiliary slot that carry an
	// identifier a provider can key on. It sits right after the release match so an id
	// that phase just landed rides along with the request, since the queue reads
	// album.mbid live, the same ordering argument the aux backfill uses.
	//
	// Its front half is the one art phase a stock install runs, because the Cover Art
	// Archive serves the release rung. An album whose members carry no embedded cover
	// otherwise shows the release group's picture, one edition standing in for all of
	// them, which is the failure a per-release ask exists to avoid. The aux half needs
	// an injected CapAuxArt provider, as at the other rungs.
	if slots := s.albumArtSlots(); slots.Any() && phaseRuns(albumIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAlbumArt, enriched: &res.AlbumArtEnriched, matched: &res.AlbumArtMatched, reach: &res.Reach.AlbumIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.AlbumsNeedingArt(ctx, q, after, lim, slots, albumIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichAlbumArt(ctx, st, res, t)
			},
		})
	}
	if identityRuns(bookIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseBook, enriched: &res.BooksEnriched, matched: &res.BooksMatched, reach: &res.Reach.BookItemIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.BooksNeedingEnrichment(ctx, q, after, lim, bookIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichBook(ctx, st, t) },
		})
	}
	// Lyrics are a per-recording phase, run only when a lyrics-capable provider is
	// registered so no marker is written for tracks nothing could ever fill. It walks
	// tracks that carry no lyrics yet, filling from LRCLIB (or an injected provider).
	if s.hasCapability(CapLyrics) && phaseRuns(lyricsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseLyrics, enriched: &res.LyricsEnriched, matched: &res.LyricsMatched, reach: &res.Reach.LyricsItemIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingLyrics(ctx, q, after, lim, lyricsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichLyrics(ctx, st, t) },
		})
	}
	// The item-rung fields walks. Each is gated by the capability that owns its rung,
	// so a stock install (which registers no fields provider) walks neither and writes
	// no markers. They come after lyrics for the reason the art backfills sit where they
	// do: nothing downstream depends on the order, and keeping the per-item phases
	// together is the readable arrangement.
	if s.hasCapability(CapFields) && phaseRuns(fieldsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseTrackFields, enriched: &res.TrackFieldsEnriched, matched: &res.TrackFieldsMatched, reach: &res.Reach.FieldsItemIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingFields(ctx, q, after, lim, model.KindTrack, fieldsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichTrackFields(ctx, st, t)
			},
		})
	}
	if s.hasCapability(CapBookMeta) && phaseRuns(fieldsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseBookFields, enriched: &res.BookFieldsEnriched, matched: &res.BookFieldsMatched, reach: &res.Reach.FieldsItemIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingFields(ctx, q, after, lim, model.KindBook, fieldsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichBookFields(ctx, st, t)
			},
		})
	}
	// The album rung of the same walk. It shares the album scope list with the release
	// match, and comes after the track walk so the two fields phases read together.
	if s.hasCapability(CapFields) && phaseRuns(albumIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAlbumFields, enriched: &res.AlbumFieldsEnriched, matched: &res.AlbumFieldsMatched, reach: &res.Reach.AlbumIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.AlbumsNeedingFields(ctx, q, after, lim, albumIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichAlbumFields(ctx, st, t)
			},
		})
	}
	return phases
}

// CheckPhases reports whether this install builds every named phase. A caller that
// submits enrichment as a job asks first, so a force naming a phase the providers gate
// off is refused rather than starting a run that walks nothing and reports itself
// complete. Run makes the same check.
func (s *Service) CheckPhases(phases []model.EnrichPhase) error {
	if len(phases) == 0 {
		return nil
	}
	built := s.phases(&runState{}, &Result{Reach: &model.EnrichScope{}}, nil)
	return checkPhasesBuilt(built, phases, "enrich.CheckPhases")
}

// checkPhasesBuilt refuses a forced key no built phase carries, naming the gate.
func checkPhasesBuilt(built []phase, want []model.EnrichPhase, op string) error {
	for _, w := range want {
		found := false
		for i := range built {
			if built[i].key == w {
				found = true
				break
			}
		}
		if !found {
			return waxerr.New(waxerr.CodeUnsupported, op,
				"phase "+string(w)+" does not run on this install: "+phaseRequirement(w))
		}
	}
	return nil
}

// phaseRequirement names, in words, what an install needs before a phase runs, for the
// refusal a phase-scoped force gives when it names one this install skips.
func phaseRequirement(p model.EnrichPhase) string {
	switch p {
	case model.EnrichPhaseArtist, model.EnrichPhaseReleaseGroup, model.EnrichPhaseBook:
		return "it needs a MusicBrainz contact"
	case model.EnrichPhaseAlbumRelease:
		return "it needs a MusicBrainz contact and enrichment.match_releases"
	case model.EnrichPhaseAuxArt:
		return "it needs a provider advertising auxiliary art"
	case model.EnrichPhaseArtistArt:
		return "it needs a provider advertising artist art"
	case model.EnrichPhaseAlbumArt:
		return "it needs a cover or auxiliary-art provider"
	case model.EnrichPhaseLyrics:
		return "it needs a lyrics provider"
	case model.EnrichPhaseTrackFields, model.EnrichPhaseAlbumFields:
		return "it needs a fields provider"
	case model.EnrichPhaseBookFields:
		return "it needs a book metadata provider"
	}
	return "it is not a phase this install builds"
}

// runState is per-run mutable state, allocated fresh each Run so the Service stays
// safe for concurrent callers (no shared field is mutated). force bypasses cached
// provider reads; acoustOff is set when the AcoustID fallback hits a (usually
// permanent) error, disabling it for the rest of the run.
type runState struct {
	force     bool
	acoustOff bool
	// forcedPhases are the phases a phase-scoped force names, and forcing is set while
	// one of them is being walked, so forced() folds it in the way it folds retrying.
	forcedPhases map[model.EnrichPhase]bool
	forcing      bool
	// sweeps are the queue walks the run makes over its whole phase list, in order, and
	// missCutoff is the instant a no-match marker has to predate to be re-asked. An
	// ordinary run walks every phase fresh, then every phase again for the expired
	// misses; a forced or scoped run walks SweepAll alone.
	sweeps     []model.EnrichSweep
	missCutoff int64
	// retrying is set while a phase walks its retry sweep, and forced() folds it into
	// force: a target whose marker already says nothing answered has to be asked the way
	// a forced run asks, or the MusicBrainz cache and a provider that caches its own
	// misses would just serve the answer that earned the marker.
	retrying bool
	// browsedGroups records, per release group this run attempted, whether the browse
	// produced a usable edition set. It does two jobs: a forced or scoped run (both bypass
	// the per-group cache read) refreshes a group once rather than once per album under
	// it, and a group whose browse never reconciles costs one attempt per run instead of
	// one per album. The pass is single-goroutine, so no lock is needed.
	browsedGroups map[string]bool
	// refreshedGroups records which groups this run has actually re-browsed. It is
	// separate from browsedGroups because a retried album can arrive under a group a
	// fresh sibling browsed earlier in the same run: that group is "attempted", so
	// without this it would read the stale edition set the fresh pass cached.
	refreshedGroups map[string]bool
}

// forced reports whether this target should be asked the way a forced run asks:
// bypassing the MusicBrainz cache and telling a caching provider to do the same.
func (st *runState) forced() bool { return st.force || st.retrying || st.forcing }

// phase describes one entity type's enrichment for the shared keyset runner: how to
// fetch a page, how to enrich one target (returning whether a provider matched), the
// counters to bump, and the Result.Reach list its targets are recorded in.
type phase struct {
	key      model.EnrichPhase
	enriched *int
	matched  *int
	reach    *[]int64
	fetch    func(ctx context.Context, q model.EnrichQueueOptions, afterID int64, limit int) ([]model.EnrichTarget, error)
	enrich   func(ctx context.Context, t model.EnrichTarget) (matched bool, err error)
}

// runSweep walks one sweep of one phase in keyset pages, enriching each target. It is
// the one loop behind artists, release groups, and books. A MusicBrainz or cancellation
// error aborts; a per-entity miss is marked by the enrich callback and the walk
// continues. The phase counters are pointers into the Result, and the retry tally is the
// Result's own, since it spans every phase.
func (s *Service) runSweep(ctx context.Context, st *runState, p phase, res *Result, q model.EnrichQueueOptions,
	beat func(string) error, remaining func() int, limitReached func() bool) error {
	var afterID int64
	for {
		if limitReached() {
			return nil
		}
		batch, err := p.fetch(ctx, q, afterID, remaining())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, t := range batch {
			if err := ctx.Err(); err != nil {
				return waxerr.FromContext("enrich.Run", err, waxerr.CodeCanceled)
			}
			matched, err := p.enrich(ctx, t)
			if err != nil {
				return err // MusicBrainz/cancel: abort rather than mark or hammer
			}
			(*p.enriched)++
			if matched {
				(*p.matched)++
			}
			verb := "enriched "
			if st.retrying {
				res.Retried++
				verb = "retried "
			}
			*p.reach = append(*p.reach, t.ID)
			if err := beat(verb + p.key.Label() + " " + t.Name); err != nil {
				return err
			}
			if limitReached() {
				return nil
			}
		}
		afterID = batch[len(batch)-1].ID
	}
}

// enrichArtist resolves one artist against MusicBrainz and applies the result. A
// miss (no MBID and no confident search hit) is still applied as a no-match marker
// so the artist is not retried every run. Returns whether a provider matched.
//
// It also asks the art providers for the artist's own imagery, mirroring the
// release-group rung: a front when the artist has none, and the auxiliary roles either
// way, since a settled front says nothing about the background where artist imagery
// actually lands. A locked artist art cancels both, because the store would refuse the
// write and a forced run would otherwise re-download one picture per locked artist.
//
// No built-in provider answers at this rung. The Cover Art Archive is release-group
// keyed, and ListenBrainz and LRCLIB answer neither, so a stock install spends no
// requests here; this exists for an injected provider advertising CapCover or CapAuxArt.
//
// The art rides the identity pass, so it only reaches artists that pass is still queued
// for, and only artists MusicBrainz matched at all. An artist already carrying an
// enrichment marker, or one no search ever resolved, is out of this queue; the
// artist-art backfill phase walks by name and reaches both. See enrichArtistArt.
func (s *Service) enrichArtist(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (bool, error) {
	enr := model.ArtistEnrichment{ArtistID: t.ID, PID: t.PID}
	a, err := s.resolveArtist(ctx, st, t)
	if err != nil {
		return false, err
	}
	if a != nil {
		enr.Matched = true
		enr.MBID = a.ID
		// Store the sort-name as an alias only when it differs from the display name
		// (e.g. "Beatles, The" for "The Beatles"); an identical sort-name adds nothing.
		if identity.MatchKey(a.SortName) != identity.MatchKey(a.Name) {
			enr.SortName = a.SortName
		}
		enr.Aliases = artistAliasNames(a)
		enr.Relations = artistRelations(a)
		if !t.ArtLocked {
			req := Request{Type: TargetArtist, Force: st.forced(), Artist: t.Name, MBID: a.ID}
			if !t.HasArt {
				art, _, _ := s.gatherArt(ctx, st, req, false)
				enr.Art = art[model.ArtRoleFront]
				enr.AuxArt = auxArtRoles(art)
			} else {
				enr.AuxArt, _ = s.gatherAuxArt(ctx, st, req)
			}
		}
	}
	if err := s.store.ApplyArtistEnrichment(ctx, enr); err != nil {
		return false, err
	}
	if enr.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(enr.AuxArt)
	return enr.Matched, nil
}

// resolveArtist looks up an artist by MBID, or searches by name when it has none.
// A CodeNotFound on an MBID lookup (a stale/wrong id) degrades to a name search
// (searchArtist itself returns no match for an empty/symbol-only name).
func (s *Service) resolveArtist(ctx context.Context, st *runState, t model.EnrichTarget) (*mbArtist, error) {
	if t.MBID != "" {
		a, err := s.mb.lookupArtist(ctx, st.forced(), t.MBID)
		if err == nil {
			return a, nil
		}
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, err
		}
	}
	return s.mb.searchArtist(ctx, st.forced(), t.Name)
}

// enrichReleaseGroup resolves one release group (MBID lookup, else text search, else
// the optional AcoustID fingerprint fallback) and applies the result, filling the
// type, genres, and (when enabled) the Cover Art Archive front cover. Returns whether
// a provider matched.
func (s *Service) enrichReleaseGroup(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (bool, error) {
	enr := model.ReleaseGroupEnrichment{ReleaseGroupID: t.ID, PID: t.PID}
	rg, err := s.resolveReleaseGroup(ctx, st, t)
	if err != nil {
		return false, err
	}
	if rg != nil {
		enr.Matched = true
		enr.MBID = rg.ID
		enr.Type = mapReleaseGroupType(rg.PrimaryType, rg.SecondaryTypes)
		// Genres: the MusicBrainz baseline merged with the genre providers (injected
		// first, then built-ins like ListenBrainz), deduped and capped. The winning
		// provider of the display-primary genre is recorded as field provenance.
		enr.Genres, enr.GenreProvider = s.gatherGenres(ctx, st, rg, genreNames(rg.Genres))
		// Art: the first cover provider to answer per role, injected first (an
		// embedder's fanart.tv beats the built-in Cover Art Archive). Best-effort:
		// never aborts. Skipped for a locked cover, which the store would refuse to
		// replace, so a forced re-run does not re-download one picture per locked group.
		if !t.ArtLocked {
			art, _, _ := s.gatherArt(ctx, st, Request{
				Type: TargetReleaseGroup, Force: st.forced(),
				Title: rg.Title, Artist: releaseGroupArtistName(rg), MBID: rg.ID,
				GroupFrontHash: t.GroupFrontHash,
			}, false)
			enr.Art = art[model.ArtRoleFront]
			enr.AuxArt = auxArtRoles(art)
		}
	}
	if err := s.store.ApplyReleaseGroupEnrichment(ctx, enr); err != nil {
		return false, err
	}
	if enr.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(enr.AuxArt)
	return enr.Matched, nil
}

// resolveReleaseGroup applies the resolution ladder: MBID lookup, text search, then
// AcoustID (when enabled and not disabled this run) via a representative file's
// fingerprint.
func (s *Service) resolveReleaseGroup(ctx context.Context, st *runState, t model.EnrichTarget) (*mbReleaseGroup, error) {
	if t.MBID != "" {
		rg, err := s.mb.lookupReleaseGroup(ctx, st.forced(), t.MBID)
		if err == nil {
			return rg, nil
		}
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, err
		}
	}
	if t.Name != "" {
		rg, err := s.mb.searchReleaseGroup(ctx, st.forced(), t.Name, t.ArtistName)
		if err != nil {
			return nil, err
		}
		if rg != nil {
			return rg, nil
		}
	}
	if s.acoustEnabled() && !st.acoustOff && t.FilePath != "" {
		if mbid := s.acoustResolveReleaseGroup(ctx, st, t); mbid != "" {
			rg, err := s.mb.lookupReleaseGroup(ctx, st.forced(), mbid)
			if err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
				return nil, err
			}
			return rg, nil
		}
	}
	return nil, nil
}

// acoustResolveReleaseGroup fingerprints a release group's representative file with
// fpcalc and asks AcoustID for a release-group MBID. It is best-effort. An fpcalc
// failure is skipped. An AcoustID error (a bad or expired key, a quota, or an endpoint
// problem usually recurs for every file) disables the fallback for the rest of the run
// instead of retrying. It never aborts the pass, since AcoustID is an optional resolver
// layered on top of MusicBrainz.
func (s *Service) acoustResolveReleaseGroup(ctx context.Context, st *runState, t model.EnrichTarget) string {
	fp, durSec, err := fingerprint.ChromaprintCompressed(ctx, s.caps.FpcalcPath, t.FilePath, acoustFingerprintMaxDur)
	if err != nil {
		s.log.Debug("acoustid fingerprint failed", "path", t.FilePath, "err", err)
		return ""
	}
	if t.DurationSec > 0 {
		durSec = t.DurationSec
	}
	m, err := s.aid.lookup(ctx, fp, durSec)
	if err != nil {
		s.log.Warn("acoustid lookup failed; disabling the fallback for this run", "err", err)
		st.acoustOff = true
		return ""
	}
	if m == nil {
		return ""
	}
	return m.ReleaseGroupMBID
}

// enrichAlbumRelease resolves which release of its group one album is, from a
// barcode, a catalog number, or (failing both) the medium and country it carries, and
// applies the result. A no-match still writes the marker so the album is not
// re-searched every run. Returns whether a release matched.
//
// The album's own title, year, and track count are never consulted, because the
// releases of one group share them. The evidence that does decide, and how the weak
// third tier differs from the two identifier tiers, is release.go's subject.
//
// It fetches no art. The album-art phase below walks by stored identifier, so it reaches
// the id this phase just landed in the same pass, and reaches the albums this phase
// never queues at all: a Picard-tagged album whose id came off the tags, and one whose
// id a user curated.
func (s *Service) enrichAlbumRelease(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	// A scan stores identifiers verbatim, so this column holds whatever a tag said, and
	// a malformed value would make a garbage query rather than a clean miss. No marker
	// either: "could not search" must stay re-queueable, and the recheck costs nothing
	// because it bails before the request.
	if !model.IsMBID(t.ReleaseGroupMBID) {
		s.log.Warn("enrichment: skipping release match, release-group mbid is not a UUID",
			"album", t.PID, "mbid", t.ReleaseGroupMBID)
		return false, nil
	}
	m, err := s.matchAlbumRelease(ctx, st, t)
	if err != nil {
		return false, err
	}
	// A transient failure is not a decision, so nothing is written. Marking here would
	// keep the album out of the queue on every later run, which is the opposite of what
	// leaving the group uncached was for.
	if m.Skip {
		s.log.Debug("enrichment: release match inconclusive this run, leaving the album queued",
			"album", t.PID, "group", t.ReleaseGroupMBID)
		return false, nil
	}
	in := model.AlbumReleaseMatch{AlbumID: t.ID, PID: t.PID, Provider: providerMusicBrainz}
	if m.MBID != "" {
		in.Matched, in.MBID, in.Reason = true, m.MBID, m.Reason
		if m.Edition {
			in.Provider = providerMBEdition
		}
		// Log which evidence decided it. With a weaker tier in play a human debugging a
		// match needs to see that, and the reason was previously computed and discarded.
		s.log.Info("enrichment: matched album release",
			"album", t.PID, "mbid", m.MBID, "by", m.Reason, "provider", in.Provider)
	}
	if err := s.store.ApplyAlbumReleaseMatch(ctx, in); err != nil {
		return false, err
	}
	return in.Matched, nil
}

// enrichAlbumArt fills one album's empty art roles from the providers that can serve the
// release rung, and marks it so the walk does not repeat. It is the album twin of
// enrichArtistArt: the queue hands it an album carrying a release mbid, a barcode or a
// catalog number, so this is a gather and an apply with no MusicBrainz round trip
// between them.
//
// The identifiers are what make the ask answerable. The releases of one group share a
// title, so a provider given only that can return some edition's picture and never this
// pressing's; a barcode or a catalog number names the pressing outright, and the Cover
// Art Archive keys on the release mbid.
//
// A run that gathered nothing still applies, because the marker is what stops the album
// being asked again next run. The store decides what actually lands: the queue's vacancy
// test is approximate, and a per-role lock is re-checked there.
//
// Most of these albums are the pressing their group's cover was taken from, so a
// provider that knows so answers the front with the group's picture rather than the same
// bytes over again, and the store copies the row it already holds.
func (s *Service) enrichAlbumArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (bool, error) {
	in := model.AlbumArtBackfill{AlbumID: t.ID, PID: t.PID}
	req := Request{
		Type: TargetRelease, Force: st.forced(),
		Title: t.Name, Artist: t.ArtistName, MBID: t.MBID,
		Barcode: t.Barcode, CatalogNumber: t.CatalogNumber,
		ReleaseGroupMBID: t.ReleaseGroupMBID, GroupFrontHash: t.GroupFrontHash,
	}
	if req.ReleaseGroupMBID != "" && !model.IsMBID(req.ReleaseGroupMBID) {
		req.ReleaseGroupMBID = ""
	}
	// A scan stores MUSICBRAINZ_ALBUMID lowercased but unvalidated, so the column can
	// hold something no provider will accept. Blanking it leaves the printed identifiers
	// to carry the ask rather than failing it outright; the edit path validates, so only
	// the scan path reaches this.
	if req.MBID != "" && !model.IsMBID(req.MBID) {
		s.log.Debug("enrichment: album art request dropping a non-UUID release mbid",
			"album", t.PID, "mbid", req.MBID)
		req.MBID = ""
	}
	var art map[model.ArtRole]*model.ArtImage
	var provider string
	if !t.HasArt {
		var fromGroup bool
		art, provider, fromGroup = s.gatherArt(ctx, st, req, true)
		in.Art = art[model.ArtRoleFront]
		in.AuxArt = auxArtRoles(art)
		in.FrontFromGroup, in.GroupFrontHash = fromGroup, t.GroupFrontHash
	} else {
		in.AuxArt, provider = s.gatherAuxArt(ctx, st, req)
	}
	if in.Art != nil || in.FrontFromGroup || len(in.AuxArt) > 0 {
		in.Matched, in.Provider = true, provider
	}
	if err := s.store.ApplyAlbumArtBackfill(ctx, in); err != nil {
		return false, err
	}
	if in.Art != nil {
		res.ArtFetched++
	}
	if in.FrontFromGroup {
		res.ArtReused++
	}
	res.AuxArtFetched += len(in.AuxArt)
	return in.Matched, nil
}

// enrichAuxArt backfills one release group's empty auxiliary art slots from the
// providers advertising CapAuxArt. It resolves no identity of its own: the queue hands
// it a group that already carries an MBID, which is what a provider keys on, so this is
// a gather and an apply with no MusicBrainz round trip between them.
//
// A run that gathered nothing still applies, because the marker is what stops the group
// being asked again next run. The store decides what actually lands: the queue's
// vacancy test is approximate, and a per-role lock is re-checked there.
func (s *Service) enrichAuxArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (bool, error) {
	in := model.ReleaseGroupAuxArt{ReleaseGroupID: t.ID, PID: t.PID}
	aux, provider := s.gatherAuxArt(ctx, st, Request{
		Type: TargetReleaseGroup, Force: st.forced(),
		Title: t.Name, Artist: t.ArtistName, MBID: t.MBID,
	})
	if len(aux) > 0 {
		in.Matched, in.AuxArt, in.Provider = true, aux, provider
	}
	if err := s.store.ApplyReleaseGroupAuxArt(ctx, in); err != nil {
		return false, err
	}
	res.AuxArtFetched += len(in.AuxArt)
	return in.Matched, nil
}

// enrichArtistArt gathers art for one artist whose identity is already settled, filling
// the front when it has none and the auxiliary roles either way, and marks it so the
// walk does not repeat. It asks only the providers advertising CapArtistArt, which is
// what keeps a stock install (whose Cover Art Archive answers nothing for an artist) from
// stamping a permanent no-match on every artist it holds.
func (s *Service) enrichArtistArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (bool, error) {
	in := model.ArtistArtBackfill{ArtistID: t.ID, PID: t.PID}
	art, provider := s.gatherArtistArt(ctx, st, Request{
		Type: TargetArtist, Force: st.forced(), Artist: t.Name, MBID: t.MBID,
	}, t.HasArt)
	if len(art) > 0 {
		in.Matched, in.Provider = true, provider
		in.Art = art[model.ArtRoleFront]
		in.AuxArt = auxArtRoles(art)
	}
	if err := s.store.ApplyArtistArtBackfill(ctx, in); err != nil {
		return false, err
	}
	if in.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(in.AuxArt)
	return in.Matched, nil
}

// gatherArtistArt returns the first offered image per role, plus the name of the first
// provider to contribute one. hasFront drops the front from what it keeps: unlike the
// release-group backfill this pass does ask about the front, but only for an artist that
// has none, so an artist queued for an auxiliary vacancy alone does not put a second
// writer on a slot that is already decided.
func (s *Service) gatherArtistArt(ctx context.Context, st *runState, req Request, hasFront bool) (map[model.ArtRole]*model.ArtImage, string) {
	req.Want = CapArtistArt
	need := len(model.AuxArtRoles())
	if !hasFront {
		need++
	}
	var out map[model.ArtRole]*model.ArtImage
	var provider string
	for _, p := range s.providers {
		if !p.Capabilities().Has(CapArtistArt) {
			continue
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil {
			continue
		}
		for role, img := range cand.Art {
			if !role.Valid() || img == nil || len(img.Data) == 0 {
				continue
			}
			if role == model.ArtRoleFront && hasFront {
				continue
			}
			if out[role] != nil {
				continue
			}
			img.Source, img.Provider = model.SourceEnrichment, p.Name()
			if out == nil {
				out = make(map[model.ArtRole]*model.ArtImage, len(cand.Art))
				provider = p.Name()
			}
			out[role] = img
		}
		// Every slot this pass can fill is held, so a further provider could only have its
		// images dropped by the guard above, after downloading them.
		if len(out) == need {
			break
		}
	}
	return out, provider
}

// gatherAuxArt returns the first offered image per auxiliary role, plus the name of the
// first provider to contribute one (the marker records that provider).
//
// It deliberately consults a different provider set from gatherArt, which is the whole
// reason CapAuxArt exists as its own bit: that pass asks every cover provider and stops
// at the front winner, so its auxiliary coverage is whatever the winner happened to
// carry, while this one asks only the providers that claim the non-front roles and
// keeps going, since there is no front to stop at. A provider serving auxiliary roles
// under CapCover alone therefore contributes to the first-pass gather and nothing here.
//
// The front role is dropped. The release-group pass owns that slot, and a group is
// queued here precisely because its front is settled, so offering one would put a
// second writer on a decided question. Every accepted image is stamped with the
// supplying provider, as in gatherArt. It is best-effort: a provider error is skipped.
//
// The loop does stop once every auxiliary role is held, which is gatherArt's stop at
// the front winner applied to a full set: a provider consulted past that point can only
// have its images dropped, after downloading them.
func (s *Service) gatherAuxArt(ctx context.Context, st *runState, req Request) (map[model.ArtRole]*model.ArtImage, string) {
	req.Want = CapAuxArt
	need := len(model.AuxArtRoles())
	var out map[model.ArtRole]*model.ArtImage
	var provider string
	for _, p := range s.providers {
		if !p.Capabilities().Has(CapAuxArt) {
			continue
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil {
			continue
		}
		for role, img := range cand.Art {
			if role == model.ArtRoleFront || !role.Valid() || img == nil || len(img.Data) == 0 {
				continue
			}
			if out[role] != nil {
				continue
			}
			img.Source, img.Provider = model.SourceEnrichment, p.Name()
			if out == nil {
				out = make(map[model.ArtRole]*model.ArtImage, len(cand.Art))
				provider = p.Name()
			}
			out[role] = img
		}
		if len(out) == need {
			break
		}
	}
	return out, provider
}

// albumMatch is what one album's tier ladder decided. Edition separates the descriptive
// medium/country evidence from a printed identifier, since only the former takes the
// edition provider marker. Skip means no tier reached a verdict for a transient reason,
// so the caller must write nothing at all rather than record a no-match.
type albumMatch struct {
	MBID    string
	Reason  string
	Edition bool
	Skip    bool
}

// matchAlbumRelease runs the tiers in order and stops at the first that decides. Barcode
// leads because it identifies a release outright where a catalog number identifies one
// only within a label, so the later requests are skipped entirely on the CD rips this
// phase exists to serve.
//
// The edition tier is last and costs a whole-group browse, so it runs only when the
// album's media/country would actually interpret. The queue gate fires on a non-empty
// column rather than an interpretable one, so without this check an album whose only
// evidence is MEDIA=FLAC would spend a request to discover it has nothing to say.
func (s *Service) matchAlbumRelease(ctx context.Context, st *runState, t model.EnrichTarget) (albumMatch, error) {
	if spellings := barcodeSpellings(t.Barcode); len(spellings) > 0 {
		byBarcode, err := s.mb.searchReleaseByIdentifier(ctx, st.forced(), t.ReleaseGroupMBID, "barcode", spellings)
		if err != nil {
			return albumMatch{}, err
		}
		if mbid, reason := matchRelease(t, byBarcode, nil); mbid != "" {
			return albumMatch{MBID: mbid, Reason: reason}, nil
		}
	}
	if cat := strings.TrimSpace(t.CatalogNumber); cat != "" {
		byCatNo, err := s.mb.searchReleaseByIdentifier(ctx, st.forced(), t.ReleaseGroupMBID, "catno", []string{cat})
		if err != nil {
			return albumMatch{}, err
		}
		if mbid, reason := matchRelease(t, nil, byCatNo); mbid != "" {
			return albumMatch{MBID: mbid, Reason: reason}, nil
		}
	}
	if !editionEvidence(t) {
		return albumMatch{}, nil
	}
	// One browse per group per run, and one refresh per group per run. A group already
	// attempted and found unusable is not re-paged for each of its remaining albums, and
	// a forced run refreshes a group once and lets every later album under it read what
	// that refresh cached.
	//
	// A retried album is the case the refreshed set exists for: it can arrive under a
	// group a fresh sibling already browsed this run, which makes the group "attempted"
	// off a cache read that predates the re-ask. So the refresh is keyed on whether this
	// run actually re-browsed the group, and an unusable attempt yields to one.
	usable, attempted := st.browsedGroups[t.ReleaseGroupMBID]
	refresh := st.forced() && !st.refreshedGroups[t.ReleaseGroupMBID]
	if attempted && !usable && !refresh {
		return albumMatch{Skip: true}, nil
	}
	group, ok, err := s.mb.releaseEditions(ctx, refresh, t.ReleaseGroupMBID)
	if err != nil {
		return albumMatch{}, err
	}
	if refresh {
		st.refreshedGroups[t.ReleaseGroupMBID] = true
	}
	st.browsedGroups[t.ReleaseGroupMBID] = ok
	if !ok {
		return albumMatch{Skip: true}, nil
	}
	mbid, reason, edition := matchEdition(t, group)
	return albumMatch{MBID: mbid, Reason: reason, Edition: edition}, nil
}

// enrichBook resolves an audiobook against a MusicBrainz release and applies its
// external identifiers and publisher. It matches only by an explicit release MBID,
// since audiobook text search throws too many false positives. Returns whether a
// provider matched.
func (s *Service) enrichBook(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	enr := model.BookEnrichment{BookItemID: t.ID, PID: t.PID}
	if t.MBID != "" {
		r, err := s.mb.lookupRelease(ctx, st.forced(), t.MBID)
		if err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
			return false, err
		}
		if r != nil {
			enr.Matched = true
			enr.MBID = r.ID
			enr.ASIN = r.ASIN
			enr.ISBN = r.Barcode
			if len(r.LabelInfo) > 0 {
				enr.Publisher = r.LabelInfo[0].Label.Name
			}
		}
	}
	if err := s.store.ApplyBookEnrichment(ctx, enr); err != nil {
		return false, err
	}
	return enr.Matched, nil
}

// enrichTrackFields fills one track's empty scalar fields from the providers advertising
// CapFields and marks it so the walk does not repeat. The rung is the request type: a
// recording target's answer lands on this one item, which is what separates it from the
// album walk, whose year fans across every member.
func (s *Service) enrichTrackFields(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	in := model.ItemFieldsEnrichment{ItemID: t.ID, PID: t.PID}
	fields, providers := s.gatherFields(ctx, Request{
		Type: TargetRecording, Force: st.forced(), Want: CapFields,
		Title: t.Name, Artist: t.ArtistName, Album: t.Album,
		DurationSec: t.DurationSec, ISRC: t.ISRC, MBID: t.MBID,
	}, CapFields, model.EnrichFillFields(model.KindTrack))
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	if err := s.store.ApplyItemFields(ctx, in); err != nil {
		return false, err
	}
	return in.Matched, nil
}

// enrichBookFields is the book twin, gated by CapBookMeta so the book providers an
// embedder registers are actually asked: before this walk existed, enrichBook consulted
// only the MusicBrainz spine and no CapBookMeta provider was ever dispatched to.
//
// The dedicated Publisher/ASIN/ISBN slots fold in as fallbacks for their own keys, so a
// provider written against those alone contributes without changing.
func (s *Service) enrichBookFields(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	in := model.ItemFieldsEnrichment{ItemID: t.ID, PID: t.PID}
	fields, providers := s.gatherFields(ctx, Request{
		Type: TargetBook, Force: st.forced(), Want: CapBookMeta,
		Title: t.Name, Artist: t.ArtistName,
		ASIN: t.ASIN, ISBN: t.ISBN, MBID: t.MBID,
	}, CapBookMeta, model.EnrichFillFields(model.KindBook))
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	if err := s.store.ApplyItemFields(ctx, in); err != nil {
		return false, err
	}
	return in.Matched, nil
}

// enrichAlbumFields fills one album's empty label and year from the providers advertising
// CapFields. The rung is the request type: a release target's answer lands on the album
// row and, for year, on every member at once, which is what separates it from the
// recording walk above.
func (s *Service) enrichAlbumFields(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	in := model.AlbumFieldsEnrichment{AlbumID: t.ID, PID: t.PID}
	fields, providers := s.gatherFields(ctx, Request{
		Type: TargetRelease, Force: st.forced(), Want: CapFields,
		Title: t.Name, Artist: t.ArtistName, MBID: t.MBID,
		Barcode: t.Barcode, CatalogNumber: t.CatalogNumber,
	}, CapFields, model.AlbumFillFields())
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	if err := s.store.ApplyAlbumFields(ctx, in); err != nil {
		return false, err
	}
	return in.Matched, nil
}

// gatherFields asks every provider advertising want for scalar fields, keeping the first
// offer per key. Keys outside allowed are dropped here rather than at apply, so a
// provider answering a field the catalog will never take does not decide anything.
//
// It returns the provider name PER KEY, not one name for the batch. Two providers
// commonly split a set (one knows the bpm, another the isrc), and the provenance row is
// where a consumer attributes a value and reasons about a conflict, so stamping the
// second provider's value with the first's name is a lie in exactly the field WaxDeck
// asked for. The marker takes the first contributor's name, which is all it claims.
//
// The loop stops once every allowed key is held, so a further provider is not called for
// answers that would be discarded. For a book request the dedicated Publisher/ASIN/ISBN
// slots fill their keys when Fields did not.
func (s *Service) gatherFields(ctx context.Context, req Request, want Capability, allowed map[string]bool) (values, providers map[string]string) {
	take := func(name, key, value string) {
		if !allowed[key] || strings.TrimSpace(value) == "" || values[key] != "" {
			return
		}
		if values == nil {
			values = make(map[string]string, len(allowed))
			providers = make(map[string]string, len(allowed))
		}
		values[key], providers[key] = value, name
	}
	for _, p := range s.providers {
		if !p.Capabilities().Has(want) {
			continue
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil {
			continue
		}
		for key, value := range cand.Fields {
			take(p.Name(), key, value)
		}
		if req.Type == TargetBook {
			take(p.Name(), "publisher", cand.Publisher)
			take(p.Name(), "asin", cand.ASIN)
			take(p.Name(), "isbn", cand.ISBN)
		}
		if len(values) == len(allowed) {
			break
		}
	}
	return values, providers
}

// firstFieldProvider names the provider a marker should credit: the one behind the
// first key in sorted order, so the choice is stable rather than map-order noise. The
// per-key names are what the provenance rows carry.
func firstFieldProvider(providers map[string]string) string {
	keys := make([]string, 0, len(providers))
	for k := range providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if providers[k] != "" {
			return providers[k]
		}
	}
	return ""
}

// enrichLyrics fills one track's lyrics from the first lyrics provider to answer
// (injected first, then LRCLIB). A provider error is best-effort (logged, skipped);
// only the store write can abort. A no-match still records the marker so the track is
// not re-queried every run. Returns whether a provider matched.
func (s *Service) enrichLyrics(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	req := Request{
		Type: TargetRecording, Force: st.forced(), Want: CapLyrics,
		Title: t.Name, Artist: t.ArtistName, Album: t.Album, DurationSec: t.DurationSec,
	}
	var got *model.Lyrics
	var provider string
	for _, p := range s.providers {
		if !p.Capabilities().Has(CapLyrics) {
			continue
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil || !cand.Lyrics.HasContent() {
			continue
		}
		got, provider = cand.Lyrics, p.Name()
		// Stamped here, not trusted from the provider, the same way gatherArt stamps a
		// cover: an injected provider cannot claim its words came off the file's tags,
		// and one that stamps nothing at all cannot reach putLyricsTx unattributed,
		// where an empty source reads as "no lyrics" and drops them silently.
		got.Source, got.Provider = model.SourceEnrichment, p.Name()
		break
	}
	in := model.LyricsEnrichment{ItemID: t.ID, PID: t.PID, Matched: got != nil, Lyrics: got, Provider: provider}
	if err := s.store.ApplyLyricsEnrichment(ctx, in); err != nil {
		return false, err
	}
	return in.Matched, nil
}

// genreCandidate is one genre display name and the provider that supplied it, used to
// attribute the display-primary genre to a provider for field provenance.
type genreCandidate struct {
	name     string
	provider string
}

// gatherGenres merges genres from the genre providers and the MusicBrainz baseline
// into one deduped union in priority order: injected providers first, then the
// MusicBrainz baseline, then the built-in providers (ListenBrainz). Every MusicBrainz
// baseline genre is kept (they were always applied before providers were merged in);
// only the non-MusicBrainz additions are capped, so a provider ranked ahead can never
// evict an authoritative MB genre. It returns the merged display names and the provider
// that supplied the display-primary genre (for field provenance), "" when nothing was
// found.
func (s *Service) gatherGenres(ctx context.Context, st *runState, rg *mbReleaseGroup, mbBaseline []string) ([]string, string) {
	req := Request{
		Type: TargetReleaseGroup, Force: st.forced(), Want: CapGenres,
		Title: rg.Title, Artist: releaseGroupArtistName(rg), MBID: rg.ID,
	}
	var cands []genreCandidate
	add := func(p Provider) {
		if !p.Capabilities().Has(CapGenres) {
			return
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil {
			return
		}
		for _, g := range cand.Genres {
			cands = append(cands, genreCandidate{name: g, provider: p.Name()})
		}
	}
	for _, p := range s.providers[:s.numInjected] {
		add(p)
	}
	for _, g := range mbBaseline {
		cands = append(cands, genreCandidate{name: g, provider: providerMusicBrainz})
	}
	for _, p := range s.providers[s.numInjected:] {
		add(p)
	}

	seen := make(map[string]bool, len(cands))
	var names []string
	var primary string
	nonMB := 0
	for _, c := range cands {
		isMB := c.provider == providerMusicBrainz
		for _, name := range identity.SplitGenres(c.name) {
			mk := identity.MatchKey(name)
			if mk == "" || seen[mk] {
				continue
			}
			// Cap only the non-MusicBrainz (injected/community) additions; a MusicBrainz
			// baseline genre is authoritative and always kept, so the cap never narrows
			// what a pre-provider run would have applied.
			if !isMB && nonMB >= maxEnrichGenres {
				continue
			}
			seen[mk] = true
			if !isMB {
				nonMB++
			}
			if primary == "" {
				primary = c.provider
			}
			names = append(names, name)
		}
	}
	return names, primary
}

// gatherArt returns the first offered image per art role, in priority order (injected
// first, then the Cover Art Archive), plus the name of the first provider to contribute
// one, which is what a marker credits at the rungs that write one.
//
// A cover provider is asked under CapCover and
// every role it offers is taken, auxiliary ones included, so a provider that has
// always served them under that one capability keeps working unchanged. A provider
// advertising CapAuxArt without CapCover is asked too, under that capability, and only
// its non-front roles are taken, the same front-drop gatherAuxArt applies: a target
// this gather settles can leave its queue for good (a matched album keeps its mbid),
// so leaving the aux-only providers to the backfill alone would never fill its
// auxiliary slots. A provider with both capabilities is asked once, under CapCover.
//
// req names the rung: a release group, or the specific release an album was
// matched to. Routing both through the provider list rather than reaching for the
// built-in CAA directly is what lets an embedder's cover provider serve either one,
// and keeps the documented priority order intact. It is best-effort: a provider error
// or a missing cover is skipped, never aborting the run.
//
// Per consulted provider the offered roles merge first-offer-wins per role (nil
// images, empty data, and invalid roles are skipped). A provider is asked only for
// something it could still contribute, and under the capability that names it, so a
// provider whose every slot is held is never called.
//
// wantAux decides what happens once the front lands. False stops there: providers after
// the front winner are not consulted, which preserves the pre-role call cadence and
// avoids extra full-cover downloads such as CAA's up-to-24MiB fetch, so aux coverage is
// opportunistic. The artist and release-group rungs pass false because a backfill phase
// behind them asks the same auxiliary providers later; consulting them here as well
// would be one duplicate request per entity per run.
//
// True keeps the walk going for the auxiliary roles alone, asking each remaining
// provider under CapAuxArt. The album rung passes true because it IS the backfill:
// nothing runs after it, and its marker is durable, so an auxiliary slot skipped here is
// skipped for good.
//
// Every accepted image is stamped here rather than trusted from the provider, the
// same way gatherGenres records which provider supplied the display-primary genre: an
// injected provider cannot claim a cover came from the tags. SourceURL is left as the
// provider set it, since only the provider knows where it fetched, so an injected
// provider can be named one thing and point at another.
//
// A provider may answer a release request with FrontIsGroupFront rather than bytes;
// that settles the front the way an image would, and the third result carries it to the
// album rung, the only caller that can act on it.
func (s *Service) gatherArt(ctx context.Context, st *runState, req Request, wantAux bool) (map[model.ArtRole]*model.ArtImage, string, bool) {
	// Stamped on the value parameter, so both callers get it without repeating it.
	req.Want = CapCover
	auxReq := req
	auxReq.Want = CapAuxArt
	auxNeed := len(model.AuxArtRoles())
	var out map[model.ArtRole]*model.ArtImage
	var provider string
	fromGroup := false
	auxHeld := 0
	for _, p := range s.providers {
		caps := p.Capabilities()
		// A provider is consulted only for something it could still contribute, and
		// under the capability that names it: the front while the front is open, else
		// the auxiliary roles while any is.
		askFront := caps.Has(CapCover) && !fromGroup && out[model.ArtRoleFront] == nil
		askAux := caps.Has(CapAuxArt) && auxHeld < auxNeed
		if !askFront && !askAux {
			continue
		}
		ask := auxReq
		if askFront {
			ask = req
		}
		cand, err := s.callProvider(ctx, p, ask)
		if err != nil || cand == nil {
			continue
		}
		// The front is settled by the picture the caller already holds, so nothing is
		// stored for it and no later provider is asked about it. A provider that answers
		// both ways at once is held to the documented contract rather than driving the
		// store's two front paths against each other: the reuse wins and the bytes are
		// dropped.
		fromGroupHere := askFront && cand.FrontIsGroupFront
		if fromGroupHere {
			fromGroup = true
			if provider == "" {
				provider = p.Name()
			}
		}
		offered := make(map[model.ArtRole]*model.ArtImage, len(cand.Art)+1)
		for role, img := range cand.Art {
			offered[role] = img
		}
		// "Present" means usable: a role-map front carrying no bytes must not
		// suppress the Cover alias and then be dropped by the empty-data skip below,
		// which would lose a cover the provider did answer with. The alias belongs to a
		// cover request alone; an auxiliary ask never writes the front.
		if askFront && !fromGroupHere {
			if f := offered[model.ArtRoleFront]; f == nil || len(f.Data) == 0 {
				offered[model.ArtRoleFront] = cand.Cover
			}
		} else {
			delete(offered, model.ArtRoleFront)
		}
		for role, img := range offered {
			if !role.Valid() || img == nil || len(img.Data) == 0 {
				continue
			}
			if out[role] != nil {
				continue
			}
			img.Source, img.Provider = model.SourceEnrichment, p.Name()
			if out == nil {
				out = make(map[model.ArtRole]*model.ArtImage, len(offered))
				provider = p.Name()
			}
			out[role] = img
			if role != model.ArtRoleFront {
				auxHeld++
			}
		}
		if !wantAux && (fromGroup || out[model.ArtRoleFront] != nil) {
			break
		}
	}
	return out, provider, fromGroup
}

// auxArtRoles splits the non-front roles out of a gathered art map, nil when there
// are none.
func auxArtRoles(art map[model.ArtRole]*model.ArtImage) map[model.ArtRole]*model.ArtImage {
	var out map[model.ArtRole]*model.ArtImage
	for role, img := range art {
		if role == model.ArtRoleFront {
			continue
		}
		if out == nil {
			out = make(map[model.ArtRole]*model.ArtImage, len(art))
		}
		out[role] = img
	}
	return out
}

// callProvider runs one candidate-provider lookup under a soft per-provider timeout so
// a slow optional provider cannot stall the pass. It is best-effort: an error is
// logged and returned for the caller to skip past. Only the identity spine (mb/aid)
// aborts a run; every port provider is optional. Run cancellation still propagates,
// because the next store write (or the runSweep loop's context check) observes it.
func (s *Service) callProvider(ctx context.Context, p Provider, req Request) (*Candidate, error) {
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	cand, err := p.Enrich(cctx, req)
	if err != nil {
		s.log.Warn("enrich provider failed; skipping", "provider", p.Name(), "target", req.Type, "err", err)
		return nil, err
	}
	return cand, nil
}

// albumArtSlots reports which album art vacancies the registered providers could
// actually fill. A slot no provider serves is left out, so a stock install never marks
// an album for an auxiliary vacancy nothing could have answered, and an install with
// neither capability skips the phase and its count outright.
func (s *Service) albumArtSlots() model.AlbumArtSlots {
	return model.AlbumArtSlots{
		Front: s.hasCapability(CapCover),
		Aux:   s.hasCapability(CapAuxArt),
	}
}

// hasCapability reports whether any registered provider advertises c, so an
// entity/recording phase that no provider can serve is skipped entirely.
func (s *Service) hasCapability(c Capability) bool {
	for _, p := range s.providers {
		if p.Capabilities().Has(c) {
			return true
		}
	}
	return false
}

// Coverage reports how many entities have been enriched, for doctor.
func (s *Service) Coverage(ctx context.Context) (model.EnrichmentCoverage, error) {
	return s.store.EnrichmentCoverage(ctx)
}

// cache adapts the Store cache methods. Force is handled per-call by the caller
// (passed into musicBrainz.get), not by mutating shared state, so the Service stays
// safe for concurrent use.
type cache struct {
	store Store
}

func (c cache) get(ctx context.Context, key string) ([]byte, bool, error) {
	return c.store.EnrichmentCacheGet(ctx, key)
}

func (c cache) put(ctx context.Context, key string, payload []byte) error {
	return c.store.EnrichmentCachePut(ctx, key, payload)
}
