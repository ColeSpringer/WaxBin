package pidpath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeCatalog implements the catalog interface without SQLite so the cache, poll, and
// span logic test in microseconds.
type fakeCatalog struct {
	mu       sync.Mutex
	items    map[model.PID]*model.ItemView
	dv       int64
	rows     []model.Change
	pageSize int
	getErr   error
	pollErr  error
	hang     chan struct{}
	gets     int
	pulls    int
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{items: make(map[model.PID]*model.ItemView), dv: 1}
}

func (f *fakeCatalog) Get(_ context.Context, pid model.PID) (*model.ItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	iv, ok := f.items[pid]
	if !ok {
		return nil, waxerr.New(waxerr.CodeNotFound, "fake.Get", "no such item")
	}
	return iv, nil
}

func (f *fakeCatalog) DataVersion(ctx context.Context) (int64, error) {
	f.mu.Lock()
	hang := f.hang
	pollErr := f.pollErr
	dv := f.dv
	f.mu.Unlock()
	if hang != nil {
		// A hung database: signal the test, then block until the query context ends.
		select {
		case hang <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if pollErr != nil {
		return 0, pollErr
	}
	return dv, nil
}

// hangPolls makes subsequent DataVersion calls signal on the returned channel and
// block until their context ends.
func (f *fakeCatalog) hangPolls() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = make(chan struct{}, 1)
	return f.hang
}

func (f *fakeCatalog) Changes(_ context.Context, sinceSeq int64) ([]model.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	var out []model.Change
	for _, ch := range f.rows {
		if ch.Seq > sinceSeq {
			out = append(out, ch)
			if f.pageSize > 0 && len(out) == f.pageSize {
				break
			}
		}
	}
	return out, nil
}

// setItem catalogs a whole-file item.
func (f *fakeCatalog) setItem(pid, filePID model.PID, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[pid] = &model.ItemView{PID: pid, FilePID: filePID, Path: []byte(path)}
}

// setVirtual catalogs a virtual track: one window of a shared rip.
func (f *fakeCatalog) setVirtual(pid, filePID model.PID, path string, rate int, start, end int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[pid] = &model.ItemView{
		PID: pid, FilePID: filePID, Path: []byte(path), SampleRate: rate,
		Virtual: true, StartFrames: start, EndFrames: end,
	}
}

// commit appends change rows and bumps the data version, like a WaxBin write
// transaction.
func (f *fakeCatalog) commit(rows ...model.Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, rows...)
	f.dv++
}

