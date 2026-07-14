package waxbin

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxbin/analyze"
	"github.com/colespringer/waxbin/audit"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/playback"
	"github.com/colespringer/waxbin/playlist"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/trash"
	"github.com/colespringer/waxbin/watch"
	"github.com/colespringer/waxbin/waxerr"
)

// The SQLite store implements the enrichment persistence port. Asserted here so a
// port/store drift is a compile error at the wiring seam.
var _ enrich.Store = (*sqlite.Store)(nil)

// The SQLite store also implements the audit persistence port.
var _ audit.Store = (*sqlite.Store)(nil)

// enrichConfig converts the config-only EnrichConfig into the enrich package's
// Config, resolving the cover-art default (on unless explicitly disabled).
func enrichConfig(c config.EnrichConfig) enrich.Config {
	return enrich.Config{
		Contact:            c.Contact,
		UserAgent:          c.UserAgent,
		AcoustIDKey:        c.AcoustIDKey,
		FetchCoverArt:      c.CoverArt == nil || *c.CoverArt,
		BlockPrivateIPs:    c.BlockPrivateIPs,
		Timeout:            time.Duration(c.TimeoutSeconds) * time.Second,
		MusicBrainzBaseURL: c.MusicBrainzBaseURL,
		CoverArtBaseURL:    c.CoverArtBaseURL,
		AcoustIDBaseURL:    c.AcoustIDBaseURL,
	}
}

// Library is the public handle to a WaxBin catalog. It is safe for concurrent
// use. A read-only Library refuses mutating operations.
type Library struct {
	store     *sqlite.Store
	jobs      *jobs.Manager
	scanner   *scan.Scanner
	organizer *organize.Organizer
	profiles  *organize.ProfileSet
	trasher   *trash.Service
	importer  *inbox.Service
	analyzer  *analyze.Analyzer
	playback  *playback.Service
	playlists *playlist.Service
	podcasts  *podcast.Service
	enricher  *enrich.Service
	auditor   *audit.Auditor
	decoders  *decode.Registry
	log       *slog.Logger
	opts      Options
}

// Open opens (creating if needed) the catalog and wires the subsystems. A
// read-write open acquires the write lock, migrates, reclaims orphaned jobs, and
// ensures the configured roots; a read-only open does none of those.
func Open(ctx context.Context, opts Options) (*Library, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	owner := opts.WriteOwner
	if owner == "" {
		owner = defaultOwner()
	}

	// Validate and normalize roots here so embedders get the same root isolation
	// as the CLI. Overlapping roots could otherwise let organize move files from
	// an in-place library.
	cfg := &config.Config{DBPath: opts.DBPath, Roots: opts.Roots}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts.DBPath, opts.Roots = cfg.DBPath, cfg.Roots

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path:          opts.DBPath,
		ReadOnly:      opts.ReadOnly,
		Owner:         owner,
		IPCSocket:     opts.IPCSocket,
		Logger:        log,
		BusyTimeoutMS: opts.BusyTimeoutMS,
		CacheSizeKB:   opts.CacheSizeKB,
		MmapSizeBytes: opts.MmapSizeBytes,
		ReadPoolSize:  opts.ReadPoolSize,
	})
	if err != nil {
		return nil, err
	}

	profiles, err := organize.NewProfileSet(toOrganizeProfiles(opts.Profiles))
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	decoders := decode.Default()
	l := &Library{
		store:     st,
		jobs:      jobs.NewManager(st, owner, log),
		scanner:   scan.New(st, meta.NewReader(), log),
		organizer: organize.New(st, meta.NewWriter(), log),
		profiles:  profiles,
		trasher:   trash.New(st, log),
		analyzer:  analyze.New(st, decoders, log),
		playback:  playback.New(st),
		playlists: playlist.New(st),
		podcasts: podcast.New(st, meta.NewReader(), podcast.Config{
			Dir:               opts.Podcasts.Dir,
			UserAgent:         opts.Podcasts.UserAgent,
			BlockPrivateIPs:   opts.Podcasts.BlockPrivateIPs,
			Timeout:           time.Duration(opts.Podcasts.TimeoutSeconds) * time.Second,
			MaxFeedBytes:      opts.Podcasts.MaxFeedBytes,
			MaxEnclosureBytes: opts.Podcasts.MaxEnclosureBytes,
			ReserveBytes:      opts.FreeSpaceReserveBytes,
			DefaultRetention:  opts.Podcasts.DefaultRetention,
			Providers:         opts.SourceProviders,
		}, log),
		enricher: enrich.New(st, enrichConfig(opts.Enrichment), log),
		decoders: decoders,
		log:      log,
		opts:     opts,
	}
	// The importer catalogs each placed file through the scanner, so it is wired
	// after the struct is built and shares that scanner.
	l.importer = inbox.New(st, meta.NewReader(), l.scanner, log)

	// The auditor's integrity check re-hashes files (identity.ContentHash) and its
	// corrupt-audio check parses essence through a WaxLabel reader.
	auditReader := meta.NewReader()
	l.auditor = audit.New(st, identity.ContentHash, func(ctx context.Context, p string) error {
		_, err := auditReader.Read(ctx, p)
		return err
	}, log)

	if !opts.ReadOnly {
		if err := l.ensureRoots(ctx); err != nil {
			_ = st.Close()
			return nil, err
		}
	}
	return l, nil
}

// Close flushes buffered playback progress, then releases the catalog and write
// lock. The flush is best effort: shutdown should save resume positions, but a
// flush error should not block release. Read-only handles skip the flush.
func (l *Library) Close() error {
	if l.playback != nil && !l.ReadOnly() {
		_ = l.playback.Flush(context.Background())
	}
	return l.store.Close()
}

// ReadOnly reports whether the library was opened read-only.
func (l *Library) ReadOnly() bool { return l.store.ReadOnly() }

