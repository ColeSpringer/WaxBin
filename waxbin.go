package waxbin

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
// Config, resolving the cover-art and lyrics defaults (each on unless explicitly
// disabled) and attaching any injected providers. The injected providers outrank the
// key-free built-ins for a value conflict.
func enrichConfig(c config.EnrichConfig, providers []enrich.Provider) enrich.Config {
	return enrich.Config{
		Contact:              c.Contact,
		UserAgent:            c.UserAgent,
		AcoustIDKey:          c.AcoustIDKey,
		FetchCoverArt:        c.CoverArt == nil || *c.CoverArt,
		FetchLyrics:          c.Lyrics == nil || *c.Lyrics,
		FetchCommunityGenres: c.CommunityGenres == nil || *c.CommunityGenres,
		MatchReleases:        c.MatchReleases == nil || *c.MatchReleases,
		Providers:            providers,
		BlockPrivateIPs:      c.BlockPrivateIPs,
		Timeout:              time.Duration(c.TimeoutSeconds) * time.Second,
		MusicBrainzBaseURL:   c.MusicBrainzBaseURL,
		CoverArtBaseURL:      c.CoverArtBaseURL,
		AcoustIDBaseURL:      c.AcoustIDBaseURL,
		ListenBrainzBaseURL:  c.ListenBrainzBaseURL,
		LRCLibBaseURL:        c.LRCLibBaseURL,
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
	decoder   *decode.Engine
	log       *slog.Logger
	opts      Options

	// jobsWG tracks in-flight asynchronous (server-run) jobs started via startJob, so
	// Close drains them against the still-open store instead of tearing it down mid-job.
	jobsWG sync.WaitGroup

	// rootMu serializes runtime root-set mutations (AddRoot, RelocateRoot) so each
	// validates against the registered set and writes its row atomically. Without it,
	// two concurrent proxy connections could each validate before the other's row lands
	// and both commit overlapping roots, the state Open's validation forbids.
	rootMu sync.Mutex
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
		Path:               opts.DBPath,
		ReadOnly:           opts.ReadOnly,
		AllowStaleBaseline: opts.AllowStaleBaseline,

		Owner:         owner,
		IPCSocket:     opts.IPCSocket,
		Logger:        log,
		SecretCipher:  opts.SecretCipher,
		SecretKeyID:   opts.SecretKeyID,
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

	decoder := decode.New(log)
	l := &Library{
		store:     st,
		jobs:      jobs.NewManager(st, owner, log),
		scanner:   scan.New(st, meta.NewReader(), log),
		organizer: organize.New(st, meta.NewWriter(), log),
		profiles:  profiles,
		trasher:   trash.New(st, log),
		analyzer:  analyze.New(st, decoder, log),
		playback:  playback.New(st),
		playlists: playlist.New(st),
		enricher:  enrich.New(st, enrichConfig(opts.Enrichment, opts.EnrichmentProviders), log),
		decoder:   decoder,
		log:       log,
		opts:      opts,
	}
	// The importer catalogs each placed file through the scanner, so it is wired
	// after the struct is built and shares that scanner.
	l.importer = inbox.New(st, meta.NewReader(), l.scanner, log)

	// The podcast service takes a lease adapter closing over l, so it is wired after
	// the struct too.
	l.podcasts = podcast.New(st, meta.NewReader(), podcast.Config{
		Dir:               opts.Podcasts.Dir,
		UserAgent:         opts.Podcasts.UserAgent,
		BlockPrivateIPs:   opts.Podcasts.BlockPrivateIPs,
		Timeout:           time.Duration(opts.Podcasts.TimeoutSeconds) * time.Second,
		MaxFeedBytes:      opts.Podcasts.MaxFeedBytes,
		MaxEnclosureBytes: opts.Podcasts.MaxEnclosureBytes,
		ReserveBytes:      opts.FreeSpaceReserveBytes,
		DefaultRetention:  opts.Podcasts.DefaultRetention,
		Providers:         opts.SourceProviders,
		Leaser:            fsLeaser{lib: l},
	}, log)

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

// Close flushes buffered playback progress, then releases the catalog and write lock.
// The flush is best effort, since a flush error should not block release, and a
// read-only handle skips it.
//
// It first drains any in-flight server-run jobs so they finalize against the still-open
// store; otherwise a shutdown mid-scan leaves a job row stuck "running", reclaimed as
// crashed only on a later open.
func (l *Library) Close() error {
	l.jobsWG.Wait()
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

// AddRoot registers a library root at runtime, without reopening the Library. The
// spec is validated against every root already registered exactly as Open validates
// the configured set, then upserted: a new path emits a `library` create delta, and
// re-adding an existing path refreshes its mode/media/profile under the same pid.
// Spec defaults match config loading (in-place, mixed, waxbin-native).
//
// The store is the single source of truth for roots, so scan, organize, and import
// pick the new root up immediately and the row survives a restart even if the embedder
// never adds it to its own configuration. A running Watch does not pick it up, since
// watch snapshots its roots at start; the create delta on the change feed is the signal
// to restart the watcher.
func (l *Library) AddRoot(ctx context.Context, spec config.Root) (*model.Library, error) {
	if l.ReadOnly() {
		return nil, waxerr.New(waxerr.CodeUnsupported, "Library.AddRoot", "adding a root requires a read-write library")
	}
	// Hold rootMu across validate + upsert so a concurrent root mutation cannot
	// slip an overlapping row in between the two.
	l.rootMu.Lock()
	defer l.rootMu.Unlock()
	normalized, err := l.validateRootSet(ctx, "", spec)
	if err != nil {
		return nil, err
	}
	return l.store.EnsureLibrary(ctx, &model.Library{
		Root:        []byte(normalized.Path),
		DisplayRoot: normalized.Path,
		Mode:        normalized.Mode,
		Media:       normalized.Media,
		Profile:     normalized.Profile,
	})
}

// validateRootSet validates candidate against every registered root except the one
// the mutation lands on, reusing config.Validate so a runtime root mutation obeys the
// same rules as Open. The registered set comes from store.Libraries rather than
// l.opts.Roots, because the store is authoritative once AddRoot can grow it beyond
// the configured set. It returns the candidate normalized.
//
// The excluded row is replaceLibPID for a relocation, and the candidate's own path
// for an add (EnsureLibrary upserts by that key, so a re-add refreshes policy instead
// of colliding with itself). Not combined, deliberately: relocating onto another
// library's exact path is a genuine overlap and must keep that row in the set.
//
// The internal podcast library is not a config.Root, so it cannot ride in cfg.Roots.
// It is checked twice: config validation against cfg.Podcasts, and a direct overlap
// check against the podcast library row's stored root. The row is authoritative and
// covers what the configured dir does not, a process opened with Podcasts.Dir unset
// while a podcast row persists from a prior run.
func (l *Library) validateRootSet(ctx context.Context, replaceLibPID model.PID, candidate config.Root) (config.Root, error) {
	const op = "Library.validateRootSet"
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return config.Root{}, err
	}
	// Registered rows hold Validate-normalized paths, so normalizing the
	// candidate the same way makes the replacement match exact.
	candidatePath := ""
	if replaceLibPID == "" && strings.TrimSpace(candidate.Path) != "" {
		if abs, err := filepath.Abs(candidate.Path); err == nil {
			candidatePath = filepath.Clean(abs)
		}
	}
	roots := make([]config.Root, 0, len(libs)+1)
	var podcastRoots []string
	for _, lib := range libs {
		if lib.Mode == model.ModePodcast {
			podcastRoots = append(podcastRoots, string(lib.Root))
			continue
		}
		if lib.PID == replaceLibPID {
			continue
		}
		// Fold, not byte-compare: the store matches a root the same way
		// (libraryByRootDB), so a re-cased `library add` has to recognize itself here
		// too or it reports "library roots overlap" against the row it would update.
		if candidatePath != "" && pathx.SamePath(string(lib.Root), candidatePath) {
			continue
		}
		roots = append(roots, config.Root{
			Path: string(lib.Root), Mode: lib.Mode, Media: lib.Media, Profile: lib.Profile,
		})
	}
	roots = append(roots, candidate)
	cfg := &config.Config{
		DBPath: l.opts.DBPath,
		Roots:  roots,
		// Copy: Validate normalizes slices in place, and opts must stay untouched.
		Inbox:    append([]string(nil), l.opts.Inbox...),
		Podcasts: l.opts.Podcasts,
	}
	if err := cfg.Validate(); err != nil {
		return config.Root{}, err
	}
	normalized := cfg.Roots[len(cfg.Roots)-1]
	for _, pr := range podcastRoots {
		if pathx.UnderRoot(pr, normalized.Path) || pathx.UnderRoot(normalized.Path, pr) {
			return config.Root{}, waxerr.New(waxerr.CodeInvalid, op,
				"root "+normalized.Path+" overlaps the internal podcast library at "+pr)
		}
	}
	return normalized, nil
}

// Query runs a compiled selection and returns matching item views. A query that
// references a per-user field (starred, rating, play_count, played, finished,
// position_ms, last_played, and the rest) evaluates against userPID's play_state, where
// an empty userPID selects the default user. A query with no user-state field is not
// scoped by user.
//
// position_ms is the in-progress predicate: `position_ms gt 0` with `finished is 0` is
// "continue listening", and the only way to tell a started item from one merely opened.
// Browse the in-progress list for the ordered, paginated form.
func (l *Library) Query(ctx context.Context, q query.Query, userPID model.PID) ([]*model.ItemView, error) {
	return l.store.QueryItems(ctx, q, userPID)
}

// Count returns the number of items matching q. userPID scopes any per-user field
// the same way Query does.
func (l *Library) Count(ctx context.Context, q query.Query, userPID model.PID) (int, error) {
	return l.store.CountItems(ctx, q, userPID)
}

// Facet groups the items matching q by a dimension and counts each bucket. userPID
// scopes any per-user filter in q, and on a dimension read.GroupBy.UserScoped reports
// (GroupPlaylist alone) it also selects which buckets exist.
//
// order picks the bucket order (empty = collation order) and limit truncates the result
// (<= 0 = every bucket), which together give a top-N shelf. limit bounds only what is
// returned, not what is aggregated, so it is not a way to page a facet. Use EntityPage
// for an index over a large dimension.
func (l *Library) Facet(ctx context.Context, q query.Query, g read.GroupBy, order read.FacetOrder, limit int, userPID model.PID) (*read.FacetResult, error) {
	return l.store.Facet(ctx, q, g, order, limit, userPID)
}

// QueryPage returns one keyset-paginated, collation-correct window of items.
// Pass an empty cursor for the first page and the returned Next cursor for each
// subsequent page; pagination is stable under concurrent mutation. userPID scopes
// any per-user filter in q.
func (l *Library) QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool, userPID model.PID) (*read.Page, error) {
	return l.store.QueryPage(ctx, q, cursor, limit, desc, userPID)
}

// Browse returns one keyset-paginated window for a discovery list such as newest,
// recently-added, most-played, random, by-year, or in-progress. Play-derived lists
// read opt.UserPID's play_state (empty selects the default user); by-year, by-genre,
// and random use opt.Year, opt.GenrePID, and opt.Seed. Pagination is stable under
// concurrent mutation.
//
// Most lists span every kind. The two that do not are counterparts, each ordering by
// something only its own kinds carry: newest covers tracks and books by release year,
// recent-episodes covers episodes by publication date. See read.DiscoveryList.Kinds
// to avoid offering a media-type filter a list cannot answer.
//
// opt.Query scopes any list to the items it matches, through the filter engine Query
// uses. The list owns the order, so the query's own sort/limit/offset are ignored.
func (l *Library) Browse(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions) (*read.Page, error) {
	return l.store.BrowsePage(ctx, list, opt)
}

// Search runs a grouped, BM25-ranked search across artists, albums, tracks, books,
// and episodes (transcript-body hits rank below metadata hits). Field weighting puts
// title hits above artist and album hits. opt.MaxCandidates bounds how many matches
// are ranked, opt.Libraries and opt.States scope the search; see read.SearchOptions
// for those contracts.
func (l *Library) Search(ctx context.Context, q string, opt read.SearchOptions) (*read.SearchResult, error) {
	return l.store.Search(ctx, q, opt)
}

// ResolveArt resolves artwork for an entity in one role (empty = front). The front
// cover walks the fallback chain (track -> album -> release_group -> artist -> genre,
// or episode -> podcast) to the first level that has one. Every other role (back,
// disc, booklet, background) resolves at the requested level only, since an
// ancestor's auxiliary image would be misleading, and a playlist has no ancestry at
// all. A non-positive size returns the original image; a positive one returns a
// thumbnail fitted to a square box of that side, generated once and cached.
// CodeNotFound means no consulted level has art in that role.
func (l *Library) ResolveArt(ctx context.Context, ref model.EntityRef, role model.ArtRole, size int) (*model.ArtBlob, error) {
	return l.store.ResolveArt(ctx, ref, role, size)
}

// ArtRoles lists the artwork slots an entity holds at its own level (no chain
// fallback): each stored role with the source image's format, dimensions, and
// content hash.
//
// A Locked entry with an empty SourceHash is a lock with no artifact behind it, so
// check SourceHash before trying to fetch bytes. That is what a cleared and locked
// cover looks like, and reporting it is the only way that state is visible. An entity
// with no art returns an empty list only when it is also unlocked.
func (l *Library) ArtRoles(ctx context.Context, ref model.EntityRef) ([]model.ArtRoleInfo, error) {
	return l.store.ArtRoles(ctx, ref)
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

// File returns a backing file's identity and quality by its public id: size,
// mtime, content and essence hashes, the analyzed-essence stamp, and the audio
// quality fields (codec, bitrate, sample rate, bit depth), or CodeNotFound. It is
// the file-level companion to Get, so an embedder can pin an item's file identity
// without re-scanning.
func (l *Library) File(ctx context.Context, filePID model.PID) (*model.File, error) {
	return l.store.FileByPID(ctx, filePID)
}

// GetMany returns item views for the given pids in input order, skipping any pid
// with no matching item and collapsing a duplicate pid to its first position.
//
// It is not an atomic snapshot: a pid array longer than the internal batch size is
// read across several SELECTs, so a concurrent write between batches yields a mixed
// view. Fine for a UI list, not for a caller needing every pid at one instant.
func (l *Library) GetMany(ctx context.Context, pids []model.PID) ([]*model.ItemView, error) {
	return l.store.ItemsByPIDs(ctx, pids)
}

// ItemsByEssence returns every item backed by a file with the given audio essence
// hash. The hash survives a retag, which makes it the dedup oracle for "do I already
// hold this audio". A single-file CUE album returns one item per virtual track carved
// from the shared file. A clean miss is an empty slice, not an error.
func (l *Library) ItemsByEssence(ctx context.Context, essence string) ([]*model.ItemView, error) {
	return l.store.ItemsByEssence(ctx, essence)
}

// ItemsByContentHash returns every item backed by a file with the given
// whole-file content hash (identity.ContentHash, "sha256:" plus hex). Unlike
// the essence hash it changes on any byte change, tag writes included, which
// makes it the pre-transfer byte-identity probe: "do I already hold these
// exact bytes". Same result shape as ItemsByEssence.
func (l *Library) ItemsByContentHash(ctx context.Context, hash string) ([]*model.ItemView, error) {
	return l.store.ItemsByContentHash(ctx, hash)
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

// EntityByPID returns the summary info for one shared entity (artist, release group,
// album, genre, or series): name, sort key, external identifiers, parent links,
// membership counts, and the libraries its members' files live in. It answers the pid a
// facet bucket or an item view hands out, without a facet scan. An unknown kind is
// CodeInvalid, an unknown pid CodeNotFound.
func (l *Library) EntityByPID(ctx context.Context, kind read.EntityKind, pid model.PID) (*read.EntityInfo, error) {
	return l.store.EntityByPID(ctx, kind, pid)
}

// EntityByPIDs is the batched form of EntityByPID: summary info for many entities of
// one kind keyed by pid, omitting unknown pids and collapsing a repeated one. It
// retires the per-hit cost when a consumer hydrates a page of entity pids. One kind per
// call, and a set larger than one internal chunk is not an atomic snapshot.
func (l *Library) EntityByPIDs(ctx context.Context, kind read.EntityKind, pids []model.PID) (map[model.PID]*read.EntityInfo, error) {
	return l.store.EntityByPIDs(ctx, kind, pids)
}

// EntityPage enumerates one kind's entities in collation order, keyset-paginated: an
// empty cursor for the first page, the returned Next for each one after. It is the
// alphabetical index a facet cannot be, since paging a facet would re-run the whole
// aggregation per page. See read.EntityPage for how its rollup-based counts differ from
// a facet's bucket counts.
func (l *Library) EntityPage(ctx context.Context, kind read.EntityKind, cursor read.Cursor, limit int) (*read.EntityPage, error) {
	return l.store.EntityPage(ctx, kind, cursor, limit)
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

// LatestChangeSeq returns the sequence number at the tail of the change feed, so a
// consumer can find where the feed ends without pulling a page of changes. An empty
// feed is 0.
//
// Read it before a bootstrap read and resume from it afterwards, never the other way
// round. There is no atomic snapshot-plus-seq, so a seq taken after the read
// permanently drops everything committed during it, while one taken before merely
// redelivers those deltas. Same order-first contract as Subscribe.
func (l *Library) LatestChangeSeq(ctx context.Context) (int64, error) {
	return l.store.LatestChangeSeq(ctx)
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

// PlayStatesForItems returns every user's playback state for each of the given items,
// keyed by item pid and ordered by user pid. Untouched and unknown items are absent.
// It goes through the playback service, so this process's buffered resume positions
// are overlaid while another process sees flushed state only; see
// playback.Service.StatesForItems for the exact contract.
func (l *Library) PlayStatesForItems(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.PlayState, error) {
	return l.playback.StatesForItems(ctx, itemPIDs)
}

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
	// IgnoreLocks re-derives every field from disk even when the user locked it, so a
	// `scan --force --ignore-locks` discards curated edits. Off by default: a scan
	// (including --force) preserves locked fields, and write-back is what propagates a
	// DB edit back onto disk.
	IgnoreLocks bool
}

// fsMutateScope is the shared lease scope held by every job that mutates files on
// disk (scan, organize, import, and trash moves), so at most one filesystem mutator
// runs at a time. Leases are per-scope, so separate scopes here would let a watch
// rescan race an in-flight organize. Read-only passes (analyze, enrich) keep their
// own scopes and can still overlap a scan.
//
// podcastFSScope is its sibling for the podcast download tree. They are separate
// because the trees are disjoint: nothing on fsMutateScope enters the podcast tree,
// so sharing one scope would only make the podcast verbs fail during a scan that
// cannot touch their files. ImportEpisodeFile spans both trees and takes both,
// fsMutateScope first (see fsLeaser.LeaseImport).
const (
	fsMutateScope  = "fs-mutate"
	podcastFSScope = "podcast-fs"
)

// fsLeaser adapts the job manager to podcast.Leaser, so the podcast package can
// serialize its filesystem verbs without knowing about jobs or scopes.
type fsLeaser struct{ lib *Library }

// Lease takes the podcast scope with no job row. Retention is the tempting exception
// and is deliberately not one: the watch loop runs ApplyRetentionAll every tick, and
// jobs.Run's CreateJob writes a change_log delta, so a row there would advance the
// change feed once per tick on a catalog where nothing happened.
func (f fsLeaser) Lease(ctx context.Context, fn func(context.Context) error) error {
	return f.lib.jobs.RunLeased(ctx, podcastFSScope, fn)
}

func (f fsLeaser) LeaseWait(ctx context.Context, fn func(context.Context) error) error {
	return f.lib.jobs.RunLeasedWait(ctx, podcastFSScope, fn)
}

// LeaseImport takes fsMutateScope then podcastFSScope. That order is fixed and
// nothing acquires them the other way round, so the pair cannot deadlock.
func (f fsLeaser) LeaseImport(ctx context.Context, fn func(context.Context) error) error {
	return f.lib.jobs.RunLeased(ctx, fsMutateScope, func(ctx context.Context) error {
		return f.lib.jobs.RunLeased(ctx, podcastFSScope, fn)
	})
}

// ScanResult reports a scan, including the job it ran under.
type ScanResult struct {
	JobPID model.PID
	Total  scan.Result
	Runs   []scan.Result
}

// Scan indexes the selected libraries under a single "scan"-scoped job. Rollups are
// maintained transactionally per scanned track, so no whole-catalog refresh is needed
// here. StartScan is the asynchronous variant a server exposes.
func (l *Library) Scan(ctx context.Context, req ScanRequest) (*ScanResult, error) {
	libs, err := l.resolveLibraries(ctx, req.LibraryPID)
	if err != nil {
		return nil, err
	}
	out := &ScanResult{}
	job, runErr := l.jobs.Run(ctx, "scan", fsMutateScope, l.scanWork(libs, req, out))
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
	job, runErr := l.jobs.Run(ctx, "analyze", "analyze", l.analyzeWork(writeRG, out))
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

// Watch runs a foreground watcher that keeps the catalog in sync with the filesystem
// until ctx is canceled (returning a CodeCanceled error on a clean shutdown). It
// refuses on a read-only library.
//
// It is a foreground mode, and a read-write WaxBin holds the catalog's exclusive
// advisory lock for the process lifetime, so while it runs every other mutating
// command in another terminal is refused (read-only queries are always allowed).
// Stop the watcher to mutate manually, or run waxbin serve, which proxies mutations
// over a local control socket. Idle lock release is deliberately post-1.0.
//
// The watched roots are snapshotted at start. A root registered later through AddRoot
// is scanned and organized but not watched until the watcher restarts; its create
// delta on the change feed is the signal to do so.
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
	// bumps SidecarsUpdated but not ItemsUpdated. Include it, or a sidecar-only change
	// reports changed=false and every downstream scheduler is silently skipped.
	changed := t.ItemsCreated > 0 || t.ItemsUpdated > 0 || t.Relinked > 0 || t.Missing > 0 || t.SidecarsUpdated > 0
	return changed, nil
}

func (e *watchEngine) Analyze(ctx context.Context) error {
	_, err := e.lib.Analyze(ctx, AnalyzeOptions{})
	return err
}

// logTick reports a per-tick failure at the level it deserves. A lease conflict is
// expected flow on a watch tick, not a fault: another mutator holds the scope and the
// next tick retries, so logging it at Warn is noise a healthy server emits routinely.
func (e *watchEngine) logTick(msg string, err error, args ...any) {
	args = append(args, "err", err)
	if waxerr.Is(err, waxerr.CodeConflict) {
		e.lib.log.Debug(msg+" (busy; retrying next tick)", args...)
		return
	}
	e.lib.log.Warn(msg, args...)
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
		e.logTick("watch: podcast retention", err)
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
			e.logTick("watch: inbox import", err, "folder", folder)
		}
	}
	return nil
}

// EnrichOptions controls a metadata enrichment run. ItemPID or EntityType+EntityPID
// (mutually exclusive) scope the pass to one item's or entity's enrichable targets: a
// track to its artist, album artist, release group, and lyrics; a book to its
// contributors and identifier fill; an entity to artist, release_group, or album (an
// album resolves to its parent release group). A scoped run implies Force.
type EnrichOptions struct {
	Force bool // re-enrich already-enriched entities
	Limit int  // cap on entities processed (0 = all needing enrichment)

	ItemPID    model.PID       // scope to one item's targets ("" = no item scope)
	EntityType read.EntityKind // with EntityPID: scope to one entity
	EntityPID  model.PID

	// WriteTags writes what the pass filled back into the backing files, ORed with
	// the library's WriteEnrichmentTags option so a run can opt in without config.
	WriteTags bool
}

// EnrichResult reports an enrichment run and the job it ran under.
type EnrichResult struct {
	JobPID model.PID
	Result enrich.Result
}

// enrichScope validates EnrichOptions' scoping fields and resolves them to
// explicit store targets, or returns nil for an unscoped pass. Validation errors
// (both scopes at once, half an entity scope) and resolution errors (unknown pid,
// an episode or unsupported entity kind) surface before any job starts. op is
// the calling method's error attribution (Enrich or StartEnrich).
func (l *Library) enrichScope(ctx context.Context, op string, opts EnrichOptions) (*model.EnrichScope, error) {
	hasItem := opts.ItemPID != ""
	hasEntity := opts.EntityType != "" || opts.EntityPID != ""
	switch {
	case hasItem && hasEntity:
		return nil, waxerr.New(waxerr.CodeInvalid, op, "scope by item or by entity, not both")
	case hasEntity && (opts.EntityType == "" || opts.EntityPID == ""):
		return nil, waxerr.New(waxerr.CodeInvalid, op, "an entity scope needs both a type and a pid")
	case hasItem:
		return l.store.EnrichScopeForItem(ctx, opts.ItemPID)
	case hasEntity:
		return l.store.EnrichScopeForEntity(ctx, opts.EntityType, opts.EntityPID)
	default:
		return nil, nil
	}
}

// Enrich runs the metadata enrichment pass under an "enrich"-scoped job: MusicBrainz
// release-group/artist/genre resolution (MBID-first), Cover Art Archive covers, and
// the optional AcoustID fallback. It is resumable and lock-respecting, caches provider
// responses, and degrades gracefully offline. It needs a MusicBrainz contact
// (Options.Enrichment.Contact) or returns CodeUnsupported. A scoped run holds the same
// lease as a full pass, so the two exclude each other.
func (l *Library) Enrich(ctx context.Context, opts EnrichOptions) (*EnrichResult, error) {
	out := &EnrichResult{}
	if !l.enricher.Enabled() {
		return out, waxerr.New(waxerr.CodeUnsupported, "waxbin.Enrich",
			"enrichment needs a MusicBrainz contact (set enrichment.contact / WAXBIN_ENRICH_CONTACT)")
	}
	scope, err := l.enrichScope(ctx, "waxbin.Enrich", opts)
	if err != nil {
		return out, err
	}
	job, runErr := l.jobs.Run(ctx, "enrich", "enrich", l.enrichWork(opts, scope, out))
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
	// altMinSharedTerms is the inverted-index candidate threshold, deliberately low:
	// the index needs recall, and full-fingerprint similarity below is the precision
	// filter. The 30-bit shingle terms are selective enough that the low threshold
	// does not flood verification.
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

// Peaks returns the stored waveform overview of an item's representative primary
// backing file, or CodeNotFound.
//
// For a multi-file audiobook that is one part of many, and not necessarily part one:
// the primary is whichever part was attached first, or the lowest-positioned survivor
// after a primary is detached. Use PeaksForItem for a scrubber over the whole book, or
// PeaksForFile with a pid from ItemFiles for a chosen part.
func (l *Library) Peaks(ctx context.Context, itemPID model.PID) (*model.PeaksData, error) {
	return l.store.LoadPeaks(ctx, itemPID)
}

// PeaksForFile returns one file's stored waveform overview, or CodeNotFound when it
// has none or the stored one predates the file's current audio. Every analyzed file
// has its own waveform, including every part of a multi-file book.
func (l *Library) PeaksForFile(ctx context.Context, filePID model.PID) (*model.PeaksData, error) {
	return l.store.LoadPeaksForFile(ctx, filePID)
}

// PeaksForItem returns every backing file's waveform for an item, in reading order:
// one entry for a track, one per analyzed part for a multi-file audiobook. It is one
// call where an ItemFiles plus a PeaksForFile per part would be N+1 round trips.
// Parts with no current waveform are omitted, so the result can be shorter than
// ItemFiles; an unknown item pid is CodeNotFound.
func (l *Library) PeaksForItem(ctx context.Context, itemPID model.PID) ([]model.ItemPeaks, error) {
	return l.store.LoadPeaksForItem(ctx, itemPID)
}

// ItemFiles returns every file backing an item in reading order: one row for a track or
// single-file book, one per part for a multi-file book. Each ref carries the file's pid,
// path, part position, and edge role, so a consumer can address a specific part and tell
// which one the primary-file reads answered for.
func (l *Library) ItemFiles(ctx context.Context, itemPID model.PID) ([]model.ItemFileRef, error) {
	return l.store.ItemFiles(ctx, itemPID)
}

// RefreshAlbumGain recomputes album-aware ReplayGain from the per-file loudness.
// Analyze runs it automatically; it is exposed for repair after a manual import.
func (l *Library) RefreshAlbumGain(ctx context.Context) error {
	return l.store.RefreshAlbumGain(ctx)
}

// Coverage reports per-codec analysis decode support for doctor.
func (l *Library) Coverage() []decode.FormatSupport { return decode.Coverage() }

// VerifyDerived runs the derived-data consistency check (FTS, rollups, and
// generated sort keys versus the source rows). It is read-only; it reports drift
// rather than repairing it.
func (l *Library) VerifyDerived(ctx context.Context) (*sqlite.DerivedReport, error) {
	return l.store.VerifyDerived(ctx)
}

// RefreshRollups recomputes the maintained rollups and every book's denormalized
// total duration, the repair for the rollup and book-duration drift VerifyDerived
// can report.
func (l *Library) RefreshRollups(ctx context.Context) error {
	return l.store.RefreshRollups(ctx)
}

// RefreshSortKeys rewrites every stored sort key that model.SortKey no longer
// generates, the repair for the sort-key drift VerifyDerived reports, and returns
// the number of rows rewritten. A rescan cannot heal one. Page cursors encode a
// sort key, so a consumer holding one should re-page from the start after a run
// that rewrote rows.
func (l *Library) RefreshSortKeys(ctx context.Context) (int, error) {
	return l.store.RefreshSortKeys(ctx)
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

// FileDiagnostics returns the persisted per-file diagnostics matching the filter (the
// zero filter returns everything), joined to each file's display path in a stable
// path/origin/code order. It is the query surface over the rows scan, organize,
// analyze, and edit write-back record, so a consumer can drive a review queue without
// running a full audit.
func (l *Library) FileDiagnostics(ctx context.Context, filter model.DiagnosticFilter) ([]model.FileDiagnostic, error) {
	return l.store.FileDiagnostics(ctx, filter)
}

// DiagnosticSummary returns the matching diagnostics grouped by writer, code,
// and severity, most severe first. The filter's dimensions apply; its Limit and
// Offset do not (a summary aggregates the whole match).
func (l *Library) DiagnosticSummary(ctx context.Context, filter model.DiagnosticFilter) ([]model.DiagnosticCount, error) {
	return l.store.DiagnosticSummary(ctx, filter)
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

// Merge collapses the loser entity onto the survivor: children (tracks, albums, genre
// links, contributor credits) are re-pointed, the survivor's MBID and enrichment
// marker are unioned when it lacks one, rollups are recomputed, and the loser is
// deleted. The survivor keeps its PID. It repairs audit's duplicate-entity findings,
// and is the seam enrichment uses to unify two heuristic rows that resolve to one
// MBID.
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

// Provenance returns an item's field provenance: the non-default scalar fields, plus
// an "art" row whenever the item carries a cover of its own, which the store overlays
// from the cover's own attribution. A non-empty result therefore does not mean the item
// was curated or locked; read Source and Locked per row. An item with no cover and no
// curated or locked field returns an empty slice.
func (l *Library) Provenance(ctx context.Context, pid model.PID) ([]model.FieldProvenance, error) {
	return l.store.FieldProvenance(ctx, pid)
}

// EditOptions controls a catalog field edit.
type EditOptions struct {
	// WriteBack also writes the new value into each backing file's on-disk tags. It is
	// off by default, so an edit is catalog-only unless the caller opts in.
	WriteBack bool
	// Lock locks each edited field against enrichment and organize overwrites. A user
	// edit is authoritative, so the CLI sets this by default. Pass false to leave the
	// field unlocked.
	Lock bool
	// Force overrides a lock. Without it, editing a locked field returns CodeLocked.
	Force bool
	// SkipLocked applies only to a multi-item batch: a locked item is skipped and
	// reported rather than failing the whole batch. Ignored by a single-item edit.
	SkipLocked bool
}

// BatchEditResult reports a multi-item edit's outcome: the items whose catalog edit
// applied, the items skipped because a target field was locked (skip-locked mode),
// and, for a write-back batch, the per-item on-disk write-back failures. The catalog
// edit is atomic; a WriteBackErrors entry means that item's catalog edit stands but
// its file tags did not follow.
type BatchEditResult struct {
	Edited          []model.PID
	Skipped         []model.PID
	WriteBackErrors map[model.PID]*WriteBackError
}

// EditField edits one metadata field on a track or book item. See EditFields.
func (l *Library) EditField(ctx context.Context, itemPID model.PID, field, value string, opts EditOptions) error {
	return l.EditFields(ctx, itemPID, map[string]string{field: value}, opts)
}

// EditFields applies metadata-field edits to a track or book item. It records user
// provenance and, unless told otherwise, locks each field so enrichment and organize
// leave it alone. Which fields are editable depends on the kind, and a field that does
// not apply to the item's kind is rejected.
//
// With opts.WriteBack set, the values are also written into the backing files' on-disk
// tags. A track writes every edited scalar to its file; a book writes the audiobook
// tags a scan reads back (title to ALBUM, author to ALBUMARTIST, narrator to NARRATOR
// and COMPOSER, series and sequence to GROUPING, plus genre and year) across all its
// parts, leaving the enrichment-only fields (subtitle, identifiers, publisher,
// description) DB-only.
//
// Write-back runs after the catalog edit has committed, so a file that cannot be
// written (a read-only mount, a permission error, an unrepresentable value, a file
// shared by several items) does not roll the edit back. EditFields then returns a
// *WriteBackError naming the failed files and records a per-file drift diagnostic.
// Surface that as "catalog updated, on-disk tag sync failed", not as a failed edit.
func (l *Library) EditFields(ctx context.Context, itemPID model.PID, edits map[string]string, opts EditOptions) error {
	if err := l.store.EditItemFields(ctx, itemPID, edits, model.SourceUser, opts.Lock, opts.Force); err != nil {
		return err
	}
	if !opts.WriteBack {
		return nil
	}
	return l.writeBackFields(ctx, itemPID, edits)
}

// EditManyFields applies the same field edits to several items in one atomic
// transaction, then optionally mirrors each item's new values into its on-disk tags.
// The catalog edit commits or rolls back as a whole. Write-back is per-item and
// best-effort: a failed sync lands in the result's WriteBackErrors rather than failing
// the batch. With opts.SkipLocked a locked item is skipped instead of failing.
//
// The returned *BatchEditResult is meaningful even when err is non-nil, because the
// catalog batch has already committed and err only reports a non-write-back failure
// during the on-disk pass. Inspect the result's Edited list in that case.
func (l *Library) EditManyFields(ctx context.Context, itemPIDs []model.PID, edits map[string]string, opts EditOptions) (*BatchEditResult, error) {
	res, err := l.store.EditManyFields(ctx, itemPIDs, edits, model.SourceUser, opts.Lock, opts.Force, opts.SkipLocked)
	if err != nil {
		return nil, err
	}
	out := &BatchEditResult{Edited: res.Edited, Skipped: res.Skipped}
	if !opts.WriteBack {
		return out, nil
	}
	return out, l.batchWriteBack(ctx, out, func(model.PID) map[string]string { return edits })
}

// EditItemsFields applies a per-item field-edit map to several items in one atomic
// transaction, where EditManyFields applies one map to every item. The batch commits
// or rolls back together; a duplicate pid or any invalid entry rejects it. Write-back
// then mirrors each item's own map into its tags, best-effort, as EditManyFields does;
// see there for the error contract.
func (l *Library) EditItemsFields(ctx context.Context, edits []model.ItemFieldEdit, opts EditOptions) (*BatchEditResult, error) {
	res, err := l.store.EditItemsFields(ctx, edits, model.SourceUser, opts.Lock, opts.Force, opts.SkipLocked)
	if err != nil {
		return nil, err
	}
	out := &BatchEditResult{Edited: res.Edited, Skipped: res.Skipped}
	if !opts.WriteBack {
		return out, nil
	}
	fieldsByPID := make(map[model.PID]map[string]string, len(edits))
	for _, e := range edits {
		fieldsByPID[e.ItemPID] = e.Fields
	}
	return out, l.batchWriteBack(ctx, out, func(pid model.PID) map[string]string { return fieldsByPID[pid] })
}

// batchWriteBack mirrors a committed batch's edits into each item's on-disk tags,
// recording a per-item WriteBackError on out instead of failing the rest. fieldsFor
// supplies the map applied to a given item: the shared one for EditManyFields, the
// item's own for EditItemsFields. Only a canceled context aborts the pass, and the
// catalog batch has committed either way, so the caller hands out back alongside it.
func (l *Library) batchWriteBack(ctx context.Context, out *BatchEditResult, fieldsFor func(model.PID) map[string]string) error {
	for _, pid := range out.Edited {
		wberr := l.writeBackFields(ctx, pid, fieldsFor(pid))
		if wberr == nil {
			continue
		}
		var wbe *WriteBackError
		if errors.As(wberr, &wbe) {
			if out.WriteBackErrors == nil {
				out.WriteBackErrors = make(map[model.PID]*WriteBackError)
			}
			out.WriteBackErrors[pid] = wbe
			continue
		}
		return wberr
	}
	return nil
}

// WriteBackFailure records one backing file whose on-disk tag write-back did not
// apply, with a human-readable reason.
type WriteBackFailure struct {
	FilePID model.PID
	Path    string
	Reason  string
}

// WriteBackError reports that a catalog edit committed but its on-disk tag write-back
// did not fully apply. The catalog holds the new values. The named files' tags stay
// out of sync until they are re-written, which is also recorded as a per-file
// diagnostic.
type WriteBackError struct {
	ItemPID  model.PID
	Edits    map[string]string
	Failures []WriteBackFailure
}

func (e *WriteBackError) Error() string {
	paths := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		paths = append(paths, f.Path)
	}
	return "catalog updated, but on-disk tag write-back failed for " +
		strconv.Itoa(len(e.Failures)) + " file(s): " + strings.Join(paths, ", ")
}

// writeBackFiles applies a per-file on-disk write across files, recording every refusal
// or failure on wbErr instead of aborting the rest. It is the shared engine behind every
// write-back fan-out. The caller supplies the file set and an apply closure; this handles
// the shared-or-virtual-file guard, the drift diagnostic, the unrepresented-value
// warning, and the optimistic file-state update, and writes a file backing several
// members once. Only a context cancellation is a hard error, and op names the caller
// in it.
func (l *Library) writeBackFiles(ctx context.Context, op string, files []model.ItemFileRef, wbErr *WriteBackError, apply func(w *meta.Writer, path string) (*meta.WriteResult, error)) error {
	w := meta.NewWriter()
	seen := make(map[model.PID]bool, len(files))
	for _, ref := range files {
		if ref.FilePID != "" {
			if seen[ref.FilePID] {
				continue
			}
			seen[ref.FilePID] = true
		}
		// A canceled context aborts the whole write-back rather than being recorded as a
		// per-file failure, so a genuine cancellation is not masked as a soft warning.
		if err := ctx.Err(); err != nil {
			return waxerr.FromContext(op, err, waxerr.CodeCanceled)
		}
		// The catalog edit is already committed, so a per-file store lookup failure is one
		// more write-back failure to record and move past, not a reason to report the
		// whole edit as failed and hide the committed catalog change.
		file, err := l.store.FileByPID(ctx, ref.FilePID)
		if err != nil {
			l.log.Warn("write-back file lookup", "file", ref.FilePID, "err", err)
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: string(ref.Path), Reason: err.Error()})
			continue
		}
		path := string(file.Path)

		// A file shared by several items, or one carrying offset windows, must not be
		// rewritten for one item, since its tags belong to the whole file. Refuse it,
		// record the drift, and move on.
		shared, err := l.store.FileSharedOrVirtual(ctx, ref.FilePID)
		if err != nil {
			l.log.Warn("write-back share check", "path", path, "err", err)
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: err.Error()})
			continue
		}
		if shared {
			const reason = "on-disk tag write-back is unavailable for a file shared by multiple items"
			l.recordWriteBackDrift(ctx, ref.FilePID, reason)
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: reason})
			continue
		}

		res, err := apply(w, path)
		if err != nil {
			l.log.Warn("tag write-back", "path", path, "err", err)
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: err.Error()})
			continue
		}
		// A value the on-disk format cannot store leaves the bytes unchanged but is still
		// a real loss, and WaxLabel reports it as an unrepresented warning even on a
		// no-op. Read the warnings before the no-op gate below so a lost value is recorded
		// as a drift diagnostic and a write-back failure instead of cleared as a clean sync.
		var lost []model.FileDiagnostic
		for _, wn := range res.Warnings {
			// A landed write whose post-commit step failed: worth a log line, but the
			// tags are on disk and match the catalog, so it is neither a lost value
			// nor drift.
			if wn.Code == meta.PostWriteWarningCode {
				l.log.Warn("tag write-back post-write", "path", path, "warning", wn.Message)
			}
			if wn.Unrepresented {
				lost = append(lost, model.FileDiagnostic{
					Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
					TagKey: wn.Key, Detail: wn.Message,
				})
			}
		}
		if len(lost) > 0 {
			l.log.Warn("tag value unrepresented", "path", path)
			if derr := l.store.PutFileDiagnostics(ctx, ref.FilePID, model.OriginEdit, lost); derr != nil {
				l.log.Warn("edit diagnostics", "path", path, "err", derr)
			}
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path,
				Reason: "some values could not be stored in this file's tag format"})
		} else if derr := l.store.PutFileDiagnostics(ctx, ref.FilePID, model.OriginEdit, nil); derr != nil {
			// The tags now match the catalog: clear any drift this file's edit left before.
			l.log.Warn("edit diagnostics clear", "path", path, "err", derr)
		}
		if !res.Changed {
			continue
		}
		// Record the re-tagged size/mtime/hash only when a concurrent scan or move has
		// not touched the file since we read it. A stale row heals on the next scan.
		if _, err := l.store.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
			FilePID:         ref.FilePID,
			ExpectedSize:    file.Size,
			ExpectedMTimeNS: file.MTimeNS,
			NewSize:         res.Size,
			NewMTimeNS:      res.MTimeNS,
			NewContentHash:  res.ContentHash,
		}); err != nil {
			l.log.Warn("edit file-state update", "path", path, "err", err)
		}
	}
	return nil
}

