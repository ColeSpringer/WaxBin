package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
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
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, root)

	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Artist", "Album", 1))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
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

func itemCount(t *testing.T, lib *waxbin.Library, title string) int {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build())
	if err != nil {
		t.Fatalf("query %q: %v", title, err)
	}
	return len(items)
}

func itemState(t *testing.T, lib *waxbin.Library, title string) model.ItemState {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build())
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