func (l *Library) ensureRoots(ctx context.Context) error {
	for _, r := range l.opts.Roots {
		if _, err := l.store.EnsureLibrary(ctx, &model.Library{
			Root:        []byte(r.Path),
			DisplayRoot: r.Path,
			Mode:        r.Mode,
			Media:       r.Media,
			Profile:     r.Profile,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Libraries lists registered library roots.
func (l *Library) Libraries(ctx context.Context) ([]*model.Library, error) {
	return l.store.Libraries(ctx)
}

// Query runs a compiled selection and returns matching item views.
func (l *Library) Query(ctx context.Context, q query.Query) ([]*model.ItemView, error) {
	return l.store.QueryItems(ctx, q)
}

// Count returns the number of items matching q.
func (l *Library) Count(ctx context.Context, q query.Query) (int, error) {
	return l.store.CountItems(ctx, q)
}

// Facet groups the items matching q by a dimension and counts each bucket. The
// CLI, OpenSubsonic adapters, and stats code use this same API, so they share
// one canonical grouping result.
func (l *Library) Facet(ctx context.Context, q query.Query, g read.GroupBy) (*read.FacetResult, error) {
	return l.store.Facet(ctx, q, g)
}

// QueryPage returns one keyset-paginated, collation-correct window of items.
// Pass an empty cursor for the first page and the returned Next cursor for each
// subsequent page; pagination is stable under concurrent mutation.
func (l *Library) QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool) (*read.Page, error) {
	return l.store.QueryPage(ctx, q, cursor, limit, desc)
}

// Browse returns one keyset-paginated window for a discovery list such as newest,
// recently-added, most-played, recently-played, random, starred, by-year,
// by-genre, or alphabetical. Play-derived lists read opt.UserPID's play_state
// (empty selects the default user). The by-year, by-genre, and random lists use
// opt.Year, opt.GenrePID, and opt.Seed respectively. Pagination is stable under
// concurrent mutation.
func (l *Library) Browse(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions) (*read.Page, error) {
	return l.store.BrowsePage(ctx, list, opt)
}

// Search runs a grouped, BM25-ranked metadata search across artists, albums, and
// tracks. Field weighting puts title hits above artist and album hits. The
// Episodes group is reserved for transcript-backed podcast search and stays empty
// until transcripts are indexed.
func (l *Library) Search(ctx context.Context, q string, opt read.SearchOptions) (*read.SearchResult, error) {
	return l.store.Search(ctx, q, opt)
}

// ResolveArt resolves cover art for an entity, walking the fallback chain (track
// -> album -> release_group -> artist -> genre) to the first level that has art. A
// non-positive size returns the original image; a positive size returns a
// thumbnail scaled to fit a square box with that maximum side (generated once and
// cached). CodeNotFound means no level in the chain has art.
func (l *Library) ResolveArt(ctx context.Context, ref model.EntityRef, size int) (*model.ArtBlob, error) {
	return l.store.ResolveArt(ctx, ref, size)
}

// GCArt reclaims orphaned art: map rows whose entity is gone, then source images
// without live map references and their cached thumbnails. It returns the source
// and thumbnail counts removed. It is the repair for the orphan counts
// VerifyDerived reports.
func (l *Library) GCArt(ctx context.Context) (sources, thumbnails int, err error) {
	return l.store.GCArt(ctx)
}

// Lyrics returns an item's structured lyrics (synced timed lines and/or an
// unsynchronized block), or CodeNotFound when it has none. Lyrics come from a
// sibling .lrc sidecar or embedded USLT/SYLT tags, captured at scan time; the
// catalog row is authoritative for reads.
func (l *Library) Lyrics(ctx context.Context, pid model.PID) (*model.Lyrics, error) {
	return l.store.LyricsByItem(ctx, pid)
}

// Get returns a single item by public id.
func (l *Library) Get(ctx context.Context, pid model.PID) (*model.ItemView, error) {
	return l.store.ItemByPID(ctx, pid)
}

// Book returns the full detail for an audiobook: subtitle, series placement,
// role-tagged contributors (author/narrator/...), backing parts in reading order,
// and chapters resolved to book-timeline offsets with the total duration.
// CodeInvalid when pid is not a book.
func (l *Library) Book(ctx context.Context, pid model.PID) (*model.BookDetail, error) {
	return l.store.BookByPID(ctx, pid)
}

// Chapters returns a book's chapters in book-timeline order. CodeInvalid when pid
// is not a book.
func (l *Library) Chapters(ctx context.Context, pid model.PID) ([]model.Chapter, error) {
	return l.store.Chapters(ctx, pid)
}

// CurrentChapter resolves the chapter a resume position falls in (the nearest
// preceding chapter when between spans). It returns nil when the book has no
// chapters.
func (l *Library) CurrentChapter(ctx context.Context, pid model.PID, positionMS int64) (*model.Chapter, error) {
	return l.store.CurrentChapter(ctx, pid, positionMS)
}

// BookResume returns a user's play state for a book together with the chapter their
// resume position falls in, the chapter-level resume answer. An empty userPID
// selects the default user.
func (l *Library) BookResume(ctx context.Context, userPID, bookPID model.PID) (*model.PlayState, *model.Chapter, error) {
	st, err := l.playback.State(ctx, userPID, bookPID)
	if err != nil {
		return nil, nil, err
	}
	ch, err := l.store.CurrentChapter(ctx, bookPID, st.PositionMS)
	if err != nil {
		return nil, nil, err
	}
	return st, ch, nil
}

// BooksInSeries lists a series' books in sequence order (decimal/string aware).
func (l *Library) BooksInSeries(ctx context.Context, seriesPID model.PID) ([]*model.ItemView, error) {
	return l.store.BooksInSeries(ctx, seriesPID)
}

// Stats returns a library summary using the same Facet grouping as browse plus
// per-user playback state. An empty userPID selects the default user; topN caps
// the ranked lists.
func (l *Library) Stats(ctx context.Context, userPID model.PID, topN int) (*read.Stats, error) {
	return l.store.Stats(ctx, userPID, topN)
}

// Changes returns change_log rows after seq.
func (l *Library) Changes(ctx context.Context, sinceSeq int64) ([]model.Change, error) {
	return l.store.ChangesSince(ctx, sinceSeq)
}

// Subscribe registers an in-process listener for change_log rows after each
// mutating commit. The cancel func unsubscribes. Cross-process consumers should
// poll DataVersion and then call Changes.
func (l *Library) Subscribe() (<-chan model.Change, func()) { return l.store.Subscribe() }

// DataVersion returns SQLite's data_version, which moves whenever any connection
// commits. A consumer in another process polls it and pulls Changes when it
// changes.
func (l *Library) DataVersion(ctx context.Context) (int64, error) {
	return l.store.DataVersion(ctx)
}

// Playback returns the playback-state service for progress, played status,
// ratings, stars, bookmarks, queue, and play sessions.
func (l *Library) Playback() *playback.Service { return l.playback }

// Playlists returns the playlist service for static and smart playlists plus
// M3U8 import/export.
func (l *Library) Playlists() *playlist.Service { return l.playlists }

// Podcasts returns the podcast service: subscribe/sync feeds, download episodes,
// store transcripts/artwork, OPML import/export, and retention.
func (l *Library) Podcasts() *podcast.Service { return l.podcasts }

// Users lists the playback users (the default first).
func (l *Library) Users(ctx context.Context) ([]*model.User, error) { return l.store.Users(ctx) }

// CreateUser adds a playback user.
func (l *Library) CreateUser(ctx context.Context, name string) (*model.User, error) {
	return l.store.CreateUser(ctx, name)
}

// DefaultUser returns the seeded default user.
func (l *Library) DefaultUser(ctx context.Context) (*model.User, error) {
	return l.store.DefaultUser(ctx)
}

// Jobs lists recent jobs, newest first.
func (l *Library) Jobs(ctx context.Context, limit int) ([]*model.Job, error) {
	return l.jobs.List(ctx, limit)
}

// OwnerInfo returns the current write-owner metadata from the lockfile.
func (l *Library) OwnerInfo() (sqlite.OwnerInfo, error) { return l.store.OwnerInfo() }

// ScanRequest selects what to scan.
type ScanRequest struct {
	LibraryPID model.PID // empty scans every library
	SubPath    string    // optional sub-path under a single library's root
	// Force bypasses the incremental fast-path, re-hashing and re-parsing every file
	// even when its size and mtime are unchanged (repair, or after an essence bump).
	Force bool
	// AdoptStampedPIDs restores item PIDs from WAXBIN_ITEM_PID tags during a rebuild
	// (essence-first, adopted only when unambiguous). Off for a normal scan.
	AdoptStampedPIDs bool
	// ForceReconcile bypasses the survival-gate floor so a deliberate large deletion
	// is reconciled to missing (the recovery path). An explicit operator action; the
	// watcher never sets it.
	ForceReconcile bool
}

// fsMutateScope is the shared lease scope held by every job that mutates files on
// disk (scan, organize, import, and trash moves), so at most one filesystem
// mutator runs at a time. Leases are per-scope, and scan and organize would
// otherwise use different scopes and not exclude each other, letting a watch rescan
// race an in-flight organize. Read-only passes (analyze, enrich) keep their own
// scopes so they can still overlap a scan.
const fsMutateScope = "fs-mutate"

// ScanResult reports a scan, including the job it ran under.
type ScanResult struct {
	JobPID model.PID
	Total  scan.Result
	Runs   []scan.Result
}

// Scan indexes the selected libraries under a single "scan"-scoped job.
func (l *Library) Scan(ctx context.Context, req ScanRequest) (*ScanResult, error) {
	libs, err := l.resolveLibraries(ctx, req.LibraryPID)
	if err != nil {
		return nil, err
	}
	out := &ScanResult{}
	job, runErr := l.jobs.Run(ctx, "scan", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		for _, lib := range libs {
			r, err := l.scanner.Scan(ctx, scan.Request{
				Library: lib, SubPath: req.SubPath, Force: req.Force,
				AdoptStampedPIDs: req.AdoptStampedPIDs, ForceReconcile: req.ForceReconcile,
			}, func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
			if err != nil {
				return err
			}
			out.Runs = append(out.Runs, *r)
			addResult(&out.Total, r)
		}
		// Rollups are maintained transactionally for the entities touched by each
		// scanned track. No whole-catalog refresh is needed here; RefreshRollups is
		// the repair path for drift reported by `db verify`.
		return nil
	})
	if job != nil {
		out.JobPID = job.PID
	}
	return out, runErr
}

// AnalyzeResult reports an analyze run and the job it ran under.
type AnalyzeResult struct {
	JobPID model.PID
	Result analyze.Result
}

// AnalyzeOptions controls one analyze run.
type AnalyzeOptions struct {
	// WriteReplayGainTags mirrors computed ReplayGain into files after aggregation,
	// for this run. It is OR-ed with the library's configured toggle, so a run enables
	// write-back if either the config or this flag asks for it.
	WriteReplayGainTags bool
}

// Analyze runs the resumable analyze pass: it decodes (the only PCM-decoding
// stage), fingerprints, and indexes every audio file whose fingerprint is
// missing or stale, under an "analyze"-scoped job. Files whose codec this build
// cannot decode are reported as skipped, not failed.
func (l *Library) Analyze(ctx context.Context, opts AnalyzeOptions) (*AnalyzeResult, error) {
	out := &AnalyzeResult{}
	writeRG := l.opts.WriteReplayGainTags || opts.WriteReplayGainTags
	job, runErr := l.jobs.Run(ctx, "analyze", "analyze", func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.analyzer.Run(ctx, func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		if r != nil {
			out.Result = *r
		}
		if err != nil {
			return err
		}
		// Album ReplayGain depends on per-file loudness and album membership.
		// Membership can change in a tag-only scan, so reconcile it after every
		// analyze pass. Catalogs with no loudness return immediately.
		if err := l.store.RefreshAlbumGain(ctx); err != nil {
			return err
		}
		// Optionally mirror the (now album-aggregated) ReplayGain into files on disk,
		// in one pass. Off by default; failures are logged, not fatal to the analyze.
		if writeRG {
			c, err := l.writeReplayGainTags(ctx)
			if err != nil {
				return err
			}
			out.Result.ReplayGainTagsWritten = c.written
			out.Result.ReplayGainTagsFailed = c.failed
			out.Result.ReplayGainTagsUnrepresented = c.unrepresented
		}
		return nil
	})
	if job != nil {
		out.JobPID = job.PID
	}
	return out, runErr
}

// WatchActivity summarizes one watch cycle for a heartbeat consumer.
type WatchActivity struct {
	Trigger string // initial | scheduled | full | live
	Changed bool
}

// WatchOptions configures a foreground watch (see watch.Options).
type WatchOptions struct {
	LibraryPID         model.PID // empty watches every user library root
	Interval           time.Duration
	FullRescanInterval time.Duration
	Live               bool
	WriteSettle        time.Duration
	MaxWatchDirs       int // 0 = unlimited; caps live fsnotify watches (see watch.Options)
	Analyze            bool
	SyncSources        bool
	// OnActivity, when set, is called after each cycle for a CLI heartbeat.
	OnActivity func(WatchActivity)
}

// Watch runs a foreground watcher that keeps the catalog in sync with the
// filesystem until ctx is canceled (returning a CodeCanceled error on a clean
// shutdown). It refuses on a read-only library.
//
// WATCH IS A FOREGROUND MODE. A read-write WaxBin holds an exclusive advisory lock
// on the catalog for the whole process lifetime, so while watch runs, every OTHER
// mutating command in another terminal (organize, analyze, enrich, import, scan
// --force) is refused (read-only queries are always allowed). Stop the watcher to
// do manual mutation. Idle lock release and a socket proxy are deliberately post-1.0.
func (l *Library) Watch(ctx context.Context, opts WatchOptions) error {
	if l.ReadOnly() {
		return waxerr.New(waxerr.CodeUnsupported, "Library.Watch", "watch requires a read-write library")
	}
	libs, err := l.resolveLibraries(ctx, opts.LibraryPID)
	if err != nil {
		return err
	}
	roots := make([]watch.Root, 0, len(libs))
	for _, lib := range libs {
		roots = append(roots, watch.Root{LibraryPID: lib.PID, Path: string(lib.Root)})
	}
	var notify func(watch.Activity)
	if opts.OnActivity != nil {
		notify = func(a watch.Activity) { opts.OnActivity(WatchActivity{Trigger: a.Trigger, Changed: a.Changed}) }
	}
	w := watch.New(&watchEngine{lib: l}, roots, watch.Options{
		Interval:           opts.Interval,
		FullRescanInterval: opts.FullRescanInterval,
		Live:               opts.Live,
		WriteSettle:        opts.WriteSettle,
		MaxWatchDirs:       opts.MaxWatchDirs,
		Analyze:            opts.Analyze,
		SyncSources:        opts.SyncSources,
		Notify:             notify,
	}, l.log)
	return w.Run(ctx)
}

// watchEngine adapts the facade to the watch.Engine port, so the watch package need
// not import waxbin.
type watchEngine struct{ lib *Library }

func (e *watchEngine) Rescan(ctx context.Context, libPID model.PID, subPath string, force bool) (bool, error) {
	res, err := e.lib.Scan(ctx, ScanRequest{LibraryPID: libPID, SubPath: subPath, Force: force})
	if err != nil {
		return false, err
	}
	t := res.Total
	// A live .lrc/.cue edit mutates the catalog without touching the audio bytes, so it
	// bumps SidecarsUpdated and NOT ItemsUpdated (ContentChanged is false either way,
	// on the fast path and the full path alike). Include it, or a sidecar-only change
	// reports changed=false and every downstream scheduler is silently skipped.
	changed := t.ItemsCreated > 0 || t.ItemsUpdated > 0 || t.Relinked > 0 || t.Missing > 0 || t.SidecarsUpdated > 0
	return changed, nil
}

func (e *watchEngine) Analyze(ctx context.Context) error {
	_, err := e.lib.Analyze(ctx, AnalyzeOptions{})
	return err
}

// SyncSources drives the layered background acquisition on top of the watcher:
// podcast feed sync + retention, and auto-import of any configured inbox staging
// folders. All are thin callers of existing primitives; each is best-effort so one
// failing source (an unreachable feed) does not stop the others or the watcher.
func (e *watchEngine) SyncSources(ctx context.Context) error {
	if _, err := e.lib.Podcasts().SyncAll(ctx); err != nil {
		e.lib.log.Warn("watch: podcast sync", "err", err)
	}
	if _, err := e.lib.Podcasts().ApplyRetentionAll(ctx); err != nil {
		e.lib.log.Warn("watch: podcast retention", "err", err)
	}
	// Live inbox import: plan then apply each configured staging folder, so a file
	// dropped into the inbox is imported into a managed root and cataloged.
	for _, folder := range e.lib.InboxFolders() {
		plan, err := e.lib.PlanImport(ctx, ImportRequest{Source: folder})
		if err != nil {
			e.lib.log.Warn("watch: inbox plan", "folder", folder, "err", err)
			continue
		}
		if plan.Importable() == 0 {
			continue
		}
		if _, err := e.lib.ApplyImport(ctx, plan); err != nil {
			e.lib.log.Warn("watch: inbox import", "folder", folder, "err", err)
		}
	}
	return nil
}

// EnrichOptions controls a metadata enrichment run.
type EnrichOptions struct {
	Force bool // re-enrich already-enriched entities
	Limit int  // cap on entities processed (0 = all needing enrichment)
}

// EnrichResult reports an enrichment run and the job it ran under.
type EnrichResult struct {
	JobPID model.PID
	Result enrich.Result
}

// Enrich runs the metadata enrichment pass under an "enrich"-scoped job:
// MusicBrainz release-group/artist/genre resolution (MBID-first), Cover Art Archive
// covers, and the optional AcoustID fingerprint fallback. It is resumable and
// lock-respecting (never overwriting a tagged or user-locked field), caches
// provider responses, and degrades gracefully offline. Enrichment requires a
// MusicBrainz contact (Options.Enrichment.Contact); without one it returns
// CodeUnsupported.
func (l *Library) Enrich(ctx context.Context, opts EnrichOptions) (*EnrichResult, error) {
	out := &EnrichResult{}
	if !l.enricher.Enabled() {
		return out, waxerr.New(waxerr.CodeUnsupported, "waxbin.Enrich",
			"enrichment needs a MusicBrainz contact (set enrichment.contact / WAXBIN_ENRICH_CONTACT)")
	}
	job, runErr := l.jobs.Run(ctx, "enrich", "enrich", func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.enricher.Run(ctx, enrich.RunOptions{Force: opts.Force, Limit: opts.Limit},
			func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		if r != nil {
			out.Result = *r
		}
		return err
	})
	if job != nil {
		out.JobPID = job.PID
	}
	return out, runErr
}

// EnrichmentCoverage reports how many entities have been enriched, for doctor.
func (l *Library) EnrichmentCoverage(ctx context.Context) (model.EnrichmentCoverage, error) {
	return l.enricher.Coverage(ctx)
}

// AltEncoding is one verified alternate encoding of a query item: a different
// file whose fingerprint matches above the similarity threshold.
type AltEncoding struct {
	ItemPID    model.PID
	FilePID    model.PID
	Similarity float64
}

const (
	// altMinSharedTerms is the inverted-index candidate threshold. It is
	// deliberately low: the index needs high recall, while full-fingerprint
	// similarity below is the precision filter. The 30-bit shingle terms are
	// selective enough that an unrelated track in the same duration bucket
	// usually shares none, so the low threshold does not flood verification.
	altMinSharedTerms  = 2
	altSimilarityFloor = 0.7 // full-vector bit-agreement threshold
)

// FindAltEncodings returns other catalog items that are alt encodings of the
// given item: the inverted index proposes candidates (shared terms within the
// duration bucket), then each is verified by full-fingerprint similarity. The
// item must already be analyzed; an unanalyzed item yields no matches.
func (l *Library) FindAltEncodings(ctx context.Context, itemPID model.PID) ([]AltEncoding, error) {
	item, err := l.store.ItemByPID(ctx, itemPID)
	if err != nil {
		return nil, err
	}
	if item.FilePID == "" {
		return nil, nil
	}
	queryFP, err := l.store.LoadFingerprint(ctx, item.FilePID)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // not analyzed yet: nothing to group on
		}
		return nil, err
	}
	candidates, err := l.store.FingerprintCandidates(ctx, item.FilePID, altMinSharedTerms)
	if err != nil {
		return nil, err
	}

	// FingerprintCandidates already returned each candidate's fingerprint vector,
	// so verification is an in-memory comparison with no per-candidate query.
	qSub := fingerprint.Unpack(queryFP)
	var out []AltEncoding
	for _, c := range candidates {
		// The candidate query guarantees c shares the query file's fingerprint
		// algorithm, so dispatching on the candidate's algo is safe and picks the
		// matching (pure-Go vs Chromaprint) similarity function.
		sim := fingerprint.SimilarByAlgo(c.AlgoVersion, qSub, fingerprint.Unpack(c.FP))
		if sim >= altSimilarityFloor {
			out = append(out, AltEncoding{ItemPID: c.ItemPID, FilePID: c.FilePID, Similarity: sim})
		}
	}
	return out, nil
}

// Loudness returns an item's measured ReplayGain (track and album gain/peak), or
// CodeNotFound when it has not been analyzed for loudness.
func (l *Library) Loudness(ctx context.Context, itemPID model.PID) (*model.Loudness, error) {
	return l.store.LoudnessByItem(ctx, itemPID)
}

// Peaks returns an item's stored waveform overview, or CodeNotFound.
func (l *Library) Peaks(ctx context.Context, itemPID model.PID) (*model.PeaksData, error) {
	return l.store.LoadPeaks(ctx, itemPID)
}

// RefreshAlbumGain recomputes album-aware ReplayGain from the per-file loudness.
// Analyze runs it automatically; it is exposed for repair after a manual import.
func (l *Library) RefreshAlbumGain(ctx context.Context) error {
	return l.store.RefreshAlbumGain(ctx)
}

// Coverage reports per-codec analysis decode support for doctor.
func (l *Library) Coverage() []decode.FormatSupport { return l.decoders.Coverage() }

// VerifyDerived runs the derived-data consistency check (FTS, rollups, and
// generated sort keys versus the source rows). It is read-only; it reports drift
// rather than repairing it.
func (l *Library) VerifyDerived(ctx context.Context) (*sqlite.DerivedReport, error) {
	return l.store.VerifyDerived(ctx)
}

// RefreshRollups recomputes the maintained rollups, the repair for the rollup
// drift that VerifyDerived can report.
func (l *Library) RefreshRollups(ctx context.Context) error {
	return l.store.RefreshRollups(ctx)
}

// AuditOptions selects which audit checks run.
type AuditOptions struct {
	// Only, when non-empty, restricts the run to these checks.
	Only []model.AuditCheck
	// Integrity re-reads every audio file to detect bitrot, missing files, and
	// corrupt audio. Off by default (I/O heavy).
	Integrity bool
	// Sample caps the per-check finding sample (0 uses a default).
	Sample int
}

// Audit runs the quality/integrity checks and returns their findings. It is
// read-only. The default run covers the catalog checks (duplicates, split albums,
// inconsistent metadata, missing art/ReplayGain, bad filenames, orphan sidecars,
// path conflicts, invalid feeds, derived-data drift); Integrity adds the on-disk
// bitrot and corrupt-audio passes.
func (l *Library) Audit(ctx context.Context, opts AuditOptions) (*audit.Report, error) {
	return l.auditor.Run(ctx, audit.Config{Only: opts.Only, Integrity: opts.Integrity, Sample: opts.Sample})
}

// OrphanGraceWindow is how long an entity must stay childless before the manual
// orphan GC sweeps it. It is the safety backstop to the scanner's survival gate: a
// transient reconciliation blip that briefly orphans an entity will not delete it
// unless it is still orphaned a full window (and a second manual run) later.
const OrphanGraceWindow = 24 * time.Hour

// VacuumReport summarizes a vacuum: the derived garbage reclaimed before the
// on-disk compaction.
type VacuumReport struct {
	ArtSourcesReclaimed int
	ThumbnailsReclaimed int
	OrphansDeleted      int
	OrphansPending      int
}

// GCOrphans deletes childless artist/release_group/album/genre/series rows that have
// stayed orphaned past the grace window, recording the rest for a later sweep. It is
// manual-only (invoked by Vacuum and db verify --fix), never the watch loop.
func (l *Library) GCOrphans(ctx context.Context) (*model.OrphanGCReport, error) {
	return l.store.GCOrphans(ctx, OrphanGraceWindow.Nanoseconds())
}

// Vacuum GCs orphaned entities and art, then compacts the database file, returning
// what was reclaimed. It takes the write lock. Orphan entities are swept before art
// so their freed art-map rows are reclaimed in the same pass.
func (l *Library) Vacuum(ctx context.Context) (*VacuumReport, error) {
	orphans, err := l.store.GCOrphans(ctx, OrphanGraceWindow.Nanoseconds())
	if err != nil {
		return nil, err
	}
	srcs, thumbs, err := l.store.GCArt(ctx)
	if err != nil {
		return nil, err
	}
	if err := l.store.Vacuum(ctx); err != nil {
		return nil, err
	}
	return &VacuumReport{
		ArtSourcesReclaimed: srcs, ThumbnailsReclaimed: thumbs,
		OrphansDeleted: orphans.Total(), OrphansPending: orphans.Pending,
	}, nil
}

// IntegrityCheck runs SQLite's PRAGMA integrity_check and returns the problems it
// reports (a healthy database returns a single "ok"). It is read-only.
func (l *Library) IntegrityCheck(ctx context.Context) ([]string, error) {
	return l.store.IntegrityCheck(ctx)
}

// PruneChangeLog trims the change_log to its newest keep rows, returning how many
// were deleted. A consumer that has fallen behind the retained horizon must
// full-resync (the documented delta-sync contract).
func (l *Library) PruneChangeLog(ctx context.Context, keep int) (int, error) {
	return l.store.PruneChangeLog(ctx, keep)
}

// YearInReview returns a per-user listening recap for one calendar year (UTC):
// session/minute/track totals, catalog additions that year, and the top
// artists/genres/tracks by play count. An empty userPID uses the default user.
func (l *Library) YearInReview(ctx context.Context, userPID model.PID, year, topN int) (*read.YearReview, error) {
	return l.store.YearInReview(ctx, userPID, year, topN)
}

// Merge collapses the loser entity onto the survivor: children (tracks, albums,
// genre links, contributor credits) are re-pointed onto the survivor, its MBID
// and enrichment marker are unioned when it lacks one, rollups are recomputed,
// and the loser is deleted. The survivor keeps its PID. This is the first-class
// entity-merge primitive for artists, release groups, albums, and genres. It
// repairs the duplicate-entity findings audit reports, and is the seam late
// enrichment uses to unify two heuristic rows that resolve to one MBID.
func (l *Library) Merge(ctx context.Context, entityType model.MergeEntity, survivorPID, loserPID model.PID) (*model.MergeReport, error) {
	return l.store.MergeEntity(ctx, entityType, survivorPID, loserPID)
}

// MergeMany collapses several loser entities onto the survivor in one atomic
// transaction: if any loser fails (e.g. an unknown PID), the whole batch rolls
// back, so a partial merge can never be left behind. Returns one report per loser.
func (l *Library) MergeMany(ctx context.Context, entityType model.MergeEntity, survivorPID model.PID, loserPIDs []model.PID) ([]*model.MergeReport, error) {
	return l.store.MergeEntities(ctx, entityType, survivorPID, loserPIDs)
}

// Lock marks item fields as protected from enrichment and organize tag
// write-back. Unknown fields are rejected.
func (l *Library) Lock(ctx context.Context, pid model.PID, fields ...string) error {
	for _, f := range fields {
		if err := l.store.LockField(ctx, pid, f); err != nil {
			return err
		}
	}
	return nil
}

// Unlock clears the lock on each field, dropping rows that no longer carry any
// curated state so provenance stays sparse.
func (l *Library) Unlock(ctx context.Context, pid model.PID, fields ...string) error {
	for _, f := range fields {
		if err := l.store.UnlockField(ctx, pid, f); err != nil {
			return err
		}
	}
	return nil
}

// Provenance returns an item's field provenance. Only non-default fields have
// rows, so a tag-only item returns an empty slice.
func (l *Library) Provenance(ctx context.Context, pid model.PID) ([]model.FieldProvenance, error) {
	return l.store.FieldProvenance(ctx, pid)
}

// PlanOrganize computes a dry-run move plan for the selected items across every
// managed library, routing each item to the library whose root already contains it.
// Roots are non-overlapping, so kind routing is implicit in the current file path.
// A single managed library behaves exactly as before. profileName overrides each
// library's configured profile when non-empty.
func (l *Library) PlanOrganize(ctx context.Context, q query.Query, profileName string) (*organize.Plan, error) {
	managed, err := l.managedLibraries(ctx)
	if err != nil {
		return nil, err
	}
	items, err := l.store.QueryItems(ctx, q)
	if err != nil {
		return nil, err
	}
	merged := &organize.Plan{Profile: profileName}
	for _, lib := range managed {
		// Default to the library's configured profile so a root registered
		// `:managed:...:waxbin-native` lays out as waxbin-native without repeating
		// --profile; an explicit profileName overrides it for every library.
		pname := profileName
		if pname == "" {
			pname = lib.Profile
		}
		prof, err := l.profiles.ByName(pname)
		if err != nil {
			return nil, err
		}
		// organize.Plan filters items to those under this library's root, so passing the
		// full item set to each library partitions the work by current location.
		p, err := l.organizer.Plan(ctx, lib, prof, items)
		if err != nil {
			return nil, err
		}
		merged.Actions = append(merged.Actions, p.Actions...)
		// Enable tag-write on the merged plan if any library's profile enabled it; each
		// action already carries its own (possibly empty) TagFields, so the executor
		// writes tags only where the source library asked for them.
		merged.TagWrite = merged.TagWrite || p.TagWrite
		if len(managed) == 1 {
			merged.Root, merged.LibraryPID, merged.Profile = p.Root, p.LibraryPID, p.Profile
		}
	}
	// PID stamping is a library-wide, managed-only identity feature; organize only
	// ever plans managed-root files, so it is safe to enable across the merged plan.
	merged.StampPID = l.opts.StampItemPID
	return merged, nil
}

// Profiles lists the organization profile names available to this library
// (built-ins plus any configured custom profiles), sorted.
func (l *Library) Profiles() []string { return l.profiles.Names() }

// toOrganizeProfiles converts config profile defs to organize profiles. The
// organize package validates the templates when building the set.
func toOrganizeProfiles(defs []config.ProfileDef) []organize.Profile {
	if len(defs) == 0 {
		return nil
	}
	out := make([]organize.Profile, 0, len(defs))
	for _, d := range defs {
		out = append(out, organize.Profile{
			Name: d.Name, Music: d.Music, Audiobook: d.Audiobook,
			Podcast: d.Podcast, TagWrite: d.TagWrite,
		})
	}
	return out
}

// ApplyOrganize executes a plan under an "organize"-scoped job.
func (l *Library) ApplyOrganize(ctx context.Context, plan *organize.Plan) (*organize.Report, error) {
	var rep *organize.Report
	_, err := l.jobs.Run(ctx, "organize", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.organizer.Execute(ctx, plan, h.JobPID(),
			func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		rep = r
		return err
	})
	return rep, err
}

// PlanDelete computes a dry-run deletion plan for the items matching q under a
// mode (trash|prune|permanent). DeleteTrash moves files to the reversible
// per-library trash; the other modes bypass it to reclaim space. Every mode keeps
// the logical item (archived when it loses its last file).
func (l *Library) PlanDelete(ctx context.Context, q query.Query, mode model.DeleteMode) (*trash.Plan, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	items, err := l.store.QueryItems(ctx, q)
	if err != nil {
		return nil, err
	}
	return l.trasher.Plan(ctx, libs, items, mode)
}

// PlanDeletePIDs computes a deletion plan for explicit item pids. It is the
// `rm <pid>` path; PlanDelete is the query-driven path used by retention/dedup.
func (l *Library) PlanDeletePIDs(ctx context.Context, pids []model.PID, mode model.DeleteMode) (*trash.Plan, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]*model.ItemView, 0, len(pids))
	for _, pid := range pids {
		it, err := l.store.ItemByPID(ctx, pid)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return l.trasher.Plan(ctx, libs, items, mode)
}