// writeBackFields mirrors committed catalog edits into the backing files' tags. Each
// file is written on its own; a refusal or failure records a drift diagnostic and joins
// a WriteBackError rather than aborting the rest, and a clean write clears any drift the
// file carried before.
//
// A track writes every edited scalar to its file (tagEditsForFields). A book writes the
// audiobook tags the scanner reads back (bookTagEditsForFields) across all of its parts,
// because a book's title and author are the key its parts group by and writing them to
// one part alone would split the book on rescan. Book fields the scanner cannot
// reconstruct from a tag (subtitle, asin, isbn, publisher, edition, description, mbid)
// stay DB-only, so a rescan can never undo them. An episode is refused.
func (l *Library) writeBackFields(ctx context.Context, itemPID model.PID, edits map[string]string) error {
	// Everything below runs after the catalog edit committed. A setup-lookup error is
	// therefore a write-back failure to report, not a hard error that would make the CLI
	// hide the committed catalog change.
	item, err := l.store.ItemByPID(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}

	var tagEdits []meta.TagEdit
	var files []model.ItemFileRef
	switch item.Kind {
	case model.KindTrack:
		tagEdits, err = tagEditsForFields(edits)
		if err != nil {
			return err
		}
		files, err = l.store.ItemFiles(ctx, itemPID)
		if err != nil {
			return writeBackSetupFailure(itemPID, edits, err)
		}
	case model.KindBook:
		// The series name shares its GROUPING tag with the sequence, so read the book's
		// current sequence when the series is edited, to write "<series> #<seq>" and keep
		// the sequence on disk.
		seriesSeq := ""
		if _, editingSeries := edits["series"]; editingSeries {
			detail, derr := l.store.BookByPID(ctx, itemPID)
			if derr != nil {
				return writeBackSetupFailure(itemPID, edits, derr)
			}
			seriesSeq = detail.SeriesSeq
		}
		tagEdits = bookTagEditsForFields(edits, seriesSeq)
		// A book edit that touched only DB-only fields has no on-disk representation; the
		// catalog edit stands and those fields are DB-only by design, so there is no drift.
		if len(tagEdits) == 0 {
			return nil
		}
		// Every part, not just the primary: a book's title and author are the key the
		// scanner groups its parts by, so writing them to one part alone would split a
		// multi-file book on the next rescan. The catalog reads a book's metadata from the
		// primary, so the same tags elsewhere are inert but keep every part on one key.
		files, err = l.store.ItemFiles(ctx, itemPID)
		if err != nil {
			return writeBackSetupFailure(itemPID, edits, err)
		}
	default:
		return l.refuseWriteBack(ctx, itemPID, edits,
			"on-disk tag write-back is not supported for "+string(item.Kind)+" items; the catalog edit was applied")
	}
	tagEdits, err = l.appendDerivedSortClears(ctx, itemPID, edits, tagEdits)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}

	wbErr := &WriteBackError{ItemPID: itemPID, Edits: edits}
	// A write-back on an item with no backing files, such as an archived item, has
	// nothing to write, and the catalog edit already committed. Report a skipped
	// write-back rather than a silent success, the same way refuseWriteBack does.
	if len(files) == 0 {
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: "no backing files present to write"})
		return wbErr
	}
	if err := l.writeBackFiles(ctx, "waxbin.EditFields", files, wbErr,
		func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.Apply(ctx, path, tagEdits)
		}); err != nil {
		return err
	}
	// A book's title and author are its identity anchor, unlike a track's essence, so
	// writing them to disk would make the next scan --force re-key the book to a new pid
	// and drop its locks. Re-anchor to the file's post-write value instead. It reads the
	// file's actual state, so a partial write-back failure is safe: a part still holding
	// the old value re-anchors to a no-op.
	if item.Kind == model.KindBook && len(files) > 0 && bookIdentityEdited(edits) {
		l.reanchorBookIdentity(ctx, itemPID, files[0].FilePID)
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}

