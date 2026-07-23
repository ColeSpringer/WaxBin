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
// It is the "metadata brain" enrichment half; the WaxLabel tag adapter lives in
// package meta. This package defines its own Store port (implemented by
// store/sqlite) so it depends on the domain model, not on SQLite.
package enrich

import (
	"context"
	"io"
	"log/slog"
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
type Store interface {
	ArtistsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ReleaseGroupsNeedingEnrichment populates each target's representative file only
	// when includeRepFile is set (the AcoustID fallback needs it), so the correlated
	// lookup is skipped on the common path where AcoustID is off.
	ReleaseGroupsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int, includeRepFile bool, ids []int64) ([]model.EnrichTarget, error)
	BooksNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// ItemsNeedingLyrics returns the next keyset page of tracks that carry no lyrics
	// yet (and, unless force, have not already been looked up), each with the title,
	// artist, album, and duration a lyrics provider keys on.
	ItemsNeedingLyrics(ctx context.Context, force bool, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error)
	// CountEntitiesNeedingEnrichment mirrors the phases a run would execute: a nil
	// scope counts everything, a scoped count covers only the scoped ids, and a
	// phase the scoped run skips (an empty id list) contributes zero.
	CountEntitiesNeedingEnrichment(ctx context.Context, force bool, includeLyrics bool, scope *model.EnrichScope) (int, error)

	ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error
	ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error
	ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error
	// ApplyLyricsEnrichment attaches a track's resolved lyrics, only when it has none
	// (fill-when-empty), and records the per-recording enrichment marker.
	ApplyLyricsEnrichment(ctx context.Context, in model.LyricsEnrichment) error

	EnrichmentCacheGet(ctx context.Context, key string) ([]byte, bool, error)
	EnrichmentCachePut(ctx context.Context, key string, payload []byte) error
	EnrichmentCoverage(ctx context.Context) (model.EnrichmentCoverage, error)
}

