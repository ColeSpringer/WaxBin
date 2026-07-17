// Package pidpath resolves an item PID to where its audio lives, cached, with the
// cache invalidated from the catalog's change feed (a cheap DataVersion read, then
// Changes rows).
//
// It exists for a consumer that serves audio by PID and has to turn a "pid:<ULID>"
// reference into a file on disk, plus the window within that file when the item is a
// virtual track. Getting it right means priming a cursor at the feed tail, knowing
// that renames surface as file rows rather than item rows, and refusing to cache a
// lookup that a change row already raced. That reasoning is WaxBin's, so it lives
// here instead of being rediscovered by every consumer.
//
// This package deliberately does not open the file or speak any transcoder's
// vocabulary. Locate returns a Location and the caller opens it. Naming a decoder
// here would bless one, and the forty-odd lines of glue that name one belong to
// whoever chose it; the Examples in this package's tests carry the WaxFlow version.
//
// A consumer's own byte identity (a signed URL, a cache key) should stay independent
// of catalog state: a rename must not kill URLs or cache entries for unchanged bytes.
package pidpath

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// DefaultPollInterval is the DataVersion poll cadence: cheap (one pragma read per
// tick) and short enough that a library reorganization is picked up before anyone
// notices stale paths.
const DefaultPollInterval = 5 * time.Second

const (
	// maxEntries bounds the PID-to-location cache; oldest-out beyond it.
	maxEntries = 4096
	// queryTimeout is the upper bound on any single catalog query: the poll loop has
	// no caller context, and a Locate caller's context may be unbounded (a long
	// download), which must not pin a hung database query for its whole life.
	queryTimeout = 10 * time.Second
)

// catalog is the slice of *waxbin.Library this package consumes, narrowed so unit
// tests can fake the catalog without SQLite. Close is deliberately absent: a Cache
// closes only a library it opened itself, and that one is held as an io.Closer.
type catalog interface {
	Get(ctx context.Context, pid model.PID) (*model.ItemView, error)
	DataVersion(ctx context.Context) (int64, error)
	Changes(ctx context.Context, sinceSeq int64) ([]model.Change, error)
}

// Location is where an item's audio lives: the containing file, plus the window
// within it when the item is a virtual track.
type Location struct {
	// Path is the containing file. For a virtual track it is the whole album rip,
	// shared with the item's siblings, so Path alone is ambiguous.
	Path       string
	FilePID    model.PID
	SampleRate int

	// Virtual reports that the item is a window into Path rather than the whole
	// file. StartFrames/EndFrames are CD frames; EndFrames is 0 when the window
	// runs to the end of the file. Use Span rather than converting these directly.
	Virtual     bool
	StartFrames int64
	EndFrames   int64
}

// Span converts a virtual track's window to source-timeline sample offsets: from is
// the window's first sample, to is one past its last, and to is 0 when the window
// runs to the end of the file. A whole-file item has no window and returns
// (0, 0, nil), so a caller omits whichever bound comes back 0 and never has to
// branch on Virtual. Omitting is also the only way to spell "to the end": a
// transcoder reads to=0 as the empty span, not as the whole file.
//
// The conversion is frames*SampleRate/75, exact whenever 75 divides the rate, which
// every CD-family and hi-res rate does. It truncates for the rates that do not
// (32000, 16000, 8000), but gaplessness does not depend on that: one track's end and
// the next's start are the same frame, so they convert to the same sample and no
// join can open a gap or overlap. waxflow/internal/cue.Samples is the same formula
// on the other side of the boundary; the two must agree and cannot share a test.
//
// It fails only for a virtual track whose file has no known rate, which WaxLabel
// leaves 0 for a header it could not read. There is deliberately no fallback:
// returning 0 would omit the bounds and serve the whole album in place of one track.
// A whole-file item needs no bounds and so does not care about the rate, which is why
// Virtual is checked first; the other order would reject a perfectly serviceable item
// over a header field it never reads.
func (l Location) Span() (from, to int64, err error) {
	if !l.Virtual {
		return 0, 0, nil
	}
	if l.SampleRate <= 0 {
		return 0, 0, waxerr.New(waxerr.CodeInvalid, "pidpath.Span",
			fmt.Sprintf("file %s declares no sample rate; a virtual track's window cannot be converted to samples", l.FilePID))
	}
	rate := int64(l.SampleRate)
	return l.StartFrames * rate / model.FramesPerSecond, l.EndFrames * rate / model.FramesPerSecond, nil
}

