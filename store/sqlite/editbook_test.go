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
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("read book pid: %v", err)
	}
	return st, model.PID(pid)
}

func TestEditBookSubtitleAndProvenance(t *testing.T) {
	t.Parallel()
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "subtitle", "A Tale", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit subtitle: %v", err)
	}
	var subtitle string
	if err := st.rdb().QueryRowContext(ctx,
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
	t.Parallel()
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "author", "Mary Writer", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	if err := st.rdb().QueryRowContext(ctx, `SELECT a.name FROM item_contributor ic
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
	if err := st.rdb().QueryRowContext(ctx, `SELECT a.name FROM item_contributor ic
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
	t.Parallel()
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemFields(ctx, pid, map[string]string{
		"series": "New Saga", "narrator": "Val Voice & Al Audio",
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	t.Parallel()
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	if err := st.EditItemFields(ctx, pid, map[string]string{
		"title": "Renamed Book", "genre": "Mystery",
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	t.Parallel()
	st, pid := bookEditFixture(t)
	// album is a track field, not valid on a book.
	err := st.EditItemField(context.Background(), pid, "album", "X", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("track field on a book: want CodeInvalid, got %v", err)
	}
}

func TestEditTrackRejectsBookOnlyField(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t)
	// author is a book field, not valid on a track.
	err := st.EditItemField(context.Background(), pid, "author", "X", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("book field on a track: want CodeInvalid, got %v", err)
	}
}

// bookColumn reads one column of a book's row by item pid.
func bookColumn(t *testing.T, st *Store, pid model.PID, col string) string {
	t.Helper()
	return scalarStr(t, st, "SELECT "+col+" FROM book WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)", string(pid))
}

// bookCreditNames lists the names of a book's credited entities in role order, joined by
// a bar, to check which entities its credits folded onto.
func bookCreditNames(t *testing.T, st *Store, pid model.PID) string {
	t.Helper()
	return scalarStr(t, st, `SELECT COALESCE(group_concat(name, '|'), '') FROM (SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid = ? ORDER BY ic.role, ic.position)`, string(pid))
}

// TestBookEditKeepsTheSpellingTheFileStated: the author and narrator displays keep the
// spelling the file stated when a credit folds onto an entity spelled otherwise, through
// an edit of another field, so a rescan of the same bytes has nothing to re-derive. The
// edit path loads the credited entities by their own names to re-link them; those names
// are not the display.
func TestBookEditKeepsTheSpellingTheFileStated(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/h.flac", essence: "eh", content: "ch",
		title: "Reading", artist: "Frank Herbert", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/s.flac", essence: "es", content: "cs",
		title: "Intro", artist: "Scott Brick", album: "Al"})
	spec := bookSpec{path: "/lib/dune.m4b", essence: "e1", content: "c1",
		title: "Dune", author: "FRANK HERBERT", narrators: []string{"SCOTT BRICK"}}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	if got := bookCreditNames(t, st, pid); got != "Frank Herbert|Scott Brick" {
		t.Fatalf("credits = %q, want both folded onto the entities the tracks minted", got)
	}
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Fatalf("scanned author = %q, narrator = %q; want the file's spellings", a, n)
	}

	// The edit is locked so a rescan honouring locks has nothing of its own to
	// re-derive: a reported change could then only be a display that moved.
	if err := st.EditItemField(ctx, pid, "year", "1965", model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("edit year: %v", err)
	}
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Errorf("after a year edit author = %q, narrator = %q; want the file's spellings kept", a, n)
	}
	spec.preserveLocks = true
	if res := putBook(t, st, lib.ID, spec); res.MetadataChanged {
		t.Errorf("a rescan of the same bytes reported a metadata change")
	}
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Errorf("after the rescan author = %q, narrator = %q; want the file's spellings still", a, n)
	}
}