// ApplyDelete executes a deletion plan under a "delete"-scoped job.
func (l *Library) ApplyDelete(ctx context.Context, plan *trash.Plan) (*trash.Report, error) {
	var rep *trash.Report
	_, err := l.jobs.Run(ctx, "delete", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.trasher.Execute(ctx, plan)
		rep = r
		return err
	})
	return rep, err
}

// Trash lists trash journal entries, newest first. includeRestored controls
// whether already-restored rows are shown; limit 0 returns all.
func (l *Library) Trash(ctx context.Context, includeRestored bool, limit int) ([]model.TrashEntry, error) {
	return l.store.TrashEntries(ctx, includeRestored, limit)
}

// RestoreTrash undoes a delete: it moves the trashed file back to its original
// path and re-scans it so the catalog re-links it (un-archiving its item). It
// refuses if the original path is occupied.
func (l *Library) RestoreTrash(ctx context.Context, trashPID model.PID) error {
	entry, err := l.store.ActiveTrashByPID(ctx, trashPID)
	if err != nil {
		return err
	}
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return err
	}
	lib := libraryContaining(libs, entry.OrigDisplay)
	if lib == nil {
		return waxerr.New(waxerr.CodeInvalid, "Library.RestoreTrash",
			"restore target is not under a known library root")
	}
	_, err = l.jobs.Run(ctx, "restore", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		// Move the file back (idempotent: a retry after a failed re-scan is a no-op).
		if err := l.trasher.Restore(*entry); err != nil {
			return err
		}
		// Re-catalog before marking the entry restored, so a re-scan failure leaves
		// the entry active and the restore retryable rather than flagging it done
		// while the item is still archived.
		if _, err := l.scanner.Scan(ctx, scan.Request{Library: lib, SubPath: entry.OrigDisplay}, nil); err != nil {
			return err
		}
		return l.store.MarkTrashRestored(ctx, trashPID)
	})
	return err
}

