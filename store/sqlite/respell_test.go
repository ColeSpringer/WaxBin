package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRespellFolderMovesItsFilesPaths: a folder renamed to another spelling takes the
// catalog paths of every file below it along, so a scan finds each where the catalog
// says, and leaves a file outside it alone, a sibling folder whose name it begins included.
func TestRespellFolderMovesItsFilesPaths(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	in := input(lib.ID, "/lib/j.r.r. tolkien/Book/x.mp3", "sha256:E1", "sha256:C1", "One")
	in.File.RelPath = []byte("j.r.r. tolkien/Book/x.mp3")
	moved, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/j.r.r. tolkiens/y.mp3", "sha256:E2", "sha256:C2", "Two")); err != nil {
		t.Fatal(err)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	n, err := st.RespellFolder(ctx, "/lib/j.r.r. tolkien", "/lib/J.R.R. Tolkien")
	if err != nil || n != 1 {
		t.Fatalf("RespellFolder = %d, %v; want the one file below it", n, err)
	}
	f, err := st.FileByPath(ctx, []byte("/lib/J.R.R. Tolkien/Book/x.mp3"))
	if err != nil || f.PID != moved.FilePID || f.DisplayPath != "/lib/J.R.R. Tolkien/Book/x.mp3" || string(f.RelPath) != "J.R.R. Tolkien/Book/x.mp3" {
		t.Fatalf("file after the respell = %+v (err %v), want the new spelling", f, err)
	}
	if _, err := st.FileByPath(ctx, []byte("/lib/j.r.r. tolkiens/y.mp3")); err != nil {
		t.Errorf("sibling folder's file: %v, want it untouched", err)
	}
	if _, err := st.FileByPath(ctx, []byte("/lib/j.r.r. tolkien/Book/x.mp3")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("old spelling still resolves: %v", err)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil || len(changes) != 1 || changes[0].EntityType != "file" || changes[0].Op != model.OpUpdate {
		t.Errorf("changes = %+v (err %v), want one file update", changes, err)
	}
}
