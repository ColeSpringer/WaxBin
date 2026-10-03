package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// TestWatchRescanCountsAPromotionAsAChange: a rescan whose only effect was promoting a
// copy into a vanished primary's place reports a change, so the watcher's analyze pass
// runs for the promoted file.
func TestWatchRescanCountsAPromotionAsAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib, err := Open(ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
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