// EmptyReport summarizes an empty-trash pass.
type EmptyReport struct {
	Purged         int
	Errored        int
	ReclaimedBytes int64
}

// EmptyTrash permanently removes every active trashed file from disk and drops
// its journal row, reclaiming space. It runs under a "delete"-scoped job.
func (l *Library) EmptyTrash(ctx context.Context) (*EmptyReport, error) {
	rep := &EmptyReport{}
	_, err := l.jobs.Run(ctx, "empty-trash", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		entries, err := l.store.TrashEntries(ctx, false, 0)
		if err != nil {
			return err
		}
		for i := range entries {
			if ctx.Err() != nil {
				return waxerr.FromContext("Library.EmptyTrash", ctx.Err(), waxerr.CodeIO)
			}
			size, perr := l.trasher.Purge(entries[i])
			if perr != nil {
				rep.Errored++
				l.log.Warn("purging trashed file", "trash", entries[i].TrashDisplay, "err", perr)
				continue
			}
			// Don't abort the whole pass on a row-delete failure: that would strand an
			// active journal row whose file is already gone (un-restorable). Tally it
			// and move on; a later empty-trash re-run drops the row (Purge tolerates the
			// already-missing file).
			if err := l.store.DeleteTrashRow(ctx, entries[i].PID); err != nil {
				rep.Errored++
				l.log.Warn("dropping trash journal row", "trash", entries[i].PID, "err", err)
				continue
			}
			rep.Purged++
			rep.ReclaimedBytes += size
		}
		return nil
	})
	return rep, err
}

