package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// touchFile creates an empty file at path, so the store's checks of what is on disk find it.
func touchFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// putAssigned puts s as a part a host assigned to its book, at position when it is not nil.
func putAssigned(t *testing.T, st *Store, libID int64, s bookSpec, position *int) *model.ScanItemResult {
	t.Helper()
	in := bookSpecInput(libID, s)
	in.Assigned = &model.AssignedPlace{Position: position}
	res, err := st.PutScannedBook(context.Background(), in)
	if err != nil {
		t.Fatalf("put assigned %s: %v", s.path, err)
	}
	return res
}

func at(n int) *int { return &n }

// assignedRow reads the assignment of an audio straight from its table.
func assignedRow(t *testing.T, st *Store, essence string) (itemPID model.PID, position sql.NullInt64, ok bool) {
	t.Helper()
	err := st.rdb().QueryRowContext(context.Background(), `SELECT pi.pid, ap.position FROM assigned_part ap
		JOIN playable_item pi ON pi.id = ap.item_id WHERE ap.essence_hash = ?`, essence).Scan(&itemPID, &position)
	if err == sql.ErrNoRows {
		return "", position, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return itemPID, position, true
}

// TestAnAssignedPutRecordsItsPlace: an assigned part is stored at the place assigned it
// over the one its tags give, and one assigned no number takes its own; each audio's row
// names the book, and AssignedPart finds it.
func TestAnAssignedPutRecordsItsPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	one := putAssigned(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "a.mp3")), essence: "e1", content: "c1",
		title: "Dune", author: "Frank Herbert", position: 7}, at(1))
	two := putAssigned(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "b.mp3")), essence: "e2", content: "c2",
		title: "Dune", author: "Frank Herbert", position: 3}, nil)
	if two.ItemPID != one.ItemPID {
		t.Fatalf("second part landed on %s, want the book %s", two.ItemPID, one.ItemPID)
	}
	files, err := st.ItemFiles(ctx, one.ItemPID)
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %+v (err %v), want the two parts", files, err)
	}
	if files[0].Position != 1 || files[1].Position != 3 || !files[0].Assigned || !files[1].Assigned {
		t.Errorf("parts = %+v, want the assigned place 1 and the part's own 3, both assigned", files)
	}
	if pid, pos, ok := assignedRow(t, st, "e1"); !ok || pid != one.ItemPID || !pos.Valid || pos.Int64 != 1 {
		t.Errorf("row for e1 = %s %+v %v, want the book at 1", pid, pos, ok)
	}
	if _, pos, ok := assignedRow(t, st, "e2"); !ok || pos.Valid {
		t.Errorf("row for e2 = %+v %v, want the book with no number", pos, ok)
	}
	got, err := st.AssignedPart(ctx, []byte(filepath.Join(dir, "a.mp3")), "e1")
	if err != nil || got == nil || got.Book.ItemPID != one.ItemPID || got.Book.Title != "Dune" || got.Position == nil || *got.Position != 1 {
		t.Fatalf("AssignedPart = %+v (err %v), want Dune at 1", got, err)
	}
	if got, err := st.AssignedPart(ctx, []byte("/nowhere.mp3"), "e9"); err != nil || got != nil {
		t.Errorf("AssignedPart of an unassigned audio = %+v (err %v), want nil", got, err)
	}
}

