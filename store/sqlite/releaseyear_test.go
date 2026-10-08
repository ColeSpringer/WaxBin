package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// releaseYearOf reads an item's maintained release_year.
func releaseYearOf(t *testing.T, st *Store, pid model.PID) int {
	t.Helper()
	return scalarInt(t, st, "SELECT release_year FROM playable_item WHERE pid = ?", string(pid))
}

// assertReleaseYearsHold checks every item's release_year against the year its track or
// book row carries, which is what newest orders by.
func assertReleaseYearsHold(t *testing.T, st *Store) {
	t.Helper()
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM playable_item pi
		LEFT JOIN track t ON t.item_id = pi.id LEFT JOIN book bk ON bk.item_id = pi.id
		WHERE pi.release_year <> COALESCE(t.year, bk.year, 0)`); n != 0 {
		t.Fatalf("%d items carry a release_year that is not their year", n)
	}
}

// TestReleaseYearFollowsTheItemsYear: release_year is the track's or book's year through
// every write that sets one (a scan, an edit, a cue carve, an enrichment fill, an album
// rename), 0 for an undated item, and 0 for an episode, whose year is its publication's.
func TestReleaseYearFollowsTheItemsYear(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	dated := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "Dated", artist: "X", albumArt: "X", album: "Al", year: 1997}).ItemPID
	undated := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Bl/1.flac", essence: "e2", content: "c2",
		title: "Undated", artist: "Y", albumArt: "Y", album: "Bl"}).ItemPID
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/bk.m4b", essence: "eb", content: "cb",
		title: "Book", author: "Author", year: 2015}).ItemPID
	bare := putBook(t, st, lib.ID, bookSpec{path: "/lib/bare.m4b", essence: "ebb", content: "cbb",
		title: "Bare", author: "Author"}).ItemPID
	putFeedSpec(t, st, "http://cast.example/f", false, epSpec{title: "Ep", pubNS: 1_000_000_000, year: 2010})
	for pid, want := range map[model.PID]int{dated: 1997, undated: 0, book: 2015, bare: 0} {
		if got := releaseYearOf(t, st, pid); got != want {
			t.Errorf("release_year of %s after the scan = %d, want %d", pid, got, want)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item WHERE kind = 'episode' AND release_year <> 0"); n != 0 {
		t.Errorf("%d episodes carry a release year", n)
	}
	assertReleaseYearsHold(t, st)

	for pid, year := range map[model.PID]string{dated: "2001", book: "2016"} {
		if err := st.EditItemFields(ctx, pid, map[string]string{"year": year}, model.Attribution{Source: model.SourceUser},
			model.LockUnchanged, false); err != nil {
			t.Fatalf("edit year: %v", err)
		}
	}
	if got := releaseYearOf(t, st, dated); got != 2001 {
		t.Errorf("track release_year after the edit = %d, want 2001", got)
	}
	if got := releaseYearOf(t, st, book); got != 2016 {
		t.Errorf("book release_year after the edit = %d, want 2016", got)
	}

	rip := vtrackSpecInput(lib.ID, "/lib/Rip/rip.flac", "er", "cr", 1000, [][2]int64{{0, 300}, {300, 0}})
	for i := range rip.Tracks {
		rip.Tracks[i].Track.Year = 1975
	}
	if _, err := st.PutScannedVirtualTracks(ctx, rip); err != nil {
		t.Fatalf("carve: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item pi JOIN track t ON t.item_id = pi.id WHERE t.album = 'Rip' AND pi.release_year = 1975"); n != 2 {
		t.Errorf("carved tracks dated 1975 = %d, want 2", n)
	}

	// The album fill dates the undated member; the book fill dates the bare book.
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: int64(scalarInt(t, st, "SELECT id FROM album WHERE title = 'Bl'")),
		PID:     model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Bl'")),
		Matched: true, Provider: "test", Fields: map[string]string{"year": "1983"},
	}); err != nil {
		t.Fatalf("album fill: %v", err)
	}
	if got := releaseYearOf(t, st, undated); got != 1983 {
		t.Errorf("release_year after the album fill = %d, want 1983", got)
	}
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(bare))), PID: bare,
		Matched: true, Provider: "test", Fields: map[string]string{"year": "2003"},
	}); err != nil {
		t.Fatalf("book fill: %v", err)
	}
	if got := releaseYearOf(t, st, bare); got != 2003 {
		t.Errorf("release_year after the book fill = %d, want 2003", got)
	}

	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Al'"))
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID, map[string]string{"album": "Renamed"},
		model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("rename album: %v", err)
	}
	if got := releaseYearOf(t, st, dated); got != 2001 {
		t.Errorf("release_year after the album rename = %d, want 2001", got)
	}
	assertReleaseYearsHold(t, st)
	assertVerifyClean(t, st)
}

// TestAnUnchangedRescanLeavesReleaseYearAlone: release_year rides on the item row's write
// and a rescan of files that say what they said leaves its value where it was; the
// counter fires on a change alone.
func TestAnUnchangedRescanLeavesReleaseYearAlone(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "Song", artist: "X", albumArt: "X", album: "Al", year: 1997}
	pid := putTrack(t, st, lib.ID, track).ItemPID
	book := bookSpec{path: "/lib/bk.m4b", essence: "eb", content: "cb", title: "Book", author: "Author", year: 2015}
	putBook(t, st, lib.ID, book)

	if _, err := st.write.ExecContext(ctx, `CREATE TABLE release_year_writes(n INTEGER);
		CREATE TRIGGER count_release_year AFTER UPDATE OF release_year ON playable_item
		WHEN new.release_year <> old.release_year
		BEGIN INSERT INTO release_year_writes VALUES (1); END`); err != nil {
		t.Fatalf("install the counter: %v", err)
	}
	putTrack(t, st, lib.ID, track)
	putBook(t, st, lib.ID, book)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_year_writes"); n != 0 {
		t.Errorf("an unchanged rescan moved release_year %d times, want none", n)
	}
	// The counter itself sees a write that moves the year.
	track.year = 1998
	putTrack(t, st, lib.ID, track)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_year_writes"); n != 1 {
		t.Errorf("a retagged year wrote release_year %d times, want 1", n)
	}
	if got := releaseYearOf(t, st, pid); got != 1998 {
		t.Errorf("release_year after the retag = %d, want 1998", got)
	}
}

// TestVerifyReportsAndRepairsReleaseYearDrift: a release_year that is not the item's year
// is derived-data drift, and the rollup repair puts it back.
func TestVerifyReportsAndRepairsReleaseYearDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "Song", artist: "X", album: "Al", year: 1997}).ItemPID
	putFeedSpec(t, st, "http://cast.example/f", false, epSpec{title: "Ep", pubNS: 1_000_000_000, year: 2010})
	if _, err := st.write.ExecContext(ctx, "UPDATE playable_item SET release_year = 1900"); err != nil {
		t.Fatalf("break the years by hand: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.ReleaseYearDrift != 2 || rep.Consistent() {
		t.Fatalf("release-year drift = %d (consistent %t), want 2 and inconsistent", rep.ReleaseYearDrift, rep.Consistent())
	}
	drift, err := st.DerivedDrift(ctx)
	if err != nil || drift.ReleaseYearDrift != 2 || drift.Consistent() {
		t.Fatalf("audit drift = %+v (err %v), want 2 release-year", drift, err)
	}
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if got := releaseYearOf(t, st, pid); got != 1997 {
		t.Errorf("release_year after the repair = %d, want 1997", got)
	}
	assertReleaseYearsHold(t, st)
	assertVerifyClean(t, st)
}

// TestRefreshRollupsRepairsISBNKeys: a book's isbn_key drifted from its ISBN is repaired by
// the same pass that repairs the years, since a rescan of an unchanged book never
// rewrites the row.
func TestRefreshRollupsRepairsISBNKeys(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{path: "/lib/bk.m4b", essence: "eb", content: "cb",
		title: "Book", author: "Author", isbn: "978-0-306-40615-7"})
	if _, err := st.write.ExecContext(ctx, "UPDATE book SET isbn_key = 'stale'"); err != nil {
		t.Fatalf("break the key by hand: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.BookISBNKeyDrift != 1 {
		t.Fatalf("isbn-key drift = %+v (err %v), want 1", rep, err)
	}
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if got := scalarStr(t, st, "SELECT isbn_key FROM book"); got != "9780306406157" {
		t.Errorf("isbn_key after the repair = %q, want 9780306406157", got)
	}
	assertVerifyClean(t, st)
}