// ImportRequest selects a staging folder and how to import it.
type ImportRequest struct {
	Source     string          // staging folder to import (required)
	LibraryPID model.PID       // target managed library; empty uses the single managed one
	Profile    string          // layout; empty uses the library's configured profile
	DupPolicy  model.DupPolicy // how to treat catalog duplicates (default skip)
	Copy       bool            // copy (keep originals) instead of move
}

// PlanImport computes a reviewable import plan for a staging folder: which files
// would be imported (with destinations), which are catalog duplicates, and which
// are quarantined. It is read-only.
func (l *Library) PlanImport(ctx context.Context, req ImportRequest) (*inbox.Plan, error) {
	if strings.TrimSpace(req.Source) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.PlanImport", "no import source folder")
	}
	// Resolve the target and, for multiple media-typed managed roots, a per-file
	// router so a staging folder splits its books into the audiobook root and its
	// tracks into the music root. A named target (LibraryPID) or a single managed
	// library imports everything into that one library (today's behavior).
	var defaultLib *model.Library
	var route func(model.Kind) *model.Library
	if req.LibraryPID != "" {
		lib, err := l.resolveManagedLibrary(ctx, req.LibraryPID)
		if err != nil {
			return nil, err
		}
		defaultLib = lib
	} else {
		managed, err := l.managedLibraries(ctx)
		if err != nil {
			return nil, err
		}
		if len(managed) == 1 {
			defaultLib = managed[0]
		} else {
			defaultLib = firstMixedOrFirst(managed)
			route = func(kind model.Kind) *model.Library { return routeManaged(managed, kind) }
		}
	}
	profileName := req.Profile
	if profileName == "" {
		profileName = defaultLib.Profile
	}
	prof, err := l.profiles.ByName(profileName)
	if err != nil {
		return nil, err
	}
	// When routing across managed roots, lay each file out under its target library's own
	// configured profile (or the explicit --profile override when given), so a book sent
	// to the audiobook root uses that root's profile, not the default library's.
	var profileFor func(*model.Library) organize.Profile
	if route != nil {
		override := req.Profile
		profileFor = func(lib *model.Library) organize.Profile {
			name := override
			if name == "" {
				name = lib.Profile
			}
			p, perr := l.profiles.ByName(name)
			if perr != nil {
				return prof // config-validated names don't error; fall back to the default
			}
			return p
		}
	}
	return l.importer.Plan(ctx, inbox.Request{
		Source: req.Source, Library: defaultLib, Route: route, Profile: prof, ProfileFor: profileFor,
		DupPolicy: req.DupPolicy, Copy: req.Copy, ReserveBytes: l.opts.FreeSpaceReserveBytes,
	})
}