// appendDerivedSortClears adds the tag clears that keep a file's derived sort names in
// step with a display-name edit. Editing composer, artist, or a book's author
// regenerates the stored sort key, but a stale COMPOSERSORT, ARTISTSORT, or
// ALBUMARTISTSORT left in the file would feed the next scan's derivation and revert it.
// Clearing the tag makes a scan derive the same key from the display name. A sort the
// caller edited explicitly wins, and a locked sort keeps its tag.
func (l *Library) appendDerivedSortClears(ctx context.Context, itemPID model.PID, edits map[string]string, tagEdits []meta.TagEdit) ([]meta.TagEdit, error) {
	for _, p := range meta.DerivedSortPairs() {
		if _, edited := edits[p.Field]; !edited {
			continue
		}
		if p.SortField != "" {
			if _, explicit := edits[p.SortField]; explicit {
				continue
			}
			locked, err := l.store.IsFieldLocked(ctx, itemPID, p.SortField)
			if err != nil {
				return nil, err
			}
			if locked {
				continue
			}
		}
		tagEdits = append(tagEdits, meta.TagEdit{Key: p.TagKey})
	}
	return tagEdits, nil
}

// bookIdentityEdited reports whether an edit touched a book field that participates in
// the book's identity key (title or author), so its on-disk write-back needs an identity
// re-anchor. The other identity-key inputs (asin/isbn/edition) are DB-only and never
// written to disk, so they cannot move the on-disk-derived key.
func bookIdentityEdited(edits map[string]string) bool {
	// Every field identity.BookKey reads that also reaches disk. Without asin and isbn
	// here, an edit stamps a new identifier onto the files while the stored key stays
	// behind, so the next scan resolves a different item and orphans this one's pid,
	// play state, and locks. edition is a BookKey input but nothing writes it to disk;
	// adding it there means adding it here.
	for _, f := range []string{"title", "author", "asin", "isbn"} {
		if _, ok := edits[f]; ok {
			return true
		}
	}
	return false
}