// Options configures New and Open.
type Options struct {
	// DBPath is the WaxBin catalog database, opened read-only by Open. The file must
	// already exist: WaxBin creates it, so start WaxBin first. New ignores it.
	DBPath string

	// PollInterval is the background DataVersion poll cadence; 0 means
	// DefaultPollInterval, negative disables the background loop (tests drive Poll
	// directly).
	PollInterval time.Duration

	// Logger, nil discards.
	Logger *slog.Logger
}

// Cache resolves item PIDs to locations against a WaxBin catalog, keeping the
// results until the change feed says otherwise. It is safe for concurrent use.
type Cache struct {
	lib catalog
	log *slog.Logger

	// queryCtx parents every catalog query (Locate lookups and poll pulls), so Close
	// aborts in-flight work immediately instead of waiting out queryTimeout on a hung
	// database.
	queryCtx    context.Context
	stopQueries context.CancelFunc

	// pollMu serializes poll cycles (the background loop and direct Poll calls); mu
	// guards the cache maps and the change cursor.
	pollMu sync.Mutex
	mu     sync.Mutex
	// entries caches item PID -> resolved location; byFile maps the location's file
	// PID back to the items resolving through it, because renames and moves arrive on
	// the change feed as file-entity rows carrying the file PID. The value is a set
	// because virtual tracks share a file by construction: every cue TRACK of a
	// single-file rip resolves through the one file PID, and a single-value index
	// would let that file's change row invalidate only the sibling stored last.
	entries map[model.PID]*entry
	byFile  map[model.PID]map[model.PID]struct{}
	// invalGen counts item/file invalidation rows. A lookup snapshots it before the
	// catalog query and store refuses to cache a result from before a newer
	// invalidation: otherwise a change row consumed between the lookup and the store
	// would leave a stale location with its invalidating row already spent (the poll
	// never revisits a seq).
	//
	// It is one counter for the whole catalog rather than one per PID, so it is
	// deliberately conservative: a row naming some other item also makes a lookup in
	// flight decline to cache. That costs a repeat query and never a wrong answer, and
	// the window it can fire in is a single catalog lookup, which a poll has to land
	// inside of, so it stays rare even mid-scan and the next Locate caches normally.
	// Tracking invalidations per PID would need either unbounded state or a bounded
	// recent-set with a floor generation, which is a lot of machinery to save an
	// occasional indexed SELECT.
	invalGen uint64
	clock    int64
	sinceSeq int64
	dataVer  int64

	stop     chan struct{}
	pollDone chan struct{}
	closed   sync.Once
	closeErr error
	// ownedLib is set when Open created the library itself; a caller-supplied one
	// (New) stays the caller's to close.
	ownedLib io.Closer
}

type entry struct {
	loc  Location
	used int64
}

// New builds a Cache over a library the caller already holds, establishes the change
// cursor at the current feed tail, and starts the background poll. Close stops the
// poll and leaves the library open, because this Cache did not open it.
func New(ctx context.Context, lib *waxbin.Library, opts Options) (*Cache, error) {
	const op = "pidpath.New"
	if lib == nil {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "library is required")
	}
	c, err := newCache(ctx, lib, opts)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return c, nil
}

// Open opens the catalog read-only (never taking WaxBin's write lock, so it coexists
// with a running WaxBin daemon), establishes the change cursor at the current feed
// tail, and starts the background poll. Close releases the handle it opened.
//
// Prefer New when the caller already has a Library: a second read-only handle onto
// the same file is a second connection pool for nothing.
func Open(ctx context.Context, opts Options) (*Cache, error) {
	const op = "pidpath.Open"
	if opts.DBPath == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "catalog DB path is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: opts.DBPath, ReadOnly: true, Logger: log})
	if err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeIO, op, err, "opening catalog %s", opts.DBPath)
	}
	c, err := newCache(ctx, lib, opts)
	if err != nil {
		lib.Close()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// Nothing reads ownedLib until Close, which cannot run before Open returns it.
	c.ownedLib = lib
	return c, nil
}

