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
	// Close is idempotent, so tests that close explicitly to reopen are unaffected;
	// without this the second store leaks its handle and Windows fails the TempDir
	// removal.
	t.Cleanup(func() { _ = st.Close() })
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
	t.Parallel()
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
	t.Parallel()
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

// TestRecoverOrganizeRollsBackAMoveListedUnderBothNames: a planned move whose source and
// destination are one file under two listed names (hard links, which fsx.Move refuses) did
// not take effect, so recovery rolls it back and the catalog keeps the source.
func TestRecoverOrganizeRollsBackAMoveListedUnderBothNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "c.db")

	st, lib := openRootedStore(t, dir, db, "owner-a")
	src := filepath.Join(dir, "song.mp3")
	dst := filepath.Join(dir, "Song.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Skipf("hard link: %v", err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, src, "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := st.PlanMove(ctx, model.RelocateInput{
		FilePID: r.FilePID, JobPID: "job", SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte("Song.mp3"),
	}); err != nil {
		t.Fatalf("plan move: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, _ := openRootedStore(t, dir, db, "owner-b")
	if _, err := st2.FileByPath(ctx, []byte(src)); err != nil {
		t.Fatalf("catalog should keep the source: %v", err)
	}
	if _, err := st2.FileByPath(ctx, []byte(dst)); err == nil {
		t.Fatal("the destination should not resolve after the rollback")
	}
}

// TestRecoverOrganizeLeavesADestinationAnotherRowHolds: a landed move whose destination
// path another file row still holds cannot be committed, so recovery rolls it back with a
// warning rather than failing the open on every start; the catalog keeps the source and
// the next scan reconciles.
func TestRecoverOrganizeLeavesADestinationAnotherRowHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "c.db")

	st, lib := openRootedStore(t, dir, db, "owner-a")
	src := filepath.Join(dir, "a.mp3")
	dst := filepath.Join(dir, "b.mp3")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, src, "sha256:E1", "sha256:C1", "A"))
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	stale, err := st.PutScannedTrack(ctx, input(lib.ID, dst, "sha256:E2", "sha256:C2", "B"))
	if err != nil {
		t.Fatalf("seed the row holding the destination: %v", err)
	}
	if _, err := st.PlanMove(ctx, model.RelocateInput{
		FilePID: r.FilePID, JobPID: "job", SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte("b.mp3"),
	}); err != nil {
		t.Fatalf("plan move: %v", err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, _ := openRootedStore(t, dir, db, "owner-b")
	f, err := st2.FileByPath(ctx, []byte(src))
	if err != nil || f.PID != r.FilePID {
		t.Errorf("source row = %+v (err %v), want the moved file's row kept there", f, err)
	}
	if f, err := st2.FileByPath(ctx, []byte(dst)); err != nil || f.PID != stale.FilePID {
		t.Errorf("destination row = %+v (err %v), want the row that held it untouched", f, err)
	}
}