// reanchorBookIdentity recomputes a book's identity key from the current on-disk state
// of one of its files, so a later scan --force resolves the same item. It reads the
// file's actual tags, so it self-corrects: a write-back that did not land leaves the old
// title in place and the re-key is a no-op. Best-effort, running after the catalog edit
// committed, so a failure is logged rather than surfaced.
func (l *Library) reanchorBookIdentity(ctx context.Context, itemPID, filePID model.PID) {
	file, err := l.store.FileByPID(ctx, filePID)
	if err != nil {
		l.log.Warn("book re-anchor file lookup", "item", itemPID, "err", err)
		return
	}
	fm, err := meta.NewReader().Read(ctx, string(file.Path))
	if err != nil {
		l.log.Warn("book re-anchor read", "item", itemPID, "err", err)
		return
	}
	// An empty key means the file now has no title/author, so the scanner would fall back
	// to an essence-anchored key. Leave the stored key alone rather than guess the essence
	// fallback here; clearing both identity fields is a degenerate edit.
	newKey := scan.BookIdentityKey(fm.Tags)
	if newKey == "" {
		return
	}
	if _, err := l.store.RekeyBook(ctx, itemPID, newKey); err != nil {
		l.log.Warn("book identity re-anchor", "item", itemPID, "err", err)
	}
}

