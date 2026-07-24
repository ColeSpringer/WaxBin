package playback

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeStore records progress writes so the coalescing behavior can be asserted.
type fakeStore struct {
	mu       sync.Mutex
	progress []int64 // positions written, in order
	last     map[string]int64
	fail     error                // when set, SetProgress fails with it
	onWrite  func(item model.PID) // hook invoked inside SetProgress (for race simulation)

	// flushed backs PlayStatesForItems for the overlay tests: the "stored" states
	// keyed by item pid, as the real store would return them.
	flushed map[model.PID][]model.PlayState
	// defaultPID is what DefaultUser answers ("u-default" unless a test overrides).
	defaultPID model.PID
}

func newFake() *fakeStore { return &fakeStore{last: map[string]int64{}} }

func (f *fakeStore) SetProgress(_ context.Context, _, item model.PID, pos int64) error {
	if f.onWrite != nil {
		f.onWrite(item)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.progress = append(f.progress, pos)
	f.last[string(item)] = pos
	return nil
}
func (f *fakeStore) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.progress)
}

func (f *fakeStore) PlayStateFor(_ context.Context, _, item model.PID) (*model.PlayState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &model.PlayState{ItemPID: item, PositionMS: f.last[string(item)]}, nil
}

func (f *fakeStore) PlayStatesForItems(_ context.Context, itemPIDs []model.PID) (map[model.PID][]model.PlayState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[model.PID][]model.PlayState)
	for _, pid := range itemPIDs {
		if states, ok := f.flushed[pid]; ok {
			out[pid] = append([]model.PlayState(nil), states...)
		}
	}
	return out, nil
}

func (f *fakeStore) DefaultUser(context.Context) (*model.User, error) {
	pid := f.defaultPID
	if pid == "" {
		pid = "u-default"
	}
	return &model.User{PID: pid, Name: "default", IsDefault: true}, nil
}

// Unused-by-these-tests methods.
func (f *fakeStore) MarkPlayed(context.Context, model.PID, model.PID, bool) error        { return nil }
func (f *fakeStore) SetRating(context.Context, model.PID, model.PID, *int, *int64) error { return nil }
func (f *fakeStore) SetStar(context.Context, model.PID, model.PID, bool, *int64) error   { return nil }
func (f *fakeStore) AddBookmark(context.Context, model.PID, model.PID, int64, string) (model.PID, error) {
	return "", nil
}
func (f *fakeStore) Bookmarks(context.Context, model.PID, model.PID) ([]model.Bookmark, error) {
	return nil, nil
}
func (f *fakeStore) DeleteBookmark(context.Context, model.PID) error             { return nil }
func (f *fakeStore) SetQueue(context.Context, model.PID, []model.PID) error      { return nil }
func (f *fakeStore) Queue(context.Context, model.PID) ([]*model.ItemView, error) { return nil, nil }
func (f *fakeStore) StartSession(context.Context, model.PID, model.PID, string) (model.PID, error) {
	return "", nil
}
func (f *fakeStore) EndSession(context.Context, model.PID, int64) error { return nil }

func TestProgressCoalesces(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	const item model.PID = "item-1"

	// A stream of ticks must not write at all until a flush.
	for i := int64(0); i < 100; i++ {
		svc.Progress("", item, i*1000)
	}
	if fake.writes() != 0 {
		t.Fatalf("progress ticks wrote %d times; coalescing should buffer until flush", fake.writes())
	}

	// State reflects the latest buffered tick (read-your-writes in-process).
	st, _ := svc.State(ctx, "", item)
	if st.PositionMS != 99000 {
		t.Errorf("buffered state position = %d, want 99000", st.PositionMS)
	}

	// Flush writes exactly once, with the latest position.
	if err := svc.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.writes() != 1 {
		t.Fatalf("flush wrote %d times, want 1 (coalesced)", fake.writes())
	}
	if fake.last[string(item)] != 99000 {
		t.Errorf("flushed position = %d, want the latest 99000", fake.last[string(item)])
	}

	// A second flush with nothing buffered is a no-op.
	_ = svc.Flush(ctx)
	if fake.writes() != 1 {
		t.Errorf("empty flush wrote again (%d total)", fake.writes())
	}
}

func TestFlushRequeuesOnError(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	svc.Progress("", "item-1", 1000)
	svc.Progress("", "item-2", 2000)

	// A failing flush must surface the error and lose nothing: both positions
	// are re-queued for the next flush.
	fake.fail = errors.New("database is locked")
	if err := svc.Flush(ctx); err == nil {
		t.Fatal("flush should return the write error")
	}
	if fake.writes() != 0 {
		t.Fatalf("no writes should have landed, got %d", fake.writes())
	}

	// The next flush (store healthy) persists both re-queued positions.
	fake.fail = nil
	if err := svc.Flush(ctx); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if fake.last["item-1"] != 1000 || fake.last["item-2"] != 2000 {
		t.Errorf("re-queued positions lost: %+v", fake.last)
	}
}

