package playback

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// fakeStore records progress writes so the coalescing behavior can be asserted.
type fakeStore struct {
	mu       sync.Mutex
	progress []int64 // positions written, in order
	last     map[string]int64
	fail     error                // when set, SetProgress fails with it
	onWrite  func(item model.PID) // hook invoked inside SetProgress (for race simulation)
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

// Unused-by-these-tests methods.
func (f *fakeStore) MarkPlayed(context.Context, model.PID, model.PID, bool) error { return nil }
func (f *fakeStore) SetRating(context.Context, model.PID, model.PID, *int) error  { return nil }
func (f *fakeStore) SetStar(context.Context, model.PID, model.PID, bool) error    { return nil }
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