// bookTagEditsForFields maps committed book field edits to the on-disk tags the
// audiobook scanner reads back, so a book edit round-trips through a rescan. It covers
// only the fields the scanner reconstructs from a tag; the rest stay DB-only. seriesSeq
// is the book's current sequence, folded into the GROUPING value only when "series" is
// among the edits. A value empty after trimming clears its tag.
func bookTagEditsForFields(edits map[string]string, seriesSeq string) []meta.TagEdit {
	out := make([]meta.TagEdit, 0, len(edits))
	add := func(key, value string) {
		e := meta.TagEdit{Key: key}
		if v := strings.TrimSpace(value); v != "" {
			e.Values = []string{v}
		}
		out = append(out, e)
	}
	for field, value := range edits {
		// The catalog stored the normalized form (editfield.go), so write that and not
		// the caller's raw string: a hyphenated ISBN on disk is read back verbatim by
		// the next rescan and undoes the normalization. Same reason the track twin
		// normalizes; a value that fails its check is written as given, matching there.
		if v, ok := model.NormalizeIdentifierField(field, strings.TrimSpace(value)); ok {
			value = v
		}
		if field == "series" {
			// The series name and sequence share one GROUPING tag; pack them so a rescan
			// splits them back apart into the same name and sequence.
			add(meta.BookSeriesTagKey, meta.PackSeriesGrouping(value, seriesSeq))
			continue
		}
		keys, ok := meta.BookFieldTagKeys(field)
		if !ok {
			continue // a DB-only book field: subtitle, asin, isbn, publisher, edition, description, mbid
		}
		for _, k := range keys {
			add(k, value)
		}
	}
	return out
}

