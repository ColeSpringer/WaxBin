package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEditItemsFieldsPerItemMaps verifies each entry's own map applies: two album
// members get distinct titles and track numbers in one batch, and the touched
// rollups come out consistent.
func TestEditItemsFieldsPerItemMaps(t *testing.T) {
	st, _, p1, p2 := twoTrackFixture(t)
	ctx := context.Background()

	res, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"title": "Opener", "track_no": "1"}},
		{ItemPID: p2, Fields: map[string]string{"title": "Closer", "track_no": "9"}},
	}, model.SourceUser, true, false, false)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Edited) != 2 {
		t.Fatalf("edited = %v, want both items", res.Edited)
	}
	for pid, want := range map[model.PID]struct {
		title string
		no    int
	}{p1: {"Opener", 1}, p2: {"Closer", 9}} {
		var title string
		var no int
		if err := st.read.QueryRowContext(ctx,
			`SELECT pi.title, t.track_no FROM playable_item pi JOIN track t ON t.item_id=pi.id WHERE pi.pid=?`,
			string(pid)).Scan(&title, &no); err != nil {
			t.Fatalf("read %s: %v", pid, err)
		}
		if title != want.title || no != want.no {
			t.Errorf("%s = %q #%d, want %q #%d", pid, title, no, want.title, want.no)
		}
	}
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean after batch: %+v (err %v)", r, err)
	}
}

// TestEditItemsFieldsAtomic verifies any hard failure rolls the whole batch back:
// a bad field on the second entry undoes the first entry's edit, and a missing
// pid does the same.
func TestEditItemsFieldsAtomic(t *testing.T) {
	st, _, p1, p2 := twoTrackFixture(t)
	ctx := context.Background()

	_, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"title": "Changed"}},
		{ItemPID: p2, Fields: map[string]string{"publisher": "X"}}, // a book field on a track
	}, model.SourceUser, true, false, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad field = %v, want CodeInvalid", err)
	}
	var title string
	if err := st.read.QueryRowContext(ctx,
		"SELECT title FROM playable_item WHERE pid=?", string(p1)).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title == "Changed" {
		t.Error("first entry's edit survived a failed batch; the batch must be atomic")
	}

	_, err = st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"title": "Changed"}},
		{ItemPID: "no-such-item", Fields: map[string]string{"title": "X"}},
	}, model.SourceUser, true, false, false)
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("missing pid = %v, want CodeNotFound", err)
	}
	if err := st.read.QueryRowContext(ctx,
		"SELECT title FROM playable_item WHERE pid=?", string(p1)).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title == "Changed" {
		t.Error("edit survived a batch aborted by a missing pid")
	}
}

// TestEditItemsFieldsSkipLocked mirrors the shared-map batch: without force a
// locked target aborts, with skipLocked it is skipped and reported while the
// rest applies.
func TestEditItemsFieldsSkipLocked(t *testing.T) {
	st, _, p1, p2 := twoTrackFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, p1, "genre", "Locked", model.SourceUser, true, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	batch := []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"genre": "Jazz"}},
		{ItemPID: p2, Fields: map[string]string{"genre": "Blues"}},
	}
	if _, err := st.EditItemsFields(ctx, batch, model.SourceUser, true, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("locked batch = %v, want CodeLocked", err)
	}

	res, err := st.EditItemsFields(ctx, batch, model.SourceUser, true, false, true)
	if err != nil {
		t.Fatalf("skip-locked batch: %v", err)
	}
	if len(res.Edited) != 1 || res.Edited[0] != p2 || len(res.Skipped) != 1 || res.Skipped[0] != p1 {
		t.Fatalf("edited=%v skipped=%v, want p2 edited, p1 skipped", res.Edited, res.Skipped)
	}
	var g1, g2 string
	st.read.QueryRowContext(ctx, "SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(p1)).Scan(&g1)
	st.read.QueryRowContext(ctx, "SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(p2)).Scan(&g2)
	if g1 != "Locked" || g2 != "Blues" {
		t.Errorf("genres = %q/%q, want Locked/Blues", g1, g2)
	}
}

// TestEditItemsFieldsDuplicatePID verifies two maps for one item reject the
// batch: conflicting entries are a caller bug, not something to merge.
func TestEditItemsFieldsDuplicatePID(t *testing.T) {
	st, _, p1, _ := twoTrackFixture(t)
	_, err := st.EditItemsFields(context.Background(), []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"title": "A"}},
		{ItemPID: p1, Fields: map[string]string{"title": "B"}},
	}, model.SourceUser, true, false, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("duplicate pid = %v, want CodeInvalid", err)
	}
}

// TestEditItemsFieldsMixedKinds verifies one batch can carry kind-appropriate
// maps for a track and a book, with the union of touched entities' rollups
// recomputed once and consistent.
func TestEditItemsFieldsMixedKinds(t *testing.T) {
	st, lib, p1, _ := twoTrackFixture(t)
	ctx := context.Background()
	bres := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/b.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", durationMS: 100,
	})

	res, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"artist": "Renamed Artist"}},
		{ItemPID: bres.ItemPID, Fields: map[string]string{"author": "Renamed Author", "isbn": "978-0-13-468599-1"}},
	}, model.SourceUser, true, false, false)
	if err != nil {
		t.Fatalf("mixed batch: %v", err)
	}
	if len(res.Edited) != 2 {
		t.Fatalf("edited = %v, want both", res.Edited)
	}
	var artist string
	if err := st.read.QueryRowContext(ctx,
		"SELECT t.artist FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(p1)).Scan(&artist); err != nil {
		t.Fatal(err)
	}
	var author, isbn string
	if err := st.read.QueryRowContext(ctx,
		"SELECT author, isbn FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?", string(bres.ItemPID)).Scan(&author, &isbn); err != nil {
		t.Fatal(err)
	}
	if artist != "Renamed Artist" || author != "Renamed Author" || isbn != "9780134685991" {
		t.Errorf("artist=%q author=%q isbn=%q, want the renames and the normalized isbn", artist, author, isbn)
	}
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean after mixed batch: %+v (err %v)", r, err)
	}
}