// TestFlushNewestWins verifies a newer tick that arrives while a flush is failing
// is not clobbered by re-queuing the older, failed position.
func TestFlushNewestWins(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	svc.Progress("", "item-1", 1000)

	// Simulate a newer tick arriving during the (failing) write of the old value.
	fake.fail = errors.New("busy")
	fake.onWrite = func(item model.PID) {
		fake.onWrite = nil // once
		svc.Progress("", item, 5000)
	}
	_ = svc.Flush(ctx) // fails; must not re-queue 1000 over the newer 5000

	fake.fail = nil
	fake.onWrite = nil
	if err := svc.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if fake.last["item-1"] != 5000 {
		t.Errorf("position = %d, want the newer 5000 (the failed older tick must not win)", fake.last["item-1"])
	}
}

// TestCheckpointFlushesAll verifies Checkpoint routes through the serialized
// flush path (persisting all buffered positions), which is what makes it safe
// against a concurrent Flush overwriting a newer position with an older one.
func TestCheckpointFlushesAll(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	svc.Progress("", "other", 1000) // buffered, different item
	if err := svc.Checkpoint(ctx, "", "item-1", 7000); err != nil {
		t.Fatal(err)
	}
	// Both the checkpointed item and the previously buffered one are now persisted.
	if fake.last["item-1"] != 7000 || fake.last["other"] != 1000 {
		t.Errorf("checkpoint did not flush all pending: %+v", fake.last)
	}
}

func TestCheckpointWritesImmediately(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	const item model.PID = "item-1"

	svc.Progress("", item, 5000) // buffered
	if err := svc.Checkpoint(ctx, "", item, 7000); err != nil {
		t.Fatal(err)
	}
	if fake.writes() != 1 || fake.last[string(item)] != 7000 {
		t.Fatalf("checkpoint should write 7000 once, got writes=%d last=%d", fake.writes(), fake.last[string(item)])
	}
	// The checkpoint cleared the buffer, so a later flush does not re-write the
	// superseded tick.
	if err := svc.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.writes() != 1 {
		t.Errorf("flush re-wrote a superseded tick (%d writes)", fake.writes())
	}
}

// TestFlushDropsNotFound verifies a CodeNotFound write failure (the user or
// item is gone) is surfaced but NOT re-queued: a retry can never succeed, and a
// re-queued tick would linger forever, resurfacing through the StatesForItems
// overlay on every read. Transient errors keep the re-queue behavior.
func TestFlushDropsNotFound(t *testing.T) {
	fake := newFake()
	svc := New(fake)
	ctx := context.Background()
	svc.Progress("", "item-gone", 1000)

	fake.fail = waxerr.New(waxerr.CodeNotFound, "store.SetProgress", "no such item")
	if err := svc.Flush(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("flush err = %v, want the CodeNotFound surfaced", err)
	}

	// The tick was dropped, not re-queued: a healthy follow-up flush writes
	// nothing, and the overlay no longer synthesizes the phantom.
	fake.fail = nil
	if err := svc.Flush(ctx); err != nil {
		t.Fatalf("follow-up flush: %v", err)
	}
	if fake.writes() != 0 {
		t.Errorf("dropped tick was re-queued and written (%d writes)", fake.writes())
	}
	got, err := svc.StatesForItems(ctx, []model.PID{"item-gone"})
	if err != nil {
		t.Fatalf("StatesForItems: %v", err)
	}
	if _, ok := got["item-gone"]; ok {
		t.Errorf("phantom state survived the not-found drop: %+v", got)
	}
}

