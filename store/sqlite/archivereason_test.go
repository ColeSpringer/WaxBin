package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// archiveReason reads an item's reason as its view does.
func archiveReason(t *testing.T, st *Store, pid model.PID) string {
	t.Helper()
	it, err := st.ItemByPID(context.Background(), pid)
	if err != nil {
		t.Fatalf("item %s: %v", pid, err)
	}
	return string(it.ArchiveReason)
}

func restorable(t *testing.T, st *Store, pid model.PID) int {
	t.Helper()
	m, err := st.ActiveTrashForItems(context.Background(), []model.PID{pid})
	if err != nil {
		t.Fatalf("restorable trash: %v", err)
	}
	return len(m[pid])
}

func trashFile(t *testing.T, st *Store, filePID model.PID) model.PID {
	t.Helper()
	res, err := st.TrashFile(context.Background(), model.TrashFileInput{
		FilePID: filePID, TrashPath: []byte("/t/" + string(filePID)), TrashDisplay: "/t/" + string(filePID),
	})
	if err != nil {
		t.Fatalf("TrashFile: %v", err)
	}
	return res.TrashPID
}

// TestATrashedItemReadsPurgeOnceItsEntryGoes: a trash delete archives as trash with the
// entry restorable, and dropping the entry leaves purge with nothing to restore.
func TestATrashedItemReadsPurgeOnceItsEntryGoes(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "ar1")
	if r := archiveReason(t, st, entry.ItemPID); r != "trash" {
		t.Fatalf("reason after the trash delete = %q, want trash", r)
	}
	if n := restorable(t, st, entry.ItemPID); n != 1 {
		t.Fatalf("restorable entries = %d, want 1", n)
	}
	if err := st.DeleteTrashRow(context.Background(), entry.PID); err != nil {
		t.Fatalf("DeleteTrashRow: %v", err)
	}
	if r := archiveReason(t, st, entry.ItemPID); r != "purge" {
		t.Errorf("reason after the purge = %q, want purge", r)
	}
	if n := restorable(t, st, entry.ItemPID); n != 0 {
		t.Errorf("restorable entries after the purge = %d, want 0", n)
	}
}

// TestABookReadsPurgeOnlyWithItsLastEntry: a book trashed part by part stays trash while
// one part is still restorable.
func TestABookReadsPurgeOnlyWithItsLastEntry(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p1 := putBookPart(t, st, lib.ID, "/lib/book/p1.m4b", "bk", "e1", 0)
	p2 := putBookPart(t, st, lib.ID, "/lib/book/p2.m4b", "bk", "e2", 1)
	t1, t2 := trashFile(t, st, p1.FilePID), trashFile(t, st, p2.FilePID)
	if s, r := itemState(t, st, p1.ItemPID), archiveReason(t, st, p1.ItemPID); s != "archived" || r != "trash" {
		t.Fatalf("book = %s (%q), want archived as trash", s, r)
	}
	if err := st.DeleteTrashRow(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if r := archiveReason(t, st, p1.ItemPID); r != "trash" {
		t.Errorf("reason with one part still restorable = %q, want trash", r)
	}
	if err := st.DeleteTrashRow(ctx, t2); err != nil {
		t.Fatal(err)
	}
	if r := archiveReason(t, st, p1.ItemPID); r != "purge" {
		t.Errorf("reason with no part restorable = %q, want purge", r)
	}
}

// TestPurgeIsReadFromTheTrash: an item reads purge once nothing in the trash restores it,
// whatever took the entry away, and a rule finds it by that reason.
func TestPurgeIsReadFromTheTrash(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "rt1")
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM trash WHERE pid = ?", string(entry.PID)); err != nil {
		t.Fatal(err)
	}
	if r := archiveReason(t, st, entry.ItemPID); r != "purge" {
		t.Errorf("reason with its only entry gone = %q, want purge", r)
	}
	for reason, want := range map[string]int{"purge": 1, "trash": 0} {
		items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("archive_reason", query.OpIs, reason).Build(), "")
		if err != nil || len(items) != want {
			t.Errorf("archive_reason is %s = %d items (err %v), want %d", reason, len(items), err, want)
		}
	}
}

// TestARestoreThatBringsNothingBackLeavesPurge: an entry marked restored whose rescan did
// not bring the item back (the file was skipped, or came back as another item) leaves the
// item with nothing to restore it, which reads purge and is announced.
func TestARestoreThatBringsNothingBackLeavesPurge(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "rs1")
	seq := latestSeq(t, st)
	if err := st.MarkTrashRestored(context.Background(), entry.PID); err != nil {
		t.Fatalf("MarkTrashRestored: %v", err)
	}
	if s, r := itemState(t, st, entry.ItemPID), archiveReason(t, st, entry.ItemPID); s != "archived" || r != "purge" {
		t.Errorf("item = %s (%q), want archived as purge", s, r)
	}
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatal(err)
	}
	if !hasItemUpdate(changes, entry.ItemPID) {
		t.Errorf("no item delta for an item the restore left behind: %+v", changes)
	}
}