// newCache is the shared constructor both entry points funnel through, over the
// narrowed catalog interface, which is also how the unit tests inject a fake without
// SQLite. It returns its error unwrapped so each constructor can name itself.
func newCache(ctx context.Context, lib catalog, opts Options) (*Cache, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// The query context outlives ctx (which may be a startup context): it is the
	// Cache's lifetime, ended by Close.
	queryCtx, stopQueries := context.WithCancel(context.Background())
	c := &Cache{
		lib:         lib,
		log:         log,
		queryCtx:    queryCtx,
		stopQueries: stopQueries,
		entries:     make(map[model.PID]*entry),
		byFile:      make(map[model.PID]map[model.PID]struct{}),
	}
	if err := c.initCursor(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("reading the catalog change feed: %w", err)
	}
	interval := opts.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}
	if interval > 0 {
		c.stop = make(chan struct{})
		c.pollDone = make(chan struct{})
		go c.pollLoop(interval)
	}
	return c, nil
}

// initCursor walks the change feed to its tail so the first poll only sees changes
// made after this cache opened; the cache is empty now, so nothing older can name a
// stale entry. Loops until an empty page rather than assuming the store's page size.
func (c *Cache) initCursor(ctx context.Context) error {
	dv, err := c.lib.DataVersion(ctx)
	if err != nil {
		return err
	}
	var seq int64
	for {
		rows, err := c.lib.Changes(ctx, seq)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		seq = rows[len(rows)-1].Seq
	}
	c.sinceSeq, c.dataVer = seq, dv
	return nil
}

// Locate resolves an item PID to its file's location, from the cache when it is
// warm. The catalog lookup honors ctx (a canceled request stops waiting) bounded by
// queryTimeout, and Close still aborts it via the query context.
//
// It propagates the catalog's error unchanged, including CodeNotFound for an unknown
// pid; translating it into a consumer's own vocabulary is the consumer's job.
func (c *Cache) Locate(ctx context.Context, pid model.PID) (Location, error) {
	if loc, hit := c.cached(pid); hit {
		return loc, nil
	}
	return c.lookup(ctx, pid)
}

// Relocate drops any cached entry for pid and resolves it fresh. Call it when
// opening a Locate result failed because the path is gone: a rename that landed
// between polls leaves a stale entry, and this heals it without waiting for the next
// tick. One retry is enough, since a second failure means the file is really missing
// rather than the cache being stale.
func (c *Cache) Relocate(ctx context.Context, pid model.PID) (Location, error) {
	c.drop(pid)
	return c.lookup(ctx, pid)
}

// lookup queries the catalog and caches the result.
func (c *Cache) lookup(ctx context.Context, pid model.PID) (Location, error) {
	gen := c.generation()
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	// Close must still abort an in-flight lookup even when the caller's context lives
	// on; bridge the lifetime context into this query.
	stop := context.AfterFunc(c.queryCtx, cancel)
	defer stop()
	iv, err := c.lib.Get(qctx, pid)
	if err != nil {
		return Location{}, err
	}
	loc := Location{
		Path:        string(iv.Path),
		FilePID:     iv.FilePID,
		SampleRate:  iv.SampleRate,
		Virtual:     iv.Virtual,
		StartFrames: iv.StartFrames,
		EndFrames:   iv.EndFrames,
	}
	c.store(pid, loc, gen)
	return loc, nil
}