// TestAnAssignedPrimaryKeepsTheBooksIdentity: the primary part, read again with tags that
// name another book, leaves the book's title, author and identifiers as the catalog holds
// them, an unlocked catalog edit included, and gives it the rest.
func TestAnAssignedPrimaryKeepsTheBooksIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	path := touchFile(t, filepath.Join(t.TempDir(), "a.mp3"))
	first := putAssigned(t, st, lib.ID, bookSpec{path: path, essence: "e1", content: "c1", title: "Dune", author: "Frank Herbert"}, at(1))
	if err := st.EditItemFields(ctx, first.ItemPID, map[string]string{"title": "Dune Messiah"}, model.Attribution{}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	in := bookSpecInput(lib.ID, bookSpec{path: path, essence: "e1", content: "c1b", title: "Wrong Book", author: "Someone Else",
		asin: "B000WRONG", isbn: "9780000000002", edition: "Abridged", mbid: "00000000-0000-0000-0000-000000000001",
		narrators: []string{"Scott Brick"}})
	in.Item.IdentityKey = mustItemKey(t, st, first.ItemPID)
	in.Assigned = &model.AssignedPlace{Position: at(1)}
	again, err := st.PutScannedBook(ctx, in)
	if err != nil || again.ItemPID != first.ItemPID {
		t.Fatalf("re-put = %+v (err %v), want the same book", again, err)
	}
	d, err := st.BookByPID(ctx, first.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Item.Title != "Dune Messiah" || d.Item.Artist != "Frank Herbert" || d.ASIN != "" || d.ISBN != "" || d.Edition != "" {
		t.Errorf("book = %q by %q (asin %q isbn %q edition %q), want the catalog's title, author and no identifiers",
			d.Item.Title, d.Item.Artist, d.ASIN, d.ISBN, d.Edition)
	}
	if len(d.Narrators) != 1 || d.Narrators[0] != "Scott Brick" || !d.IdentityHeld {
		t.Errorf("narrators = %v, identity held %v; want the primary's narrator and the identity held", d.Narrators, d.IdentityHeld)
	}
}

// mustItemKey reads an item's identity key.
func mustItemKey(t *testing.T, st *Store, pid model.PID) string {
	t.Helper()
	var key string
	if err := st.rdb().QueryRow("SELECT identity_key FROM playable_item WHERE pid = ?", string(pid)).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestAReencodedAssignedPartCarriesItsRow: a part whose audio changes at its path carries
// its assignment to the new audio, leaving the old one's row only while another file holds
// that audio.
func TestAReencodedAssignedPartCarriesItsRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	path := touchFile(t, filepath.Join(dir, "a.mp3"))
	first := putAssigned(t, st, lib.ID, bookSpec{path: path, essence: "e1", content: "c1", title: "Dune", author: "Frank Herbert"}, at(2))
	got, err := st.AssignedPart(ctx, []byte(path), "e9")
	if err != nil || got == nil || got.Book.ItemPID != first.ItemPID {
		t.Fatalf("AssignedPart of new audio at the path = %+v (err %v), want the row the path's audio held", got, err)
	}
	putAssigned(t, st, lib.ID, bookSpec{path: path, essence: "e9", content: "c9", title: "Dune", author: "Frank Herbert"}, got.Position)
	if _, _, ok := assignedRow(t, st, "e1"); ok {
		t.Error("the old audio's row stayed with no file holding it")
	}
	if pid, pos, ok := assignedRow(t, st, "e9"); !ok || pid != first.ItemPID || pos.Int64 != 2 {
		t.Errorf("new audio's row = %s %+v %v, want the book at 2", pid, pos, ok)
	}

	// A copy keeps the audio held, so its row stays when the original is re-encoded.
	copyPath := touchFile(t, filepath.Join(dir, "copy", "a.mp3"))
	putAssigned(t, st, lib.ID, bookSpec{path: copyPath, essence: "e9", content: "c9", title: "Dune", author: "Frank Herbert"}, at(2))
	putAssigned(t, st, lib.ID, bookSpec{path: path, essence: "e10", content: "c10", title: "Dune", author: "Frank Herbert"}, at(2))
	if _, _, ok := assignedRow(t, st, "e9"); !ok {
		t.Error("the copy's audio lost its row")
	}
}

// TestAForcedTrackReadDropsOnlyAPartsAssignment: a file read as a track by force withdraws
// its audio's assignment when it is the book's part, never when it is a copy of the part.
func TestAForcedTrackReadDropsOnlyAPartsAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	part := touchFile(t, filepath.Join(dir, "a.mp3"))
	book := putAssigned(t, st, lib.ID, bookSpec{path: part, essence: "e1", content: "c1", title: "Dune", author: "Frank Herbert"}, at(1))
	putAssigned(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "b.mp3")), essence: "e2", content: "c2",
		title: "Dune", author: "Frank Herbert"}, at(2))
	cp := touchFile(t, filepath.Join(dir, "copy", "a.mp3"))
	putAssigned(t, st, lib.ID, bookSpec{path: cp, essence: "e1", content: "c1", title: "Dune", author: "Frank Herbert"}, at(1))

	asTrack := func(path string) {
		t.Helper()
		in := trackSpecInput(lib.ID, trackSpec{path: path, essence: "e1", content: "c1", title: "Chapter One", artist: "Frank Herbert"})
		in.DropAssigned, in.KindForced = book.ItemPID, true
		if _, err := st.PutScannedTrack(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	asTrack(cp)
	if _, _, ok := assignedRow(t, st, "e1"); !ok {
		t.Fatal("a copy's track read dropped the part's assignment")
	}
	asTrack(part)
	if _, _, ok := assignedRow(t, st, "e1"); ok {
		t.Error("the part's track read left its assignment")
	}
	if _, _, ok := assignedRow(t, st, "e2"); !ok {
		t.Error("another part's assignment went with it")
	}
}

// TestAnAssignmentFollowsItsBookIntoAnother: a book whose assigned part joins another book
// folds into it, and the assignment goes with it; a book read as a track in place drops
// its assignments.
func TestAnAssignmentFollowsItsBookIntoAnother(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	a := touchFile(t, filepath.Join(dir, "a.mp3"))
	lone := putAssigned(t, st, lib.ID, bookSpec{path: a, essence: "e1", content: "c1", title: "Lone", author: "Someone"}, at(1))
	other := putBook(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "b.mp3")), essence: "e2", content: "c2",
		title: "Dune", author: "Frank Herbert", position: 2})
	moved := putBook(t, st, lib.ID, bookSpec{path: a, essence: "e1", content: "c1", title: "Dune", author: "Frank Herbert", position: 1})
	if moved.ItemPID != other.ItemPID || moved.Absorbed[lone.ItemPID] != other.ItemPID {
		t.Fatalf("put = %+v, want the part in Dune with Lone folded into it", moved)
	}
	if pid, _, ok := assignedRow(t, st, "e1"); !ok || pid != other.ItemPID {
		t.Errorf("row after the fold = %s %v, want it on Dune", pid, ok)
	}

	single := touchFile(t, filepath.Join(dir, "c.mp3"))
	putAssigned(t, st, lib.ID, bookSpec{path: single, essence: "e3", content: "c3", title: "Solo", author: "Anyone"}, nil)
	if _, err := st.PutScannedTrack(ctx, trackSpecInput(lib.ID, trackSpec{path: single, essence: "e3", content: "c3",
		title: "Solo", artist: "Anyone"})); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := assignedRow(t, st, "e3"); ok {
		t.Error("a book made a track in place kept its assignment")
	}
}