// TestADetachArchivesUnderItsMode: a delete that bypasses the trash archives as its mode,
// a purge of an older entry does not overwrite that, and a detach is refused a reason
// that is not a bypass mode's.
func TestADetachArchivesUnderItsMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for mode, want := range map[model.DeleteMode]string{model.DeletePrune: "prune", model.DeletePermanent: "permanent"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ad1", content: "ad1c", title: "A", artist: "A"})
			if _, err := st.DetachFile(ctx, r.FilePID, mode); err != nil {
				t.Fatalf("DetachFile: %v", err)
			}
			if got := archiveReason(t, st, r.ItemPID); got != want {
				t.Errorf("reason = %q, want %q", got, want)
			}
		})
	}

	t.Run("the purge of an older entry", func(t *testing.T) {
		t.Parallel()
		st, lib := entityFixture(t)
		dir := t.TempDir()
		p1 := putBookPart(t, st, lib.ID, realFile(t, filepath.Join(dir, "p1.m4b")), "bk", "e1", 0)
		p2 := putBookPart(t, st, lib.ID, realFile(t, filepath.Join(dir, "p2.m4b")), "bk", "e2", 1)
		t1 := trashFile(t, st, p1.FilePID)
		if _, err := st.DetachFile(ctx, p2.FilePID, model.DeletePrune); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteTrashRow(ctx, t1); err != nil {
			t.Fatal(err)
		}
		if r := archiveReason(t, st, p1.ItemPID); r != "prune" {
			t.Errorf("reason = %q, want prune (the delete that archived it)", r)
		}
	})

	for _, mode := range []model.DeleteMode{"", model.DeleteTrash, "bogus"} {
		t.Run("refused "+string(mode), func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ad2", content: "ad2c", title: "A", artist: "A"})
			if _, err := st.DetachFile(ctx, r.FilePID, mode); !waxerr.Is(err, waxerr.CodeInvalid) {
				t.Fatalf("DetachFile with %q: err = %v, want CodeInvalid", mode, err)
			}
			if s := itemState(t, st, r.ItemPID); s != "present" {
				t.Errorf("item = %s after a refused detach, want present", s)
			}
		})
	}
}

// TestRemoveLibraryTellsARootRemovalFromAPurge: an item the removal archived reads
// root-removed, even beside a trash entry the removal drops or one in another library
// purged later, while an item the trash had already archived, whose only entry the
// removal drops, reads purge.
func TestRemoveLibraryTellsARootRemovalFromAPurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := entityFixture(t)
	g, k := t.TempDir(), t.TempDir()
	gone, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(g), DisplayRoot: g, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(k), DisplayRoot: k, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(dir, name, essence string) trackSpec {
		return trackSpec{path: realFile(t, filepath.Join(dir, name)), essence: essence, content: essence + "c",
			title: name, artist: "Artist", album: "Album"}
	}
	plain := putTrack(t, st, gone.ID, spec(g, "1.mp3", "rr1"))
	trashed := putTrack(t, st, gone.ID, spec(g, "2.mp3", "rr2"))
	trashFile(t, st, trashed.FilePID)
	p1 := putBookPart(t, st, gone.ID, realFile(t, filepath.Join(g, "b1.m4b")), "bk", "rrb1", 0)
	p2 := putBookPart(t, st, gone.ID, realFile(t, filepath.Join(g, "b2.m4b")), "bk", "rrb2", 1)
	trashFile(t, st, p2.FilePID)
	stray := putTrack(t, st, gone.ID, spec(g, "3.mp3", "rr3"))
	cp := spec(k, "3.mp3", "rr3")
	cp.content = "rr3k"
	copied := putTrack(t, st, kept.ID, cp)
	if !copied.AttachedAsCopy || copied.ItemPID != stray.ItemPID {
		t.Fatalf("copy = %+v, want an alternate of %s", copied, stray.ItemPID)
	}
	elsewhere := trashFile(t, st, copied.FilePID)

	if _, _, err := st.removeLibrary(ctx, gone.PID, removeBatch, nil); err != nil {
		t.Fatalf("remove library: %v", err)
	}
	if err := st.DeleteTrashRow(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		pid  model.PID
		want string
	}{
		{"a track", plain.ItemPID, "root-removed"},
		{"a trashed track", trashed.ItemPID, "purge"},
		{"a book with a part trashed there", p1.ItemPID, "root-removed"},
		{"a track whose copy elsewhere was purged", stray.ItemPID, "root-removed"},
	} {
		if s, r := itemState(t, st, c.pid), archiveReason(t, st, c.pid); s != "archived" || r != c.want {
			t.Errorf("%s = %s (%q), want archived as %s", c.name, s, r, c.want)
		}
	}
}

