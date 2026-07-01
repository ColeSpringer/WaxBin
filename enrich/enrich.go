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

	"github.com/colespringer/waxbin/art"
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
// set, still advances and terminates.
type Store interface {
	ArtistsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int) ([]model.EnrichTarget, error)
	// ReleaseGroupsNeedingEnrichment populates each target's representative file only
	// when includeRepFile is set (the AcoustID fallback needs it), so the correlated
	// lookup is skipped on the common path where AcoustID is off.
	ReleaseGroupsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int, includeRepFile bool) ([]model.EnrichTarget, error)
	BooksNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int) ([]model.EnrichTarget, error)
	CountEntitiesNeedingEnrichment(ctx context.Context, force bool) (int, error)

	ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error
	ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error
	ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error

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

	// Network policy applied to the shared netsafe client.
	BlockPrivateIPs bool
	Timeout         time.Duration
	// MinRequestInterval is the per-host spacing (MusicBrainz requires >= 1s). Zero
	// takes the 1s default; tests set a tiny value.
	MinRequestInterval time.Duration

	// Endpoint overrides. Empty fields default to the public services.
	MusicBrainzBaseURL string
	CoverArtBaseURL    string
	AcoustIDBaseURL    string
}

const (
	defaultUserAgentBase = "WaxBin/1.0 (+https://github.com/colespringer/waxbin)"
	defaultMBBaseURL     = "https://musicbrainz.org/ws/2"
	defaultCAABaseURL    = "https://coverartarchive.org"
	defaultAcoustBaseURL = "https://api.acoustid.org"
	defaultMBInterval    = time.Second // MusicBrainz: at most 1 request/second
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

	mb  *musicBrainz
	caa *coverArt
	aid *acoustID
}

// New builds an enrichment service from cfg, constructing the shared netsafe client
// with the contact User-Agent and MusicBrainz pacing.
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
		caa:   &coverArt{client: client, baseURL: baseOr(cfg.CoverArtBaseURL, defaultCAABaseURL)},
		aid:   &acoustID{client: client, baseURL: baseOr(cfg.AcoustIDBaseURL, defaultAcoustBaseURL), key: cfg.AcoustIDKey},
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
}

// Result tallies an enrichment run.
type Result struct {
	ArtistsEnriched       int
	ArtistsMatched        int
	ReleaseGroupsEnriched int
	ReleaseGroupsMatched  int
	BooksEnriched         int
	BooksMatched          int
	ArtFetched            int
}

func (r *Result) total() int { return r.ArtistsEnriched + r.ReleaseGroupsEnriched + r.BooksEnriched }

// Heartbeat reports progress; it may be nil.
type Heartbeat func(progress float64, msg string) error

// Run enriches artists, then release groups, then books, until each set is
// exhausted or the limit is reached. It is resumable: each entity is committed
// independently and marked, so an interrupted run resumes where it left off. A
// per-entity miss marks the entity looked-up-with-no-match and continues; a network
// failure (offline, cancellation) aborts with the underlying error rather than
// hammering an unreachable service.
func (s *Service) Run(ctx context.Context, opts RunOptions, hb Heartbeat) (*Result, error) {
	const op = "enrich.Run"
	res := &Result{}
	if !s.Enabled() {
		return res, waxerr.New(waxerr.CodeUnsupported, op,
			"enrichment needs a MusicBrainz contact (set enrichment.contact)")
	}
	st := &runState{force: opts.Force}

	// The total is only needed to report a heartbeat ratio, so skip the three
	// counting queries entirely when there is no heartbeat.
	var total int
	if hb != nil {
		n, err := s.store.CountEntitiesNeedingEnrichment(ctx, opts.Force)
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

	// Artists first: a release group's artist credit is more useful once its primary
	// artist carries an MBID.
	phases := []phase{
		{
			label: "artist", enriched: &res.ArtistsEnriched, matched: &res.ArtistsMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ArtistsNeedingEnrichment(ctx, st.force, after, lim)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichArtist(ctx, st, t) },
		},
		{
			label: "album", enriched: &res.ReleaseGroupsEnriched, matched: &res.ReleaseGroupsMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.ReleaseGroupsNeedingEnrichment(ctx, st.force, after, lim, s.acoustEnabled())
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) {
				return s.enrichReleaseGroup(ctx, st, res, t)
			},
		},
		{
			label: "book", enriched: &res.BooksEnriched, matched: &res.BooksMatched,
			fetch: func(ctx context.Context, after int64, lim int) ([]model.EnrichTarget, error) {
				return s.store.BooksNeedingEnrichment(ctx, st.force, after, lim)
			},
			enrich: func(ctx context.Context, t model.EnrichTarget) (bool, error) { return s.enrichBook(ctx, st, t) },
		},
	}
	for i := range phases {
		if err := s.runPhase(ctx, res, phases[i], beat, remaining, limitReached); err != nil {
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
// continues.
func (s *Service) runPhase(ctx context.Context, res *Result, p phase, beat func(string) error, remaining func() int, limitReached func() bool) error {
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
		enr.Genres = genreNames(rg.Genres)
		if s.cfg.FetchCoverArt {
			enr.Art = s.fetchCover(ctx, rg.ID) // best-effort; never aborts the run
		}
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

// fetchCover downloads and decodes a release group's front cover. It is best-effort:
// any failure (a 404 "no cover", a transient network error, or an undecodable image)
// logs and returns nil instead of aborting the run, since cover art is optional and a
// skipped cover is re-fetchable with --force. MusicBrainz failures abort the pass; a
// Cover Art Archive failure never does.
func (s *Service) fetchCover(ctx context.Context, mbid string) *model.ArtImage {
	data, err := s.caa.frontCover(ctx, mbid)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			s.log.Debug("no cover art for release group", "mbid", mbid)
		} else {
			s.log.Warn("cover art fetch failed; skipping cover", "mbid", mbid, "err", err)
		}
		return nil
	}
	img := &model.ArtImage{Data: data, Hash: art.Hash(data)}
	format, w, h, err := art.Probe(data)
	if err != nil {
		s.log.Debug("cover art undecodable", "mbid", mbid, "err", err)
		return nil
	}
	img.Format, img.Width, img.Height = format, w, h
	return img
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
