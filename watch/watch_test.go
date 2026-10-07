package watch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/fsnotify/fsnotify"
)

// mockEngine records the operations the watcher drives. roots is what its Roots
// answers, which the watcher re-reads on every scheduled tick, so a test moves a root
// under a running watcher by setting it.
type mockEngine struct {
	mu        sync.Mutex
	rescans   []rescanCall
	analyzes  int
	syncs     int
	changed   bool
	rescanErr error
	roots     []Root
	rootsErr  error
}

type rescanCall struct {
	libPID  model.PID
	subPath string
	force   bool
}

func (m *mockEngine) Rescan(ctx context.Context, libPID model.PID, subPath string, force bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rescans = append(m.rescans, rescanCall{libPID, subPath, force})
	return m.changed, m.rescanErr
}

func (m *mockEngine) Analyze(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.analyzes++
	return nil
}

func (m *mockEngine) SyncSources(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncs++
	return nil
}

func (m *mockEngine) Roots(ctx context.Context) ([]Root, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rootsErr != nil {
		return nil, m.rootsErr
	}
	return slices.Clone(m.roots), nil
}

func (m *mockEngine) setRoots(roots ...Root) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roots = roots
}

// newWatcher builds a watcher whose engine reports the roots it starts with, the way
// the facade's does.
func newWatcher(eng *mockEngine, roots []Root, opts Options) *Watcher {
	eng.setRoots(roots...)
	return New(eng, roots, opts, nil)
}

func (m *mockEngine) rescanCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rescans)
}

func TestWatcherRefusesNoRoots(t *testing.T) {
	w := New(&mockEngine{}, nil, Options{Interval: time.Second}, nil)
	if err := w.Run(context.Background()); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("Run with no roots: want CodeInvalid, got %v", err)
	}
}