// refuseWriteBack reports that write-back was not attempted for an item, such as a
// book whose on-disk tag conventions need their own design. The catalog edit already
// committed, so it records a drift diagnostic on each backing file and returns a
// WriteBackError, the same shape the per-file refusal path returns.
func (l *Library) refuseWriteBack(ctx context.Context, itemPID model.PID, edits map[string]string, reason string) error {
	wbErr := &WriteBackError{ItemPID: itemPID, Edits: edits}
	files, err := l.store.ItemFiles(ctx, itemPID)
	if err != nil {
		// The catalog edit already committed, so report the refusal as a setup failure
		// rather than a hard error that would mask it.
		return writeBackSetupFailure(itemPID, edits, err)
	}
	for _, ref := range files {
		l.recordWriteBackDrift(ctx, ref.FilePID, reason)
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: string(ref.Path), Reason: reason})
	}
	if len(wbErr.Failures) == 0 {
		// An archived item has no backing files. Still report the refusal so the caller
		// does not read a silent success.
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: reason})
	}
	return wbErr
}

// recordWriteBackDrift stamps a queryable diagnostic that a file's on-disk tags are
// out of sync with the catalog because write-back did not apply, so WaxDeck's review
// queue can find it. It is best-effort, and a diagnostic write failure is logged
// rather than surfaced.
func (l *Library) recordWriteBackDrift(ctx context.Context, filePID model.PID, detail string) {
	diags := []model.FileDiagnostic{{
		Code:     model.DiagTagWriteUnsynced,
		Severity: model.SeverityWarn,
		Detail:   detail,
	}}
	if err := l.store.PutFileDiagnostics(ctx, filePID, model.OriginEdit, diags); err != nil {
		l.log.Warn("edit drift diagnostic", "file", filePID, "err", err)
	}
}