// TestVerifyCountsAndReclaimsStrandedAssignments: an assignment whose item is no book is
// reclaimable, and the repair deletes it alone.
func TestVerifyCountsAndReclaimsStrandedAssignments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	putAssigned(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "a.mp3")), essence: "e1", content: "c1",
		title: "Dune", author: "Frank Herbert"}, at(1))
	track := putTrack(t, st, lib.ID, trackSpec{path: touchFile(t, filepath.Join(dir, "t.mp3")), essence: "t1", content: "tc1", title: "Song", artist: "Band"})
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO assigned_part(essence_hash, item_id, position)
			SELECT 't1', id, NULL FROM playable_item WHERE pid = ?`, string(track.ItemPID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.StrandedAssignments != 1 || !rep.Reclaimable() {
		t.Fatalf("verify = %+v (err %v), want one stranded assignment, reclaimable", rep, err)
	}
	if n, err := st.GCStrandedAssignments(ctx); err != nil || n != 1 {
		t.Fatalf("reclaimed %d (err %v), want 1", n, err)
	}
	if _, _, ok := assignedRow(t, st, "e1"); !ok {
		t.Error("the repair took a book's assignment")
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.StrandedAssignments != 0 {
		t.Errorf("verify after the repair = %+v (err %v)", rep, err)
	}
}

// TestAnAssignmentGoesWithItsItem: a book whose assigned part becomes a copy of a track folds
// into the track and takes its assignment with it, and a book deleted takes its own.
func TestAnAssignmentGoesWithItsItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	dir := t.TempDir()
	putTrack(t, st, lib.ID, trackSpec{path: touchFile(t, filepath.Join(dir, "t.mp3")), essence: "e1", content: "c1", title: "Song", artist: "Band"})
	a := touchFile(t, filepath.Join(dir, "a.mp3"))
	putAssigned(t, st, lib.ID, bookSpec{path: a, essence: "e1", content: "c1b", title: "Lone", author: "Someone"}, at(1))
	if _, _, ok := assignedRow(t, st, "e1"); !ok {
		t.Fatal("no assignment to start from")
	}
	putTrack(t, st, lib.ID, trackSpec{path: a, essence: "e1", content: "c1b", title: "Song", artist: "Band"})
	if n := assignmentRows(t, st, "e1"); n != 0 {
		t.Errorf("a book folded into a track left %d assignment rows", n)
	}

	book := putAssigned(t, st, lib.ID, bookSpec{path: touchFile(t, filepath.Join(dir, "b.mp3")), essence: "e2", content: "c2",
		title: "Gone", author: "Someone"}, at(1))
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = ?", string(book.ItemPID)).Scan(&id); err != nil {
			return err
		}
		_, err := deleteItemCascade(ctx, tx, id, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := assignmentRows(t, st, "e2"); n != 0 {
		t.Errorf("a deleted book left %d assignment rows", n)
	}
}

// assignmentRows counts an audio's assignment rows, whatever item they name.
func assignmentRows(t *testing.T, st *Store, essence string) int {
	t.Helper()
	var n int
	if err := st.rdb().QueryRow("SELECT COUNT(*) FROM assigned_part WHERE essence_hash = ?", essence).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