func TestWatcherScheduledRescan(t *testing.T) {
	eng := &mockEngine{changed: true}
	roots := []Root{{LibraryPID: "L1", Path: "/lib"}}
	var acts []Activity
	var mu sync.Mutex
	w := newWatcher(eng, roots, Options{
		Interval:           40 * time.Millisecond,
		FullRescanInterval: -1, // disable the full ticker for this test
		Analyze:            true,
		Notify: func(a Activity) {
			mu.Lock()
			acts = append(acts, a)
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Initial rescan + at least one scheduled tick.
	deadline := time.After(2 * time.Second)
	for eng.rescanCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("scheduled rescan did not fire; rescans=%d", eng.rescanCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("Run returned %v, want CodeCanceled", err)
	}

	// The first rescan was the initial catch-up; every rescan targeted the root.
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.rescans[0].libPID != "L1" || eng.rescans[0].subPath != "" {
		t.Errorf("initial rescan = %+v, want whole-library L1", eng.rescans[0])
	}
	if eng.analyzes == 0 {
		t.Error("analyze never ran despite changed rescans")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acts) == 0 || acts[0].Trigger != "initial" {
		t.Errorf("first activity = %+v, want initial", acts)
	}
}

// TestWatcherFullRescan confirms the long-cadence ticker forces a rescan.
func TestWatcherFullRescan(t *testing.T) {
	eng := &mockEngine{}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{
		Interval:           time.Hour, // keep the fast ticker out of the way
		FullRescanInterval: 40 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	forced := func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		for _, r := range eng.rescans {
			if r.force {
				return true
			}
		}
		return false
	}
	for !forced() {
		select {
		case <-deadline:
			t.Fatal("full-content (force) rescan never fired")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestRescanSkipsOnConflict verifies a busy (CodeConflict) rescan is swallowed so
// the watcher keeps running.
func TestRescanSkipsOnConflict(t *testing.T) {
	eng := &mockEngine{rescanErr: waxerr.New(waxerr.CodeConflict, "test", "busy")}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{Interval: time.Hour, FullRescanInterval: -1})
	changed, err := w.rescan(context.Background(), "L1", "", false)
	if err != nil || changed {
		t.Fatalf("conflict rescan = (%v, %v), want (false, nil)", changed, err)
	}
}

// TestDebouncerCoalesces runs on synctest's clock, which keeps each 5ms gap inside the
// settle window however long a loaded runner stalls the goroutine.
func TestDebouncerCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		got := map[string]int{}
		d := newDebouncer(30*time.Millisecond, func(dir string) {
			mu.Lock()
			got[dir]++
			mu.Unlock()
		})
		defer d.stop()

		// Five rapid schedules of the same dir collapse to one call.
		for range 5 {
			d.schedule("/a")
			time.Sleep(5 * time.Millisecond)
		}
		d.schedule("/b")
		time.Sleep(120 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		if got["/a"] != 1 {
			t.Errorf("dir /a fired %d times, want 1 (coalesced)", got["/a"])
		}
		if got["/b"] != 1 {
			t.Errorf("dir /b fired %d times, want 1", got["/b"])
		}
	})
}

// TestWatcherLive exercises the real fsnotify path: a file created in a watched
// directory triggers a directory-scoped rescan. It degrades gracefully: if watches
// cannot be armed (ENOSPC, unsupported fs) the watcher reports degraded and we skip.
func TestWatcherLive(t *testing.T) {
	dir := t.TempDir()
	eng := &mockEngine{changed: true}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: dir}}, Options{
		Interval:           time.Hour, // keep the scheduled ticker out of the way
		FullRescanInterval: -1,
		Live:               true,
		WriteSettle:        30 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Give the live layer a moment to arm.
	time.Sleep(200 * time.Millisecond)
	if w.Degraded() {
		t.Skip("filesystem watches unavailable in this environment")
	}

	if err := os.WriteFile(filepath.Join(dir, "new.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	liveRescan := func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		for _, r := range eng.rescans {
			if r.subPath == dir { // a directory-scoped (live) rescan
				return true
			}
		}
		return false
	}
	for !liveRescan() {
		select {
		case <-deadline:
			t.Fatal("live filesystem event did not trigger a directory rescan")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestLiveWatchAddTreeCap verifies the watch-count cap stops arming watches once the
// configured maximum is reached and reports exhaustion, so the caller degrades rather
// than silently over-consuming inotify watches.
func TestLiveWatchAddTreeCap(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} { // root + 4 = 5 watchable dirs
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lw := &liveWatch{src: testSource(t), log: quietLog, max: 3}
	added, exhausted := lw.addTree(root)
	if !exhausted {
		t.Fatal("expected exhausted=true when the directory count exceeds the cap")
	}
	if added == 0 {
		t.Skip("environment cannot arm any fsnotify watches")
	}
	if added != 3 || lw.total() != 3 {
		t.Errorf("added=%d total=%d, want exactly 3 (capped)", added, lw.total())
	}
}

// TestLiveWatchAddTreeUnbounded confirms max=0 arms every directory (no artificial cap).
func TestLiveWatchAddTreeUnbounded(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lw := &liveWatch{src: testSource(t), log: quietLog, max: 0}
	added, exhausted := lw.addTree(root)
	if added == 0 {
		t.Skip("environment cannot arm any fsnotify watches")
	}
	if exhausted {
		t.Error("unbounded addTree should not report exhausted on a tiny tree")
	}
	if added != 3 { // root + a + b
		t.Errorf("added=%d, want 3 (root + 2 subdirs)", added)
	}
}

func TestWithDefaultsFullRescanDisable(t *testing.T) {
	// A zero FullRescanInterval means "disabled" and must NOT be defaulted, so an
	// explicit `--full-interval 0` turns the periodic full rescan off.
	o := Options{}
	o.withDefaults()
	if o.FullRescanInterval != 0 {
		t.Errorf("FullRescanInterval = %v, want 0 (disabled, not defaulted)", o.FullRescanInterval)
	}
	// The other cadences still get their friendly defaults.
	if o.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want the 30s default", o.Interval)
	}
	if o.WriteSettle != 2*time.Second {
		t.Errorf("WriteSettle = %v, want the 2s default", o.WriteSettle)
	}
	// A caller-supplied cadence is preserved.
	o2 := Options{FullRescanInterval: time.Hour}
	o2.withDefaults()
	if o2.FullRescanInterval != time.Hour {
		t.Errorf("FullRescanInterval = %v, want the supplied 1h", o2.FullRescanInterval)
	}
}

func TestLibraryForPath(t *testing.T) {
	w := newWatcher(&mockEngine{}, []Root{
		{LibraryPID: "MUSIC", Path: "/lib/music"},
		{LibraryPID: "BOOKS", Path: "/lib/books"},
	}, Options{})
	if got := w.libraryForPath("/lib/music/artist/a.mp3"); got != "MUSIC" {
		t.Errorf("libraryForPath music = %q, want MUSIC", got)
	}
	if got := w.libraryForPath("/lib/books/b.m4b"); got != "BOOKS" {
		t.Errorf("libraryForPath books = %q, want BOOKS", got)
	}
	if got := w.libraryForPath("/elsewhere/x.mp3"); got != "" {
		t.Errorf("libraryForPath outside = %q, want empty", got)
	}
}

// waitFor polls until cond holds, failing with why rather than sleeping a fixed length,
// which the coarse Windows clock makes unreliable.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal(why)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestWatcherFollowsARootAddedLater: a root registered while the watcher runs is picked
// up on the next scheduled tick, and that tick's rescan is its catch-up.
func TestWatcherFollowsARootAddedLater(t *testing.T) {
	eng := &mockEngine{}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{
		Interval: 30 * time.Millisecond, FullRescanInterval: -1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the initial rescan never ran", func() bool { return eng.rescanCount() > 0 })
	eng.setRoots(Root{LibraryPID: "L1", Path: "/lib"}, Root{LibraryPID: "L2", Path: "/lib2"})

	waitFor(t, "the new root was never rescanned", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		for _, r := range eng.rescans {
			if r.libPID == "L2" && r.subPath == "" {
				return true
			}
		}
		return false
	})
}

// TestWatcherFollowsARelocatedRoot: a root moved under the running watcher resolves
// events at its new path and stops claiming its old one.
func TestWatcherFollowsARelocatedRoot(t *testing.T) {
	eng := &mockEngine{}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{
		Interval: 30 * time.Millisecond, FullRescanInterval: -1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the initial rescan never ran", func() bool { return eng.rescanCount() > 0 })
	eng.setRoots(Root{LibraryPID: "L1", Path: "/moved"})

	waitFor(t, "the relocated root was never followed", func() bool {
		return w.libraryForPath("/moved/a.mp3") == "L1"
	})
	if got := w.libraryForPath("/lib/a.mp3"); got != "" {
		t.Errorf("the old path still resolves to %q, want nothing", got)
	}
}

// TestWatcherKeepsRootsWhenTheEngineFails: a read failure is not evidence that a library
// lost its roots, so the current set stands and the rescans keep coming.
func TestWatcherKeepsRootsWhenTheEngineFails(t *testing.T) {
	eng := &mockEngine{rootsErr: waxerr.New(waxerr.CodeIO, "test", "unreadable")}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{
		Interval: 30 * time.Millisecond, FullRescanInterval: -1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the rescans stopped after a roots read failure", func() bool { return eng.rescanCount() >= 3 })
	eng.mu.Lock()
	defer eng.mu.Unlock()
	for _, r := range eng.rescans {
		if r.libPID != "L1" {
			t.Fatalf("rescanned %q, want only the root the watcher started with", r.libPID)
		}
	}
}

// TestLiveWatchArmsARootAddedLater: the live layer arms a new root's tree too, so a file
// dropped into it is picked up between ticks rather than at the next one.
func TestLiveWatchArmsARootAddedLater(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	eng := &mockEngine{changed: true}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: first}}, Options{
		Interval: 60 * time.Millisecond, FullRescanInterval: -1,
		Live: true, WriteSettle: 30 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	time.Sleep(200 * time.Millisecond)
	if w.Degraded() {
		t.Skip("filesystem watches unavailable in this environment")
	}

	eng.setRoots(Root{LibraryPID: "L1", Path: first}, Root{LibraryPID: "L2", Path: second})
	waitFor(t, "the new root was never followed", func() bool {
		return w.libraryForPath(filepath.Join(second, "x.mp3")) == "L2"
	})
	// The arm rides the same tick, on the event worker; give it a moment to land before
	// the write, or the event has no watch to fire on.
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(second, "new.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a file dropped into the new root triggered no live rescan", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		for _, r := range eng.rescans {
			if r.libPID == "L2" && r.subPath == second {
				return true
			}
		}
		return false
	})
}

// TestLiveWatchForgetsARemovedFolder: a watched folder that is removed, as a prune
// removes one, gives its place under the cap back, and the watched folders below it
// theirs (a Recycle Bin delete takes a folder whole and reports the folder alone), so
// the watcher can still arm the folders that arrive later.
func TestLiveWatchForgetsARemovedFolder(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(gone, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	lw := armedTree(t, root, 3)
	w := &Watcher{log: lw.log}
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()

	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: gone, Op: fsnotify.Remove})
	if lw.total() != 1 {
		t.Fatalf("total after a removal = %d, want 1", lw.total())
	}
	armArrivals(t, lw, root, "d", "e")
}

// TestLiveWatchForgetsAFolderMovedAway: a watched folder moved away gives back its own
// place under the cap and those of the watched folders below it. On Windows the move
// itself is the test: one watch per root leaves no handle below the folder to refuse it.
func TestLiveWatchForgetsAFolderMovedAway(t *testing.T) {
	root := t.TempDir()
	moved := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(moved, "c", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	lw := armedTree(t, root, 4)
	w := &Watcher{log: lw.log}
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()

	if err := os.Rename(moved, filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: moved, Op: fsnotify.Rename})
	if lw.total() != 1 {
		t.Fatalf("total after a subtree moved away = %d, want 1", lw.total())
	}
	armArrivals(t, lw, root, "e", "f", "g")
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// testSource opens the platform's event source, skipping where it cannot, and closes
// it with the test.
func testSource(t *testing.T) source {
	t.Helper()
	src, err := newSource()
	if err != nil {
		t.Skipf("filesystem events unavailable: %v", err)
	}
	t.Cleanup(func() { src.Close() })
	return src
}

// armedTree arms root and every folder below it under a cap the tree fills exactly,
// skipping where the environment cannot arm that many.
func armedTree(t *testing.T, root string, limit int) *liveWatch {
	t.Helper()
	lw := &liveWatch{src: testSource(t), log: quietLog, max: limit}
	if added, _ := lw.addTree(root); added != limit {
		t.Skipf("armed %d of %d watches", added, limit)
	}
	return lw
}

// armArrivals makes each named folder under root and arms it, failing when one finds the
// cap full.
func armArrivals(t *testing.T, lw *liveWatch, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, exhausted := lw.addTree(filepath.Join(root, name)); exhausted {
			t.Fatalf("arming %s hit the cap with %d armed", name, lw.total())
		}
	}
}

// TestLiveWatchRescansARemovedFolderItself: a watched folder's removal schedules a
// rescan of that folder, which a scan reads as gone and reconciles, not of the folder
// above it, which for a first-level folder is the whole library.
func TestLiveWatchRescansARemovedFolderItself(t *testing.T) {
	root := t.TempDir()
	artist := filepath.Join(root, "Artist")
	if err := os.Mkdir(artist, 0o755); err != nil {
		t.Fatal(err)
	}
	lw := armedTree(t, root, 2)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	if err := os.Remove(artist); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{log: lw.log}
	w.handleEvent(lw, deb, fsnotify.Event{Name: artist, Op: fsnotify.Remove})
	if dirs := pending(deb); !slices.Equal(dirs, []string{artist}) {
		t.Fatalf("rescans scheduled for %v, want the removed folder alone", dirs)
	}
}

// TestLiveWatchIgnoresAWriteOnAFolder: Windows reports a write on a folder whose
// entries changed, beside the entry's own event. It schedules nothing, where the folder
// above would be the whole library for a first-level folder; a write on a file still
// rescans the file's folder.
func TestLiveWatchIgnoresAWriteOnAFolder(t *testing.T) {
	root := t.TempDir()
	artist := filepath.Join(root, "Artist")
	if err := os.Mkdir(artist, 0o755); err != nil {
		t.Fatal(err)
	}
	lw := armedTree(t, root, 2)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: lw.log}
	w.handleEvent(lw, deb, fsnotify.Event{Name: artist, Op: fsnotify.Write})
	if dirs := pending(deb); len(dirs) != 0 {
		t.Fatalf("a write on a folder scheduled %v, want nothing", dirs)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: filepath.Join(artist, "x.mp3"), Op: fsnotify.Write})
	if dirs := pending(deb); !slices.Equal(dirs, []string{artist}) {
		t.Fatalf("a write on a file scheduled %v, want its folder", dirs)
	}
}

// pending lists the folders a debouncer holds a rescan for.
func pending(deb *debouncer) []string {
	deb.mu.Lock()
	defer deb.mu.Unlock()
	return slices.Sorted(maps.Keys(deb.timers))
}

// fakeSource is a source a test feeds by hand; it records the folders it is told about.
type fakeSource struct {
	events chan fsnotify.Event
	errs   chan error
	once   sync.Once
	mu     sync.Mutex
	adds   []string
	drops  []string
}

func newFakeSource() *fakeSource {
	return &fakeSource{events: make(chan fsnotify.Event), errs: make(chan error)}
}

func (f *fakeSource) Add(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, dir)
	return nil
}

