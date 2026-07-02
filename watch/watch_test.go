package watch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/fsnotify/fsnotify"
)

// mockEngine records the operations the watcher drives.
type mockEngine struct {
	mu        sync.Mutex
	rescans   []rescanCall
	analyzes  int
	syncs     int
	changed   bool
	rescanErr error
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
	w := New(eng, roots, Options{
		Interval:           40 * time.Millisecond,
		FullRescanInterval: -1, // disable the full ticker for this test
		Analyze:            true,
		Notify: func(a Activity) {
			mu.Lock()
			acts = append(acts, a)
			mu.Unlock()
		},
	}, nil)

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
	w := New(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{
		Interval:           time.Hour, // keep the fast ticker out of the way
		FullRescanInterval: 40 * time.Millisecond,
	}, nil)

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
	w := New(eng, []Root{{LibraryPID: "L1", Path: "/lib"}}, Options{Interval: time.Hour, FullRescanInterval: -1}, nil)
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
	w := New(eng, []Root{{LibraryPID: "L1", Path: dir}}, Options{
		Interval:           time.Hour, // keep the scheduled ticker out of the way
		FullRescanInterval: -1,
		Live:               true,
		WriteSettle:        30 * time.Millisecond,
	}, nil)

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
	w := New(&mockEngine{}, []Root{
		{LibraryPID: "MUSIC", Path: "/lib/music"},
		{LibraryPID: "BOOKS", Path: "/lib/books"},
	}, Options{}, nil)
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