// Config tunes the enrichment service: the mandatory MusicBrainz contact, the
// network policy, provider endpoints (overridable for tests), the optional
// AcoustID key, and toggles. Enrichment is disabled unless a contact is set, since
// MusicBrainz requires an identifying User-Agent.
type Config struct {
	// Contact is the operator contact (email or URL) folded into the User-Agent, as
	// MusicBrainz requires. When empty (and UserAgent is empty) enrichment is disabled.
	Contact string
	// UserAgent overrides the full User-Agent string; when empty one is built from
	// the app name and Contact.
	UserAgent string
	// AcoustIDKey enables the AcoustID fingerprint fallback (requires fpcalc). Empty
	// disables it.
	AcoustIDKey string
	// FetchCoverArt enables Cover Art Archive lookups (default enabled when a contact
	// is set; the facade sets it explicitly).
	FetchCoverArt bool
	// FetchLyrics enables the LRCLIB lyrics provider (default enabled when a contact
	// is set; the facade sets it explicitly). Lyrics are filled only for a track that
	// has none.
	FetchLyrics bool
	// FetchCommunityGenres enables the ListenBrainz community-genre provider (default
	// enabled when a contact is set; the facade sets it explicitly). MusicBrainz genres
	// always flow through the identity spine regardless of this toggle.
	FetchCommunityGenres bool

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
	s.providers = append(s.providers, cfg.Providers...)
	s.numInjected = len(s.providers)

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
			caa: &coverArt{client: client, baseURL: baseOr(cfg.CoverArtBaseURL, defaultCAABaseURL)},
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

// Enabled reports whether enrichment is configured. MusicBrainz requires an
// identifying contact, so without one the pass refuses to run rather than send
// requests the service would reject.
func (s *Service) Enabled() bool {
	return s.cfg.Contact != "" || s.cfg.UserAgent != ""
}

// acoustEnabled reports whether the AcoustID fingerprint fallback is usable: a key
// is set and fpcalc is present to produce a Chromaprint fingerprint.
func (s *Service) acoustEnabled() bool { return s.cfg.AcoustIDKey != "" && s.caps.Fpcalc }

// RunOptions controls one enrichment pass.
type RunOptions struct {
	Force bool // re-enrich already-enriched entities
	Limit int  // cap on entities processed (0 = all needing enrichment)
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
	BooksEnriched         int
	BooksMatched          int
	LyricsEnriched        int
	LyricsMatched         int
	ArtFetched            int
}

func (r *Result) total() int {
	return r.ArtistsEnriched + r.ReleaseGroupsEnriched + r.BooksEnriched + r.LyricsEnriched
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
		return res, waxerr.New(waxerr.CodeUnsupported, op,
			"enrichment needs a MusicBrainz contact (set enrichment.contact)")
	}
	// A scoped run implies force: the caller pointed at these targets, so markers
	// and cached provider responses are bypassed and the lookup actually re-runs.
	scope := opts.Scope
	st := &runState{force: opts.Force || scope != nil}
	var artistIDs, rgIDs, bookIDs, lyricsIDs []int64
	if scope != nil {
		artistIDs, rgIDs, bookIDs, lyricsIDs = scope.ArtistIDs, scope.ReleaseGroupIDs, scope.BookItemIDs, scope.LyricsItemIDs
	}

	// The total is only needed to report a heartbeat ratio, so skip the three
	// counting queries entirely when there is no heartbeat.
	var total int
	if hb != nil {
		// includeLyrics and the scope must match the phase list below, which adds the
		// lyrics phase only when a lyrics-capable provider is registered and skips a
		// phase the scope leaves empty, so the denominator counts exactly the work
		// that will run.
		n, err := s.store.CountEntitiesNeedingEnrichment(ctx, st.force, s.hasCapability(CapLyrics), scope)
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

	// A phase runs when the pass is unscoped or the scope names targets for it; a
	// scoped phase with nothing to do is skipped outright (no fetch, no count).
	phaseRuns := func(ids []int64) bool { return scope == nil || len(ids) > 0 }

	// Artists first: a release group's artist credit is more useful once its primary
	// artist carries an MBID.
	var phases []phase
	if phaseRuns(artistIDs) {
		phases = append(phases, phase{
			label: "artist", enriched: &res.ArtistsEnriched, matched: &res.ArtistsMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ArtistsNeedingEnrichment(ctx, st.force, after, lim, artistIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichArtist(ctx, st, t) },
		})
	}
	if phaseRuns(rgIDs) {
		phases = append(phases, phase{
			label: "album", enriched: &res.ReleaseGroupsEnriched, matched: &res.ReleaseGroupsMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingEnrichment(ctx, st.force, after, lim, s.acoustEnabled(), rgIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichReleaseGroup(ctx, st, res, t)
			},
		})
	}
	if phaseRuns(bookIDs) {
		phases = append(phases, phase{
			label: "book", enriched: &res.BooksEnriched, matched: &res.BooksMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.BooksNeedingEnrichment(ctx, st.force, after, lim, bookIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichBook(ctx, st, t) },
		})
	}
	// Lyrics are a per-recording phase, run only when a lyrics-capable provider is
	// registered so no marker is written for tracks nothing could ever fill. It walks
	// tracks that carry no lyrics yet, filling from LRCLIB (or an injected provider).
	if s.hasCapability(CapLyrics) && phaseRuns(lyricsIDs) {
		phases = append(phases, phase{
			label: "lyrics", enriched: &res.LyricsEnriched, matched: &res.LyricsMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ItemsNeedingLyrics(ctx, st.force, after, lim, lyricsIDs)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichLyrics(ctx, st, t) },
		})
	}
	for i := range phases {
		if err := s.runPhase(ctx, phases[i], beat, remaining, limitReached); err != nil {
			return res, err
		}
	}
	_ = beat("enriched " + strconv.Itoa(res.total()) + " entities")
	return res, nil
}

// runState is per-run mutable state, allocated fresh each Run so the Service stays
// safe for concurrent callers (no shared field is mutated). force bypasses cached
// provider reads; acoustOff is set when the AcoustID fallback hits a (usually
// permanent) error, disabling it for the rest of the run.
type runState struct {
	force     bool
	acoustOff bool
}

// phase describes one entity type's enrichment for the shared keyset runner: how to
// fetch a page, how to enrich one target (returning whether a provider matched), and
// the counters to bump.
type phase struct {
	label    string
	enriched *int
	matched  *int
	fetch    func(ctx context.Context, afterID int64, limit int) ([]model.EnrichTarget, error)
	enrich   func(ctx context.Context, t model.EnrichTarget) (matched bool, err error)
}

