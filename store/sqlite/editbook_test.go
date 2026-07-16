package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// bookEditFixture scans one single-file book and returns its item pid.
func bookEditFixture(t *testing.T) (*Store, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"},
		series: "The Series", seq: "1", genres: []string{"Fantasy"}, year: 2010,
	})
	var pid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("read book pid: %v", err)
	}
	return st, model.PID(pid)
}

func TestEditBookSubtitleAndProvenance(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "subtitle", "A Tale", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit subtitle: %v", err)
	}
	var subtitle string
	if err := st.read.QueryRowContext(ctx,
		"SELECT b.subtitle FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?", string(pid)).Scan(&subtitle); err != nil {
		t.Fatalf("read subtitle: %v", err)
	}
	if subtitle != "A Tale" {
		t.Fatalf("subtitle = %q, want %q", subtitle, "A Tale")
	}
	rows, _ := st.FieldProvenance(ctx, pid)
	if len(rows) != 1 || rows[0].Field != "subtitle" || rows[0].Source != model.SourceUser || !rows[0].Locked {
		t.Fatalf("provenance = %+v, want one locked user subtitle row", rows)
	}
}

func TestEditBookAuthorReResolvesContributor(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "author", "Mary Writer", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit author: %v", err)
	}

	// The item view reads the book's author into the artist column.
	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "Mary Writer" {
		t.Fatalf("book author (artist col) = %q, want Mary Writer", v.Artist)
	}

	// A Mary Writer artist entity exists and is the book's linked author contributor.
	var name string
	if err := st.read.QueryRowContext(ctx, `SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid=? AND ic.role='author'`, string(pid)).Scan(&name); err != nil {
		t.Fatalf("read author contributor: %v", err)
	}
	if name != "Mary Writer" {
		t.Fatalf("author contributor = %q, want Mary Writer", name)
	}

	// The narrator contributor is preserved across an author-only edit.
	var narr string
	if err := st.read.QueryRowContext(ctx, `SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid=? AND ic.role='narrator'`, string(pid)).Scan(&narr); err != nil {
		t.Fatalf("read narrator: %v", err)
	}
	if narr != "Ned Narrator" {
		t.Fatalf("narrator = %q, want preserved Ned Narrator", narr)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean after author edit: %+v (err %v)", rep, err)
	}
}

func TestEditBookSeriesAndNarrator(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemFields(ctx, pid, map[string]string{
		"series": "New Saga", "narrator": "Val Voice & Al Audio",
	}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	v, _ := st.ItemByPID(ctx, pid)
	if v.Series != "New Saga" {
		t.Errorf("series = %q, want New Saga", v.Series)
	}
	if v.Narrator != "Val Voice, Al Audio" {
		t.Errorf("narrator display = %q, want joined 'Val Voice, Al Audio'", v.Narrator)
	}
	// The narrator value split into two contributor entities.
	n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id = ic.item_id WHERE pi.pid=? AND ic.role='narrator'`, string(pid))
	if n != 2 {
		t.Errorf("narrator contributors = %d, want 2", n)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

func TestEditBookGenreAndTitle(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemFields(ctx, pid, map[string]string{
		"title": "Renamed Book", "genre": "Mystery",
	}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	v, _ := st.ItemByPID(ctx, pid)
	if v.Title != "Renamed Book" {
		t.Errorf("title = %q, want Renamed Book", v.Title)
	}
	if v.Genre != "Mystery" {
		t.Errorf("genre = %q, want Mystery", v.Genre)
	}
	// FTS reflects the new title, and the book row still resolves as a book.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'renamed'"); n != 1 {
		t.Errorf("FTS match for new book title = %d, want 1", n)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean after book genre/title edit: %+v (err %v)", rep, err)
	}
}

func TestEditBookRejectsTrackOnlyField(t *testing.T) {
	st, pid := bookEditFixture(t)
	// album is a track field, not valid on a book.
	err := st.EditItemField(context.Background(), pid, "album", "X", model.SourceUser, true, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("track field on a book: want CodeInvalid, got %v", err)
	}
}

func TestEditTrackRejectsBookOnlyField(t *testing.T) {
	st, pid := editFixture(t)
	// author is a book field, not valid on a track.
	err := st.EditItemField(context.Background(), pid, "author", "X", model.SourceUser, true, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("book field on a track: want CodeInvalid, got %v", err)
	}
}
