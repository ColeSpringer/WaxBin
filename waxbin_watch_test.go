package waxbin_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestWatchRefusesReadOnly confirms watch is refused on a read-only library.
func TestWatchRefusesReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Create the catalog first so a read-only open succeeds.
	openManaged(t, ctx, db, root).Close()

	ro, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db, ReadOnly: true,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged}},
	})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	if err := ro.Watch(ctx, waxbin.WatchOptions{Interval: time.Second}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("watch read-only: want CodeUnsupported, got %v", err)
	}
}

// TestFsMutateSharedLease confirms scan and organize run under one shared lease
// scope, so at most one filesystem mutator runs at a time (the coordination the
// watcher relies on).
func TestFsMutateSharedLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, root)

	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Artist", "Album", 1))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply organize: %v", err)
	}

	jobs, err := lib.Jobs(ctx, 50)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	var scanScope, orgScope string
	for _, j := range jobs {
		switch j.Kind {
		case "scan":
			scanScope = j.Scope
		case "organize":
			orgScope = j.Scope
		}
	}
	if scanScope == "" || orgScope == "" {
		t.Fatalf("missing jobs: scan=%q organize=%q", scanScope, orgScope)
	}
	if scanScope != orgScope {
		t.Errorf("scan scope %q != organize scope %q; a shared fs-mutate lease is required", scanScope, orgScope)
	}
}

// TestWatchScheduledCatalogsAndReconciles runs a real watcher on a short interval:
// a dropped file is cataloged within an interval and a deleted one is reconciled to
// missing, then Ctrl-C (context cancel) exits with CodeCanceled.
func TestWatchScheduledCatalogsAndReconciles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, root)

	// Seed one file so the library is non-empty (the survival gate needs a floor).
	writeFile(t, filepath.Join(root, "seed.mp3"), testaudio.BuildMP3WithAudio("Seed", "Artist", "Album", 1, testaudio.AudioWithSeed(9)))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- lib.Watch(runCtx, waxbin.WatchOptions{Interval: 80 * time.Millisecond, FullRescanInterval: -1})
	}()
	defer func() {
		cancel()
		if err := <-done; !waxerr.Is(err, waxerr.CodeCanceled) {
			t.Errorf("watch exit = %v, want CodeCanceled", err)
		}
	}()

	// Drop a new file; the scheduled rescan should catalog it.
	writeFile(t, filepath.Join(root, "drop.mp3"), testaudio.BuildMP3WithAudio("Dropped", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "Dropped") == 1 })

	// Delete it; the next rescan should reconcile it to missing (seed keeps the floor).
	if err := os.Remove(filepath.Join(root, "drop.mp3")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 4*time.Second, func() bool { return itemState(t, lib, "Dropped") == model.StateMissing })
}

// TestWatchFollowsAddRoot: a root registered while the watcher runs is followed on its
// next scheduled tick, so its files are cataloged without a restart.
func TestWatchFollowsAddRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, rootA)

	writeFile(t, filepath.Join(rootA, "seed.mp3"), testaudio.BuildMP3WithAudio("Seed", "Artist", "Album", 1, testaudio.AudioWithSeed(9)))
	writeFile(t, filepath.Join(rootB, "later.mp3"), testaudio.BuildMP3WithAudio("Later", "Artist", "Album", 2, testaudio.AudioWithSeed(3)))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- lib.Watch(runCtx, waxbin.WatchOptions{Interval: 80 * time.Millisecond, FullRescanInterval: -1})
	}()
	defer func() {
		cancel()
		if err := <-done; !waxerr.Is(err, waxerr.CodeCanceled) {
			t.Errorf("watch exit = %v, want CodeCanceled", err)
		}
	}()

	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "Seed") == 1 })
	if _, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged, Profile: "waxbin-native"}); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "Later") == 1 })
}

func itemCount(t *testing.T, lib *waxbin.Library, title string) int {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build(), "")
	if err != nil {
		t.Fatalf("query %q: %v", title, err)
	}
	return len(items)
}