// tagEditsForFields turns committed field edits into on-disk tag edits. Each tag key
// comes from meta.TagKeyForField, the source of truth shared with organize. Values are
// trimmed and identifier-normalized the same way the store persisted them, so neither
// whitespace nor an identifier's separators can put the tag out of step with the
// column. An empty value clears the tag. An unmapped field is a programming error and
// returns CodeInternal.
func tagEditsForFields(edits map[string]string) ([]meta.TagEdit, error) {
	out := make([]meta.TagEdit, 0, len(edits))
	for field, value := range edits {
		key, ok := meta.TagKeyForField(field)
		if !ok {
			return nil, waxerr.New(waxerr.CodeInternal, "waxbin.EditFields", "no tag key for field: "+field)
		}
		e := meta.TagEdit{Key: key}
		// compilation is a flag: normalize whatever spelling the user gave to the
		// canonical "1"/"0" the COMPILATION tag expects (the store has already
		// validated it is a boolean), and always write it rather than clearing.
		if field == "compilation" {
			e.Values = []string{compilationTagValue(value)}
		} else if v, _ := model.NormalizeIdentifierField(field, strings.TrimSpace(value)); v != "" {
			// The store already rejected a malformed identifier before commit, so the
			// normalization here cannot fail; it just reproduces the stored form.
			e.Values = []string{v}
		}
		out = append(out, e)
	}
	return out, nil
}

// compilationTagValue maps a validated boolean edit value to the "1"/"0" the
// COMPILATION tag uses, via the SAME vocabulary the store validated it against (so a
// spelling the store accepts can never write the wrong tag). The store rejects an
// un-parseable value before write-back, so ok is always true here.
func compilationTagValue(value string) string {
	if v, _ := model.ParseBoolValue(value); v {
		return "1"
	}
	return "0"
}