// newTestCache wires a Cache over the fake exactly as New does, minus the background
// loop.
func newTestCache(t *testing.T, f catalog) *Cache {
	t.Helper()
	c, err := newCache(context.Background(), f, Options{
		PollInterval: -1, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func wantCode(t *testing.T, err error, code waxerr.Code) {
	t.Helper()
	if waxerr.CodeOf(err) != code {
		t.Fatalf("error = %v (code %s), want %s", err, waxerr.CodeOf(err), code)
	}
}

func TestLocateCaches(t *testing.T) {
	fake := newFakeCatalog()
	pid, filePID := model.NewPID(), model.NewPID()
	fake.setItem(pid, filePID, "/lib/track.wav")
	c := newTestCache(t, fake)

	loc, err := c.Locate(context.Background(), pid)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if loc.Path != "/lib/track.wav" || loc.FilePID != filePID || loc.Virtual {
		t.Fatalf("location = %+v, want the whole file at /lib/track.wav", loc)
	}

	// Second locate is a cache hit: no catalog query.
	if _, err := c.Locate(context.Background(), pid); err != nil {
		t.Fatalf("cached Locate: %v", err)
	}
	if fake.gets != 1 {
		t.Fatalf("catalog queries = %d, want 1 (second locate must hit the cache)", fake.gets)
	}
}

// TestLocatePropagatesCatalogErrors: translating the catalog's vocabulary is the
// consumer's job, so an unknown pid stays CodeNotFound and a broken catalog stays
// whatever the catalog said.
func TestLocatePropagatesCatalogErrors(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCache(t, fake)

	_, err := c.Locate(context.Background(), model.NewPID())
	wantCode(t, err, waxerr.CodeNotFound)

	sentinel := errors.New("disk on fire")
	fake.mu.Lock()
	fake.getErr = sentinel
	fake.mu.Unlock()
	_, err = c.Locate(context.Background(), model.NewPID())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Locate error = %v, want the catalog's own error unchanged", err)
	}
}

// TestSpanIsExactAtTheSample is the pidpath half of the guard the frame unit exists
// for. Cue time 03:15:22 is 14647 frames, which at 44.1 kHz must be sample 8612436
// (14647*588) exactly. The millisecond path this replaced reached 8612421, 15 samples
// early: a third of a millisecond of the previous track at the head of this one.
func TestSpanIsExactAtTheSample(t *testing.T) {
	fake := newFakeCatalog()
	pid, filePID := model.NewPID(), model.NewPID()
	fake.setVirtual(pid, filePID, "/lib/rip.flac", 44100, 14647, 22000)
	c := newTestCache(t, fake)

	loc, err := c.Locate(context.Background(), pid)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	from, to, err := loc.Span()
	if err != nil {
		t.Fatalf("Span: %v", err)
	}
	if from != 8612436 {
		t.Errorf("from = %d, want 8612436", from)
	}
	if to != 22000*588 {
		t.Errorf("to = %d, want %d", to, 22000*588)
	}
}

// TestSpanAbutsAtTheJoin: consecutive tracks share a frame boundary, so their spans
// must share a sample. This is what keeps a gapless record gapless, and it holds at
// every rate, including the ones 75 does not divide.
func TestSpanAbutsAtTheJoin(t *testing.T) {
	for _, rate := range []int{44100, 48000, 88200, 96000, 192000, 32000, 16000, 8000} {
		boundary := int64(14647)
		first := Location{Virtual: true, SampleRate: rate, StartFrames: 0, EndFrames: boundary}
		second := Location{Virtual: true, SampleRate: rate, StartFrames: boundary, EndFrames: 20000}
		_, firstEnd, err := first.Span()
		if err != nil {
			t.Fatalf("rate %d: Span: %v", rate, err)
		}
		secondStart, _, err := second.Span()
		if err != nil {
			t.Fatalf("rate %d: Span: %v", rate, err)
		}
		if firstEnd != secondStart {
			t.Errorf("rate %d: track 1 ends at sample %d, track 2 starts at %d; the join opened",
				rate, firstEnd, secondStart)
		}
	}
}

// TestSpanOpenEndOmits: a window running to the end of the file reports to=0, which a
// caller omits. Spelling it as the file's sample count would be a guess, and to=0 is
// the empty span to a transcoder, never the whole file.
func TestSpanOpenEndOmits(t *testing.T) {
	loc := Location{Virtual: true, SampleRate: 44100, StartFrames: 375, EndFrames: 0}
	from, to, err := loc.Span()
	if err != nil {
		t.Fatalf("Span: %v", err)
	}
	if from != 375*588 || to != 0 {
		t.Fatalf("span = (%d, %d), want (%d, 0)", from, to, 375*588)
	}
}

// TestSpanChecksVirtualBeforeRate pins Span's branch order. Both halves are needed: an
// implementation that reads the rate before Virtual passes the first and fails the
// second, rejecting a perfectly serviceable whole-file item over a header field it
// does not need.
func TestSpanChecksVirtualBeforeRate(t *testing.T) {
	virtual := Location{Virtual: true, SampleRate: 0, StartFrames: 375, EndFrames: 750}
	if _, _, err := virtual.Span(); err == nil {
		t.Fatal("a virtual track with no sample rate must fail rather than omit its bounds " +
			"and serve the whole album")
	} else {
		wantCode(t, err, waxerr.CodeInvalid)
	}

	whole := Location{Virtual: false, SampleRate: 0}
	from, to, err := whole.Span()
	if err != nil || from != 0 || to != 0 {
		t.Fatalf("whole-file Span = (%d, %d, %v), want (0, 0, nil): it needs no bounds, "+
			"so it does not care about the rate", from, to, err)
	}
}

func TestRelocateHealsAStaleEntry(t *testing.T) {
	fake := newFakeCatalog()
	pid, filePID := model.NewPID(), model.NewPID()
	fake.setItem(pid, filePID, "/lib/old.wav")
	c := newTestCache(t, fake)

	if _, err := c.Locate(context.Background(), pid); err != nil {
		t.Fatalf("warm Locate: %v", err)
	}

	// The file moves and the catalog knows, but no poll has run: the cached location is
	// stale. The caller's open fails, and Relocate re-asks the catalog rather than
	// waiting for the next tick.
	fake.setItem(pid, filePID, "/lib/new.wav")
	if loc, _ := c.Cached(pid); loc.Path != "/lib/old.wav" {
		t.Fatalf("cache = %q, want the stale path still held", loc.Path)
	}
	loc, err := c.Relocate(context.Background(), pid)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if loc.Path != "/lib/new.wav" {
		t.Fatalf("relocated to %q, want /lib/new.wav", loc.Path)
	}
	if fake.gets != 2 {
		t.Fatalf("catalog queries = %d, want 2 (one warm, one relocate)", fake.gets)
	}
	if cached, _ := c.Cached(pid); cached.Path != "/lib/new.wav" {
		t.Fatalf("cache after relocate = %q, want the fresh path", cached.Path)
	}
}

func TestPollInvalidation(t *testing.T) {
	fake := newFakeCatalog()
	type row struct{ pid, filePID model.PID }
	items := make([]row, 3)
	for i := range items {
		items[i] = row{model.NewPID(), model.NewPID()}
		fake.setItem(items[i].pid, items[i].filePID, fmt.Sprintf("/lib/t%d.wav", i))
	}
	c := newTestCache(t, fake)
	for _, it := range items {
		if _, err := c.Locate(context.Background(), it.pid); err != nil {
			t.Fatalf("warm Locate: %v", err)
		}
	}

	// An unchanged DataVersion is a no-op poll: no Changes pull.
	pullsBefore := fake.pulls
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("no-op Poll: %v", err)
	}
	if fake.pulls != pullsBefore {
		t.Fatal("no-op poll pulled the change feed")
	}

	// One item row, one file row (how renames surface), one row of an entity type a
	// location cache does not care about.
	fake.commit(
		model.Change{Seq: 1, EntityType: "item", EntityPID: items[0].pid, Op: model.OpUpdate},
		model.Change{Seq: 2, EntityType: "file", EntityPID: items[1].filePID, Op: model.OpUpdate},
		model.Change{Seq: 3, EntityType: "play_state", EntityPID: model.NewPID(), Op: model.OpUpdate},
	)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, ok := c.cached(items[0].pid); ok {
		t.Fatal("item change row did not drop the cached location")
	}
	if _, ok := c.cached(items[1].pid); ok {
		t.Fatal("file change row did not drop the cached location (rename signal)")
	}
	if _, ok := c.cached(items[2].pid); !ok {
		t.Fatal("unrelated change dropped a live entry")
	}
}

func TestPollPagesToTheTail(t *testing.T) {
	fake := newFakeCatalog()
	fake.pageSize = 2
	pid, filePID := model.NewPID(), model.NewPID()
	fake.setItem(pid, filePID, "/lib/t.wav")
	c := newTestCache(t, fake)
	if _, err := c.Locate(context.Background(), pid); err != nil {
		t.Fatal(err)
	}

	// Five rows across three pages; the one that names our item sits on the last page,
	// so stopping early would miss it.
	rows := make([]model.Change, 5)
	for i := range rows {
		rows[i] = model.Change{Seq: int64(i + 1), EntityType: "album", EntityPID: model.NewPID(), Op: model.OpUpdate}
	}
	rows[4] = model.Change{Seq: 5, EntityType: "item", EntityPID: pid, Op: model.OpUpdate}
	fake.commit(rows...)

	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, ok := c.cached(pid); ok {
		t.Fatal("poll stopped before the last change page")
	}
	c.mu.Lock()
	seq := c.sinceSeq
	c.mu.Unlock()
	if seq != 5 {
		t.Fatalf("cursor = %d, want 5", seq)
	}
}

func TestInitCursorStartsAtTail(t *testing.T) {
	fake := newFakeCatalog()
	fake.pageSize = 2
	for i := 1; i <= 5; i++ {
		fake.rows = append(fake.rows, model.Change{Seq: int64(i), EntityType: "item", EntityPID: model.NewPID(), Op: model.OpCreate})
	}
	c := newTestCache(t, fake)
	c.mu.Lock()
	seq := c.sinceSeq
	c.mu.Unlock()
	if seq != 5 {
		t.Fatalf("cursor after open = %d, want the feed tail 5", seq)
	}
}

func TestCloseAbortsHungPoll(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCache(t, fake)
	polling := fake.hangPolls()
	c.stop = make(chan struct{})
	c.pollDone = make(chan struct{})
	go c.pollLoop(time.Millisecond)
	<-polling

	// Close must cancel the in-flight query via the cache's query context, not wait out
	// queryTimeout behind a hung database.
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v waiting out a hung poll; want immediate abort", elapsed)
	}
}