// TestBookLockedDisplaysSurviveARescan: a locked author or narrator keeps the spelling
// the edit typed through a rescan that honours locks, even though its credit links an
// entity spelled otherwise. The overlay carries the stored display beside the entity
// names, and the put stores the display it was given.
func TestBookLockedDisplaysSurviveARescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/h.flac", essence: "eh", content: "ch",
		title: "Reading", artist: "Frank Herbert", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/s.flac", essence: "es", content: "cs",
		title: "Intro", artist: "Scott Brick", album: "Al"})
	spec := bookSpec{path: "/lib/dune.m4b", essence: "e1", content: "c1",
		title: "Dune", author: "Frank Herbert", narrators: []string{"Scott Brick"}}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	user := model.Attribution{Source: model.SourceUser}
	if err := st.EditItemField(ctx, pid, "author", "FRANK HERBERT", user, model.LockOn, false); err != nil {
		t.Fatalf("edit author: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "narrator", "SCOTT BRICK", user, model.LockOn, false); err != nil {
		t.Fatalf("edit narrator: %v", err)
	}
	if got := bookCreditNames(t, st, pid); got != "Frank Herbert|Scott Brick" {
		t.Fatalf("credits after the edits = %q, want the same two entities", got)
	}

	spec.content, spec.preserveLocks = "c2", true
	putBook(t, st, lib.ID, spec)
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Errorf("after a locked rescan author = %q, narrator = %q; want the locked spellings", a, n)
	}
}

// TestBookAuthorEditSplitsLikeTheScannerAndKeepsRestatedCredits: an author edit credits
// the people the text names the way a scan does ("A & B" is two authors shown as "A, B"),
// restating the display the catalog shows, which no splitter can take apart, leaves the
// credits as they are instead of minting one artist named after the whole line, a real
// change re-credits in the new order, and text naming nobody clears the author and the
// narrator alike.
func TestBookAuthorEditSplitsLikeTheScannerAndKeepsRestatedCredits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	pid := putBook(t, st, lib.ID, bookSpec{path: "/lib/omens.m4b", essence: "e1", content: "c1",
		title: "Good Omens", author: "Terry Pratchett & Neil Gaiman"}).ItemPID
	if got := bookCreditNames(t, st, pid); got != "Terry Pratchett|Neil Gaiman" {
		t.Fatalf("scanned credits = %q, want both authors", got)
	}
	if got := bookColumn(t, st, pid, "author"); got != "Terry Pratchett, Neil Gaiman" {
		t.Fatalf("scanned author = %q, want the joined display", got)
	}

	for _, same := range []string{"Terry Pratchett, Neil Gaiman", "Terry Pratchett & Neil Gaiman"} {
		if err := st.EditItemField(ctx, pid, "author", same, user, model.LockUnchanged, false); err != nil {
			t.Fatalf("restate author as %q: %v", same, err)
		}
		if got := bookCreditNames(t, st, pid); got != "Terry Pratchett|Neil Gaiman" {
			t.Errorf("after restating %q credits = %q, want both authors kept", same, got)
		}
		if got := bookColumn(t, st, pid, "author"); got != "Terry Pratchett, Neil Gaiman" {
			t.Errorf("after restating %q author = %q, want the display kept", same, got)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name LIKE '%,%'"); n != 0 {
		t.Errorf("%d artist(s) named after a joined display", n)
	}

	if err := st.EditItemField(ctx, pid, "author", "Neil Gaiman & Terry Pratchett", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("reorder authors: %v", err)
	}
	if got := bookCreditNames(t, st, pid); got != "Neil Gaiman|Terry Pratchett" {
		t.Errorf("reordered credits = %q, want Gaiman first", got)
	}
	if got := bookColumn(t, st, pid, "author"); got != "Neil Gaiman, Terry Pratchett" {
		t.Errorf("reordered author = %q, want the new display", got)
	}
	if got := scalarStr(t, st, `SELECT a.name FROM book bk JOIN artist a ON a.id = bk.author_id
		WHERE bk.item_id = (SELECT id FROM playable_item WHERE pid = ?)`, string(pid)); got != "Neil Gaiman" {
		t.Errorf("author_id names %q, want the first credited author", got)
	}

	for _, f := range []string{"author", "narrator"} {
		if err := st.EditItemField(ctx, pid, f, " / ", user, model.LockUnchanged, false); err != nil {
			t.Fatalf("clear %s: %v", f, err)
		}
		if got := bookColumn(t, st, pid, f); got != "" {
			t.Errorf("%s set to a bare separator = %q, want cleared", f, got)
		}
	}
	if got := bookCreditNames(t, st, pid); got != "" {
		t.Errorf("credits after clearing = %q, want none", got)
	}
}

