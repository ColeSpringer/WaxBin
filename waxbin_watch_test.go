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