func TestCacheEvictsOldest(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCache(t, fake)

	pids := make([]model.PID, maxEntries+1)
	for i := range pids {
		pids[i] = model.PID(fmt.Sprintf("%026d", i))
		c.store(pids[i], Location{
			Path: fmt.Sprintf("/x/%d", i), FilePID: model.PID(fmt.Sprintf("F%025d", i)),
		}, c.generation())
	}
	if len(c.entries) != maxEntries {
		t.Fatalf("entries = %d, want the %d bound", len(c.entries), maxEntries)
	}
	if _, ok := c.cached(pids[0]); ok {
		t.Fatal("oldest entry survived insert at capacity")
	}
	if _, ok := c.cached(pids[1]); !ok {
		t.Fatal("second-oldest entry evicted too")
	}
}

// A change row consumed between a lookup's catalog query and its store must not leave
// a stale entry behind: the row is already spent, so nothing would ever invalidate the
// late insert. store refuses results from before a newer invalidation.
func TestStoreLosesRaceToInvalidation(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCache(t, fake)
	pid := model.NewPID()
	filePID := model.PID("F0000000000000000000000001")

	// Snapshot before the "lookup", as lookup does, then let an invalidating item row
	// land in the window before the store.
	gen := c.generation()
	c.invalidate([]model.Change{{Seq: 1, EntityType: "item", EntityPID: pid, Op: model.OpUpdate}})
	c.store(pid, Location{Path: "/stale/path.flac", FilePID: filePID}, gen)
	if _, ok := c.cached(pid); ok {
		t.Fatal("store cached a location from before a consumed invalidation row")
	}

	// A store with no intervening invalidation still lands.
	c.store(pid, Location{Path: "/fresh/path.flac", FilePID: filePID}, c.generation())
	if loc, ok := c.cached(pid); !ok || loc.Path != "/fresh/path.flac" {
		t.Fatalf("clean store missing: %q, %v", loc.Path, ok)
	}

	// Unrelated entity rows (play state and friends) do not spend the generation, so
	// they cannot starve stores under churn.
	gen = c.generation()
	c.invalidate([]model.Change{{Seq: 2, EntityType: "play_state", EntityPID: model.NewPID(), Op: model.OpUpdate}})
	c.store(model.PID("00000000000000000000000042"), Location{Path: "/other.flac"}, gen)
	if _, ok := c.cached(model.PID("00000000000000000000000042")); !ok {
		t.Fatal("a play_state row aborted an unrelated store")
	}
}

