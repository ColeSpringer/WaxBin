package watch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
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

func TestDebouncerCoalesces(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	d := newDebouncer(30*time.Millisecond, func(dir string) {
		mu.Lock()
		got[dir]++
		mu.Unlock()
	})
	defer d.stop()

	// Five rapid schedules of the same dir collapse to one call.
	for i := 0; i < 5; i++ {
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
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer watcher.Close()

	lw := &liveWatch{watcher: watcher, log: slog.New(slog.NewTextHandler(io.Discard, nil)), max: 3}
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
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer watcher.Close()

	lw := &liveWatch{watcher: watcher, log: slog.New(slog.NewTextHandler(io.Discard, nil)), max: 0}
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
// removes one, gives its place under the cap back, and one moved away gives back its
// subtree's, so the watcher can still arm the folders that arrive later.
func TestLiveWatchForgetsARemovedFolder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", filepath.Join("b", "c")} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer watcher.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lw := &liveWatch{watcher: watcher, log: log, max: 4}
	if added, _ := lw.addTree(root); added != 4 {
		t.Skipf("armed %d of 4 watches", added)
	}
	w := &Watcher{log: log}
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()

	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: filepath.Join(root, "a"), Op: fsnotify.Remove})
	if lw.total() != 3 {
		t.Fatalf("total after a removal = %d, want 3", lw.total())
	}
	if err := os.Rename(filepath.Join(root, "b"), filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: filepath.Join(root, "b"), Op: fsnotify.Rename})
	if lw.total() != 1 {
		t.Fatalf("total after a subtree moved away = %d, want 1", lw.total())
	}
	for _, name := range []string{"d", "e", "f"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, exhausted := lw.addTree(filepath.Join(root, name)); exhausted {
			t.Fatalf("arming %s hit the cap with %d armed", name, lw.total())
		}
	}
}

// TestLiveWatchRescansARemovedFolderItself: a watched folder's removal, which fsnotify
// reports on the folder's own watch and on its parent's, schedules a rescan of that
// folder, which a scan reads as gone and reconciles, not of the folder above it, which
// for a first-level folder is the whole library.
func TestLiveWatchRescansARemovedFolderItself(t *testing.T) {
	root := t.TempDir()
	artist := filepath.Join(root, "Artist")
	if err := os.Mkdir(artist, 0o755); err != nil {
		t.Fatal(err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer watcher.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lw := &liveWatch{watcher: watcher, log: log}
	if added, _ := lw.addTree(root); added != 2 {
		t.Skipf("armed %d of 2 watches", added)
	}
	got := make(chan string, 8)
	deb := newDebouncer(time.Millisecond, func(dir string) { got <- dir })
	defer deb.stop()
	if err := os.Remove(artist); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{log: log}
	for range 2 {
		w.handleEvent(lw, deb, fsnotify.Event{Name: artist, Op: fsnotify.Remove})
	}
	var dirs []string
	timeout := time.After(200 * time.Millisecond)
collect:
	for {
		select {
		case d := <-got:
			dirs = append(dirs, d)
		case <-timeout:
			break collect
		}
	}
	if len(dirs) != 1 || dirs[0] != artist {
		t.Fatalf("rescans scheduled for %v, want the removed folder alone", dirs)
	}
}
