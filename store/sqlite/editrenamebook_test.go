package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// TestEditBookAuthorRenamesArtistInPlace: a whole-set author edit rewrites the author
// artist in place, so its pid, alias, star, and curation survive and the book's
// contributor row keeps pointing at the same entity. The item's identity key is
// deliberately untouched: a DB-only author edit does not re-anchor the book (that is
// write-back's job), so a rescan still finds it by the old key.
func TestEditBookAuthorRenamesArtistInPlace(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	artistID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	artPID := entityPID(t, st, "artist", "Jane Author")
	idKey0 := scalarStr(t, st, "SELECT identity_key FROM playable_item WHERE pid=?", string(pid))

	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artPID,
		map[string]string{"mbid": "77777777-7777-7777-7777-777777777777"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, artPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemField(ctx, pid, "author", "Janet Author",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Jane Author'"); n != 0 {
		t.Fatalf("old author rows = %d, want 0 (renamed in place)", n)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", artistID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Janet Author" || matchKey != identity.MatchKey("Janet Author") || gotPID != string(artPID) {
		t.Fatalf("artist = %q/%q/%s, want Janet Author with kept pid %s", name, matchKey, gotPID, artPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Jane Author' AND is_primary=0", artistID); n != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", artistID); n != 1 {
		t.Errorf("star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='artist' AND entity_id=? AND field='mbid' AND locked=1", artistID); n != 1 {
		t.Errorf("curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id=ic.item_id
		WHERE pi.pid=? AND ic.role='author' AND ic.artist_id=?`, string(pid), artistID); n != 1 {
		t.Errorf("author contributor rows on the kept entity = %d, want 1", n)
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != artistID {
		t.Errorf("book author_id = %d, want the kept entity %d", id, artistID)
	}
	if k := scalarStr(t, st, "SELECT identity_key FROM playable_item WHERE pid=?", string(pid)); k != idKey0 {
		t.Errorf("identity_key = %q, want unchanged %q (a DB-only edit does not re-anchor)", k, idKey0)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 1 {
		t.Errorf("artist updates = %d, want 1", n)
	}
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq0, "artist", op); n != 0 {
			t.Errorf("artist %s deltas = %d, want 0", op, n)
		}
	}
	assertVerifyClean(t, st)
}

// TestBookAuthorRenameBlockedByOutsideBook: another book by the same author is a
// reference the batch does not move, so the rename falls back to the split and the old
// author keeps its pid and that book.
func TestBookAuthorRenameBlockedByOutsideBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/One/b1.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Author",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Two/b2.m4b", essence: "be2", content: "bc2",
		title: "Book Two", author: "Jane Author",
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book One'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book Two'"))
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if err := st.EditItemField(ctx, pid1, "author", "Janet Author",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author: %v", err)
	}

	if pid := entityPID(t, st, "artist", "Jane Author"); pid != janePID {
		t.Fatalf("Jane Author pid = %s, want kept %s", pid, janePID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Janet Author'"); n != 1 {
		t.Fatalf("Janet Author rows = %d, want 1 (split, not rename)", n)
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid2)); int64(id) != janeID {
		t.Errorf("outside book author_id = %d, want still %d", id, janeID)
	}
	assertVerifyClean(t, st)
}

// TestBookAuthorRenameBlockedByNarratorRef: the same person narrating the book they
// wrote keeps a narrator credit that the author edit does not move, so the rename is
// refused even though the book is in the batch.
func TestBookAuthorRenameBlockedByNarratorRef(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Solo/b.m4b", essence: "be1", content: "bc1",
		title: "Read By The Author", author: "Jane Author", narrators: []string{"Jane Author"},
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE kind='book'"))
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if err := st.EditItemField(ctx, pid, "author", "Janet Author",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author: %v", err)
	}

	if pid := entityPID(t, st, "artist", "Jane Author"); pid != janePID {
		t.Fatalf("Jane Author pid = %s, want kept %s", pid, janePID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM item_contributor WHERE artist_id=? AND role='narrator'", janeID); n != 1 {
		t.Errorf("narrator credit rows on the old entity = %d, want 1", n)
	}
	janetID := entityIDByCol(t, st, "artist", "name", "Janet Author")
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != janetID {
		t.Errorf("book author_id = %d, want the fresh entity %d", id, janetID)
	}
	assertVerifyClean(t, st)
}

// TestBookAuthorRenameBlockedByIncomingNarrator: one batch moves the old author's name
// onto the narrator credit, so the author entity is still referenced by the very edit
// that would rename it. Renaming would carry its pid and curation to the new author and
// mint a bare row for the narrator, so the rename is refused and the author splits.
func TestBookAuthorRenameBlockedByIncomingNarrator(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if err := st.EditItemFields(ctx, pid,
		map[string]string{"author": "Real Writer", "narrator": "Jane Author"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author and narrator: %v", err)
	}

	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", janeID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Jane Author" || matchKey != identity.MatchKey("Jane Author") || gotPID != string(janePID) {
		t.Fatalf("old author = %q/%q/%s, want it untouched", name, matchKey, gotPID)
	}
	writerID := entityIDByCol(t, st, "artist", "name", "Real Writer")
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != writerID {
		t.Errorf("book author_id = %d, want the fresh writer %d", id, writerID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM item_contributor WHERE artist_id=? AND role='narrator'", janeID); n != 1 {
		t.Errorf("narrator credit rows on the kept entity = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestBookAuthorClearSplits: a cleared author names no target to rename onto, so the
// author entity is left where it is and the book simply un-links from it.
func TestBookAuthorClearSplits(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janePID := entityPID(t, st, "artist", "Jane Author")

	if err := st.EditItemField(ctx, pid, "author", "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("clear author: %v", err)
	}

	if pid := entityPID(t, st, "artist", "Jane Author"); pid != janePID {
		t.Fatalf("Jane Author pid = %s, want kept %s", pid, janePID)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=? AND b.author_id IS NOT NULL`, string(pid)); n != 0 {
		t.Errorf("book author_id rows = %d, want the link cleared", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id=ic.item_id WHERE pi.pid=? AND ic.role='author'`, string(pid)); n != 0 {
		t.Errorf("author credit rows = %d, want 0", n)
	}
	assertVerifyClean(t, st)
}

// TestBookAuthorRenameOntoExistingArtistMerges: an author referenced only by books,
// renamed onto a name another artist already owns, takes the taken-key branch. The
// incumbent survives with its pid and inherits the loser's references and attachments,
// so the fix lands in place instead of leaving a drained author behind.
func TestBookAuthorRenameOntoExistingArtistMerges(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Misspelled/b1.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Authr",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Right/b2.m4b", essence: "be2", content: "bc2",
		title: "Book Two", author: "Jane Author",
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book One'"))
	survivorPID := entityPID(t, st, "artist", "Jane Author")
	survivorID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	loserPID := entityPID(t, st, "artist", "Jane Authr")
	loserID := entityIDByCol(t, st, "artist", "name", "Jane Authr")

	// The survivor curates sort unlocked and the loser locked, so the merge's locked-wins
	// fold is visible on the row that comes out; a star on the loser has to fold too.
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, survivorPID,
		map[string]string{"sort": "Author, Jane"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed survivor curation: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, loserPID,
		map[string]string{"sort": "Authr, Jane"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("seed loser curation: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, loserPID, true, nil); err != nil {
		t.Fatalf("seed loser star: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemField(ctx, pid1, "author", "Jane Author",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE id=?", loserID); n != 0 {
		t.Fatalf("loser artist rows = %d, want it merged away", n)
	}
	if p := entityPID(t, st, "artist", "Jane Author"); p != survivorPID {
		t.Fatalf("surviving artist pid = %s, want the incumbent %s", p, survivorPID)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM artist WHERE id=?", survivorID); k != identity.MatchKey("Jane Author") {
		t.Fatalf("survivor match_key = %q, want %q", k, identity.MatchKey("Jane Author"))
	}
	if n := changeCount(t, st, seq0, "artist", model.OpDelete); n != 1 {
		t.Errorf("artist delete deltas = %d, want the loser's 1", n)
	}

	// Both books now hang off the incumbent, through the book row and the credit alike.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM book WHERE author_id=?", survivorID); n != 2 {
		t.Errorf("books on the survivor = %d, want both", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM item_contributor WHERE artist_id=? AND role='author'", survivorID); n != 2 {
		t.Errorf("author credit rows on the survivor = %d, want both", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_contributor WHERE artist_id=?", loserID); n != 0 {
		t.Errorf("credit rows left on the loser = %d, want 0", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Jane Authr' AND is_primary=0", survivorID); n != 1 {
		t.Errorf("alias rows = %d, want the loser's spelling recorded", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", survivorID); n != 1 {
		t.Errorf("star rows on the survivor = %d, want the loser's folded in", n)
	}

	rows, err := st.EntityCuration(ctx, model.MergeArtist, survivorPID)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "sort" {
		t.Fatalf("curation rows = %+v, want one sort row", rows)
	}
	if rows[0].Value != "Author, Jane" || !rows[0].Locked {
		t.Errorf("curation row = %+v, want the survivor's value with the loser's lock", rows[0])
	}
	assertVerifyClean(t, st)
}

// TestEditBookSeriesRenamesInPlace: when every book on a series moves at once, the
// series row is rewritten in place, so its pid survives and the batch emits an update
// rather than a create.
func TestEditBookSeriesRenamesInPlace(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	seriesID := scalarInt(t, st, "SELECT id FROM series")
	seriesPID := scalarStr(t, st, "SELECT pid FROM series")

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemField(ctx, pid, "series", "New Saga",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit series: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series"); n != 1 {
		t.Fatalf("series rows = %d, want 1 (renamed in place)", n)
	}
	var name, matchKey, sortKey, gotPID string
	if err := st.read.QueryRowContext(ctx,
		"SELECT name, match_key, sort_key, pid FROM series WHERE id=?", seriesID).
		Scan(&name, &matchKey, &sortKey, &gotPID); err != nil {
		t.Fatalf("read series: %v", err)
	}
	if name != "New Saga" || matchKey != identity.MatchKey("New Saga") ||
		sortKey != model.SortKey("New Saga") || gotPID != seriesPID {
		t.Fatalf("series = %q/%q/%q/%s, want New Saga with kept pid %s", name, matchKey, sortKey, gotPID, seriesPID)
	}
	if id := scalarInt(t, st, `SELECT b.series_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); id != seriesID {
		t.Errorf("book series_id = %d, want the kept row %d", id, seriesID)
	}
	if n := changeCount(t, st, seq0, "series", model.OpUpdate); n != 1 {
		t.Errorf("series updates = %d, want 1", n)
	}
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq0, "series", op); n != 0 {
			t.Errorf("series %s deltas = %d, want 0", op, n)
		}
	}
	assertVerifyClean(t, st)
}

// TestSeriesRenameSplitsOnPartialCoverage: a book the batch leaves behind keeps the
// series referenced, so the edited book forks onto a fresh row as today.
func TestSeriesRenameSplitsOnPartialCoverage(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, title := range []string{"Book One", "Book Two"} {
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/Author/" + title + "/b.m4b", essence: "be" + string(rune('1'+i)),
			content: "bc" + string(rune('1'+i)),
			title:   title, author: "Jane Author", series: "The Series", seq: "1",
		})
	}
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book One'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book Two'"))
	seriesID := scalarInt(t, st, "SELECT id FROM series")
	seriesPID := scalarStr(t, st, "SELECT pid FROM series")

	if err := st.EditItemField(ctx, pid1, "series", "New Saga",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit series: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series"); n != 2 {
		t.Fatalf("series rows = %d, want 2 (split)", n)
	}
	var name, gotPID string
	if err := st.read.QueryRowContext(ctx,
		"SELECT name, pid FROM series WHERE id=?", seriesID).Scan(&name, &gotPID); err != nil {
		t.Fatalf("read series: %v", err)
	}
	if name != "The Series" || gotPID != seriesPID {
		t.Fatalf("old series = %q/%s, want The Series with kept pid %s", name, gotPID, seriesPID)
	}
	if id := scalarInt(t, st, `SELECT b.series_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid2)); id != seriesID {
		t.Errorf("unedited book series_id = %d, want kept %d", id, seriesID)
	}
	if id := scalarInt(t, st, `SELECT b.series_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid1)); id == seriesID {
		t.Errorf("edited book still on the old series")
	}
	assertVerifyClean(t, st)
}

// TestSeriesRenameSkipsWhenBatchReintroducesName: the batch moves every book off "Dune"
// and puts another book on it in the same call, so the name outlives the move. Renaming
// the row would take its pid to the new name and mint a fresh "Dune" for the arriving
// book, so the series is left to split instead.
func TestSeriesRenameSkipsWhenBatchReintroducesName(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, title := range []string{"Book One", "Book Two"} {
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/Author/" + title + "/b.m4b", essence: "be" + string(rune('1'+i)),
			content: "bc" + string(rune('1'+i)),
			title:   title, author: "Jane Author", series: "Dune", seq: "1",
		})
	}
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Other/Three/b.m4b", essence: "be3", content: "bc3",
		title: "Book Three", author: "Mary Writer", series: "Other Saga", seq: "1",
	})
	pids := make([]model.PID, 0, 3)
	for _, title := range []string{"Book One", "Book Two", "Book Three"} {
		pids = append(pids, model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title=?", title)))
	}
	duneID := entityIDByCol(t, st, "series", "name", "Dune")
	dunePID := entityPID(t, st, "series", "Dune")

	if _, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: pids[0], Fields: map[string]string{"series": "Dune Chronicles"}},
		{ItemPID: pids[1], Fields: map[string]string{"series": "Dune Chronicles"}},
		{ItemPID: pids[2], Fields: map[string]string{"series": "Dune"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("batch edit: %v", err)
	}

	var name, gotPID string
	if err := st.read.QueryRowContext(ctx,
		"SELECT name, pid FROM series WHERE id=?", duneID).Scan(&name, &gotPID); err != nil {
		t.Fatalf("read series: %v", err)
	}
	if name != "Dune" || gotPID != string(dunePID) {
		t.Fatalf("series = %q/%s, want Dune with the kept pid %s", name, gotPID, dunePID)
	}
	if id := scalarInt(t, st, `SELECT b.series_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pids[2])); int64(id) != duneID {
		t.Errorf("arriving book series_id = %d, want the kept Dune row %d", id, duneID)
	}
	chronID := entityIDByCol(t, st, "series", "name", "Dune Chronicles")
	for _, pid := range pids[:2] {
		if id := scalarInt(t, st, `SELECT b.series_id FROM book b
			JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != chronID {
			t.Errorf("moved book series_id = %d, want the fresh row %d", id, chronID)
		}
	}
	assertVerifyClean(t, st)
}