// TestAPutBackClearsTheReason: a file read back onto its archived item makes it present
// with no reason.
func TestAPutBackClearsTheReason(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "pb1")
	back := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "pb1", content: "pb1c", title: "/lib/a.mp3", artist: "A"})
	if back.ItemPID != entry.ItemPID {
		t.Fatalf("the put landed on %s, want the archived %s", back.ItemPID, entry.ItemPID)
	}
	if s, r := itemState(t, st, entry.ItemPID), archiveReason(t, st, entry.ItemPID); s != "present" || r != "" {
		t.Errorf("item = %s (%q), want present with no reason", s, r)
	}
}

// TestADownloadClearsTheReason: an episode written present by a download carries no
// reason, whatever archived it before.
func TestADownloadClearsTheReason(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	putFeed(t, st, "http://feed.example/a", "One")
	var ep model.PID
	if err := st.rdb().QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind = 'episode'").Scan(&ep); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx,
		"UPDATE playable_item SET state = 'archived', archive_reason = 'trash' WHERE pid = ?", string(ep)); err != nil {
		t.Fatal(err)
	}
	lib, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{EpisodePID: ep, LibraryID: lib.ID,
		File: model.File{Path: []byte("/podcasts/one.mp3"), DisplayPath: "/podcasts/one.mp3", RelPath: []byte("one.mp3"),
			Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed}}); err != nil {
		t.Fatalf("AttachEpisodeFile: %v", err)
	}
	if s, r := itemState(t, st, ep), archiveReason(t, st, ep); s != "present" || r != "" {
		t.Errorf("episode = %s (%q), want present with no reason", s, r)
	}
}

// TestTheReasonIsAQueryField: a rule enumerates the items one reason archived.
func TestTheReasonIsAQueryField(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	trashed := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "qf1")
	pruned := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.mp3", essence: "qf2", content: "qf2c", title: "B", artist: "A"})
	if _, err := st.DetachFile(context.Background(), pruned.FilePID, model.DeletePrune); err != nil {
		t.Fatal(err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.mp3", essence: "qf3", content: "qf3c", title: "C", artist: "A"})
	for reason, want := range map[string]model.PID{"trash": trashed.ItemPID, "prune": pruned.ItemPID} {
		items, err := st.QueryItems(context.Background(),
			query.New(query.EntityItems).Where("archive_reason", query.OpIs, reason).Build(), "")
		if err != nil {
			t.Fatalf("query %s: %v", reason, err)
		}
		if len(items) != 1 || items[0].PID != want || items[0].ArchiveReason != model.ArchiveReason(reason) {
			t.Errorf("archive_reason is %s = %+v, want %s alone", reason, items, want)
		}
	}
}

// TestAPlanGoneBeforeItsFinishFinishesNothing: recovery finishing a move whose pending row
// is no longer there writes no journal row and leaves the file row where it is.
func TestAPlanGoneBeforeItsFinishFinishesNothing(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "pg1", content: "pg1c", title: "A", artist: "X"})
	var fileID int64
	if err := st.rdb().QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(r.FilePID)).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	p := pendingTrash{pid: string(model.NewPID()), fileID: sql.NullInt64{Int64: fileID, Valid: true}}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error { return finishPendingTrashTx(ctx, tx, p) }); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if s := itemState(t, st, r.ItemPID); s != "present" {
		t.Errorf("item = %s, want present (nothing finished)", s)
	}
	var trashed int
	if err := st.rdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM trash").Scan(&trashed); err != nil || trashed != 0 {
		t.Errorf("trash rows = %d (err %v), want none", trashed, err)
	}
}

// TestTheSchemaPairsTheReasonWithTheArchivedState: a write that archives without a reason,
// or leaves archived keeping one, fails in its own statement.
func TestTheSchemaPairsTheReasonWithTheArchivedState(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	live := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ck1", content: "ck1c", title: "A", artist: "A"})
	gone := trashOneFile(t, st, lib.ID, "/lib/b.mp3", "ck2")
	for _, c := range []struct {
		q   string
		pid model.PID
	}{
		{"UPDATE playable_item SET state = 'archived' WHERE pid = ?", live.ItemPID},
		{"UPDATE playable_item SET archive_reason = 'prune' WHERE pid = ?", live.ItemPID},
		{"UPDATE playable_item SET state = 'present' WHERE pid = ?", gone.ItemPID},
		{"UPDATE playable_item SET archive_reason = '' WHERE pid = ?", gone.ItemPID},
	} {
		_, err := st.wdb().ExecContext(ctx, c.q, string(c.pid))
		if err == nil || !strings.Contains(err.Error(), "item_archive_reason") {
			t.Errorf("%s: err = %v, want the item_archive_reason constraint", c.q, err)
		}
	}
}
