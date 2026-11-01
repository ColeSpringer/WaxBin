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

// TestATrashMoveCutOffByACrashRestores: a process that dies after moving a file into the
// trash and before writing its journal row leaves a pending row; the next open finishes
// the move from it, and the entry restores the item, pid and all.
func TestATrashMoveCutOffByACrashRestores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, testaudio.BuildMP3("Song", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}}
	lib, err := OpenSeeded(t, ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	f, err := lib.store.FileByPath(ctx, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	item, _, err := lib.store.FileOwner(ctx, f.PID)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, model.TrashDirName, string(model.NewPID()), "song.mp3")
	pending := model.TrashPending{PID: model.NewPID(), FilePID: f.PID, TrashPath: []byte(dst), TrashDisplay: dst}
	if err := lib.store.PlanTrash(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	lib, err = OpenSeeded(t, ctx, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if it, err := lib.store.ItemByPID(ctx, item); err != nil || it.State != model.StateArchived {
		t.Fatalf("item after the recovery = %+v (err %v), want it archived", it, err)
	}
	if err := lib.RestoreTrash(ctx, pending.PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	it, err := lib.store.ItemByPID(ctx, item)
	if err != nil || it.State != model.StatePresent {
		t.Fatalf("item after the restore = %+v (err %v), want it present again", it, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the file is not back at its path: %v", err)
	}
}
