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
// Beside the MusicBrainz spine sit the port phases, each running with or without a
// MusicBrainz contact when some provider serves its capability at its rung: the three
// art backfills (release group, artist, album), lyrics, and the fields walks that fill
// a track's, a book's, or an album's empty scalar fields from Candidate.Fields. The
// group and album fronts are the port phases a stock install runs, since the Cover Art
// Archive answers at both rungs. The providers a pass consults are
// Config.Providers ahead of the built-ins, or whatever Config.ProviderList answers for
// that pass when the hook is set.
//
// It is the "metadata brain" enrichment half; the WaxLabel tag adapter lives in
// package meta. This package defines its own Store port (implemented by
// store/sqlite) so it depends on the domain model, not on SQLite.
package enrich

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// Every queue takes model.EnrichQueueOptions, which names the sweep it should walk: the
// targets no marker covers (SweepFresh, the zero value and the ordinary run), the lookups
// earlier passes left owed (SweepDeferred), the ones whose no-match marker has expired
// against MissCutoff (SweepRetry), the union of all three (SweepDue, which only the count
// asks for), or everything (SweepAll, the forced run). A run walks its whole phase list
// fresh, then again for the owed lookups, then again for the expired misses, so a capped
// run reaches the new files of every phase before it re-asks about anything. A
// phase-scoped force walks SweepAll for its phases and the ordinary sweeps for the rest.
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
	// ReleaseGroupsNeedingArt returns the next keyset page of release groups the
	// group-art backfill should ask about: they carry a title, no whole-entity art lock,
	// and an empty slot slots names as askable. An MBID rides along when the catalog has
	// one but does not gate the walk.
	ReleaseGroupsNeedingArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error)
	BooksNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ItemsNeedingLyrics returns the next keyset page of tracks that carry no lyrics
	// yet and that the sweep selects, each with the title, artist, album, and duration
	// a lyrics provider keys on.
	ItemsNeedingLyrics(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ArtistsNeedingArtBackfill returns the next keyset page of artists with a half of
	// their art lookup due among the slots the argument names as askable. The artist-art
	// backfill is the one asker for an artist's art, and it walks by name, so an artist
	// MusicBrainz never matched is asked about too.
	ArtistsNeedingArtBackfill(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error)
	// AlbumsNeedingArt returns the next keyset page of albums with a half of their art
	// lookup due among the slots the argument names as askable. Unlike the two backfills
	// above it walks by identifier rather than by name: the releases of one group share a
	// title, so an album carrying no release mbid, barcode or catalog number is skipped
	// rather than asked about with a title that can only return the wrong edition.
	AlbumsNeedingArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error)
	// ItemsNeedingFields returns the next keyset page of items of one kind whose fill
	// set (model.EnrichFillFields) still has a gap, each carrying the title, credit,
	// duration, and identifiers a provider keys on.
	ItemsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, kind model.Kind, ids []int64) ([]model.EnrichTarget, error)
	// AlbumsNeedingFields returns the next keyset page of albums missing a label or a
	// year, the entity rung of the same walk.
	AlbumsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// CountEntitiesNeedingEnrichment counts the phases the run built, passed as their
	// keys, so the denominator cannot drift from the walk: a nil scope counts everything,
	// a scoped count covers only the scoped ids, and a phase the scoped run skips (an
	// empty id list) contributes zero. An ordinary run asks for SweepDue, so the
	// denominator covers every sweep it walks, and a phase a phase-scoped force names is
	// counted under SweepAll.
	CountEntitiesNeedingEnrichment(ctx context.Context, q model.EnrichQueueOptions, opts model.EnrichCountOptions, scope *model.EnrichScope) (int, error)

	// ApplyItemFields writes the scalar fields a provider supplied for one item and
	// records the fields marker, unless Incomplete, which records the lookup as owed so
	// the item is asked again, or Unasked, which records a miss. Only the keys in the kind's fill set
	// that are empty and unlocked land, each stamped with the provider's name; a value
	// that fails validation is skipped rather than failing the pass, and nothing
	// surviving writes the marker alone.
	ApplyItemFields(ctx context.Context, in model.ItemFieldsEnrichment) error
	// ApplyAlbumFields writes an album's label on the album row and its year across
	// every member at once, both fill-when-empty and lock-respecting, and records the
	// album fields marker, settled by Incomplete and Unasked as ApplyItemFields settles
	// its own. The year fill is vetoed unless the album has no year and every member is
	// present, year-less, and unlocked, since it moves the album identity key.
	ApplyAlbumFields(ctx context.Context, in model.AlbumFieldsEnrichment) error

	ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error
	ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error
	// ApplyReleaseGroupArtBackfill fills a release group's empty art roles, front and
	// auxiliary (fill-when-empty, lock-respecting per role), and records a marker for each
	// half of the lookup the walk asked whether or not anything was found, so a group no
	// provider serves is not re-asked every run, unless the half was Incomplete, which
	// records it as owed so it is asked again, or Unasked, which records a miss.
	ApplyReleaseGroupArtBackfill(ctx context.Context, in model.ReleaseGroupArtBackfill) error
	// ApplyArtistArtBackfill fills an artist's empty art roles, front and auxiliary
	// (fill-when-empty, lock-respecting per role), and records a marker for each half of
	// the lookup the walk asked, settled as the release-group backfill's are.
	ApplyArtistArtBackfill(ctx context.Context, in model.ArtistArtBackfill) error
	// ApplyAlbumArtBackfill is the album twin: it fills the album's own art rung,
	// fill-when-empty against what the art chain already resolves, and records a marker
	// for each half of the lookup the walk asked, settled as the release-group backfill's
	// are. An album that has vanished since the queue page takes neither the fill nor a
	// marker.
	ApplyAlbumArtBackfill(ctx context.Context, in model.AlbumArtBackfill) error
	// ApplyAlbumReleaseMatch fills an album's release MBID when it has none and records
	// the marker either way (under the deciding tier's provider) so a no-match is not
	// re-searched every run, unless Incomplete, which fills nothing and records the lookup
	// as owed so the album is asked again. It writes no art: the album-art phase keys on the
	// stored identifiers instead.
	ApplyAlbumReleaseMatch(ctx context.Context, in model.AlbumReleaseMatch) error
	ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error
	// ApplyLyricsEnrichment attaches a track's resolved lyrics, only when it has none
	// (fill-when-empty), and records the per-recording enrichment marker, settled by
	// Incomplete and Unasked as ApplyItemFields settles its own.
	ApplyLyricsEnrichment(ctx context.Context, in model.LyricsEnrichment) error

	// ExpiredMissesExist reports whether any no-match marker predates cutoff, so a run
	// can skip the retry sweep entirely rather than have every phase re-query its base
	// table for a population that is usually empty.
	ExpiredMissesExist(ctx context.Context, cutoff int64) (bool, error)
	// ExpireDeferredLookups settles every lookup owed since at or before cutoff as it
	// stands and answers the same question for the owed sweep: 0 when nothing is left
	// owed, else the instant the sweep measures against, after every marker written
	// before the call and before every one written after it.
	ExpireDeferredLookups(ctx context.Context, cutoff int64) (int64, error)

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
	// Last.fm, Audnexus, ...). A pass consults them in the list's order, which is these
	// ahead of the built-in field/genre/cover/lyrics providers unless ProviderList
	// reorders it, and the first to answer wins a value conflict; the MusicBrainz identity
	// spine still resolves the anchoring MBID first regardless. Each needs a name of its
	// own (see Provider.Name). The default CLI build injects none.
	Providers []Provider
	// ProviderList, when set, supplies the providers a pass consults, in priority order.
	// It is handed the fixed list New assembled, Providers ahead of the key-free
	// built-ins, and what it returns is used in its place: reordered, with a provider left
	// out or added, the built-ins moved or dropped like any other. It is asked once at the
	// start of every pass and the pass holds the answer for its whole walk, so a change
	// while a pass runs takes effect on the next one. It is also asked whenever Phases or
	// Enabled is read outside a pass, so it has to be cheap and safe for concurrent use,
	// and the Service never modifies what it returns. A nil or nameless entry is dropped
	// as a nameless one is from Providers, a name listed twice keeps its first place, an
	// entry under a built-in's name has to be that built-in as the fixed list handed it
	// over (a wrapper or a stand-in is dropped), and a nil hook keeps the fixed list.
	//
	// A provider left out is not asked, and nothing records that it was left out: the
	// pass settles its targets on the providers it does list, as it would with the
	// provider never registered. A port phase that loses its last provider does not run,
	// so its targets wait for a pass that lists one, but the release-group identity always
	// runs and settles its genre rider on the list it has: a group resolved while the
	// genre providers were left out keeps what MusicBrainz gave it, until
	// RunOptions.ForcePhases re-asks its phase.
	ProviderList func(fixed []Provider) []Provider

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
	providerTimeout = 15 * time.Second
	// providerTripAfter is the run of consecutive failures after which a provider drops
	// out of the pass. A quota window or an outage fails every call the same way, and
	// with a failure leaving its lookup owed rather than answered, a pass would otherwise
	// spend one failed request, or one providerTimeout, per remaining target on a service
	// that is not answering; three in a row is past what one bad entity produces. The
	// targets after the trip are settled on what the live providers say, with the slots
	// the tripped provider serves recorded as a miss so they fall due at the retry
	// window; a target whose open slots only it served is passed over unmarked, and a
	// phase left with no live provider ends its sweep. The next run starts clean.
	providerTripAfter = 3
	// owedLookupWindow bounds how long a lookup stays owed when no pass asks it again: its
	// slot was filled some other way, so no queue selects it, or every pass since found
	// the provider it needs out. Past it the lookup is settled as it stands, an identity
	// as its match and a port lookup that found nothing as a miss the retry window
	// re-asks.
	owedLookupWindow     = 7 * 24 * time.Hour
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

	// fixed is the provider list New assembled, the named injected providers then the
	// key-free built-ins, and builtins is its tail. A pass consults fixed, or whatever
	// ProviderList answers for it, in the list's order: first non-nil wins for a
	// single-value candidate (cover, lyrics), and genres merge as a union in list order,
	// the MusicBrainz baseline at its own entry's place.
	fixed    []Provider
	builtins []Provider

	// artistRungWarned names the providers warnReleaseArtForArtists has already logged.
	artistRungWarned sync.Map
}

