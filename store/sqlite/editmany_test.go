package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// twoTrackFixture scans two distinct track items and returns their pids.
func twoTrackFixture(t *testing.T) (*Store, *model.Library, model.PID, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/1/01.flac", essence: "e1", content: "c1",
		title: "One", artist: "Alpha", albumArt: "Alpha", album: "First", genre: "Rock",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/B/1/01.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Beta", albumArt: "Beta", album: "Second", genre: "Rock",
	})
	var p1, p2 string
	rows, err := st.read.QueryContext(context.Background(),
		"SELECT pid FROM playable_item WHERE kind='track' ORDER BY id")
	if err != nil {
		t.Fatalf("pids: %v", err)
	}
	defer rows.Close()
	rows.Next()
	rows.Scan(&p1)
	rows.Next()
	rows.Scan(&p2)
	return st, lib, model.PID(p1), model.PID(p2)
}

func TestEditManyFieldsApplies(t *testing.T) {
	st, _, p1, p2 := twoTrackFixture(t)
	ctx := context.Background()

	res, err := st.EditManyFields(ctx, []model.PID{p1, p2, p1}, map[string]string{"genre": "Jazz"},
		model.SourceUser, true, false, false)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	// The duplicate p1 is collapsed.
	if len(res.Edited) != 2 {
		t.Fatalf("edited = %v, want 2 unique", res.Edited)
	}
	for _, pid := range []model.PID{p1, p2} {
		var genre string
		if err := st.read.QueryRowContext(ctx,
			"SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(pid)).Scan(&genre); err != nil {
			t.Fatalf("read %s: %v", pid, err)
		}
		if genre != "Jazz" {
			t.Fatalf("%s genre = %q, want Jazz", pid, genre)
		}
	}
	// Rollups unioned once and consistent.
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean after batch: %+v (err %v)", r, err)
	}
}

func TestEditManyFieldsAtomicOnKindMismatch(t *testing.T) {
	st, lib, p1, _ := twoTrackFixture(t)
	ctx := context.Background()

	// Add a book, then batch a book-only field across a track and the book: the track
	// rejects it, so the whole batch must roll back (the book stays unedited too).
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/b.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author",
	})
	var bpid string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&bpid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	_, err := st.EditManyFields(ctx, []model.PID{model.PID(bpid), p1}, map[string]string{"publisher": "X"},
		model.SourceUser, true, false, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("batch = %v, want CodeInvalid (publisher on a track)", err)
	}
	// The book's publisher was NOT written (rolled back).
	var publisher string
	if err := st.read.QueryRowContext(ctx,
		"SELECT publisher FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?", bpid).Scan(&publisher); err != nil {
		t.Fatalf("read publisher: %v", err)
	}
	if publisher != "" {
		t.Fatalf("publisher = %q, want empty (batch should have rolled back)", publisher)
	}
}

func TestEditManyFieldsSkipLocked(t *testing.T) {
	st, _, p1, p2 := twoTrackFixture(t)
	ctx := context.Background()

	// Lock genre on p1.
	if err := st.EditItemField(ctx, p1, "genre", "Locked", model.SourceUser, true, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// Without force, a batch touching genre would fail on p1; skip-locked skips it.
	res, err := st.EditManyFields(ctx, []model.PID{p1, p2}, map[string]string{"genre": "Jazz"},
		model.SourceUser, true, false, true)
	if err != nil {
		t.Fatalf("skip-locked batch: %v", err)
	}
	if len(res.Edited) != 1 || res.Edited[0] != p2 {
		t.Fatalf("edited = %v, want [%s]", res.Edited, p2)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != p1 {
		t.Fatalf("skipped = %v, want [%s]", res.Skipped, p1)
	}
	// p1's locked genre is untouched.
	var g1 string
	if err := st.read.QueryRowContext(ctx,
		"SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(p1)).Scan(&g1); err != nil {
		t.Fatalf("read p1: %v", err)
	}
	if g1 != "Locked" {
		t.Fatalf("p1 genre = %q, want Locked (untouched)", g1)
	}

	// Without skip-locked and without force, the same batch fails fast (atomic).
	if _, err := st.EditManyFields(ctx, []model.PID{p1, p2}, map[string]string{"genre": "Pop"},
		model.SourceUser, true, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("locked batch = %v, want CodeLocked", err)
	}
}
