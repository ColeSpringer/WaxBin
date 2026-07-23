package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// trashOneFile catalogs a track and journals its file into the trash, returning
// the entry.
func trashOneFile(t *testing.T, st *Store, libID int64, path, essence string) model.TrashEntry {
	t.Helper()
	ctx := context.Background()
	putTrack(t, st, libID, trackSpec{path: path, essence: essence, content: essence + "c", title: path, artist: "A"})
	var filePID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file WHERE path=?", []byte(path)).Scan(&filePID); err != nil {
		t.Fatalf("file pid: %v", err)
	}
	tpid, err := st.TrashFile(ctx, model.TrashFileInput{
		FilePID: filePID, TrashPath: []byte(path + ".trash"), TrashDisplay: path + ".trash",
	})
	if err != nil {
		t.Fatalf("TrashFile: %v", err)
	}
	entry, err := st.ActiveTrashByPID(ctx, tpid)
	if err != nil {
		t.Fatalf("ActiveTrashByPID: %v", err)
	}
	return *entry
}

func TestTrashEntriesCutoffIsStrict(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "te1")

	// The cutoff is strictly before: a row trashed exactly AT the cutoff is kept.
	kept, err := st.TrashEntries(ctx, false, entry.TrashedAt, 0)
	if err != nil {
		t.Fatalf("TrashEntries: %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("row at the cutoff instant returned (%d rows); strict < violated", len(kept))
	}
	hit, err := st.TrashEntries(ctx, false, entry.TrashedAt+1, 0)
	if err != nil || len(hit) != 1 || hit[0].PID != entry.PID {
		t.Fatalf("row older than the cutoff missing: %v (err %v)", hit, err)
	}
	// Zero cutoff means no age filter.
	all, err := st.TrashEntries(ctx, false, 0, 0)
	if err != nil || len(all) != 1 {
		t.Fatalf("unfiltered = %v (err %v), want the row", all, err)
	}
}

func TestTrashEntriesCutoffExcludesRestored(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/b.mp3", "te2")
	if err := st.MarkTrashRestored(ctx, entry.PID); err != nil {
		t.Fatalf("mark restored: %v", err)
	}
	// The restored row stays out of an active listing whatever the cutoff, and
	// shows up again with includeRestored.
	got, err := st.TrashEntries(ctx, false, entry.TrashedAt+1, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("restored row leaked into an active cutoff listing: %v (err %v)", got, err)
	}
	all, err := st.TrashEntries(ctx, true, entry.TrashedAt+1, 0)
	if err != nil || len(all) != 1 {
		t.Fatalf("includeRestored with cutoff = %v (err %v), want the row", all, err)
	}
}