// Poll runs one poll cycle: a cheap DataVersion read and, when it moved, a Changes
// pull that drops the cached locations the changed rows name. The background loop
// calls it every PollInterval; tests call it directly to observe invalidation
// deterministically.
func (c *Cache) Poll(ctx context.Context) error {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()

	dv, err := c.lib.DataVersion(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	seq, same := c.sinceSeq, dv == c.dataVer
	c.mu.Unlock()
	if same {
		return nil
	}
	dropped := 0
	for {
		rows, err := c.lib.Changes(ctx, seq)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		seq = rows[len(rows)-1].Seq
		dropped += c.invalidate(rows)
	}
	c.mu.Lock()
	c.sinceSeq, c.dataVer = seq, dv
	c.mu.Unlock()
	if dropped > 0 {
		c.log.Debug("catalog changes invalidated cached locations", "dropped", dropped, "seq", seq)
	}
	return nil
}

// invalidate drops the cache entries a batch of change rows names and reports how
// many it dropped. Item rows carry the item PID directly; file rows (how renames and
// moves surface) map back through byFile. Every other entity type (albums,
// playlists, play state) is noise to a location cache.
func (c *Cache) invalidate(rows []model.Change) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for _, ch := range rows {
		switch ch.EntityType {
		case "item":
			c.invalGen++
			dropped += c.dropLocked(ch.EntityPID)
		case "file":
			c.invalGen++
			for itemPID := range c.byFile[ch.EntityPID] {
				dropped += c.dropLocked(itemPID)
			}
		}
	}
	return dropped
}

// generation snapshots the invalidation counter for a store that follows an unlocked
// catalog lookup.
func (c *Cache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalGen
}

func (c *Cache) cached(pid model.PID) (Location, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[pid]
	if !ok {
		return Location{}, false
	}
	c.clock++
	e.used = c.clock
	return e.loc, true
}

// store caches a resolved location. gen is the invalidation snapshot taken before
// the catalog lookup that produced it; a lookup raced by newer item/file change rows
// is not cached (its invalidating rows are already consumed, so a stale insert would
// survive until the next change).
func (c *Cache) store(pid model.PID, loc Location, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalGen != gen {
		return
	}
	if _, exists := c.entries[pid]; !exists && len(c.entries) >= maxEntries {
		c.evictOldestLocked()
	}
	if old := c.entries[pid]; old != nil {
		c.unindexLocked(old.loc.FilePID, pid)
	}
	c.clock++
	c.entries[pid] = &entry{loc: loc, used: c.clock}
	if loc.FilePID != "" {
		items, ok := c.byFile[loc.FilePID]
		if !ok {
			items = make(map[model.PID]struct{}, 1)
			c.byFile[loc.FilePID] = items
		}
		items[pid] = struct{}{}
	}
}

// unindexLocked removes one item from a file's reverse index, deleting the set when
// it empties.
func (c *Cache) unindexLocked(filePID, pid model.PID) {
	items, ok := c.byFile[filePID]
	if !ok {
		return
	}
	delete(items, pid)
	if len(items) == 0 {
		delete(c.byFile, filePID)
	}
}

func (c *Cache) drop(pid model.PID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(pid)
}

func (c *Cache) dropLocked(pid model.PID) int {
	e, ok := c.entries[pid]
	if !ok {
		return 0
	}
	delete(c.entries, pid)
	c.unindexLocked(e.loc.FilePID, pid)
	return 1
}

// evictOldestLocked is the cache bound: linear oldest-out, run only on insert at
// capacity, over entries that are a path, two PIDs, and three ints each.
func (c *Cache) evictOldestLocked() {
	var oldest model.PID
	minUsed := int64(math.MaxInt64)
	for pid, e := range c.entries {
		if e.used < minUsed {
			minUsed, oldest = e.used, pid
		}
	}
	if oldest != "" {
		c.dropLocked(oldest)
	}
}

// pollLoop ticks Poll until Close. Failures keep serving cached locations (the
// consumer's own byte identity still guards content changes) and log once per
// outage, not once per tick; the outage flag is goroutine-local, no lock.
func (c *Cache) pollLoop(interval time.Duration) {
	defer close(c.pollDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	down := false
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.queryCtx, queryTimeout)
			err := c.Poll(ctx)
			cancel()
			switch {
			case err != nil && !down:
				c.log.Warn("catalog poll failing; serving cached locations", "err", err)
			case err == nil && down:
				c.log.Info("catalog poll recovered")
			}
			down = err != nil
		}
	}
}

// Close aborts in-flight catalog queries, stops the poll loop, and releases the
// library only if this package opened it. Idempotent.
func (c *Cache) Close() error {
	c.closed.Do(func() {
		c.stopQueries()
		if c.stop != nil {
			close(c.stop)
			<-c.pollDone
		}
		if c.ownedLib != nil {
			c.closeErr = c.ownedLib.Close()
		}
	})
	return c.closeErr
}