func (f *fakeSource) Remove(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, dir)
	return nil
}

func (f *fakeSource) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeSource) Errors() <-chan error          { return f.errs }
func (f *fakeSource) Close() error {
	f.once.Do(func() { close(f.events); close(f.errs) })
	return nil
}

// told counts how often dir was added, or with dropped how often it was removed.
func (f *fakeSource) told(dir string, dropped bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.adds
	if dropped {
		list = f.drops
	}
	n := 0
	for _, d := range list {
		if d == dir {
			n++
		}
	}
	return n
}

// TestServeLiveDropsTrashEventsAndMarksLostCoverage: a change inside the library trash
// schedules nothing, since a rescan starting there would re-catalog what it holds; a
// change beside it rescans its folder; and a source that lost a root's coverage leaves
// the watcher degraded.
func TestServeLiveDropsTrashEventsAndMarksLostCoverage(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{WriteSettle: 10 * time.Millisecond})
	src := newFakeSource()
	reqs := make(chan rescanReq, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.serveLive(ctx, src, reqs, nil); close(done) }()
	defer func() { cancel(); <-done }()

	src.events <- fsnotify.Event{Name: filepath.Join(root, model.TrashDirName, "x", "a.mp3"), Op: fsnotify.Create}
	src.events <- fsnotify.Event{Name: filepath.Join(root, "Artist", "a.mp3"), Op: fsnotify.Create}
	select {
	case r := <-reqs:
		if r.libPID != "L1" || r.subPath != filepath.Join(root, "Artist") {
			t.Fatalf("first rescan = %+v, want the folder beside the trash", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no rescan for a change beside the trash")
	}
	// The trash change was scheduled first if it was scheduled at all, so by now its
	// rescan would be here or arrive shortly.
	select {
	case r := <-reqs:
		t.Fatalf("a change inside the trash scheduled %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	if w.Degraded() {
		t.Fatal("degraded before any coverage was lost")
	}
	src.errs <- fmt.Errorf("%w: %s", errCoverageLost, root)
	waitFor(t, "a lost root left the watcher undegraded", w.Degraded)
}

// TestServeLiveDegradesWhenTheSourceEnds: a source whose streams close while the
// watcher runs leaves it degraded, since nothing live is left behind them.
func TestServeLiveDegradesWhenTheSourceEnds(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{})
	src := newFakeSource()
	done := make(chan struct{})
	go func() { w.serveLive(context.Background(), src, make(chan rescanReq, 8), nil); close(done) }()
	waitFor(t, "the root was never armed", func() bool { return src.told(root, false) == 1 })
	src.Close()
	<-done
	if !w.Degraded() {
		t.Fatal("the source ended and the watcher is not degraded")
	}
}

// TestServeLiveRescansEveryRootOnOverflow: an overflow lost events nothing can name, so
// every root is rescanned now rather than at the scheduled tick.
func TestServeLiveRescansEveryRootOnOverflow(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	w := newWatcher(&mockEngine{}, []Root{{LibraryPID: "A", Path: a}, {LibraryPID: "B", Path: b}}, Options{WriteSettle: 10 * time.Millisecond})
	src := newFakeSource()
	reqs := make(chan rescanReq, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.serveLive(ctx, src, reqs, nil); close(done) }()
	defer func() { cancel(); <-done }()

	src.errs <- fsnotify.ErrEventOverflow
	got := map[model.PID]string{}
	for range 2 {
		select {
		case r := <-reqs:
			got[r.libPID] = r.subPath
		case <-time.After(3 * time.Second):
			t.Fatalf("rescans after an overflow = %v, want both roots", got)
		}
	}
	if got["A"] != a || got["B"] != b {
		t.Fatalf("rescans after an overflow = %v, want both roots whole", got)
	}
}

// TestServeLiveRearmsARootOfferedAgain: a root whose watch went with its folder is
// armed again when the tick offers it, and one still armed is left alone.
func TestServeLiveRearmsARootOfferedAgain(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{})
	src := newFakeSource()
	added := make(chan Root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.serveLive(ctx, src, make(chan rescanReq, 8), added); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the root was never armed", func() bool { return src.told(root, false) == 1 })

	added <- Root{LibraryPID: "L1", Path: root}
	src.events <- fsnotify.Event{Name: root, Op: fsnotify.Rename}
	waitFor(t, "the renamed root was never forgotten", func() bool { return src.told(root, true) == 1 })
	if n := src.told(root, false); n != 1 {
		t.Fatalf("the root was armed %d times while it held, want once", n)
	}
	added <- Root{LibraryPID: "L1", Path: root}
	waitFor(t, "the root was not armed again", func() bool { return src.told(root, false) == 2 })
}

// TestRefreshRootsOffersEveryRoot: the tick offers the live layer every root, not only
// the new ones, so a root whose watch went can be armed again.
func TestRefreshRootsOffersEveryRoot(t *testing.T) {
	eng := &mockEngine{}
	w := newWatcher(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{})
	eng.setRoots(Root{LibraryPID: "L1", Path: "/lib"}, Root{LibraryPID: "L2", Path: "/lib2"})
	added := make(chan Root, 4)
	w.refreshRoots(context.Background(), added)
	var got []Root
	for len(added) > 0 {
		got = append(got, <-added)
	}
	want := []Root{{LibraryPID: "L1", Path: "/lib"}, {LibraryPID: "L2", Path: "/lib2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("offered %v, want %v", got, want)
	}
}