// TestStatesForItemsOverlay pins the bulk-read overlay: a buffered position
// replaces the flushed one on its (user, item) state, a buffered-but-unflushed
// pair gets a position-only state synthesized (the window an unsubscribe check
// must not miss), the empty-pid default-user sentinel matches the default
// user's flushed row instead of duplicating it, and per-item user order holds.
func TestStatesForItemsOverlay(t *testing.T) {
	fake := newFake()
	fake.flushed = map[model.PID][]model.PlayState{
		"item-1": {
			{UserPID: "u-alice", ItemPID: "item-1", PositionMS: 1000, Starred: true},
			{UserPID: "u-zed", ItemPID: "item-1", PositionMS: 2000},
		},
		"item-4": {
			{UserPID: "u-default", ItemPID: "item-4", PositionMS: 4000, PlayCount: 3},
		},
	}
	svc := New(fake)
	ctx := context.Background()

	svc.Progress("u-alice", "item-1", 5000)  // replaces alice's flushed position
	svc.Progress("u-bob", "item-2", 7000)    // synthesized: nothing flushed for item-2
	svc.Progress("u-middle", "item-1", 1500) // synthesized between alice and zed
	svc.Progress("", "item-4", 4321)         // sentinel: must hit u-default's row, not duplicate it
	svc.Progress("u-x", "item-9", 9000)      // not requested: must not appear

	got, err := svc.StatesForItems(ctx, []model.PID{"item-1", "item-2", "item-3", "item-4"})
	if err != nil {
		t.Fatalf("StatesForItems: %v", err)
	}

	s1 := got["item-1"]
	if len(s1) != 3 {
		t.Fatalf("item-1 states = %+v, want alice + synthesized middle + zed", s1)
	}
	for i := 1; i < len(s1); i++ {
		if !(s1[i-1].UserPID < s1[i].UserPID) {
			t.Fatalf("item-1 states out of user order: %+v", s1)
		}
	}
	byUser := map[model.PID]model.PlayState{}
	for _, s := range s1 {
		byUser[s.UserPID] = s
	}
	if s := byUser["u-alice"]; s.PositionMS != 5000 || !s.Starred {
		t.Errorf("alice overlay = %+v, want buffered position 5000 on the flushed state", s)
	}
	if s := byUser["u-middle"]; s.PositionMS != 1500 || s.ItemPID != "item-1" {
		t.Errorf("synthesized middle = %+v, want a position-only state", s)
	}
	if s := byUser["u-zed"]; s.PositionMS != 2000 {
		t.Errorf("zed = %+v, want the flushed position untouched", s)
	}

	if s2 := got["item-2"]; len(s2) != 1 || s2[0].UserPID != "u-bob" || s2[0].PositionMS != 7000 {
		t.Errorf("item-2 states = %+v, want one synthesized state for bob", s2)
	}
	if _, ok := got["item-3"]; ok {
		t.Error("item-3 (untouched, nothing buffered) must be absent")
	}
	if _, ok := got["item-9"]; ok {
		t.Error("item-9 was not requested and must be absent")
	}

	// The sentinel-buffered position landed on the default user's row: one state,
	// its position replaced, its other fields intact.
	s4 := got["item-4"]
	if len(s4) != 1 {
		t.Fatalf("item-4 states = %+v, want the sentinel matched onto one row", s4)
	}
	if s4[0].UserPID != "u-default" || s4[0].PositionMS != 4321 || s4[0].PlayCount != 3 {
		t.Errorf("item-4 overlay = %+v, want u-default with buffered 4321 and flushed play count", s4[0])
	}

	// With nothing buffered for the requested items, the flushed map passes
	// through untouched.
	if err := svc.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got2, err := svc.StatesForItems(ctx, []model.PID{"item-1"})
	if err != nil {
		t.Fatalf("StatesForItems after flush: %v", err)
	}
	if len(got2["item-1"]) != 2 {
		t.Errorf("after flush item-1 = %+v, want only the flushed rows (fake ignores writes)", got2["item-1"])
	}
}

func TestPlayStateImporterDefaultNoop(t *testing.T) {
	s := New(&fakeStore{})
	// The default importer is a no-op: it imports nothing and reports every record skipped.
	res, err := s.Importer().ImportPlayState(context.Background(), []PlayStateRecord{
		{ItemPID: "i1", PositionMS: 1000}, {ItemPID: "i2", Played: true},
	})
	if err != nil {
		t.Fatalf("default import: %v", err)
	}
	if res.Imported != 0 || res.Skipped != 2 {
		t.Errorf("default importer result = %+v, want 0 imported / 2 skipped", res)
	}

	// A concrete adapter can be installed; SetImporter(nil) restores the no-op.
	s.SetImporter(recordingImporter{})
	if _, ok := s.Importer().(recordingImporter); !ok {
		t.Fatal("SetImporter did not install the adapter")
	}
	s.SetImporter(nil)
	if _, ok := s.Importer().(noopImporter); !ok {
		t.Fatal("SetImporter(nil) did not restore the no-op default")
	}
}

type recordingImporter struct{}

func (recordingImporter) ImportPlayState(_ context.Context, r []PlayStateRecord) (PlayStateImportResult, error) {
	return PlayStateImportResult{Imported: len(r)}, nil
}