// TestBookAuthorEditSurvivesALaterCreditWrite: the plain author edit records the display
// it stored, so a later credit write naming the same people finds the row describing
// the column and keeps it, lock included, instead of retiring an edit that still holds.
func TestBookAuthorEditSurvivesALaterCreditWrite(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	pid := putBook(t, st, lib.ID, bookSpec{path: "/lib/mort.m4b", essence: "e1", content: "c1",
		title: "Mort", author: "Terry Pratchett"}).ItemPID
	id := scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid))
	if err := st.EditItemField(ctx, pid, "author", "Terry Pratchett & Neil Gaiman", user, model.LockOn, false); err != nil {
		t.Fatalf("edit author: %v", err)
	}
	if got := scalarStr(t, st, "SELECT value FROM field_provenance WHERE item_id = ? AND field = 'author'", id); got != "Terry Pratchett, Neil Gaiman" {
		t.Fatalf("recorded author = %q, want the display the column holds", got)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Terry Pratchett", "Neil Gaiman"},
		user, model.LockUnchanged, true, false); err != nil {
		t.Fatalf("re-credit the same people: %v", err)
	}
	if got := bookColumn(t, st, pid, "author"); got != "Terry Pratchett, Neil Gaiman" {
		t.Errorf("author after the credit write = %q, want unchanged", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE item_id = ? AND field = 'author' AND locked = 1", id); n != 1 {
		t.Errorf("locked plain author rows after the credit write = %d, want the edit's row kept", n)
	}
}

// TestBookCreditResavedSurvivesARescan: a credit saved again with the people the file
// already names, spelled as the catalog spells them, keeps the file's display (the same
// rule), and a rescan of the same bytes then has nothing to re-derive: the credit rows
// stay and no metadata change is reported.
func TestBookCreditResavedSurvivesARescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/h.flac", essence: "eh", content: "ch",
		title: "Reading", artist: "Frank Herbert", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/s.flac", essence: "es", content: "cs",
		title: "Intro", artist: "Scott Brick", album: "Al"})
	spec := bookSpec{path: "/lib/dune.m4b", essence: "e1", content: "c1",
		title: "Dune", author: "FRANK HERBERT", narrators: []string{"SCOTT BRICK"}}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	id := scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid))
	for role, names := range map[model.ContributorRole][]string{model.RoleAuthor: {"Frank Herbert"}, model.RoleNarrator: {"Scott Brick"}} {
		if _, _, err := st.SetItemCredits(ctx, pid, role, names, user, model.LockUnchanged, false, false); err != nil {
			t.Fatalf("re-save %s: %v", role, err)
		}
	}
	credits := func() int {
		return scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE item_id = ? AND field IN ('credit.author', 'credit.narrator')", id)
	}
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Fatalf("after re-saving author = %q, narrator = %q; want the file's spellings kept", a, n)
	}
	if credits() != 2 {
		t.Fatalf("credit rows after re-saving = %d, want 2", credits())
	}

	if res := putBook(t, st, lib.ID, spec); res.MetadataChanged {
		t.Errorf("a rescan of the same bytes reported a metadata change")
	}
	if credits() != 2 {
		t.Errorf("credit rows after the rescan = %d, want both kept", credits())
	}
	if a, n := bookColumn(t, st, pid, "author"), bookColumn(t, st, pid, "narrator"); a != "FRANK HERBERT" || n != "SCOTT BRICK" {
		t.Errorf("after the rescan author = %q, narrator = %q; want the file's spellings still", a, n)
	}
}

// TestPutScannedBookDerivesAMissingDisplay: a put handed split names without the joined
// display stores the display those names make, so a column never disagrees with the
// credits beside it; a display that was given is stored as given.
func TestPutScannedBookDerivesAMissingDisplay(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	in := bookSpecInput(lib.ID, bookSpec{path: "/lib/tome.m4b", essence: "e1", content: "c1",
		title: "Tome", author: "Ann Author"})
	in.Book.Author, in.Book.Authors = "", []string{"Ann Author", "Bob Writer"}
	in.Book.Narrator, in.Book.Narrators = "", []string{"Cy Reader"}
	res, err := st.PutScannedBook(context.Background(), in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if a, n := bookColumn(t, st, res.ItemPID, "author"), bookColumn(t, st, res.ItemPID, "narrator"); a != "Ann Author, Bob Writer" || n != "Cy Reader" {
		t.Errorf("author = %q, narrator = %q; want the displays the split names make", a, n)
	}
}