// firstMixedOrFirst returns a mixed managed library if any, else the first managed
// library. The caller still uses routeManaged to quarantine ambiguous typed routes.
func firstMixedOrFirst(managed []*model.Library) *model.Library {
	for _, lib := range managed {
		if lib.MediaType() == model.MediaMixed {
			return lib
		}
	}
	return managed[0]
}

// ApplyImport executes an import plan under an "import"-scoped job.
func (l *Library) ApplyImport(ctx context.Context, plan *inbox.Plan) (*inbox.Report, error) {
	var rep *inbox.Report
	_, err := l.jobs.Run(ctx, "import", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.importer.Execute(ctx, plan)
		rep = r
		return err
	})
	return rep, err
}

// ImportBatches lists recorded import batches, newest first (limit 0 = all).
func (l *Library) ImportBatches(ctx context.Context, limit int) ([]*model.ImportBatch, error) {
	return l.store.ImportBatches(ctx, limit)
}

// AcquiredFile is a local media file to ingest as externally-acquired media (for
// example one a source provider already fetched to disk). Path is required for a
// track/book and optional for an episode (an episode may be ingested remote, to be
// downloaded later from meta.SourceURL).
type AcquiredFile struct {
	Path string
}

// AcquiredMeta carries the origin provenance recorded against an acquired item plus
// the per-kind ingest options.
type AcquiredMeta struct {
	// Origin provenance recorded in the acquisition table. SourceType defaults to
	// manual when empty; an explicitly acquired item is never plain local. Local is
	// the read-side default for an item with no acquisition row.
	SourceType      model.SourceType
	SourceURL       string
	SourceID        string
	Provider        string
	ProviderVersion string
	OptionsJSON     string

	// Track/book placement.
	Profile   string          // organization profile override (empty = the target library's)
	Copy      bool            // copy instead of move the source file
	DupPolicy model.DupPolicy // catalog-duplicate policy (default skip)

	// Episode ingest.
	ShowPID   model.PID // existing show to add the episode under; empty creates a manual show
	ShowTitle string    // manual show title when ShowPID is empty (default "Acquired")
	Title     string    // episode title (default the file base name)
	Pinned    *bool     // pinned episode; default true for acquired episodes
}