// TestFileRowInvalidatesAllSharingItems: virtual tracks share a file by construction,
// so one file change row must drop every sibling resolving through it. A single-value
// reverse index would invalidate only the one stored last.
func TestFileRowInvalidatesAllSharingItems(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCache(t, fake)
	filePID := model.PID("F0000000000000000000000009")
	a, b := model.NewPID(), model.NewPID()
	c.store(a, Location{Path: "/rip.flac", FilePID: filePID, Virtual: true, StartFrames: 0, EndFrames: 375}, c.generation())
	c.store(b, Location{Path: "/rip.flac", FilePID: filePID, Virtual: true, StartFrames: 375}, c.generation())

	c.invalidate([]model.Change{{Seq: 1, EntityType: "file", EntityPID: filePID, Op: model.OpUpdate}})
	if _, ok := c.cached(a); ok {
		t.Fatal("virtual track a survived its file's change row")
	}
	if _, ok := c.cached(b); ok {
		t.Fatal("virtual track b survived its file's change row")
	}
	if len(c.byFile) != 0 {
		t.Fatalf("byFile index not emptied: %v", c.byFile)
	}
}

// closableCatalog is a fake that also reports being closed, standing in for the
// library a constructor may or may not own.
type closableCatalog struct {
	*fakeCatalog
	closed bool
}

