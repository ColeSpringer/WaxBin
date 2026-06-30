package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// openRootedStore opens a store whose single managed library root is dir, so
// recovery can compare journal paths against real files on disk.
func openRootedStore(t *testing.T, dir, db, owner string) (*sqlite.Store, *model.Library) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: owner})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(dir), DisplayRoot: dir, Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		_ = st.Close()
		t.Fatalf("ensure library: %v", err)
	}
	return st, lib
}

// TestRecoverOrganizeFinishesCompletedMove simulates a crash after the on-disk
// move but before CommitMove: the planned journal row is left behind and the file
// row still points at the source. Reopening must finish the move.
func TestRecoverOrganizeFinishesCompletedMove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "c.db")

	st, lib := openRootedStore(t, dir, db, "owner-a")
	src := filepath.Join(dir, "old.mp3")
	dst := filepath.Join(dir, "Artist", "Album", "01 - Song.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, src, "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Plan the move and perform it on disk, but never commit (the crash).
	if _, err := st.PlanMove(ctx, model.RelocateInput{
		FilePID: r.FilePID, JobPID: "job", SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte(filepath.Join("Artist", "Album", "01 - Song.mp3")),
	}); err != nil {
		t.Fatalf("plan move: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil { // crash: close holding a planned row
		t.Fatalf("close: %v", err)
	}

	// Reopen: recovery sees dst present and src gone, so it finishes the move.
	st2, _ := openRootedStore(t, dir, db, "owner-b")
	if _, err := st2.FileByPath(ctx, []byte(dst)); err != nil {
		t.Fatalf("recovery did not point the file at the destination: %v", err)
	}
	if _, err := st2.FileByPath(ctx, []byte(src)); err == nil {
		t.Fatal("source path should no longer resolve after recovery")
	}
}

// TestRecoverOrganizeRollsBackUnstartedMove simulates a crash after PlanMove but
// before the on-disk move: the source is still in place, so recovery must roll the
// journal row back and leave the catalog pointing at the source.
func TestRecoverOrganizeRollsBackUnstartedMove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "c.db")

	st, lib := openRootedStore(t, dir, db, "owner-a")
	src := filepath.Join(dir, "keep.mp3")
	dst := filepath.Join(dir, "Artist", "Album", "01 - Keep.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, src, "sha256:E", "sha256:C", "Keep"))
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := st.PlanMove(ctx, model.RelocateInput{
		FilePID: r.FilePID, JobPID: "job", SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte("x"),
	}); err != nil {
		t.Fatalf("plan move: %v", err)
	}
	// No on-disk move happened.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, _ := openRootedStore(t, dir, db, "owner-b")
	if _, err := st2.FileByPath(ctx, []byte(src)); err != nil {
		t.Fatalf("catalog should still point at the untouched source: %v", err)
	}
	if !fileOnDisk(src) {
		t.Fatal("source file should be untouched")
	}
}

func fileOnDisk(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
