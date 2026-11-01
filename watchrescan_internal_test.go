package waxbin

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestWatchRescanCountsAPromotionAsAChange: a rescan whose only effect was promoting a
// copy into a vanished primary's place reports a change, so the watcher's analyze pass
// runs for the promoted file.
func TestWatchRescanCountsAPromotionAsAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	primary := filepath.Join(root, "1.mp3")
	writeRaw(t, primary, testaudio.BuildMP3WithAudio("Original", "Band", "One", 1, testaudio.AudioWithSeed(9)))
	writeRaw(t, filepath.Join(root, "2.mp3"), testaudio.BuildMP3WithAudio("Copy", "Band", "One", 1, testaudio.AudioWithSeed(9)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := os.Remove(primary); err != nil {
		t.Fatal(err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := (&watchEngine{lib: lib}).Rescan(ctx, libs[0].PID, "", false)
	if err != nil || !changed {
		t.Errorf("rescan changed = %v (err %v), want a change", changed, err)
	}
}

// TestLogTickCallsOnlyAHeldLeaseBusy: a tick that lost the lease to another mutator is
// quiet, and any other conflict, which the next tick meets again, is a warning.
func TestLogTickCallsOnlyAHeldLeaseBusy(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	e := &watchEngine{lib: &Library{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	e.logTick("watch: inbox import", waxerr.New(waxerr.CodeConflict, "jobs.RunLeased", "busy").WithReason(jobs.ReasonLeaseHeld))
	e.logTick("watch: inbox import", waxerr.New(waxerr.CodeConflict, "store.EnsurePodcastLibrary", "podcast dir is already a music library root"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "level=DEBUG") || !strings.Contains(lines[0], "busy; retrying next tick") ||
		!strings.Contains(lines[1], "level=WARN") || strings.Contains(lines[1], "retrying next tick") {
		t.Fatalf("log = %q, want the held lease at debug and the other conflict a warning", lines)
	}
}

// TestWatchRescanCountsARevivalAsAChange: a rescan whose only effect was finding a missing
// item's file again reports a change, so the watcher's downstream passes run for it.
func TestWatchRescanCountsARevivalAsAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	path := filepath.Join(root, "1.mp3")
	data := testaudio.BuildMP3WithAudio("Song", "Band", "One", 1, testaudio.AudioWithSeed(4))
	writeRaw(t, path, data)
	res, err := lib.Scan(ctx, ScanRequest{})
	if err != nil || res.Total.ItemsCreated != 1 {
		t.Fatalf("scan = %+v (err %v)", res, err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	file, err := lib.store.FileByPath(ctx, []byte(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.store.MarkFilesMissing(ctx, []model.PID{file.PID}); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, path, data)
	changed, err := (&watchEngine{lib: lib}).Rescan(ctx, libs[0].PID, "", false)
	if err != nil || !changed {
		t.Errorf("rescan changed = %v (err %v), want a change", changed, err)
	}
}