// writeBackSetupFailure wraps a store-lookup error hit while preparing a write-back
// into a WriteBackError. The catalog edit already committed, so the caller needs to
// learn that the edit stands and only the on-disk sync could not run.
func writeBackSetupFailure(itemPID model.PID, edits map[string]string, err error) *WriteBackError {
	return &WriteBackError{
		ItemPID: itemPID, Edits: edits,
		Failures: []WriteBackFailure{{Reason: "write-back could not run: " + err.Error()}},
	}
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
	// Organize acts on catalog rows; a per-user filter in q resolves against the
	// default user.
	items, err := l.store.QueryItems(ctx, q, "")
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

// PlanDelete computes a dry-run deletion plan for the items matching q under a mode
// (trash|prune|permanent). DeleteTrash moves files to the reversible per-library trash;
// the other modes bypass it to reclaim space. Every mode keeps the logical item,
// archived when it loses its last file.
//
// Items in the internal podcast library are dropped before planning and counted as
// SkippedPodcast, so a sweep over a mixed query does not fail because it matched an
// episode. The count is surfaced rather than dropped, since a silent skip would
// misreport what the sweep covered.
func (l *Library) PlanDelete(ctx context.Context, q query.Query, mode model.DeleteMode) (*trash.Plan, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	// Delete acts on catalog rows; a per-user filter in q resolves against the
	// default user.
	matched, err := l.store.QueryItems(ctx, q, "")
	if err != nil {
		return nil, err
	}
	items := make([]*model.ItemView, 0, len(matched))
	skipped := 0
	for _, it := range matched {
		if podcastOwned(libs, it) {
			skipped++
			continue
		}
		items = append(items, it)
	}
	plan, err := l.trasher.Plan(ctx, libs, items, mode)
	if err != nil {
		return nil, err
	}
	plan.SkippedPodcast = skipped
	return plan, nil
}

// PlanDeletePIDs computes a deletion plan for explicit item pids. It is the
// `rm <pid>` path; PlanDelete is the query-driven path used by retention/dedup.
//
// An episode is refused here rather than skipped: the caller named the item, so a
// silent skip would lie about what happened. See inPodcastLibrary.
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
		if podcastOwned(libs, it) {
			return nil, waxerr.New(waxerr.CodeInvalid, "Library.PlanDeletePIDs",
				"cannot delete a podcast episode: "+string(pid)+
					"; use `podcast unfetch` to reclaim its bytes and keep it re-fetchable")
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

// MarkMissingOptions tunes a mark-missing.
type MarkMissingOptions struct {
	// Force skips the on-disk verification and the mount gate, for a caller whose view of
	// the filesystem is the authoritative one (one in a different container, say). The
	// store's state rule still applies, so it cannot turn an archived item into a missing
	// one.
	Force bool
}

// MarkMissing records that an item's bytes are gone, for a caller that discovered it
// out of band and would otherwise keep requeuing doomed work. The outcome says what
// happened, refusals included: files-present means the bytes really are on disk and the
// caller's failure is something else.
//
// It verifies before it writes. Every backing file is stat'ed and the item is marked
// only when none is on disk; a stat failing with anything other than "does not exist"
// refuses the call with CodeIO rather than answering from a partial view. Once a file
// comes back absent the owning library root is stat'ed too, and an absent, unreadable,
// or non-directory root is a dropped mount rather than a deletion, so it is refused.
// The gate is on the root and not each file's parent directory because the ordinary
// genuine deletion is a user removing an album folder, which a parent gate would refuse.
// A file under no registered root has no mount to check.
//
// An item with no file rows has nothing to stat and is answered from state alone. Only
// the named item is marked: siblings sharing one file (a rip carved into virtual tracks)
// keep their state, so a caller repairing such a rip passes every pid. It takes no job
// lease, since it only stats and writes one transaction; racing a scan is benign,
// because the last commit wins and missing is recoverable by the next scan.
func (l *Library) MarkMissing(ctx context.Context, itemPID model.PID, opts MarkMissingOptions) (model.MarkMissingOutcome, error) {
	const op = "Library.MarkMissing"
	if !opts.Force {
		files, err := l.store.ItemFiles(ctx, itemPID)
		if err != nil {
			return "", err
		}
		present, absent := false, false
		for _, ref := range files {
			// pathx.Long or a Windows long path reports a present file as absent, which
			// here would flip a perfectly present item to missing.
			switch _, err := os.Stat(pathx.Long(string(ref.Path))); {
			case err == nil:
				present = true
			case errors.Is(err, fs.ErrNotExist):
				absent = true
			default:
				return "", waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if present {
			return model.OutcomeFilesPresent, nil
		}
		if absent {
			if err := l.checkRootsMounted(ctx, files, op); err != nil {
				return "", err
			}
		}
	}
	return l.store.MarkItemMissing(ctx, itemPID)
}

// checkRootsMounted refuses when a library root holding one of these files is not a
// readable directory, which is a dropped mount rather than a deletion. The roots are
// listed once per call and each distinct one is stat'ed once, however many parts sit
// under it.
func (l *Library) checkRootsMounted(ctx context.Context, files []model.ItemFileRef, op string) error {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return err
	}
	checked := map[model.PID]bool{}
	for _, ref := range files {
		lib := libraryForRawPath(libs, ref.Path)
		if lib == nil || checked[lib.PID] {
			continue
		}
		checked[lib.PID] = true
		info, err := os.Stat(pathx.Long(rawRoot(lib)))
		if err != nil || !info.IsDir() {
			return waxerr.New(waxerr.CodeIO, op, "library root "+lib.DisplayRoot+
				" is not a readable directory, so its files cannot be confirmed gone;"+
				" remount it, or narrow a rescan with --sub-path, or pass force to record it anyway")
		}
	}
	return nil
}

// libraryForRawPath returns the library whose root contains a raw path, or nil. It
// matches raw bytes on both sides, unlike libraryContaining, because its caller stats
// the result and DisplayRoot is a lossy UTF-8 rendering: a root carrying non-UTF-8
// bytes would stat as absent. Roots are validated non-overlapping, so the first match
// is the only one.
func libraryForRawPath(libs []*model.Library, path []byte) *model.Library {
	for _, lib := range libs {
		if pathx.UnderRoot(rawRoot(lib), string(path)) {
			return lib
		}
	}
	return nil
}

// rawRoot returns a library's root as raw OS bytes, falling back to the display
// rendering only when the raw column is empty.
func rawRoot(lib *model.Library) string {
	if len(lib.Root) > 0 {
		return string(lib.Root)
	}
	return lib.DisplayRoot
}

// Trash lists trash journal entries, newest first. includeRestored controls
// whether already-restored rows are shown; limit 0 returns all.
func (l *Library) Trash(ctx context.Context, includeRestored bool, limit int) ([]model.TrashEntry, error) {
	return l.store.TrashEntries(ctx, includeRestored, 0, limit)
}

// RestorableTrash returns the restorable trash entries for each of the given items,
// keyed by item pid, newest first. An item with nothing restorable is absent, and so is
// an unknown pid, which is what makes it safe against an item purged between a delta
// page and this lookup. It takes no limit deliberately: a bounded listing that missed
// an item would read as permanently removed.
func (l *Library) RestorableTrash(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.TrashEntry, error) {
	return l.store.ActiveTrashForItems(ctx, itemPIDs)
}

// RestoreTrash undoes a delete: it moves the trashed file back to its original path and
// re-scans it so the catalog re-links it, un-archiving its item. It refuses if the
// original path is occupied.
//
// A file whose original path is in the internal podcast library is also refused. The
// re-scan goes straight to the scanner and would generic-scan a library
// resolveLibraries exists to keep out, cataloging the episode as a track. Nothing new
// can enter the trash from there, but a pre-1.0 catalog may already hold such an
// entry.
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
	if lib.Mode == model.ModePodcast {
		return waxerr.New(waxerr.CodeInvalid, "Library.RestoreTrash",
			"cannot restore a file into the internal podcast library; re-download the episode instead")
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

// EmptyTrashOptions scopes an empty-trash pass.
type EmptyTrashOptions struct {
	// OlderThan keeps recently-trashed files: only entries trashed strictly
	// before now-OlderThan are purged, so an undo window survives a routine
	// space-reclaim pass. Zero purges everything; negative is refused.
	OlderThan time.Duration
}

// EmptyTrash permanently removes active trashed files from disk and drops their
// journal rows, reclaiming space. Options.OlderThan narrows the pass to entries
// older than that window. It runs under a "delete"-scoped job. Each purged entry
// announces itself on the change feed as an item update, so a client holding a copy
// of an already-archived item learns those bytes are now unrecoverable.
func (l *Library) EmptyTrash(ctx context.Context, opts EmptyTrashOptions) (*EmptyReport, error) {
	if opts.OlderThan < 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.EmptyTrash", "older-than window cannot be negative")
	}
	var cutoff int64
	if opts.OlderThan > 0 {
		cutoff = time.Now().Add(-opts.OlderThan).UnixNano()
	}
	rep := &EmptyReport{}
	_, err := l.jobs.Run(ctx, "empty-trash", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		entries, err := l.store.TrashEntries(ctx, false, cutoff, 0)
		if err != nil {
			return err
		}
		for i := range entries {
			if ctx.Err() != nil {
				return waxerr.FromContext("Library.EmptyTrash", ctx.Err(), waxerr.CodeIO)
			}
			// One entry's failure must not abort the pass: a purge error leaves the entry
			// retryable, and a row-delete failure after a successful purge would strand an
			// active journal row whose file is already gone. Tally it and move on, since a
			// later re-run drops the row.
			size, perr := l.purgeTrashEntry(ctx, entries[i])
			if perr != nil {
				rep.Errored++
				l.log.Warn("purging trash entry", "trash", entries[i].PID, "path", entries[i].TrashDisplay, "err", perr)
				continue
			}
			rep.Purged++
			rep.ReclaimedBytes += size
		}
		return nil
	})
	return rep, err
}

// PurgeTrash permanently removes a single active trash entry (file and journal row),
// returning the bytes reclaimed, and announces it on the change feed as an item update
// (see DeleteTrashRow for why the item's own columns do not move). A pid that is
// unknown, already purged, or already restored is CodeNotFound. It runs under an
// fs-mutate job and resolves the entry inside the lease, so a restore racing in cannot
// slip between the check and the purge.
func (l *Library) PurgeTrash(ctx context.Context, trashPID model.PID) (int64, error) {
	var size int64
	_, err := l.jobs.Run(ctx, "purge-trash", fsMutateScope, func(ctx context.Context, h *jobs.Handle) error {
		entry, err := l.store.ActiveTrashByPID(ctx, trashPID)
		if err != nil {
			return err
		}
		n, err := l.purgeTrashEntry(ctx, *entry)
		size = n
		return err
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// purgeTrashEntry is the shared purge step: remove the trashed file (and its
// unique trash sub-directory) from disk, then drop the journal row. Returns the
// bytes reclaimed.
func (l *Library) purgeTrashEntry(ctx context.Context, e model.TrashEntry) (int64, error) {
	size, err := l.trasher.Purge(e)
	if err != nil {
		return 0, err
	}
	if err := l.store.DeleteTrashRow(ctx, e.PID); err != nil {
		return 0, err
	}
	return size, nil
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

	// AlreadyPresent reports that the acquired file's audio essence is already in the
	// catalog, resolved independent of DupPolicy, so a DupAllow import of a genuine
	// duplicate still reports it. AlreadyPresentPID names the existing item. Set for the
	// track/book path only; the host can pre-check an episode via ResolveRef. Whether the
	// file was skipped or imported is the action's Outcome.
	AlreadyPresent    bool
	AlreadyPresentPID model.PID
}

// ImportAcquired routes an acquired or manual file by kind. Tracks and books go through
// the import planner for the matching managed library, with duplicate checks,
// destination rendering, free-space checks, and acquisition provenance. Episodes go into
// the internal podcast library under an existing or manual show, pinned by default.
// WaxBin never performs platform extraction itself.
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
	res := &AcquiredResult{Kind: kind, Plan: plan}
	// Report already-present independent of DupPolicy. The plan sets Action.Essence before
	// the dup gate, so resolving by essence here surfaces the existing item even for a
	// DupAllow import that will go ahead anyway. The guard covers a quarantine or
	// zero-action plan, and a resolve error is non-fatal.
	if len(plan.Actions) > 0 && plan.Actions[0].Essence != "" {
		if item, _, err := l.ResolveRef(ctx, model.PortableRef{Essence: plan.Actions[0].Essence}); err == nil && item != nil {
			res.AlreadyPresent, res.AlreadyPresentPID = true, item.PID
		}
	}
	return res, nil
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
	allItems, err := l.store.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
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

// RelocateRoot re-points a library and every file under it at a new root path, for a
// portable restore onto a different machine or mount. The new path is validated with the
// moved library's entry substituted, so a relocation cannot create the overlap Open
// would refuse. The internal podcast library is not relocatable: its root follows the
// podcasts dir config.
func (l *Library) RelocateRoot(ctx context.Context, libPID model.PID, newRoot string) error {
	const op = "Library.RelocateRoot"
	// Serialize with AddRoot (and other relocations) so the read-validate-write is
	// atomic against a concurrent root mutation; see rootMu.
	l.rootMu.Lock()
	defer l.rootMu.Unlock()
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return err
	}
	var moved *model.Library
	for _, lib := range libs {
		if lib.PID == libPID {
			moved = lib
			break
		}
	}
	if moved == nil {
		return waxerr.New(waxerr.CodeNotFound, op, "no such library: "+string(libPID))
	}
	if moved.Mode == model.ModePodcast {
		return waxerr.New(waxerr.CodeInvalid, op,
			"the internal podcast library follows the podcasts dir config and cannot be relocated")
	}
	normalized, err := l.validateRootSet(ctx, libPID, config.Root{
		Path: newRoot, Mode: moved.Mode, Media: moved.Media, Profile: moved.Profile,
	})
	if err != nil {
		return err
	}
	return l.store.RelocateLibraryRoot(ctx, libPID, normalized.Path)
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

// ReSealSecrets seals every plaintext secret with the configured cipher, leaving
// already-sealed values untouched, in one transaction. It is the one-time adoption
// step after a secret cipher is first configured and is idempotent. It requires a
// configured cipher (Options.SecretCipher). Returns the number of secrets sealed.
func (l *Library) ReSealSecrets(ctx context.Context) (int, error) {
	return l.store.ReSealSecrets(ctx)
}

// RotateSecrets re-seals every secret from oldCipher to newCipher under newKeyID in
// one transaction, so a crash rolls the whole rotation back rather than leaving a
// mix of key generations. After it succeeds the caller reopens the Library with
// newCipher as Options.SecretCipher. Returns the number of secrets rotated.
func (l *Library) RotateSecrets(ctx context.Context, oldCipher, newCipher model.SecretCipher, newKeyID string) (int, error) {
	return l.store.RotateSecrets(ctx, oldCipher, newCipher, newKeyID)
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

// podcastOwned reports whether the trash/delete family must keep its hands off an item.
// `podcast unfetch` is the only verb that removes episode bytes, because `rm` archives
// the item ("files gone, history kept") where an episode's correct end state is remote
// and re-fetchable, and RestoreTrash would re-scan a trashed episode through the generic
// scanner into the library resolveLibraries refuses.
//
// It tests two things, because neither alone covers the set. The library mode catches
// anything ever placed in that tree but needs a path, and a never-downloaded episode has
// none. The kind test closes that, and holds because AttachEpisodeFile always binds the
// podcast library id.
func podcastOwned(libs []*model.Library, it *model.ItemView) bool {
	if it == nil {
		return false
	}
	return it.Kind == model.KindEpisode || inPodcastLibrary(libs, it.DisplayPath)
}

// inPodcastLibrary reports whether path sits under a ModePodcast root.
func inPodcastLibrary(libs []*model.Library, path string) bool {
	if path == "" {
		return false
	}
	lib := libraryContaining(libs, path)
	return lib != nil && lib.Mode == model.ModePodcast
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