// AcquiredResult reports an ImportAcquired. For a track/book it carries a reviewable
// import Plan (apply it with ApplyImport); for an episode the ingest is immediate and
// EpisodePID/FilePID/Path name the result.
type AcquiredResult struct {
	Kind       model.Kind
	Plan       *inbox.Plan // track/book: review, then ApplyImport
	EpisodePID model.PID   // episode: the ingested episode
	FilePID    model.PID   // episode: its attached file, when a local file was provided
	Path       string      // episode: the placed file path, when attached
}

// ImportAcquired routes an acquired or manual file by kind. Tracks and books go
// through the import planner for the matching managed library, including duplicate
// checks, destination rendering, free-space checks, and acquisition provenance.
// Episodes go into the internal podcast library under an existing or manual show and
// are pinned by default. WaxBin never performs platform extraction itself; callers
// hand it an already acquired file or a remote enclosure URL.
func (l *Library) ImportAcquired(ctx context.Context, file AcquiredFile, kind model.Kind, meta AcquiredMeta) (*AcquiredResult, error) {
	switch kind {
	case model.KindTrack, model.KindBook:
		return l.importAcquiredMedia(ctx, file, kind, meta)
	case model.KindEpisode:
		return l.importAcquiredEpisode(ctx, file, meta)
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.ImportAcquired", "unsupported acquired kind: "+string(kind))
	}
}

// importAcquiredMedia plans one acquired track or book into the matching managed
// library. The returned plan is still dry-run; ApplyImport performs the move/copy and
// records the acquisition row.
func (l *Library) importAcquiredMedia(ctx context.Context, file AcquiredFile, kind model.Kind, meta AcquiredMeta) (*AcquiredResult, error) {
	const op = "Library.ImportAcquired"
	if strings.TrimSpace(file.Path) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no acquired file path")
	}
	lib, err := l.managedLibraryForKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	profileName := meta.Profile
	if profileName == "" {
		profileName = lib.Profile
	}
	prof, err := l.profiles.ByName(profileName)
	if err != nil {
		return nil, err
	}
	plan, err := l.importer.PlanFile(ctx, inbox.Request{
		Library: lib, Profile: prof, DupPolicy: meta.DupPolicy, Copy: meta.Copy,
		ReserveBytes: l.opts.FreeSpaceReserveBytes, Acquisition: acquisitionInput(meta),
	}, file.Path, kind)
	if err != nil {
		return nil, err
	}
	return &AcquiredResult{Kind: kind, Plan: plan}, nil
}

// importAcquiredEpisode ingests an acquired episode into the internal podcast
// library: it resolves or creates the target show, upserts the episode (pinned), and
// attaches the local file when one is provided (else the episode stays remote for a
// later download). It records the origin provenance on the episode item.
func (l *Library) importAcquiredEpisode(ctx context.Context, file AcquiredFile, meta AcquiredMeta) (*AcquiredResult, error) {
	const op = "Library.ImportAcquired"
	showPID := meta.ShowPID
	if showPID == "" {
		title := strings.TrimSpace(meta.ShowTitle)
		if title == "" {
			title = "Acquired"
		}
		pod, err := l.podcasts.AddManual(ctx, title, podcast.ManualOptions{})
		if err != nil {
			return nil, err
		}
		showPID = pod.PID
	}
	epTitle := strings.TrimSpace(meta.Title)
	if epTitle == "" && file.Path != "" {
		epTitle = filepath.Base(file.Path)
	}
	if epTitle == "" && meta.SourceURL == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "acquired episode needs a title, file, or source url")
	}
	pinned := true
	if meta.Pinned != nil {
		pinned = *meta.Pinned
	}
	res, err := l.podcasts.AddEpisode(ctx, showPID, model.FeedEpisode{
		Title: epTitle, EnclosureURL: meta.SourceURL,
	}, pinned)
	if err != nil {
		return nil, err
	}
	out := &AcquiredResult{Kind: model.KindEpisode, EpisodePID: res.EpisodePID}
	if strings.TrimSpace(file.Path) != "" {
		dr, err := l.podcasts.ImportEpisodeFile(ctx, res.EpisodePID, file.Path, meta.Copy)
		if err != nil {
			return nil, err
		}
		out.FilePID, out.Path = dr.FilePID, dr.Path
	}
	// Record origin provenance on the episode item, so reads and queries can report
	// where it came from.
	if err := l.store.PutAcquisition(ctx, res.EpisodePID, *acquisitionInput(meta)); err != nil {
		return nil, err
	}
	return out, nil
}

// acquisitionInput builds the provenance row input from acquired metadata, defaulting
// the source type to manual (an explicitly acquired item is never plain local).
func acquisitionInput(meta AcquiredMeta) *model.AcquisitionInput {
	st := meta.SourceType
	if st == "" {
		st = model.SourceManual
	}
	return &model.AcquisitionInput{
		SourceType: st, SourceURL: meta.SourceURL, SourceID: meta.SourceID,
		Provider: meta.Provider, ProviderVersion: meta.ProviderVersion, OptionsJSON: meta.OptionsJSON,
	}
}

// Acquisition returns an item's origin provenance, or CodeNotFound when it was
// locally scanned (no acquisition row).
func (l *Library) Acquisition(ctx context.Context, pid model.PID) (*model.Acquisition, error) {
	return l.store.AcquisitionByItem(ctx, pid)
}

// Backup writes a self-contained byte copy of the catalog to dest. The copy
// contains every table, including the secret table; with redact, secrets are
// stripped from the copy while the live catalog is untouched. A full backup is
// the disaster-recovery artifact.
func (l *Library) Backup(ctx context.Context, dest string, redact bool) error {
	if err := l.store.BackupTo(ctx, dest); err != nil {
		return err
	}
	if redact {
		return port.RedactBackupFile(ctx, dest)
	}
	return nil
}

