package waxbin

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxbin/analyze"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/fingerprint"
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
	"github.com/colespringer/waxbin/waxerr"
)

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
		organizer: organize.New(st, log),
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
		}, log),
		decoders: decoders,
		log:      log,
		opts:     opts,
	}
	// The importer catalogs each placed file through the scanner, so it is wired
	// after the struct is built and shares that scanner.
	l.importer = inbox.New(st, meta.NewReader(), l.scanner, log)

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
}

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
	job, runErr := l.jobs.Run(ctx, "scan", "scan", func(ctx context.Context, h *jobs.Handle) error {
		for _, lib := range libs {
			r, err := l.scanner.Scan(ctx, scan.Request{Library: lib, SubPath: req.SubPath},
				func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
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

// Analyze runs the resumable analyze pass: it decodes (the only PCM-decoding
// stage), fingerprints, and indexes every audio file whose fingerprint is
// missing or stale, under an "analyze"-scoped job. Files whose codec this build
// cannot decode are reported as skipped, not failed.
func (l *Library) Analyze(ctx context.Context) (*AnalyzeResult, error) {
	out := &AnalyzeResult{}
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
		return l.store.RefreshAlbumGain(ctx)
	})
	if job != nil {
		out.JobPID = job.PID
	}
	return out, runErr
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
		sim := fingerprint.Similar(qSub, fingerprint.Unpack(c.FP))
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

// PlanOrganize computes a dry-run move plan for the selected items under a
// profile. This build organizes one managed library at a time.
func (l *Library) PlanOrganize(ctx context.Context, q query.Query, profileName string) (*organize.Plan, error) {
	lib, err := l.singleManagedLibrary(ctx)
	if err != nil {
		return nil, err
	}
	// Default to the library's configured profile so a root registered
	// `:managed:waxbin-native` lays out as waxbin-native without repeating --profile.
	if profileName == "" {
		profileName = lib.Profile
	}
	prof, err := l.profiles.ByName(profileName)
	if err != nil {
		return nil, err
	}
	items, err := l.store.QueryItems(ctx, q)
	if err != nil {
		return nil, err
	}
	return l.organizer.Plan(ctx, lib, prof, items)
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
	_, err := l.jobs.Run(ctx, "organize", "organize", func(ctx context.Context, h *jobs.Handle) error {
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
	_, err := l.jobs.Run(ctx, "delete", "delete", func(ctx context.Context, h *jobs.Handle) error {
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
	_, err = l.jobs.Run(ctx, "restore", "delete", func(ctx context.Context, h *jobs.Handle) error {
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
	_, err := l.jobs.Run(ctx, "empty-trash", "delete", func(ctx context.Context, h *jobs.Handle) error {
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
	lib, err := l.resolveManagedLibrary(ctx, req.LibraryPID)
	if err != nil {
		return nil, err
	}
	profileName := req.Profile
	if profileName == "" {
		profileName = lib.Profile
	}
	prof, err := l.profiles.ByName(profileName)
	if err != nil {
		return nil, err
	}
	return l.importer.Plan(ctx, inbox.Request{
		Source: req.Source, Library: lib, Profile: prof, DupPolicy: req.DupPolicy,
		Copy: req.Copy, ReserveBytes: l.opts.FreeSpaceReserveBytes,
	})
}

// ApplyImport executes an import plan under an "import"-scoped job.
func (l *Library) ApplyImport(ctx context.Context, plan *inbox.Plan) (*inbox.Report, error) {
	var rep *inbox.Report
	_, err := l.jobs.Run(ctx, "import", "import", func(ctx context.Context, h *jobs.Handle) error {
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
	for _, it := range allItems {
		if it.Kind != model.KindEpisode {
			items = append(items, it)
		}
	}
	plays, err := l.store.AllPlayStates(ctx)
	if err != nil {
		return nil, err
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

func (l *Library) singleManagedLibrary(ctx context.Context) (*model.Library, error) {
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
	switch len(managed) {
	case 1:
		return managed[0], nil
	case 0:
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.Organize", "no managed library to organize")
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.Organize",
			"this build organizes a single managed library; multiple are configured")
	}
}

func addResult(dst *scan.Result, src *scan.Result) {
	dst.FilesSeen += src.FilesSeen
	dst.AudioFiles += src.AudioFiles
	dst.ItemsCreated += src.ItemsCreated
	dst.ItemsUpdated += src.ItemsUpdated
	dst.Relinked += src.Relinked
	dst.Skipped += src.Skipped
	dst.Errored += src.Errored
}
