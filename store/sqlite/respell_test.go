package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRespellFolderMovesItsFilesPaths: a folder renamed to another spelling takes the
// catalog paths of every file below it along, so a scan finds each where the catalog
// says, and leaves a file outside it alone, a sibling folder whose name it begins included.
// The paths are native, as a scan's are, since the folder prefix ends in the OS separator.
func TestRespellFolderMovesItsFilesPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	nat := filepath.FromSlash
	in := input(lib.ID, nat("/lib/j.r.r. tolkien/Book/x.mp3"), "sha256:E1", "sha256:C1", "One")
	in.File.RelPath = []byte(nat("j.r.r. tolkien/Book/x.mp3"))
	moved, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutScannedTrack(ctx, input(lib.ID, nat("/lib/j.r.r. tolkiens/y.mp3"), "sha256:E2", "sha256:C2", "Two")); err != nil {
		t.Fatal(err)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	n, err := st.RespellFolder(ctx, nat("/lib/j.r.r. tolkien"), nat("/lib/J.R.R. Tolkien"))
	if err != nil || n != 1 {
		t.Fatalf("RespellFolder = %d, %v; want the one file below it", n, err)
	}
	f, err := st.FileByPath(ctx, []byte(nat("/lib/J.R.R. Tolkien/Book/x.mp3")))
	if err != nil || f.PID != moved.FilePID || f.DisplayPath != nat("/lib/J.R.R. Tolkien/Book/x.mp3") || string(f.RelPath) != nat("J.R.R. Tolkien/Book/x.mp3") {
		t.Fatalf("file after the respell = %+v (err %v), want the new spelling", f, err)
	}
	if _, err := st.FileByPath(ctx, []byte(nat("/lib/j.r.r. tolkiens/y.mp3"))); err != nil {
		t.Errorf("sibling folder's file: %v, want it untouched", err)
	}
	if _, err := st.FileByPath(ctx, []byte(nat("/lib/j.r.r. tolkien/Book/x.mp3"))); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("old spelling still resolves: %v", err)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil || len(changes) != 1 || changes[0].EntityType != "file" || changes[0].Op != model.OpUpdate {
		t.Errorf("changes = %+v (err %v), want one file update", changes, err)
	}
}

// TestCommitMoveAfterARespellLogsNoChange: the move that had its folder respelled lands
// on the path the respell already gave the catalog, so its commit moves no path and logs
// nothing, while still closing its journal row; the file's one move is the respell's one
// change.
func TestCommitMoveAfterARespellLogsNoChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	nat := filepath.FromSlash
	in := input(lib.ID, nat("/lib/author/Book/x.mp3"), "sha256:E1", "sha256:C1", "One")
	in.File.RelPath = []byte(nat("author/Book/x.mp3"))
	put, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	move := model.RelocateInput{FilePID: put.FilePID, JobPID: "job", SrcPath: []byte(nat("/lib/author/Book/x.mp3")),
		NewPath: []byte(nat("/lib/Author/Book/x.mp3")), NewDisplayPath: nat("/lib/Author/Book/x.mp3"), NewRelPath: []byte(nat("Author/Book/x.mp3"))}
	jpid, err := st.PlanMove(ctx, move)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RespellFolder(ctx, nat("/lib/author"), nat("/lib/Author")); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMove(ctx, jpid, move); err != nil {
		t.Fatal(err)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil || len(changes) != 1 || changes[0].EntityType != "file" {
		t.Errorf("changes = %+v (err %v), want the respell's one file update", changes, err)
	}
	if f, err := st.FileByPath(ctx, move.NewPath); err != nil || f.DisplayPath != move.NewDisplayPath || string(f.RelPath) != string(move.NewRelPath) {
		t.Errorf("file after the commit = %+v (err %v), want it at the new spelling", f, err)
	}
	var state string
	if err := roConn(t, dbPath).QueryRowContext(ctx, "SELECT state FROM organize_journal WHERE pid = ?", string(jpid)).Scan(&state); err != nil || state != "committed" {
		t.Errorf("journal row = %q (err %v), want committed", state, err)
	}
}