func itemState(t *testing.T, lib *waxbin.Library, title string) model.ItemState {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build(), "")
	if err != nil || len(items) == 0 {
		return ""
	}
	return items[0].State
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met within timeout")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestSidecarEditReportsChanged is the watch-mode regression guard for every route a
// .lrc change can take. No pre-existing test referenced SidecarsUpdated, so nothing
// else catches this.
//
// The failure it guards is silent: a .lrc edit changes no audio bytes, so
// ContentChanged is false and ItemCreated is false. Without ScanItemResult
// .SidecarsChanged feeding the full path's counter switch, every counter stays zero,
// the scan reports changed=false, and watch mode's downstream schedulers (analyze,
// enrich, source sync) simply stop firing on sidecar edits, with no error anywhere.
func TestSidecarEditReportsChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, root)

	audio := filepath.Join(root, "a.mp3")
	if err := os.WriteFile(audio, testaudio.BuildMP3("A", "Band", "One", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lrc := filepath.Join(root, "a.lrc")
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("initial scan: %v", err)
	}

	writeLRC := func(body string) {
		t.Helper()
		if err := os.WriteFile(lrc, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Each destructive case starts from a known-good, already-ingested .lrc, so the
	// step under test is the only thing the catalog has left to change. (Chaining two
	// destructive steps would make the second a real no-op, since the lyrics would
	// already be gone, leaving the assertion vacuous.)
	restoreGood := func() {
		t.Helper()
		writeLRC("[00:00.00]hi\n[00:01.00]there\n")
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			t.Fatal(err)
		}
	}

	// Each step edits only the .lrc; the audio bytes never change.
	steps := []struct {
		name  string
		setup func()
		mut   func()
	}{
		{"added", nil, func() { writeLRC("[00:00.00]hi\n[00:01.00]there\n") }},
		{"edited", nil, func() { writeLRC("[00:00.00]hi\n[00:02.50]changed\n[00:04.00]more\n") }},
		// The two holes that already existed before the .lrc routing change: both
		// already reached the full path and already reported changed=false.
		{"edited to nothing usable", restoreGood, func() { writeLRC("just plain text, no timestamps\n") }},
		{"vanished", restoreGood, func() {
			if err := os.Remove(lrc); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.setup != nil {
				s.setup()
			}
			s.mut()
			res, err := lib.Scan(ctx, waxbin.ScanRequest{})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			tot := res.Total
			if tot.SidecarsUpdated != 1 {
				t.Errorf("SidecarsUpdated = %d, want 1", tot.SidecarsUpdated)
			}
			if tot.ItemsUpdated != 0 || tot.ItemsCreated != 0 {
				t.Errorf("ItemsUpdated=%d ItemsCreated=%d, want 0/0: the audio bytes did not change",
					tot.ItemsUpdated, tot.ItemsCreated)
			}
			// Exactly the expression watch mode's Rescan uses to decide whether to run
			// the downstream schedulers.
			changed := tot.ItemsCreated > 0 || tot.ItemsUpdated > 0 || tot.Relinked > 0 ||
				tot.Missing > 0 || tot.SidecarsUpdated > 0
			if !changed {
				t.Error("scan reports changed=false for a sidecar edit; watch mode's downstream schedulers would be silently skipped")
			}
		})
	}
}

// lockedLog is a log sink the watcher and the scans it runs can write while a test reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLog) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// removeRootPast removes a library, retrying while the watcher's own rescan holds the
// filesystem lease.
func removeRootPast(t *testing.T, lib *waxbin.Library, pid model.PID, force bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		_, err := lib.RemoveRoot(context.Background(), pid, waxbin.RemoveRootOptions{Force: force})
		if err == nil {
			return
		}
		if !waxerr.Is(err, waxerr.CodeConflict) || time.Now().After(deadline) {
			t.Fatalf("remove root: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWatchOfARemovedLibraryStops: a watch scoped to one library stops once that library
// is removed, saying so once, where it used to warn on every tick that it could not
// rescan it.
func TestWatchOfARemovedLibraryStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	var logs lockedLog
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	writeFile(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3WithAudio("In B", "Artist", "Album", 1, testaudio.AudioWithSeed(7)))
	b, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- lib.Watch(ctx, waxbin.WatchOptions{LibraryPID: b.PID, Interval: 40 * time.Millisecond, FullRescanInterval: -1})
	}()
	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "In B") == 1 })
	removeRootPast(t, lib, b.PID, false)
	select {
	case err := <-done:
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Fatalf("watch exit = %v, want CodeNotFound", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the watch kept running after its library was removed")
	}
	if n := logs.count("watched library is gone"); n != 1 {
		t.Errorf("logged the stop %d times, want once", n)
	}
}

// TestWatchIdlesWhenItsLastRootIsRemoved: a watch over every library whose last one is
// removed stops rescanning it, where it used to keep the stale set and warn each tick,
// and follows a root registered after.
func TestWatchIdlesWhenItsLastRootIsRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	var logs lockedLog
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	writeFile(t, filepath.Join(rootA, "a.mp3"), testaudio.BuildMP3WithAudio("In A", "Artist", "Album", 1, testaudio.AudioWithSeed(8)))
	writeFile(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3WithAudio("In B", "Artist", "Album", 2, testaudio.AudioWithSeed(9)))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- lib.Watch(runCtx, waxbin.WatchOptions{Interval: 40 * time.Millisecond, FullRescanInterval: -1})
	}()
	defer func() {
		cancel()
		if err := <-done; !waxerr.Is(err, waxerr.CodeCanceled) {
			t.Errorf("watch exit = %v, want CodeCanceled", err)
		}
	}()
	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "In A") == 1 })
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatalf("libraries = %+v (err %v)", libs, err)
	}
	removeRootPast(t, lib, libs[0].PID, true)
	time.Sleep(300 * time.Millisecond)
	if n := logs.count("rescan error") + logs.count("roots unreadable"); n > 1 {
		t.Errorf("the watch warned %d times after its last root went, want it quiet", n)
	}
	if _, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeInPlace}); err != nil {
		t.Fatalf("add root: %v", err)
	}
	waitFor(t, 4*time.Second, func() bool { return itemCount(t, lib, "In B") == 1 })
}