// TestSeriesRenameTakenKeySplits pins the residue: renaming a fully covered series onto
// a name another series already owns leaves both rows alone (there is no series merge
// primitive), so the books move onto the incumbent and the old row drains.
func TestSeriesRenameTakenKeySplits(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/One/b1.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Author", series: "Alpha Saga", seq: "1",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Two/b2.m4b", essence: "be2", content: "bc2",
		title: "Book Two", author: "Mary Writer", series: "Beta Saga", seq: "1",
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book One'"))
	alphaID := entityIDByCol(t, st, "series", "name", "Alpha Saga")
	alphaPID := entityPID(t, st, "series", "Alpha Saga")
	betaID := entityIDByCol(t, st, "series", "name", "Beta Saga")

	if err := st.EditItemField(ctx, pid1, "series", "Beta Saga",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit series: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series"); n != 2 {
		t.Fatalf("series rows = %d, want 2 (no merge primitive)", n)
	}
	var name, gotPID string
	if err := st.read.QueryRowContext(ctx,
		"SELECT name, pid FROM series WHERE id=?", alphaID).Scan(&name, &gotPID); err != nil {
		t.Fatalf("read series: %v", err)
	}
	if name != "Alpha Saga" || gotPID != string(alphaPID) {
		t.Fatalf("old series = %q/%s, want Alpha Saga with kept pid %s", name, gotPID, alphaPID)
	}
	if id := scalarInt(t, st, `SELECT b.series_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid1)); int64(id) != betaID {
		t.Errorf("edited book series_id = %d, want the incumbent %d", id, betaID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM book WHERE series_id=?", alphaID); n != 0 {
		t.Errorf("old series still holds %d books, want 0 (drained)", n)
	}
	assertVerifyClean(t, st)
}