// New builds an enrichment service from cfg, constructing the shared netsafe client
// with the contact User-Agent and MusicBrainz pacing, then registering the injected
// providers ahead of the key-free built-ins (Cover Art Archive cover, the MusicBrainz
// genre entry, ListenBrainz genres, LRCLIB lyrics). Each rate-limited built-in gets its
// own paced client.
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

	// Injected providers rank first in the fixed list, ahead of the built-ins.
	//
	// A provider's name is the provenance mark stamped on everything it supplies, and the
	// store refuses an enrichment value that names no provider. Dropping a nameless one
	// here keeps that refusal from aborting the whole pass for every item it answers,
	// which would leave the catalog retrying forever with nothing to show for it. The name
	// is also the provider's identity within a pass (the breaker counts by it, and the
	// genre merge finds the MusicBrainz entry by it), so a name the built-ins or the
	// markers already use, or one an earlier injected provider took, is dropped too.
	seen := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p == nil {
			log.Warn("enrichment: dropping a nil injected provider")
			continue
		}
		name := p.Name()
		switch {
		case name == "":
			log.Warn("enrichment: dropping an injected provider with no name; its values could carry no provenance")
			continue
		case reservedProviderName(name):
			log.Warn("enrichment: dropping an injected provider named after a built-in or a marker label; its values could not be told apart",
				"provider", name)
			continue
		case seen[name]:
			log.Warn("enrichment: dropping an injected provider whose name an earlier one took; the two could not be told apart",
				"provider", name)
			continue
		}
		seen[name] = true
		s.fixed = append(s.fixed, p)
	}

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
	// genre built-ins each get their own paced client. The MusicBrainz genre entry sits
	// ahead of ListenBrainz, so the fixed list merges the spine's genres before the
	// community tags, and it is always registered, since the spine resolves them
	// whenever a contact is set.
	builtinInterval := cfg.MinRequestInterval
	if builtinInterval == 0 {
		builtinInterval = defaultBuiltinInterval
	}
	builtinPolicy := netsafe.Policy{UserAgent: ua, Timeout: timeout, BlockPrivateIPs: cfg.BlockPrivateIPs, MinHostInterval: builtinInterval}
	if cfg.FetchCoverArt {
		s.builtins = append(s.builtins, &caaProvider{
			caa: &coverArt{client: client, baseURL: baseOr(cfg.CoverArtBaseURL, defaultCAABaseURL), cache: c},
			log: log,
		})
	}
	s.builtins = append(s.builtins, mbGenres{})
	if cfg.FetchCommunityGenres {
		s.builtins = append(s.builtins, &listenBrainz{
			client: netsafe.New(builtinPolicy), baseURL: baseOr(cfg.ListenBrainzBaseURL, defaultLBBaseURL),
		})
	}
	if cfg.FetchLyrics {
		s.builtins = append(s.builtins, &lrclib{
			client: netsafe.New(builtinPolicy), baseURL: baseOr(cfg.LRCLibBaseURL, defaultLRCLibBaseURL),
		})
	}
	s.fixed = append(s.fixed, s.builtins...)
	return s
}

// Builtins returns the key-free built-in providers New registered, in registration
// order: the Cover Art Archive, the MusicBrainz genre entry, ListenBrainz and LRCLIB. The
// genre entry comes with a contact, and the other three with a contact and their own
// toggle, so an install without a contact has none. It is what a settings surface lists
// as on offer before any pass runs; a ProviderList hook finds the same values, by name,
// in the fixed list it is handed.
func (s *Service) Builtins() []Provider { return slices.Clone(s.builtins) }