// Export writes a versioned logical JSON export of catalog metadata plus critical
// per-user playback state. It never contains secrets and is for inspection and
// cross-tool portability; a byte Backup is the disaster-recovery path. It
// returns the export manifest.
func (l *Library) Export(ctx context.Context, w io.Writer) (*port.Manifest, error) {
	allLibs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	// Podcast downloads are local cache. The portable record is the subscription
	// list, exported through OPML, not the internal library or remote episode rows.
	libs := make([]*model.Library, 0, len(allLibs))
	for _, lib := range allLibs {
		if lib.Mode != model.ModePodcast {
			libs = append(libs, lib)
		}
	}
	allItems, err := l.store.QueryItems(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		return nil, err
	}
	items := make([]*model.ItemView, 0, len(allItems))
	exported := make(map[model.PID]bool, len(allItems))
	for _, it := range allItems {
		if it.Kind != model.KindEpisode {
			items = append(items, it)
			exported[it.PID] = true
		}
	}
	allPlays, err := l.store.AllPlayStates(ctx)
	if err != nil {
		return nil, err
	}
	// Drop play states for items the export omits (episodes), so the manifest never
	// carries a play state referencing an item that is not in it.
	plays := make([]model.PlayState, 0, len(allPlays))
	for _, ps := range allPlays {
		if exported[ps.ItemPID] {
			plays = append(plays, ps)
		}
	}
	schema, err := l.store.CatalogVersion(ctx)
	if err != nil {
		return nil, err
	}

	// Capture each item's path relative to its library root, so the export carries
	// a portable rel path rather than a machine-specific absolute one.
	relByPID := make(map[model.PID]string, len(items))
	for _, it := range items {
		if it.DisplayPath == "" {
			continue
		}
		if lib := libraryContaining(libs, it.DisplayPath); lib != nil {
			root := lib.DisplayRoot
			if root == "" {
				root = string(lib.Root)
			}
			if rel, err := filepath.Rel(root, it.DisplayPath); err == nil {
				relByPID[it.PID] = rel
			}
		}
	}
	relOf := func(pid model.PID) string { return relByPID[pid] }

	snap := port.BuildSnapshot(schema, time.Now().UnixNano(), libs, items, plays, relOf)
	if err := port.WriteSnapshot(w, snap); err != nil {
		return nil, err
	}
	return &snap.Manifest, nil
}

// RelocateRoot re-points a library and every file under it at a new root path,
// for a portable restore onto a different machine or mount.
func (l *Library) RelocateRoot(ctx context.Context, libPID model.PID, newRoot string) error {
	return l.store.RelocateLibraryRoot(ctx, libPID, newRoot)
}

// SetSecret stores a named credential in the secret table. Values are never
// logged or written to a logical export, but a full byte Backup contains them
// unless redacted.
func (l *Library) SetSecret(ctx context.Context, key, value string) error {
	return l.store.SetSecret(ctx, key, value)
}

// GetSecret returns a stored credential, or CodeNotFound.
func (l *Library) GetSecret(ctx context.Context, key string) (string, error) {
	return l.store.GetSecret(ctx, key)
}

// DeleteSecret removes a stored credential.
func (l *Library) DeleteSecret(ctx context.Context, key string) error {
	return l.store.DeleteSecret(ctx, key)
}

// InboxFolders returns the configured staging folders.
func (l *Library) InboxFolders() []string { return l.opts.Inbox }

// resolveManagedLibrary returns the managed library identified by pid, or the
// single managed library when pid is empty.
func (l *Library) resolveManagedLibrary(ctx context.Context, pid model.PID) (*model.Library, error) {
	if pid == "" {
		return l.singleManagedLibrary(ctx)
	}
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	for _, lib := range libs {
		if lib.PID == pid {
			if lib.Mode != model.ModeManaged {
				return nil, waxerr.New(waxerr.CodeInvalid, "Library.import", "target library is not managed")
			}
			return lib, nil
		}
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "Library.import", "no such library: "+string(pid))
}

// libraryContaining returns the library whose root contains path, or nil.
func libraryContaining(libs []*model.Library, path string) *model.Library {
	for _, lib := range libs {
		root := lib.DisplayRoot
		if root == "" {
			root = string(lib.Root)
		}
		if pathx.UnderRoot(root, path) {
			return lib
		}
	}
	return nil
}

func (l *Library) resolveLibraries(ctx context.Context, pid model.PID) ([]*model.Library, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	if pid == "" {
		// Exclude the internal podcast library. scan/rebuild walk user roots; podcast
		// downloads are cataloged by the podcast engine.
		var userLibs []*model.Library
		for _, lib := range libs {
			if lib.Mode != model.ModePodcast {
				userLibs = append(userLibs, lib)
			}
		}
		if len(userLibs) == 0 {
			return nil, waxerr.New(waxerr.CodeInvalid, "Library.Scan", "no library roots configured")
		}
		return userLibs, nil
	}
	for _, lib := range libs {
		if lib.PID == pid {
			// A generic scan would catalog downloaded episodes as tracks, so refuse the
			// internal podcast library even when named directly.
			if lib.Mode == model.ModePodcast {
				return nil, waxerr.New(waxerr.CodeInvalid, "Library.Scan",
					"cannot scan the internal podcast library")
			}
			return []*model.Library{lib}, nil
		}
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "Library.Scan", "no such library: "+string(pid))
}

// managedLibraries returns every managed library, or an error when none exist.
func (l *Library) managedLibraries(ctx context.Context) ([]*model.Library, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	var managed []*model.Library
	for _, lib := range libs {
		if lib.Mode == model.ModeManaged {
			managed = append(managed, lib)
		}
	}
	if len(managed) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.managed", "no managed library configured")
	}
	return managed, nil
}

func (l *Library) singleManagedLibrary(ctx context.Context) (*model.Library, error) {
	managed, err := l.managedLibraries(ctx)
	if err != nil {
		return nil, err
	}
	if len(managed) != 1 {
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.managed",
			"multiple managed libraries configured; select one by kind or pid")
	}
	return managed[0], nil
}

// managedLibraryForKind picks the managed library for an item kind. A single
// type-specific library (music/audiobook) that accepts the kind wins over a mixed
// root. The choice errors when no library accepts the kind or more than one does.
func (l *Library) managedLibraryForKind(ctx context.Context, kind model.Kind) (*model.Library, error) {
	managed, err := l.managedLibraries(ctx)
	if err != nil {
		return nil, err
	}
	if lib := routeManaged(managed, kind); lib != nil {
		return lib, nil
	}
	return nil, waxerr.New(waxerr.CodeInvalid, "Library.import",
		"no managed library holds "+string(kind)+" media (or the choice is ambiguous); configure a media-typed root")
}

// routeManaged returns the managed library a kind routes to, or nil when there is no
// clear match. A single type-specific library wins; if none exists, a single mixed
// library wins. Any other case is ambiguous.
func routeManaged(managed []*model.Library, kind model.Kind) *model.Library {
	var typed, mixed *model.Library
	typedN, mixedN := 0, 0
	for _, lib := range managed {
		switch {
		case lib.MediaType() == model.MediaMixed:
			mixed, mixedN = lib, mixedN+1
		case lib.MediaType().Accepts(kind):
			typed, typedN = lib, typedN+1
		}
	}
	switch {
	case typedN == 1:
		return typed
	case typedN == 0 && mixedN == 1:
		return mixed
	default:
		return nil
	}
}

func addResult(dst *scan.Result, src *scan.Result) {
	dst.FilesSeen += src.FilesSeen
	dst.AudioFiles += src.AudioFiles
	dst.ItemsCreated += src.ItemsCreated
	dst.ItemsUpdated += src.ItemsUpdated
	dst.Relinked += src.Relinked
	dst.Unchanged += src.Unchanged
	dst.SidecarsUpdated += src.SidecarsUpdated
	dst.Missing += src.Missing
	dst.Skipped += src.Skipped
	dst.Errored += src.Errored
}
