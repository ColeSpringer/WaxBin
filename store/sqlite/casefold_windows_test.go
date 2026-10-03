//go:build windows

package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestRecoverOrganizeFinishesACaseOnlyRename: on a case-insensitive filesystem the source
// of a case-only rename resolves whether or not the rename happened, so recovery reads the
// folders' spellings: a crash before the rename rolls the move back, a crash after it
// finishes the move.
func TestRecoverOrganizeFinishesACaseOnlyRename(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "c.db")

	st, lib := openRootedStore(t, dir, db, "owner-a")
	src := filepath.Join(dir, "author", "song.mp3")
	dst := filepath.Join(dir, "Author", "Song.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, src, "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	move := model.RelocateInput{
		FilePID: r.FilePID, JobPID: "job", SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte(filepath.Join("Author", "Song.mp3")),
	}
	if _, err := st.PlanMove(ctx, move); err != nil {
		t.Fatalf("plan move: %v", err)
	}
	if err := st.Close(); err != nil { // crash before the rename
		t.Fatalf("close: %v", err)
	}
	st2, _ := openRootedStore(t, dir, db, "owner-b")
	if _, err := st2.FileByPath(ctx, []byte(src)); err != nil {
		t.Fatalf("catalog should keep the source before the rename: %v", err)
	}

	if _, err := st2.PlanMove(ctx, move); err != nil {
		t.Fatalf("plan move again: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "author"), filepath.Join(dir, "Author")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "Author", "song.mp3"), dst); err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil { // crash after the rename
		t.Fatalf("close: %v", err)
	}
	st3, _ := openRootedStore(t, dir, db, "owner-c")
	if _, err := st3.FileByPath(ctx, []byte(dst)); err != nil {
		t.Fatalf("recovery should finish the landed rename: %v", err)
	}
	if _, err := st3.FileByPath(ctx, []byte(src)); err == nil {
		t.Fatal("the old spelling should no longer resolve after recovery")
	}
}

// TestScanRelinksAnExternalCaseOnlyRename: a file re-cased outside WaxBin on a
// case-insensitive filesystem still resolves under its old spelling, and relinks to the new
// one as a move rather than landing as a copy, since the folders no longer list the old.
func TestScanRelinksAnExternalCaseOnlyRename(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, lib := openRootedStore(t, dir, filepath.Join(t.TempDir(), "c.db"), "owner")
	src := filepath.Join(dir, "author", "x.mp3")
	dst := filepath.Join(dir, "Author", "X.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := mustPut(t, st, input(lib.ID, src, "sha256:SAME", "sha256:C", "One"))
	if err := os.Rename(filepath.Join(dir, "author"), filepath.Join(dir, "Author")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "Author", "x.mp3"), dst); err != nil {
		t.Fatal(err)
	}
	again := mustPut(t, st, input(lib.ID, dst, "sha256:SAME", "sha256:C", "One"))
	if !again.Relinked || again.FilePID != first.FilePID || again.RelinkedFrom != src || again.AttachedAsCopy {
		t.Fatalf("second put = %+v, want the first file relinked from %s", again, src)
	}
	if _, err := st.FileByPath(ctx, []byte(dst)); err != nil {
		t.Errorf("new spelling: %v, want the relinked row", err)
	}
	if _, err := st.FileByPath(ctx, []byte(src)); err == nil {
		t.Error("the old spelling still resolves in the catalog")
	}
}