func (f *closableCatalog) Close() error { f.closed = true; return nil }

// TestCloseReleasesOnlyAnOwnedLibrary pins the field the two constructors differ on.
// Open opens the library itself and must release it; New is handed one the caller
// already holds and must leave it alone, or an embedder's Close of the Cache would
// take down the library it is still using.
//
// It is whitebox because a leaked SQLite handle is invisible to behavior: the driver
// holds its descriptors open past sql.DB.Close, so counting them proves nothing.
// TestNewOverACallerOpenedLibrary covers the observable half against a real catalog.
func TestCloseReleasesOnlyAnOwnedLibrary(t *testing.T) {
	owned := &closableCatalog{fakeCatalog: newFakeCatalog()}
	c := newTestCache(t, owned)
	c.ownedLib = owned // as Open does with the handle it created
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !owned.closed {
		t.Error("Close did not release the library Open owned")
	}

	borrowed := &closableCatalog{fakeCatalog: newFakeCatalog()}
	c2 := newTestCache(t, borrowed) // ownedLib stays nil, as New leaves it
	if err := c2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if borrowed.closed {
		t.Error("Close released a library the caller owns")
	}
}

// TestOpenOwnsItsLibrary is the other half: Close releases ownedLib (above), so Open
// has to set it. Whitebox for the same reason.
func TestOpenOwnsItsLibrary(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// WaxBin creates the catalog; a read-only open cannot, so author it first.
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db})
	if err != nil {
		t.Fatalf("authoring the catalog: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := Open(ctx, Options{DBPath: db, PollInterval: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.ownedLib == nil {
		t.Error("Open did not take ownership of the handle it created; Close would leak it")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	if _, err := Open(ctx, Options{}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("Open with no DBPath = %v, want an invalid-argument error", err)
	}
}

// blockingGetCatalog hangs every Get until its query context ends, signaling on
// getting when a lookup is in flight.
type blockingGetCatalog struct {
	*fakeCatalog
	getting chan struct{}
}

func (b *blockingGetCatalog) Get(ctx context.Context, _ model.PID) (*model.ItemView, error) {
	select {
	case b.getting <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestLocateHonorsCallerContext: a canceled request context aborts a hung catalog
// lookup instead of waiting out queryTimeout.
func TestLocateHonorsCallerContext(t *testing.T) {
	b := &blockingGetCatalog{fakeCatalog: newFakeCatalog(), getting: make(chan struct{}, 1)}
	c := newTestCache(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Locate(ctx, model.NewPID()); err == nil {
		t.Fatal("Locate returned nil error from a canceled lookup")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Locate took %v; want prompt abort on the caller's deadline", elapsed)
	}
}

// TestCloseAbortsHungLocate pins the other half of the bridge: Close aborts an
// in-flight lookup even while the caller's context lives on.
func TestCloseAbortsHungLocate(t *testing.T) {
	b := &blockingGetCatalog{fakeCatalog: newFakeCatalog(), getting: make(chan struct{}, 1)}
	c := newTestCache(t, b)

	done := make(chan error, 1)
	go func() {
		_, err := c.Locate(context.Background(), model.NewPID())
		done <- err
	}()
	<-b.getting
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Locate returned nil error after Close aborted it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Locate still hung 2s after Close; the query-context bridge is broken")
	}
}