// providerList returns the providers a pass consults, in priority order: the fixed list,
// or the hook's answer to a copy of it with nil, nameless and repeated entries dropped,
// along with an entry that takes a built-in's name without being that built-in.
func (s *Service) providerList() []Provider {
	if s.cfg.ProviderList == nil {
		return s.fixed
	}
	answer := s.cfg.ProviderList(slices.Clone(s.fixed))
	out := make([]Provider, 0, len(answer))
	seen := make(map[string]bool, len(answer))
	for _, p := range answer {
		if p == nil {
			continue
		}
		name := p.Name()
		if name == "" || seen[name] || (reservedProviderName(name) && !slices.Contains(s.builtins, p)) {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	return out
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
// the phase its capability gates at that phase's rung, without one.
const enrichDisabledMessage = "enrichment needs a MusicBrainz contact " +
	"(set enrichment.contact) or an injected provider serving one of its phases"

// Enabled reports whether any phase can run, which is Phases being non-empty: the spine
// is configured, or a provider in the current list serves a phase's capability at that
// phase's rung. An injected provider brings its own credentials and its own service
// agreement, so a catalog with one and no contact still has work to do.
func (s *Service) Enabled() bool { return len(s.Phases()) > 0 }

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
	// GroupArtEnriched and GroupArtMatched count release groups the group-art backfill
	// phase walked, and the ones some provider answered for: entities, like every other
	// Enriched/Matched pair here. The images land in ArtFetched and AuxArtFetched with
	// every other pass's, so a run can report GroupArtEnriched=40 with ArtFetched=6.
	GroupArtEnriched int
	GroupArtMatched  int
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
	// Retried counts the targets walked to re-ask an earlier miss whose marker had expired
	// against the configured window: the retry sweep's, and an art backfill target an
	// earlier sweep reached with such a half due. They are counted in the phase totals
	// beside it too, since a re-ask spends the same budget as a first ask; this says how
	// much of the run was re-asking. A forced run walks one sweep and leaves it zero.
	Retried int
	// Deferred counts the walked targets whose lookup was left owed: a provider erred with
	// a slot it could have filled still open, the release match's edition browse came back
	// short, or an identity's rider provider was out of the pass. What the other providers
	// answered was applied, and a later pass asks again after its new targets; that ask
	// settles the lookup even if it fails again. They count in the phase totals and
	// against --limit like every walked target, since each spent a real request; this says
	// how much of the run is waiting on a lookup to succeed.
	Deferred int
	// Stalled names the phases whose sweep ended early because every provider serving
	// them had dropped out of the pass. Their remaining targets were not walked and keep
	// whatever marker they had, as a --limit cutoff leaves them.
	Stalled []model.EnrichPhase

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
		r.GroupArtEnriched + r.ArtistArtEnriched + r.AlbumArtEnriched + r.BooksEnriched +
		r.LyricsEnriched + r.TrackFieldsEnriched + r.BookFieldsEnriched + r.AlbumFieldsEnriched
}

// Heartbeat reports progress; it may be nil.
type Heartbeat func(progress float64, msg string) error

// Run enriches artists, then release groups, then books, until each set is
// exhausted or the limit is reached. It is resumable: each entity is committed
// independently and marked, so an interrupted run resumes where it left off. A miss
// marks the entity looked-up-with-no-match and continues, and a provider failure
// applies what the other providers answered and records the lookup as owed, asked again
// on later passes after their new targets; a MusicBrainz network failure (offline,
// cancellation) aborts with the
// underlying error rather than hammering an unreachable service. A scoped run
// (RunOptions.Scope) walks only the scoped targets through the same pipeline,
// provenance and markers included, and implies force.
func (s *Service) Run(ctx context.Context, opts RunOptions, hb Heartbeat) (*Result, error) {
	const op = "enrich.Run"
	res := &Result{}
	// The pass's provider list, read once: the refusal here, the phase list and every
	// gather consult this snapshot, so a list change made while the pass runs takes effect
	// on the next one. An Enabled read the caller made first is advice, and a list emptied
	// since then refuses here rather than walking nothing and reporting success.
	providers := s.providerList()
	if len(s.phaseKeys(providers)) == 0 {
		return res, waxerr.New(waxerr.CodeUnsupported, op, enrichDisabledMessage)
	}
	s.warnReleaseArtForArtists(providers)
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
		providers:       providers,
		failures:        map[string]int{},
		tripped:         map[string]bool{},
		stalled:         map[model.EnrichPhase]bool{},
		force:           opts.Force || scope != nil,
		forcedPhases:    map[model.EnrichPhase]bool{},
		browsedGroups:   map[string]bool{},
		refreshedGroups: map[string]bool{},
	}
	for _, p := range opts.ForcePhases {
		st.forcedPhases[p] = true
	}
	// The run's instant, resolved once so every phase measures against it: the retry
	// window's cutoff, and the week an owed lookup nothing asked again is kept for. A
	// forced run has one sweep that takes everything, so no cutoff applies; otherwise the
	// fresh targets are walked first, then the lookups earlier passes left owed, then the
	// expired misses.
	start := time.Now()
	st.sweeps = []model.EnrichSweep{model.SweepAll}
	if !st.force {
		st.sweeps = []model.EnrichSweep{model.SweepFresh}
		// One look at the marker table decides whether each later sweep is worth walking.
		// Most nights nothing is owed or due, and adding the sweeps regardless makes every
		// phase re-query its own base table for an empty answer. The owed sweep's line is
		// the store's rather than start, since only the store can place it strictly
		// between the markers written before this run and the ones it writes.
		asOf, err := s.store.ExpireDeferredLookups(ctx, start.Add(-owedLookupWindow).UnixNano())
		if err != nil {
			return res, err
		}
		if asOf != 0 {
			st.deferredBefore = asOf
			st.sweeps = append(st.sweeps, model.SweepDeferred)
		}
		if s.cfg.RetryMissesAfter > 0 {
			st.missCutoff = start.Add(-s.cfg.RetryMissesAfter).UnixNano()
			due, err := s.store.ExpiredMissesExist(ctx, st.missCutoff)
			if err != nil {
				return res, err
			}
			if due {
				st.sweeps = append(st.sweeps, model.SweepRetry)
			}
		}
	}
	res.Reach = &model.EnrichScope{}
	phases := s.phases(st, res, scope)
	built := keysOf(phases)
	if err := checkPhasesBuilt(built, opts.ForcePhases, op); err != nil {
		return res, err
	}

	// The total is only needed to report a heartbeat ratio, so skip the counting query
	// entirely when there is no heartbeat.
	var total int
	if hb != nil {
		// The count takes the keys of the list above, scoped as the run scoped it, under
		// SweepDue, the union of every sweep the run walks. An entity due on several of them
		// is walked once, on the first, so it counts once. The pass can still do more than
		// the catalog held at its start: an id the release-group phase lands re-queues a
		// group the backfill had marked from a request without one.
		countSweep := model.SweepDue
		if st.force {
			countSweep = model.SweepAll
		}
		n, err := s.store.CountEntitiesNeedingEnrichment(ctx,
			model.EnrichQueueOptions{Sweep: countSweep, MissCutoff: st.missCutoff, DeferredBefore: st.deferredBefore},
			model.EnrichCountOptions{
				Phases:    built,
				AlbumArt:  artSlots(st.providers, TargetRelease),
				GroupArt:  artSlots(st.providers, TargetReleaseGroup),
				ArtistArt: artSlots(st.providers, TargetArtist),
				Forced:    opts.ForcePhases,
			}, scope)
		if err != nil {
			return res, err
		}
		total = n
	}
	// However far the work runs past the count, a mid-run ratio stays short of 1 and
	// never goes back; only the last beat reports the pass done.
	progress := func() float64 {
		done := res.total()
		return float64(done) / float64(max(total, done+1))
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
	//
	// An art backfill's two halves can fall due on different sweeps. Its queue reports
	// every half due in the run, so the first sweep to reach the entity asks them all and
	// the later ones find it settled.
	for n, sweep := range st.sweeps {
		st.retrying = sweep == model.SweepRetry
		st.owedWalk = sweep == model.SweepDeferred
		for i := range phases {
			q := model.EnrichQueueOptions{Sweep: sweep, MissCutoff: st.missCutoff, DeferredBefore: st.deferredBefore}
			st.forcing = st.forcedPhases[phases[i].key]
			if st.forcing {
				// The first sweep took everything, so the later sweeps have nothing left.
				if n > 0 {
					continue
				}
				q.Sweep = model.SweepAll
			}
			if err := s.runSweep(ctx, st, phases[i], res, q, beat, remaining, limitReached); err != nil {
				return res, err
			}
		}
		st.retrying, st.owedWalk = false, false
	}
	st.forcing = false
	if hb != nil {
		_ = hb(1, "enriched "+strconv.Itoa(res.total())+" entities")
	}
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
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichArtist(ctx, st, t)
			},
		})
	}
	if identityRuns(rgIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseReleaseGroup, enriched: &res.ReleaseGroupsEnriched, matched: &res.ReleaseGroupsMatched, reach: &res.Reach.ReleaseGroupIDs,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingEnrichment(ctx, q, after, lim, s.acoustEnabled(), rgIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
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
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichAlbumRelease(ctx, st, t)
			},
		})
	}
	// The group-art backfill: release groups whose front or auxiliary slots are empty. It
	// is the one asker for a vacant group front (the release-group pass only refreshes a
	// held one) and for the auxiliary roles, and each half keeps its own marker, so a cover
	// provider registered after a group was asked, one a provider list hook left out of
	// that pass, and a provider serving the auxiliary roles that joins later all reach the
	// group. Each half needs a provider serving its capability at the release-group rung.
	// The built-in Cover Art Archive serves the front, so a stock install walks the front
	// half.
	//
	// It runs after the release-group phase, so an id that phase just filled rides along
	// with the request (the queue reads release_group.mbid live).
	if slots := artSlots(st.providers, TargetReleaseGroup); slots.Any() && phaseRuns(rgIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseGroupArt, enriched: &res.GroupArtEnriched, matched: &res.GroupArtMatched, reach: &res.Reach.ReleaseGroupIDs,
			rung: TargetReleaseGroup, caps: CapCover | CapAuxArt, need: halvesNeed(TargetReleaseGroup),
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingArt(ctx, q, after, lim, slots, rgIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichGroupArt(ctx, st, res, t)
			},
		})
	}
	// The artist-art backfill: artists whose front or background is empty. It is the one
	// asker for every artist slot, since the identity phase asks about no art, and each
	// half needs a provider serving its own bit at the artist rung (artCaps), so a
	// provider serving fronts alone opens the front half alone. No built-in serves either,
	// so a stock install walks nothing and writes no markers.
	//
	// After the identity phase for the reason the group-art backfill is after the
	// release-group one: the queue reads artist.mbid live, so an id that phase just
	// filled rides along with the request. The walk is keyed on the name, so an
	// unmatched artist is reached either way. Its place among the art phases is
	// otherwise free.
	if slots := artSlots(st.providers, TargetArtist); slots.Any() && phaseRuns(artistIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseArtistArt, enriched: &res.ArtistArtEnriched, matched: &res.ArtistArtMatched, reach: &res.Reach.ArtistIDs,
			rung: TargetArtist, caps: CapArtistArt, need: halvesNeed(TargetArtist),
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ArtistsNeedingArtBackfill(ctx, q, after, lim, slots, artistIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichArtistArt(ctx, st, res, t)
			},
		})
	}
	// The album-art backfill: albums with an empty front or auxiliary slot that carry an
	// identifier a provider can key on. It sits right after the release match so an id
	// that phase just landed rides along with the request, since the queue reads
	// album.mbid live, the same ordering argument the group-art backfill uses.
	//
	// Its front half runs on a stock install, as the group-art one does, because the Cover
	// Art Archive serves the release rung. An album whose members carry no embedded cover
	// otherwise shows the release group's picture, one edition standing in for all of
	// them, which is the failure a per-release ask exists to avoid. Each half needs a
	// provider serving its capability at the release rung, so a group-keyed fan-art
	// service does not open the aux half: it would walk every identified album for an
	// answer it cannot give and mark each a miss every retry window.
	if slots := artSlots(st.providers, TargetRelease); slots.Any() && phaseRuns(albumIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAlbumArt, enriched: &res.AlbumArtEnriched, matched: &res.AlbumArtMatched, reach: &res.Reach.AlbumIDs,
			rung: TargetRelease, caps: CapCover | CapAuxArt, need: halvesNeed(TargetRelease),
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.AlbumsNeedingArt(ctx, q, after, lim, slots, albumIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
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
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) { return s.enrichBook(ctx, st, t) },
		})
	}
	// Lyrics are a per-recording phase, run only when a lyrics-capable provider is
	// registered so no marker is written for tracks nothing could ever fill. It walks
	// tracks that carry no lyrics yet, filling from LRCLIB (or an injected provider).
	if hasCapabilityAt(st.providers, TargetRecording, CapLyrics) && phaseRuns(lyricsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseLyrics, enriched: &res.LyricsEnriched, matched: &res.LyricsMatched, reach: &res.Reach.LyricsItemIDs,
			rung: TargetRecording, caps: CapLyrics,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingLyrics(ctx, q, after, lim, lyricsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) { return s.enrichLyrics(ctx, st, t) },
		})
	}
	// The item-rung fields walks. Each is gated by the capability that owns its rung,
	// so a stock install (which registers no fields provider) walks neither and writes
	// no markers. They come after lyrics for the reason the art backfills sit where they
	// do: nothing downstream depends on the order, and keeping the per-item phases
	// together is the readable arrangement.
	if hasCapabilityAt(st.providers, TargetRecording, CapFields) && phaseRuns(fieldsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseTrackFields, enriched: &res.TrackFieldsEnriched, matched: &res.TrackFieldsMatched, reach: &res.Reach.FieldsItemIDs,
			rung: TargetRecording, caps: CapFields,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingFields(ctx, q, after, lim, model.KindTrack, fieldsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichTrackFields(ctx, st, t)
			},
		})
	}
	if hasCapabilityAt(st.providers, TargetBook, CapBookMeta) && phaseRuns(fieldsIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseBookFields, enriched: &res.BookFieldsEnriched, matched: &res.BookFieldsMatched, reach: &res.Reach.FieldsItemIDs,
			rung: TargetBook, caps: CapBookMeta,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingFields(ctx, q, after, lim, model.KindBook, fieldsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichBookFields(ctx, st, t)
			},
		})
	}
	// The album rung of the same walk. It shares the album scope list with the release
	// match, and comes after the track walk so the two fields phases read together.
	if hasCapabilityAt(st.providers, TargetRelease, CapFields) && phaseRuns(albumIDs) {
		phases = append(phases, phase{
			key: model.EnrichPhaseAlbumFields, enriched: &res.AlbumFieldsEnriched, matched: &res.AlbumFieldsMatched, reach: &res.Reach.AlbumIDs,
			rung: TargetRelease, caps: CapFields,
			fetch: func(ctx context.Context, q model.EnrichQueueOptions, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.AlbumsNeedingFields(ctx, q, after, lim, albumIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (outcome, error) {
				return s.enrichAlbumFields(ctx, st, t)
			},
		})
	}
	return phases
}

// Phases reports the phases an unscoped run on this install would walk now, in run
// order: the identity phases with a MusicBrainz contact, the release match with its
// toggle as well, and each port phase when some provider in the current list serves its
// capability at its rung. It is the list a status surface shows and the one a forced
// phase is checked against.
func (s *Service) Phases() []model.EnrichPhase { return s.phaseKeys(s.providerList()) }

// CheckPhases reports whether this install builds every named phase. A caller that
// submits enrichment as a job asks first, so a force naming a phase the providers gate
// off is refused rather than starting a run that walks nothing and reports itself
// complete. Run makes the same check.
func (s *Service) CheckPhases(phases []model.EnrichPhase) error {
	if len(phases) == 0 {
		return nil
	}
	return checkPhasesBuilt(s.Phases(), phases, "enrich.CheckPhases")
}

// phaseKeys reports the phases an unscoped run over providers would walk, in run order.
func (s *Service) phaseKeys(providers []Provider) []model.EnrichPhase {
	return keysOf(s.phases(&runState{providers: providers}, &Result{Reach: &model.EnrichScope{}}, nil))
}

// keysOf lists a built phase list's keys, in order.
func keysOf(phases []phase) []model.EnrichPhase {
	keys := make([]model.EnrichPhase, len(phases))
	for i := range phases {
		keys[i] = phases[i].key
	}
	return keys
}

// checkPhasesBuilt refuses a forced key the built list lacks, naming the gate.
func checkPhasesBuilt(built, want []model.EnrichPhase, op string) error {
	for _, w := range want {
		if !slices.Contains(built, w) {
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
	case model.EnrichPhaseGroupArt:
		return "it needs a provider serving a cover or auxiliary art for a release group"
	case model.EnrichPhaseArtistArt:
		return "it needs a provider serving artist art"
	case model.EnrichPhaseAlbumArt:
		return "it needs a provider serving a cover or auxiliary art for a release"
	case model.EnrichPhaseLyrics:
		return "it needs a lyrics provider"
	case model.EnrichPhaseTrackFields:
		return "it needs a provider serving fields for a recording"
	case model.EnrichPhaseAlbumFields:
		return "it needs a provider serving fields for a release"
	case model.EnrichPhaseBookFields:
		return "it needs a book metadata provider"
	}
	return "it is not a phase this install builds"
}

// runState is per-run mutable state, allocated fresh each Run so the Service stays
// safe for concurrent callers (no shared field is mutated). providers is the pass's
// provider list, read once at its start; force bypasses cached provider reads; acoustOff
// is set when the AcoustID fallback hits a (usually permanent) error, disabling it for
// the rest of the run.
type runState struct {
	providers []Provider
	// failures counts each provider's consecutive failures this run, by name, and tripped
	// holds the providers that reached providerTripAfter and are out of the pass. stalled
	// holds the phases whose sweep ended for want of a live provider.
	failures  map[string]int
	tripped   map[string]bool
	stalled   map[model.EnrichPhase]bool
	force     bool
	acoustOff bool
	// forcedPhases are the phases a phase-scoped force names, and forcing is set while
	// one of them is being walked, so forced() folds it in the way it folds retrying.
	forcedPhases map[model.EnrichPhase]bool
	forcing      bool
	// sweeps are the queue walks the run makes over its whole phase list, in order;
	// missCutoff is the instant a no-match marker has to predate to be re-asked, and
	// deferredBefore the instant an owed lookup has to predate, which the store places
	// between the markers earlier passes wrote and the ones this run writes. An
	// ordinary run walks every phase fresh, then every phase again for the lookups
	// earlier passes left owed, then again for the expired misses; a forced or scoped
	// run walks SweepAll alone.
	sweeps         []model.EnrichSweep
	missCutoff     int64
	deferredBefore int64
	// owedWalk is set while a phase walks its owed sweep. That walk is the one more ask a
	// failed lookup is owed, so it settles each lookup whatever it finds; see owes.
	owedWalk bool
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

// owes reports whether a lookup a provider failed on is left owed. Anywhere but the owed
// sweep it is: a later pass asks again. The owed sweep's ask is that later ask, and it
// settles the lookup as it stands even when it fails again, so a target a provider
// always fails on costs one more request, and a run of them at the head of the owed
// sweep can trip the provider for one pass at most rather than hold up every owed lookup
// behind them until they age out. A walk that could not ask the provider is not that
// ask: an identity's unasked rider stays owed wherever it is walked. An art backfill
// decides it per half instead (gatherHalves), since the halves one walk asks can stand
// on different sweeps.
func (st *runState) owes(failed bool) bool { return failed && !st.owedWalk }

// artForced reports whether an art backfill target is asked the way a forced run asks:
// when the walk is, or when a half due on it is a miss whose window has expired, which
// the first sweep to reach the entity asks whichever sweep that is.
func (st *runState) artForced(t model.EnrichTarget) bool {
	return st.forced() || t.FrontExpired || t.AuxExpired
}

// live reports whether some provider in the pass's list serves c at t and is still in
// the pass. A port phase asks before each target and stalls when the answer is no.
func (st *runState) live(t TargetType, c Capability) bool {
	for _, p := range st.providers {
		if !st.tripped[p.Name()] && capabilitiesAt(p, t).Has(c) {
			return true
		}
	}
	return false
}

// skippedServing reports whether one of the providers a gather skipped for being out of
// the pass serves a slot it still has open, as serves decides per provider. A gather asks
// it at the end, so a slot a live provider filled later in the list does not count as
// unasked. The provider that dropped out on this target's own failure is not among them:
// it was asked.
func skippedServing(skipped []Provider, serves func(Provider) bool) bool {
	for _, p := range skipped {
		if serves(p) {
			return true
		}
	}
	return false
}

// halvesNeed narrows an art backfill's capabilities to the halves one target has due.
func halvesNeed(rung TargetType) func(model.EnrichTarget) Capability {
	front, aux := artCaps(rung)
	return func(t model.EnrichTarget) Capability {
		var c Capability
		if t.FrontDue {
			c |= front
		}
		if t.AuxDue {
			c |= aux
		}
		return c
	}
}

// phase describes one entity type's enrichment for the shared keyset runner: how to
// fetch a page, how to enrich one target (returning what its walk decided), the
// counters to bump, and the Result.Reach list its targets are recorded in.
type phase struct {
	key      model.EnrichPhase
	enriched *int
	matched  *int
	reach    *[]int64
	fetch    func(ctx context.Context, q model.EnrichQueueOptions, afterID int64, limit int) ([]model.EnrichTarget, error)
	enrich   func(ctx context.Context, t model.EnrichTarget) (outcome, error)
	// rung and caps are what a port phase needs a live provider for: runSweep stalls
	// the phase once no provider in the pass serves caps at rung. An identity phase
	// leaves caps zero and never stalls. need narrows caps to what one target's open
	// slots call for, in the phase whose capabilities split between its slots, and
	// runSweep passes over a target no live provider can answer the way a stall leaves
	// the rest: not asked, counted, marked or heartbeaten.
	rung TargetType
	caps Capability
	need func(model.EnrichTarget) Capability
}

// outcome is what one target's walk decided: whether some provider answered, and
// whether the lookup was left owed instead of answered, because a provider called for it
// failed with a slot it could have filled still open, or an identity's rider provider
// was out of the pass. Open is measured against the gather's full set (every auxiliary
// role a provider in the pass serves, every key in the fill set), not the target's real
// vacancies, so a failure beside an answer that filled every real vacancy still reads
// deferred. Its owed marker then waits unwalked, since the queue's vacancy test no
// longer selects the target, until owedLookupWindow settles it as it stood; carrying
// the vacancies on every queue row is not worth saving that.
type outcome struct {
	matched  bool
	deferred bool
	// retried says an art backfill walk re-asked a half whose miss had expired, on a
	// sweep other than the retry sweep (Result.Retried).
	retried bool
}

// shortfall is what a gather reports beside its values, the half of an outcome the
// apply records: incomplete when a provider it called failed with a slot of the full set
// still open, unasked when a provider serving such a slot was out of the pass.
type shortfall struct {
	incomplete bool
	unasked    bool
}

// runSweep walks one sweep of one phase in keyset pages, enriching each target. It is
// the one loop behind artists, release groups, and books. A MusicBrainz or cancellation
// error aborts; a per-entity miss is marked by the enrich callback, a provider failure
// leaves the lookup owed, and the walk continues either way. The phase counters are
// pointers into the Result, and the retry and deferral tallies are the Result's own,
// since they span every phase.
func (s *Service) runSweep(ctx context.Context, st *runState, p phase, res *Result, q model.EnrichQueueOptions,
	beat func(string) error, remaining func() int, limitReached func() bool) error {
	var afterID int64
	for {
		// A phase that stalled on the fresh sweep has nobody left to ask on the retry one.
		if limitReached() || st.stalled[p.key] {
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
			if p.caps != 0 && !st.live(p.rung, p.caps) {
				return s.stall(st, p, res, beat)
			}
			if p.need != nil && !st.live(p.rung, p.need(t)) {
				continue
			}
			o, err := p.enrich(ctx, t)
			if err != nil {
				return err // MusicBrainz/cancel: abort rather than mark or hammer
			}
			(*p.enriched)++
			if o.matched {
				(*p.matched)++
			}
			verb := "enriched "
			if st.retrying || o.retried {
				res.Retried++
				verb = "retried "
			}
			if o.deferred {
				res.Deferred++
				verb = "deferred "
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

// stall ends a phase's sweep once every provider serving it has dropped out of the pass,
// without counting, reaching, or marking the target in hand. It reports the phase with a
// Warn naming the providers that serve it and dropped out, the phase key on
// Result.Stalled, and one heartbeat; runSweep skips a stalled phase's later sweeps, so
// this runs once per phase.
func (s *Service) stall(st *runState, p phase, res *Result, beat func(string) error) error {
	st.stalled[p.key] = true
	res.Stalled = append(res.Stalled, p.key)
	var out []string
	for _, q := range st.providers {
		if st.tripped[q.Name()] && capabilitiesAt(q, p.rung).Has(p.caps) {
			out = append(out, q.Name())
		}
	}
	names := strings.Join(out, ", ")
	s.log.Warn("enrichment phase stalled; every provider serving it is out of this pass", "phase", p.key, "providers", names)
	return beat("stalled " + p.key.Label() + ": " + names + " out of this pass")
}

// enrichArtist resolves one artist against MusicBrainz and applies the result. A
// miss (no MBID and no confident search hit) is still applied as a no-match marker
// so the artist is not retried every run. Returns whether a provider matched.
//
// It asks about no art. An artist's front and auxiliary roles belong to the artist-art
// backfill, which runs later in the same pass and sends the id this phase lands
// (artistMBIDLandedTx re-opens its markers when one does), so the artist identity has no
// rider and always settles.
func (s *Service) enrichArtist(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	enr := model.ArtistEnrichment{ArtistID: t.ID, PID: t.PID}
	a, err := s.resolveArtist(ctx, st, t)
	if err != nil {
		return outcome{}, err
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
	}
	if err := s.store.ApplyArtistEnrichment(ctx, enr); err != nil {
		return outcome{}, err
	}
	return outcome{matched: enr.Matched}, nil
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
// type and genres, and refreshing a front cover the group already holds. Returns
// whether a provider matched.
//
// A vacant front is the group-art backfill's, which runs after this phase in the same
// pass and asks with the id this phase just landed. Asking here too would ask every
// coverless group twice, once here and once when the backfill found the front still
// empty, so the backfill is its one asker. A held front is only refreshed, and a failed
// or skipped refresh defers nothing, which keeps a forced run during an archive outage
// from leaving the whole catalog owed.
//
// A genre provider that failed, or was out of the pass, defers a group that found no
// genre: MBID, type and whatever landed are applied, and the lookup is recorded as owed
// rather than settled, since a settled identity is not walked again and nothing else
// asks about its genres. MusicBrainz already answered, so the re-walk on a later pass
// reads its cache, the applies are fill-when-empty no-ops, and only the rider is really
// re-asked. This is the one rung where a tripped provider defers rather than settles on
// the live ones. During a rider outage that costs one cache read, one marker write and
// one heartbeat per group walked; the owed sweep runs after every phase's new targets, so
// the re-walks never hold up newer work. The pass that next asks the rider settles the
// group whatever the rider answers.
func (s *Service) enrichReleaseGroup(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (outcome, error) {
	enr := model.ReleaseGroupEnrichment{ReleaseGroupID: t.ID, PID: t.PID}
	rg, err := s.resolveReleaseGroup(ctx, st, t)
	if err != nil {
		return outcome{}, err
	}
	if rg != nil {
		enr.Matched = true
		enr.MBID = rg.ID
		enr.Type = mapReleaseGroupType(rg.PrimaryType, rg.SecondaryTypes)
		// Genres: the MusicBrainz baseline merged with the genre providers in the pass's
		// list order, deduped and capped. The winning provider of the display-primary
		// genre is recorded as field provenance.
		var genres shortfall
		enr.Genres, enr.GenreProvider, genres = s.gatherGenres(ctx, st, rg, genreNames(rg.Genres))
		// The fill reaches only members with no genre yet, so once any genre lands, or when
		// every member already carries one, a re-walk could add nothing the failed
		// provider would answer. Only an empty merge with a member to fill leaves genres
		// owed.
		owesGenres := len(enr.Genres) == 0 && t.NeedsGenres
		enr.Incomplete, enr.Unasked = st.owes(owesGenres && genres.incomplete), owesGenres && genres.unasked
		// A held front's refresh: the first cover provider to answer per role, in the pass's
		// list order (the fixed list puts an embedder's fanart.tv ahead of the built-in
		// Cover Art Archive). Best-effort: never aborts, and owes nothing. Skipped for a
		// locked cover, which the store would refuse to replace, so a forced re-run does
		// not re-download one picture per locked group.
		if t.HasArt && !t.ArtLocked {
			g := s.gatherArt(ctx, st, Request{
				Type: TargetReleaseGroup, Force: st.forced(),
				Title: rg.Title, Artist: releaseGroupArtistName(rg), MBID: rg.ID,
				GroupFrontHash: t.GroupFrontHash,
			}, auxOffered)
			enr.Art = g.art[model.ArtRoleFront]
			enr.AuxArt = auxArtRoles(g.art)
		}
	}
	if err := s.store.ApplyReleaseGroupEnrichment(ctx, enr); err != nil {
		return outcome{}, err
	}
	if enr.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(enr.AuxArt)
	return outcome{matched: enr.Matched, deferred: enr.Incomplete || enr.Unasked}, nil
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
func (s *Service) enrichAlbumRelease(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	// A scan stores identifiers verbatim, so this column holds whatever a tag said, and
	// a malformed value would make a garbage query rather than a clean miss. No marker
	// either: "could not search" must stay re-queueable, and the recheck costs nothing
	// because it bails before the request.
	if !model.IsMBID(t.ReleaseGroupMBID) {
		s.log.Warn("enrichment: skipping release match, release-group mbid is not a UUID",
			"album", t.PID, "mbid", t.ReleaseGroupMBID)
		return outcome{}, nil
	}
	m, err := s.matchAlbumRelease(ctx, st, t)
	if err != nil {
		return outcome{}, err
	}
	// A transient failure is not a decision, so nothing is filled. A no-match marker here
	// would keep the album out of the queue on every later run, which is the opposite of
	// what leaving the group uncached was for, so the lookup is recorded as owed instead,
	// replacing a standing marker: on a forced run the album carries one, and keeping it
	// would make the album wait out the window for an answer this run never got.
	if m.Skip {
		s.log.Debug("enrichment: release match inconclusive this run, leaving the album queued",
			"album", t.PID, "group", t.ReleaseGroupMBID)
		in := model.AlbumReleaseMatch{AlbumID: t.ID, PID: t.PID, Incomplete: st.owes(true)}
		if err := s.store.ApplyAlbumReleaseMatch(ctx, in); err != nil {
			return outcome{}, err
		}
		return outcome{deferred: in.Incomplete}, nil
	}
	in := model.AlbumReleaseMatch{AlbumID: t.ID, PID: t.PID, Provider: ProviderMusicBrainz}
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
		return outcome{}, err
	}
	return outcome{matched: in.Matched}, nil
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
// It asks about the halves the queue found due, and each half settles on its own
// (model.AlbumArtBackfill). A half that gathered nothing still applies, because its
// marker is what stops it being asked again next run, unless a provider failed, when the
// apply records the half as owed so a later pass asks again. The store decides what
// actually lands: the queue's vacancy test is approximate, and a per-role lock is
// re-checked there.
//
// Most of these albums are the pressing their group's cover was taken from, so a
// provider that knows so answers the front with the group's picture rather than the same
// bytes over again, and the store copies the row it already holds.
func (s *Service) enrichAlbumArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (outcome, error) {
	in := model.AlbumArtBackfill{AlbumID: t.ID, PID: t.PID}
	req := Request{
		Type: TargetRelease, Force: st.artForced(t),
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
	g, front, aux := s.gatherHalves(ctx, st, req, t)
	in.Art, in.AuxArt = g.art[model.ArtRoleFront], auxArtRoles(g.art)
	in.Front, in.Aux = front, aux
	if g.fromGroup {
		in.FrontFromGroup, in.GroupFrontHash = true, t.GroupFrontHash
	}
	if err := s.store.ApplyAlbumArtBackfill(ctx, in); err != nil {
		return outcome{}, err
	}
	if in.Art != nil {
		res.ArtFetched++
	}
	if in.FrontFromGroup {
		res.ArtReused++
	}
	res.AuxArtFetched += len(in.AuxArt)
	return outcome{matched: in.Art != nil || in.FrontFromGroup || len(in.AuxArt) > 0,
		deferred: in.Front.Incomplete || in.Aux.Incomplete, retried: t.FrontExpired || t.AuxExpired}, nil
}

// enrichGroupArt backfills one release group's empty art slots from the providers
// serving the release-group rung, and marks it so the walk does not repeat. It is the
// release-group twin of enrichArtistArt: this is a gather and an apply with no
// MusicBrainz round trip between them, and the one place a vacant group front is asked
// about (see enrichReleaseGroup). It asks about the halves the queue found due, the
// front, the auxiliary roles, or both in one gather, and each half settles on its own
// (model.ReleaseGroupArtBackfill).
//
// A half that gathered nothing still applies, because its marker is what stops it being
// asked again next run, unless a provider failed, when the apply records the half as owed
// so a later pass asks again. The store decides what actually lands: the queue's vacancy
// test is approximate, and the front and the per-role locks are re-checked there.
func (s *Service) enrichGroupArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (outcome, error) {
	in := model.ReleaseGroupArtBackfill{ReleaseGroupID: t.ID, PID: t.PID}
	g, front, aux := s.gatherHalves(ctx, st, Request{
		Type: TargetReleaseGroup, Force: st.artForced(t),
		Title: t.Name, Artist: t.ArtistName, MBID: t.MBID,
	}, t)
	in.Art, in.AuxArt = g.art[model.ArtRoleFront], auxArtRoles(g.art)
	in.Front, in.Aux = front, aux
	if err := s.store.ApplyReleaseGroupArtBackfill(ctx, in); err != nil {
		return outcome{}, err
	}
	if in.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(in.AuxArt)
	return outcome{matched: in.Art != nil || len(in.AuxArt) > 0, deferred: in.Front.Incomplete || in.Aux.Incomplete,
		retried: t.FrontExpired || t.AuxExpired}, nil
}

// enrichArtistArt backfills one artist's empty art slots from the providers serving the
// artist rung, and marks it so the walk does not repeat. It is the artist twin of
// enrichGroupArt and the one asker for an artist's art (see enrichArtist): it asks about
// the halves the queue found due, and each half settles on its own
// (model.ArtistArtBackfill). Only the artist bits are consulted here, which is what keeps
// a stock install, whose Cover Art Archive answers nothing for an artist, from stamping a
// permanent no-match on every artist it holds.
func (s *Service) enrichArtistArt(ctx context.Context, st *runState, res *Result, t model.EnrichTarget) (outcome, error) {
	in := model.ArtistArtBackfill{ArtistID: t.ID, PID: t.PID}
	g, front, aux := s.gatherHalves(ctx, st, Request{
		Type: TargetArtist, Force: st.artForced(t), Artist: t.Name, MBID: t.MBID,
	}, t)
	in.Art, in.AuxArt = g.art[model.ArtRoleFront], auxArtRoles(g.art)
	in.Front, in.Aux = front, aux
	if err := s.store.ApplyArtistArtBackfill(ctx, in); err != nil {
		return outcome{}, err
	}
	if in.Art != nil {
		res.ArtFetched++
	}
	res.AuxArtFetched += len(in.AuxArt)
	return outcome{matched: in.Art != nil || len(in.AuxArt) > 0, deferred: in.Front.Incomplete || in.Aux.Incomplete,
		retried: t.FrontExpired || t.AuxExpired}, nil
}

// gatherHalves asks about the halves of a backfill target's art lookup its queue found
// due: the front, the auxiliary roles, or both in one gather. It returns what came back
// and each asked half's shortfall as the half the store settles; a half not due comes
// back not asked. A due front takes the auxiliary roles its providers offer on the way
// past even when that half is not due, since the store fills them only where empty.
//
// A failed half is owed unless the queue reports it owed already, in which case this was
// its one more ask and it settles (runState.owes, decided per half here).
func (s *Service) gatherHalves(ctx context.Context, st *runState, req Request, t model.EnrichTarget) (artGather, model.ArtHalf, model.ArtHalf) {
	half := func(sf shortfall, provider string, owed bool) model.ArtHalf {
		return model.ArtHalf{Asked: true, Incomplete: sf.incomplete && !owed, Unasked: sf.unasked, Provider: provider}
	}
	var front, aux model.ArtHalf
	switch {
	case t.FrontDue:
		mode := auxOffered
		if t.AuxDue {
			mode = auxFull
		}
		g := s.gatherArt(ctx, st, req, mode)
		front = half(g.front, g.frontProvider, t.FrontOwed)
		if t.AuxDue {
			aux = half(g.aux, g.auxProvider, t.AuxOwed)
		}
		return g, front, aux
	case t.AuxDue:
		art, provider, sf := s.gatherAuxArt(ctx, st, req)
		return artGather{art: art, auxProvider: provider}, front, half(sf, provider, t.AuxOwed)
	}
	return artGather{}, front, aux
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
// At the artist rung the capability is CapArtistAuxArt (artCaps).
//
// The front role is dropped. A backfill calls this for a target whose front half is not
// due, so offering one would put a second writer on a decided question. Every accepted
// image is stamped with the supplying provider, as in gatherArt. A provider error is
// skipped past and reported as a shortfall when a role is still empty at the end.
//
// The loop does stop once every auxiliary role is held, which is gatherArt's stop at
// the front winner applied to a full set: a provider consulted past that point can only
// have its images dropped, after downloading them.
func (s *Service) gatherAuxArt(ctx context.Context, st *runState, req Request) (map[model.ArtRole]*model.ArtImage, string, shortfall) {
	_, auxCap := artCaps(req.Type)
	req.Want = auxCap
	roles := auxRolesAt(req.Type)
	need := len(roles)
	var out map[model.ArtRole]*model.ArtImage
	var provider string
	failed, skipped := false, false
	for _, p := range st.providers {
		if !capabilitiesAt(p, req.Type).Has(auxCap) {
			continue
		}
		if st.tripped[p.Name()] {
			skipped = true
			continue
		}
		cand, err := s.callProvider(ctx, st, p, req)
		if err != nil {
			failed = true
			continue
		}
		if cand == nil {
			continue
		}
		for role, img := range cand.Art {
			if !slices.Contains(roles, role) || img == nil || len(img.Data) == 0 {
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
	open := len(out) < need
	return out, provider, shortfall{incomplete: failed && open, unasked: skipped && open}
}

// albumMatch is what one album's tier ladder decided. Edition separates the descriptive
// medium/country evidence from a printed identifier, since only the former takes the
// edition provider marker. Skip means no tier reached a verdict for a transient reason,
// which is the release match's incomplete answer: the caller fills nothing and drops a
// standing marker rather than record a no-match.
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
func (s *Service) enrichBook(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	enr := model.BookEnrichment{BookItemID: t.ID, PID: t.PID}
	if t.MBID != "" {
		r, err := s.mb.lookupRelease(ctx, st.forced(), t.MBID)
		if err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
			return outcome{}, err
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
		return outcome{}, err
	}
	return outcome{matched: enr.Matched}, nil
}

// enrichTrackFields fills one track's empty scalar fields from the providers advertising
// CapFields and marks it so the walk does not repeat. The rung is the request type: a
// recording target's answer lands on this one item, which is what separates it from the
// album walk, whose year fans across every member.
func (s *Service) enrichTrackFields(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	in := model.ItemFieldsEnrichment{ItemID: t.ID, PID: t.PID}
	fields, providers, sf := s.gatherFields(ctx, st, Request{
		Type: TargetRecording, Force: st.forced(), Want: CapFields,
		Title: t.Name, Artist: t.ArtistName, Album: t.Album,
		DurationSec: t.DurationSec, ISRC: t.ISRC, MBID: t.MBID,
	}, CapFields, model.EnrichFillFields(model.KindTrack))
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	in.Incomplete, in.Unasked = st.owes(sf.incomplete), sf.unasked
	if err := s.store.ApplyItemFields(ctx, in); err != nil {
		return outcome{}, err
	}
	return outcome{matched: in.Matched, deferred: in.Incomplete}, nil
}

// enrichBookFields is the book twin, gated by CapBookMeta so the book providers an
// embedder registers are actually asked: before this walk existed, enrichBook consulted
// only the MusicBrainz spine and no CapBookMeta provider was ever dispatched to.
//
// The dedicated Publisher/ASIN/ISBN slots fold in as fallbacks for their own keys, so a
// provider written against those alone contributes without changing.
func (s *Service) enrichBookFields(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	in := model.ItemFieldsEnrichment{ItemID: t.ID, PID: t.PID}
	fields, providers, sf := s.gatherFields(ctx, st, Request{
		Type: TargetBook, Force: st.forced(), Want: CapBookMeta,
		Title: t.Name, Artist: t.ArtistName,
		ASIN: t.ASIN, ISBN: t.ISBN, MBID: t.MBID,
	}, CapBookMeta, model.EnrichFillFields(model.KindBook))
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	in.Incomplete, in.Unasked = st.owes(sf.incomplete), sf.unasked
	if err := s.store.ApplyItemFields(ctx, in); err != nil {
		return outcome{}, err
	}
	return outcome{matched: in.Matched, deferred: in.Incomplete}, nil
}

// enrichAlbumFields fills one album's empty label and year from the providers advertising
// CapFields. The rung is the request type: a release target's answer lands on the album
// row and, for year, on every member at once, which is what separates it from the
// recording walk above.
func (s *Service) enrichAlbumFields(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	in := model.AlbumFieldsEnrichment{AlbumID: t.ID, PID: t.PID}
	fields, providers, sf := s.gatherFields(ctx, st, Request{
		Type: TargetRelease, Force: st.forced(), Want: CapFields,
		Title: t.Name, Artist: t.ArtistName, MBID: t.MBID,
		Barcode: t.Barcode, CatalogNumber: t.CatalogNumber,
	}, CapFields, model.AlbumFillFields())
	if len(fields) > 0 {
		in.Matched, in.Fields, in.Providers = true, fields, providers
		in.Provider = firstFieldProvider(providers)
	}
	in.Incomplete, in.Unasked = st.owes(sf.incomplete), sf.unasked
	if err := s.store.ApplyAlbumFields(ctx, in); err != nil {
		return outcome{}, err
	}
	return outcome{matched: in.Matched, deferred: in.Incomplete}, nil
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
func (s *Service) gatherFields(ctx context.Context, st *runState, req Request, want Capability, allowed map[string]bool) (values, providers map[string]string, sf shortfall) {
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
	failed, skipped := false, false
	for _, p := range st.providers {
		if !capabilitiesAt(p, req.Type).Has(want) {
			continue
		}
		if st.tripped[p.Name()] {
			skipped = true
			continue
		}
		cand, err := s.callProvider(ctx, st, p, req)
		if err != nil {
			failed = true
			continue
		}
		if cand == nil {
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
	open := len(values) < len(allowed)
	sf.incomplete, sf.unasked = failed && open, skipped && open
	return values, providers, sf
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

// enrichLyrics fills one track's lyrics from the first lyrics provider to answer, in the
// pass's list order (the fixed list asks an injected provider before LRCLIB). A provider
// error is logged and skipped, and only the store write can abort. A no-match still
// records the marker so the track is not re-queried every run; a failure with nothing
// found leaves the track queued for the next pass instead.
func (s *Service) enrichLyrics(ctx context.Context, st *runState, t model.EnrichTarget) (outcome, error) {
	req := Request{
		Type: TargetRecording, Force: st.forced(), Want: CapLyrics,
		Title: t.Name, Artist: t.ArtistName, Album: t.Album, DurationSec: t.DurationSec,
	}
	var got *model.Lyrics
	var provider string
	failed, skipped := false, false
	for _, p := range st.providers {
		if !capabilitiesAt(p, req.Type).Has(CapLyrics) {
			continue
		}
		if st.tripped[p.Name()] {
			skipped = true
			continue
		}
		cand, err := s.callProvider(ctx, st, p, req)
		if err != nil {
			failed = true
			continue
		}
		if cand == nil || !cand.Lyrics.HasContent() {
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
	in := model.LyricsEnrichment{ItemID: t.ID, PID: t.PID, Matched: got != nil, Lyrics: got, Provider: provider,
		Incomplete: st.owes(failed && got == nil),
		Unasked:    skipped && got == nil,
	}
	if err := s.store.ApplyLyricsEnrichment(ctx, in); err != nil {
		return outcome{}, err
	}
	return outcome{matched: in.Matched, deferred: in.Incomplete}, nil
}

// genreCandidate is one genre display name and the provider that supplied it, used to
// attribute the display-primary genre to a provider for field provenance.
type genreCandidate struct {
	name     string
	provider string
}

// gatherGenres merges genres from the genre providers and the MusicBrainz baseline
// into one deduped union in the pass's list order: the baseline enters at the
// MusicBrainz entry's place (the fixed list puts it after the injected providers and
// ahead of ListenBrainz), and a list without that entry leaves it out. Every baseline
// genre that enters is kept, and only the non-MusicBrainz additions are capped, so a
// provider ranked ahead can never evict an authoritative MB genre. It returns the merged
// display names and the provider that supplied the display-primary genre (for field
// provenance), "" when nothing was found.
func (s *Service) gatherGenres(ctx context.Context, st *runState, rg *mbReleaseGroup, mbBaseline []string) ([]string, string, shortfall) {
	req := Request{
		Type: TargetReleaseGroup, Force: st.forced(), Want: CapGenres,
		Title: rg.Title, Artist: releaseGroupArtistName(rg), MBID: rg.ID,
	}
	var cands []genreCandidate
	var sf shortfall
	for _, p := range st.providers {
		if p.Name() == ProviderMusicBrainz {
			for _, g := range mbBaseline {
				cands = append(cands, genreCandidate{name: g, provider: ProviderMusicBrainz})
			}
			continue
		}
		if !capabilitiesAt(p, req.Type).Has(CapGenres) {
			continue
		}
		if st.tripped[p.Name()] {
			sf.unasked = true
			continue
		}
		cand, err := s.callProvider(ctx, st, p, req)
		if err != nil {
			// A merge has no full set to measure against, so any failure leaves it short.
			sf.incomplete = true
			continue
		}
		if cand == nil {
			continue
		}
		for _, g := range cand.Genres {
			cands = append(cands, genreCandidate{name: g, provider: p.Name()})
		}
	}

	seen := make(map[string]bool, len(cands))
	var names []string
	var primary string
	nonMB := 0
	for _, c := range cands {
		isMB := c.provider == ProviderMusicBrainz
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
	return names, primary, sf
}

// gatherArt returns the first offered image per art role, in the pass's list order (which
// is Config.Providers ahead of the Cover Art Archive unless ProviderList reorders it),
// plus the name of the first provider to contribute one, which is what a marker credits
// at the rungs that write one.
//
// A cover provider is asked under CapCover and every role it offers is taken, auxiliary
// ones included, so a provider that has always served them under that one capability
// keeps working unchanged. Unless aux is auxOffered, a provider advertising CapAuxArt
// without CapCover is asked too, under that capability, and only its non-front roles
// are taken, the same front-drop gatherAuxArt applies. A provider with both capabilities
// is asked once: under both when the walk asks for the auxiliary roles too, so one that
// honors Want answers both halves, and under CapCover alone otherwise. At the artist rung
// the pair is CapArtistFront and CapArtistAuxArt (artCaps), and a provider advertising
// CapArtistArt holds both.
//
// req names the rung: a release group, or the specific release an album was
// matched to. Routing both through the provider list rather than reaching for the
// built-in CAA directly is what lets an embedder's cover provider serve either one,
// and keeps the documented priority order intact. A provider error or a missing cover is
// skipped, never aborting the run; an error leaving a half open is reported as that
// half's shortfall.
//
// Per consulted provider the offered roles merge first-offer-wins per role (nil
// images, empty data, and roles the rung does not carry are skipped). A provider is
// asked only for something it could still contribute, and under the capability that
// names it, so a provider whose every slot is held is never called.
//
// aux decides what the gather does about the auxiliary roles (see artAux). Short of
// auxFull it stops once the front lands: providers after the front winner are not
// consulted, which preserves the pre-role call cadence and avoids extra full-cover
// downloads such as CAA's up-to-24MiB fetch, so aux coverage there is opportunistic.
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
func (s *Service) gatherArt(ctx context.Context, st *runState, req Request, aux artAux) artGather {
	frontCap, auxCap := artCaps(req.Type)
	// Auxiliary roles no provider in the pass serves at this rung are nobody's to fill, so
	// leaving them open is no shortfall.
	wantAux := aux == auxFull && hasCapabilityAt(st.providers, req.Type, auxCap)
	// Stamped on the value parameter, so every caller gets it without repeating it.
	req.Want = frontCap
	auxReq := req
	auxReq.Want = auxCap
	auxRoles := auxRolesAt(req.Type)
	auxNeed := len(auxRoles)
	var out map[model.ArtRole]*model.ArtImage
	var frontProvider, auxProvider string
	fromGroup := false
	auxHeld := 0
	failedFront, failedAux := false, false
	var skipped []Provider
	for _, p := range st.providers {
		caps := capabilitiesAt(p, req.Type)
		// A provider is consulted only for something it could still contribute, and
		// under the capability that names it: the front while the front is open, else
		// the auxiliary roles while any is.
		askFront := caps.Has(frontCap) && !fromGroup && out[model.ArtRoleFront] == nil
		askAux := aux != auxOffered && caps.Has(auxCap) && auxHeld < auxNeed
		if !askFront && !askAux {
			continue
		}
		if st.tripped[p.Name()] {
			skipped = append(skipped, p)
			continue
		}
		ask := auxReq
		if askFront {
			ask = req
			// One call asking for both halves says so, or a provider honoring Want would
			// leave out the auxiliary roles this walk is the asker for.
			if askAux {
				ask.Want = frontCap | auxCap
			}
		}
		cand, err := s.callProvider(ctx, st, p, ask)
		if err != nil {
			failedFront = failedFront || askFront
			failedAux = failedAux || (wantAux && askAux)
			continue
		}
		if cand == nil {
			continue
		}
		// The front is settled by the picture the caller already holds, so nothing is
		// stored for it and no later provider is asked about it. A provider that answers
		// both ways at once is held to the documented contract rather than driving the
		// store's two front paths against each other: the reuse wins and the bytes are
		// dropped.
		fromGroupHere := askFront && cand.FrontIsGroupFront
		if fromGroupHere {
			fromGroup, frontProvider = true, p.Name()
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
			if (role != model.ArtRoleFront && !slices.Contains(auxRoles, role)) || img == nil || len(img.Data) == 0 {
				continue
			}
			if out[role] != nil {
				continue
			}
			img.Source, img.Provider = model.SourceEnrichment, p.Name()
			if out == nil {
				out = make(map[model.ArtRole]*model.ArtImage, len(offered))
			}
			out[role] = img
			switch {
			case role == model.ArtRoleFront:
				frontProvider = p.Name()
			case auxProvider == "":
				auxProvider = p.Name()
				auxHeld++
			default:
				auxHeld++
			}
		}
		if !wantAux && (fromGroup || out[model.ArtRoleFront] != nil) {
			break
		}
	}
	frontSettled := fromGroup || out[model.ArtRoleFront] != nil
	auxOpen := wantAux && auxHeld < auxNeed
	servesFront := func(p Provider) bool { return capabilitiesAt(p, req.Type).Has(frontCap) }
	servesAux := func(p Provider) bool { return capabilitiesAt(p, req.Type).Has(auxCap) }
	frontUnasked := !frontSettled && skippedServing(skipped, servesFront)
	auxUnasked := auxOpen && skippedServing(skipped, servesAux)
	return artGather{art: out, frontProvider: frontProvider, auxProvider: auxProvider, fromGroup: fromGroup,
		front: shortfall{incomplete: failedFront && !frontSettled, unasked: frontUnasked},
		aux:   shortfall{incomplete: failedAux && auxOpen, unasked: auxUnasked}}
}

// artAux is what a gather does about the auxiliary roles beside the front.
type artAux int

const (
	// auxOffered keeps what the providers asked for the front offer beside it and asks
	// nobody else. It is for a walk whose rung has a backfill asking the auxiliary
	// providers itself: the release-group refresh, and a backfill walk when its auxiliary
	// half is not due.
	auxOffered artAux = iota
	// auxFull keeps asking, under the auxiliary capability, until every auxiliary role is
	// held. The art backfills use it when their auxiliary half is due, because they are
	// that half's asker: a matched marker is durable, so an auxiliary slot skipped there
	// is skipped for good.
	auxFull
)

// artGather is what gatherArt collected: the first offered image per role, the provider
// behind the front (its image or the group's picture) and the first one behind an
// auxiliary role, whether the front was answered with the group's picture rather than
// bytes, and each half's shortfall, which the art backfills settle apart.
type artGather struct {
	art                        map[model.ArtRole]*model.ArtImage
	frontProvider, auxProvider string
	fromGroup                  bool
	front, aux                 shortfall
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
// a slow optional provider cannot stall the pass. An error is logged and returned for
// the caller's gather to count as a shortfall, which leaves the lookup owed rather
// than marked. Only the identity spine (mb/aid) aborts a run; every port provider is
// optional. Run cancellation still propagates, because the next store write (or the
// runSweep loop's context check) observes it.
func (s *Service) callProvider(ctx context.Context, st *runState, p Provider, req Request) (*Candidate, error) {
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	cand, err := p.Enrich(cctx, req)
	if err != nil {
		// The run is aborting, which says nothing about the provider.
		if ctx.Err() != nil {
			return nil, err
		}
		s.log.Warn("enrich provider failed", "provider", p.Name(), "target", req.Type, "err", err)
		st.failures[p.Name()]++
		if st.failures[p.Name()] == providerTripAfter {
			st.tripped[p.Name()] = true
			s.log.Warn("enrich provider failed "+strconv.Itoa(providerTripAfter)+" times in a row; leaving it out for the rest of this pass",
				"provider", p.Name())
		}
		return nil, err
	}
	delete(st.failures, p.Name())
	return cand, nil
}

// artSlots reports which art vacancies the providers in a pass's list could actually
// fill at one rung, for the backfill walking it. A slot no provider serves there is left
// out, so a stock install never marks a target for an auxiliary vacancy nothing could
// have answered, and an install with neither capability at that rung skips the phase
// and its count outright.
func artSlots(providers []Provider, rung TargetType) model.ArtSlots {
	front, aux := artCaps(rung)
	return model.ArtSlots{
		Front: hasCapabilityAt(providers, rung, front),
		Aux:   hasCapabilityAt(providers, rung, aux),
	}
}

// auxRolesAt returns the auxiliary roles the entity at one rung carries
// (model.AuxArtRolesOf), which is what a gather asks for and keeps there: the background
// alone for an artist, and every auxiliary role at the release rungs.
func auxRolesAt(rung TargetType) []model.ArtRole {
	if rung == TargetArtist {
		return model.AuxArtRolesOf(model.ArtArtist)
	}
	return model.AuxArtRoles()
}

// artCaps names the capabilities consulted at one rung for the front and for the
// auxiliary roles: CapArtistFront and CapArtistAuxArt at the artist rung, CapCover and
// CapAuxArt everywhere else. The artist rung has its own pair because a release art
// provider that declares no rungs is taken to serve every rung, and consulting its
// capabilities there would ask it about every artist.
func artCaps(rung TargetType) (front, aux Capability) {
	if rung == TargetArtist {
		return CapArtistFront, CapArtistAuxArt
	}
	return CapCover, CapAuxArt
}

// warnReleaseArtForArtists logs, once per provider for the Service's life, each provider
// in the pass offering CapCover or CapAuxArt at the artist rung without the artist bit for
// that half, one declaring no rungs included. The artist rung consults neither, so a
// provider that used to serve artist art under them would otherwise stop being asked
// without a word. Declaring its rungs through TargetCapabilities, or advertising the
// artist bits, silences it.
func (s *Service) warnReleaseArtForArtists(providers []Provider) {
	for _, p := range providers {
		at := capabilitiesAt(p, TargetArtist)
		if (!at.Has(CapCover) || at.Has(CapArtistFront)) && (!at.Has(CapAuxArt) || at.Has(CapArtistAuxArt)) {
			continue
		}
		if _, warned := s.artistRungWarned.LoadOrStore(p.Name(), true); warned {
			continue
		}
		s.log.Warn("enrichment: a provider offers CapCover or CapAuxArt at the artist rung, which consults only CapArtistFront and CapArtistAuxArt",
			"provider", p.Name())
	}
}

// capabilitiesAt reports what p serves for one target type: its per-target declaration
// narrowed by its union, so a declaration cannot widen what the provider advertises, else
// the union alone.
func capabilitiesAt(p Provider, t TargetType) Capability {
	if tc, ok := p.(TargetCapabilities); ok {
		return tc.CapabilitiesAt(t) & p.Capabilities()
	}
	return p.Capabilities()
}

// hasCapabilityAt reports whether any provider in the pass's list serves c for t. A
// phase is gated on its capability at its own rung, which is what keeps an install whose
// only auxiliary-art provider is keyed on release groups from walking every identified
// album's empty slots for a certain miss.
func hasCapabilityAt(providers []Provider, t TargetType, c Capability) bool {
	for _, p := range providers {
		if capabilitiesAt(p, t).Has(c) {
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