// runPhase walks one entity type in keyset pages, enriching each target. It is the
// one loop behind artists, release groups, and books. A MusicBrainz or cancellation
// error aborts; a per-entity miss is marked by the enrich callback and the walk
// continues. Counters live on the phase (pointers into the Result), so the loop
// needs no Result of its own.
func (s *Service) runPhase(ctx context.Context, p phase, beat func(string) error, remaining func() int, limitReached func() bool) error {
	var afterID int64
	for {
		if limitReached() {
			return nil
		}
		batch, err := p.fetch(ctx, afterID, remaining())
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
			if err := beat("enriched " + p.label + " " + t.Name); err != nil {
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
func (s *Service) enrichArtist(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
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
	}
	if err := s.store.ApplyArtistEnrichment(ctx, enr); err != nil {
		return false, err
	}
	return enr.Matched, nil
}

// resolveArtist looks up an artist by MBID, or searches by name when it has none.
// A CodeNotFound on an MBID lookup (a stale/wrong id) degrades to a name search
// (searchArtist itself returns no match for an empty/symbol-only name).
func (s *Service) resolveArtist(ctx context.Context, st *runState, t model.EnrichTarget) (*mbArtist, error) {
	if t.MBID != "" {
		a, err := s.mb.lookupArtist(ctx, st.force, t.MBID)
		if err == nil {
			return a, nil
		}
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, err
		}
	}
	return s.mb.searchArtist(ctx, st.force, t.Name)
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
		// Cover: the first cover provider to answer, injected first (an embedder's
		// fanart.tv beats the built-in Cover Art Archive). Best-effort: never aborts.
		enr.Art = s.gatherCover(ctx, st, rg)
	}
	if err := s.store.ApplyReleaseGroupEnrichment(ctx, enr); err != nil {
		return false, err
	}
	if enr.Art != nil {
		res.ArtFetched++
	}
	return enr.Matched, nil
}

// resolveReleaseGroup applies the resolution ladder: MBID lookup, text search, then
// AcoustID (when enabled and not disabled this run) via a representative file's
// fingerprint.
func (s *Service) resolveReleaseGroup(ctx context.Context, st *runState, t model.EnrichTarget) (*mbReleaseGroup, error) {
	if t.MBID != "" {
		rg, err := s.mb.lookupReleaseGroup(ctx, st.force, t.MBID)
		if err == nil {
			return rg, nil
		}
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, err
		}
	}
	if t.Name != "" {
		rg, err := s.mb.searchReleaseGroup(ctx, st.force, t.Name, t.ArtistName)
		if err != nil {
			return nil, err
		}
		if rg != nil {
			return rg, nil
		}
	}
	if s.acoustEnabled() && !st.acoustOff && t.FilePath != "" {
		if mbid := s.acoustResolveReleaseGroup(ctx, st, t); mbid != "" {
			rg, err := s.mb.lookupReleaseGroup(ctx, st.force, mbid)
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

// enrichBook resolves an audiobook against a MusicBrainz release and applies its
// external identifiers and publisher. It matches only by an explicit release MBID,
// since audiobook text search throws too many false positives. Returns whether a
// provider matched.
func (s *Service) enrichBook(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	enr := model.BookEnrichment{BookItemID: t.ID, PID: t.PID}
	if t.MBID != "" {
		r, err := s.mb.lookupRelease(ctx, st.force, t.MBID)
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

// enrichLyrics fills one track's lyrics from the first lyrics provider to answer
// (injected first, then LRCLIB). A provider error is best-effort (logged, skipped);
// only the store write can abort. A no-match still records the marker so the track is
// not re-queried every run. Returns whether a provider matched.
func (s *Service) enrichLyrics(ctx context.Context, st *runState, t model.EnrichTarget) (bool, error) {
	req := Request{
		Type: TargetRecording, Force: st.force,
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
		Type: TargetReleaseGroup, Force: st.force,
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

// gatherCover returns the first non-nil cover from a cover provider, in priority
// order (injected first, then the Cover Art Archive). It passes the same identity
// hints as gatherGenres. The built-in CAA keys only on the MBID, but an injected cover
// provider (fanart.tv, a Discogs-style source) may key on the release title and artist,
// so withholding them would leave such a provider unable to match. It is best-effort: a
// provider error or a missing cover is skipped, never aborting the run.
func (s *Service) gatherCover(ctx context.Context, st *runState, rg *mbReleaseGroup) *model.ArtImage {
	req := Request{
		Type: TargetReleaseGroup, Force: st.force,
		Title: rg.Title, Artist: releaseGroupArtistName(rg), MBID: rg.ID,
	}
	for _, p := range s.providers {
		if !p.Capabilities().Has(CapCover) {
			continue
		}
		cand, err := s.callProvider(ctx, p, req)
		if err != nil || cand == nil || cand.Cover == nil {
			continue
		}
		return cand.Cover
	}
	return nil
}

// callProvider runs one candidate-provider lookup under a soft per-provider timeout so
// a slow optional provider cannot stall the pass. It is best-effort: an error is
// logged and returned for the caller to skip past. Only the identity spine (mb/aid)
// aborts a run; every port provider is optional. Run cancellation still propagates,
// because the next store write (or the runPhase loop's context check) observes it.
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
