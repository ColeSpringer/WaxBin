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

// hasItemUpdate reports whether the change set carries an item update for pid. The
// same-named helper beside the archive tests lives in the external test package and
// is not reachable from here.
func hasItemUpdate(changes []model.Change, pid model.PID) bool {
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == pid && c.Op == model.OpUpdate {
			return true
		}
	}
	return false
}

func trashRowCount(t *testing.T, st *Store, pid model.PID) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM trash WHERE pid = ?", string(pid)).Scan(&n); err != nil {
		t.Fatalf("trash row count: %v", err)
	}
	return n
}

// TestTrashRecordsBookPartItem pins that trashing a non-primary book part journals
// the book it belonged to. Only the first part carries the primary edge, so the
// journal used to land with a blank item for every other part, which both blanked
// the ITEM column in `trash list` and left a later purge with nothing to announce.
func TestTrashRecordsBookPartItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	book := putBookPart(t, st, lib.ID, "/lib/book/p1.m4b", "bk1", "e1", 0)
	part2 := putBookPart(t, st, lib.ID, "/lib/book/p2.m4b", "bk1", "e2", 1)

	tpid, err := st.TrashFile(ctx, model.TrashFileInput{
		FilePID: part2.FilePID, TrashPath: []byte("/lib/t/p2.m4b"), TrashDisplay: "/lib/t/p2.m4b",
	})
	if err != nil {
		t.Fatalf("TrashFile: %v", err)
	}
	entry, err := st.ActiveTrashByPID(ctx, tpid)
	if err != nil {
		t.Fatalf("ActiveTrashByPID: %v", err)
	}
	if entry.ItemPID != book.ItemPID {
		t.Errorf("trashed part recorded item %q, want the book %q", entry.ItemPID, book.ItemPID)
	}
}

// TestPurgeEmitsItemDelta pins that dropping a trash row announces itself, including
// for an item whose own columns do not move, and stays quiet where there is nothing
// to announce. A dropped row is dropped in every case: the quiet paths are guards on
// the delta, not on the delete.
func TestPurgeEmitsItemDelta(t *testing.T) {
	t.Run("archived item", func(t *testing.T) {
		st, lib := entityFixture(t)
		ctx := context.Background()
		entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "pd1")

		before := latestSeq(t, st)
		if err := st.DeleteTrashRow(ctx, entry.PID); err != nil {
			t.Fatalf("DeleteTrashRow: %v", err)
		}
		changes, err := st.ChangesSince(ctx, before)
		if err != nil {
			t.Fatalf("changes: %v", err)
		}
		if !hasItemUpdate(changes, entry.ItemPID) {
			t.Errorf("no item delta for the purge: %+v", changes)
		}
		if n := trashRowCount(t, st, entry.PID); n != 0 {
			t.Errorf("trash rows left = %d, want 0", n)
		}
	})

	// The book keeps a present part, so its state does not change; the delta is still
	// owed, because that one part's bytes are now unrecoverable.
	t.Run("still-present multi-file book", func(t *testing.T) {
		st, lib := entityFixture(t)
		ctx := context.Background()
		book := putBookPart(t, st, lib.ID, "/lib/book/p1.m4b", "bk1", "e1", 0)
		part2 := putBookPart(t, st, lib.ID, "/lib/book/p2.m4b", "bk1", "e2", 1)
		tpid, err := st.TrashFile(ctx, model.TrashFileInput{
			FilePID: part2.FilePID, TrashPath: []byte("/lib/t/p2.m4b"), TrashDisplay: "/lib/t/p2.m4b",
		})
		if err != nil {
			t.Fatalf("TrashFile: %v", err)
		}
		if s := itemState(t, st, book.ItemPID); s != string(model.StatePresent) {
			t.Fatalf("book state = %q, want present (a surviving part keeps it present)", s)
		}

		before := latestSeq(t, st)
		if err := st.DeleteTrashRow(ctx, tpid); err != nil {
			t.Fatalf("DeleteTrashRow: %v", err)
		}
		changes, err := st.ChangesSince(ctx, before)
		if err != nil {
			t.Fatalf("changes: %v", err)
		}
		if !hasItemUpdate(changes, book.ItemPID) {
			t.Errorf("no item delta for the purged part: %+v", changes)
		}
		if s := itemState(t, st, book.ItemPID); s != string(model.StatePresent) {
			t.Errorf("book state = %q, want present (the delta carries no state change)", s)
		}
	})

	t.Run("tombstoned item", func(t *testing.T) {
		st, lib := entityFixture(t)
		ctx := context.Background()
		entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "pd2")
		if _, err := st.write.ExecContext(ctx,
			"DELETE FROM playable_item WHERE pid = ?", string(entry.ItemPID)); err != nil {
			t.Fatalf("tombstone the item: %v", err)
		}

		before := latestSeq(t, st)
		if err := st.DeleteTrashRow(ctx, entry.PID); err != nil {
			t.Fatalf("DeleteTrashRow: %v", err)
		}
		if latestSeq(t, st) != before {
			t.Error("a pid with no playable_item row must not earn a delta")
		}
		if n := trashRowCount(t, st, entry.PID); n != 0 {
			t.Errorf("trash rows left = %d, want 0 (the guard is on the delta, not the delete)", n)
		}
	})

	// A row written before the journal recorded book parts correctly, or one for an
	// edge-less file: nothing to name, so nothing is emitted.
	t.Run("empty item pid", func(t *testing.T) {
		st, _ := entityFixture(t)
		ctx := context.Background()
		tpid := model.NewPID()
		if _, err := st.write.ExecContext(ctx,
			`INSERT INTO trash (pid, item_pid, orig_path, orig_display, trash_path, trash_display,
				reason, size, trashed_at) VALUES (?,'',?,?,?,?,'user',1,1)`,
			string(tpid), []byte("/lib/x.mp3"), "/lib/x.mp3", []byte("/lib/t/x.mp3"), "/lib/t/x.mp3"); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}

		before := latestSeq(t, st)
		if err := st.DeleteTrashRow(ctx, tpid); err != nil {
			t.Fatalf("DeleteTrashRow: %v", err)
		}
		if latestSeq(t, st) != before {
			t.Error("an itemless row must not earn a delta")
		}
		if n := trashRowCount(t, st, tpid); n != 0 {
			t.Errorf("trash rows left = %d, want 0", n)
		}
	})

	t.Run("missing row", func(t *testing.T) {
		st, _ := entityFixture(t)
		ctx := context.Background()
		before := latestSeq(t, st)
		if err := st.DeleteTrashRow(ctx, "nope"); err != nil {
			t.Fatalf("an unknown pid must stay a silent no-op: %v", err)
		}
		if latestSeq(t, st) != before {
			t.Error("an unknown pid must not emit a delta")
		}
	})
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
