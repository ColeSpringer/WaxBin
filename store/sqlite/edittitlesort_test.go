package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEditTitleSortMatrix covers title_sort, the composer_sort contract on the item's
// title: an explicit edit stores the spelling and keys the item by it, a title edit drops
// an unlocked spelling and keeps a locked one, a combined edit keeps the explicit one,
// and a clear keys the item by its title again. A book takes the same field.
func TestEditTitleSortMatrix(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t) // title "Original", no spelling
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	want := func(step, spelling, key string) {
		t.Helper()
		if sp, k := itemSortCols(t, st, pid); sp != spelling || k != key {
			t.Fatalf("%s: title_sort/sort_key = (%q, %q), want (%q, %q)", step, sp, k, spelling, key)
		}
	}
	if err := st.EditItemField(ctx, pid, "title_sort", "Zz Original", user, model.LockOf(true), false); err != nil {
		t.Fatalf("edit title_sort: %v", err)
	}
	want("explicit spelling", "Zz Original", model.SortKey("Zz Original"))
	if err := st.EditItemField(ctx, pid, "title_sort", "X", user, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit locked title_sort = %v, want CodeLocked", err)
	}
	if err := st.EditItemField(ctx, pid, "title", "Renamed", user, model.LockOf(false), false); err != nil {
		t.Fatalf("title edit over a locked spelling: %v", err)
	}
	want("title edit over a locked spelling", "Zz Original", model.SortKey("Zz Original"))
	if err := st.UnlockField(ctx, pid, "title_sort"); err != nil {
		t.Fatal(err)
	}
	if err := st.EditItemField(ctx, pid, "title", "Third", user, model.LockOf(false), false); err != nil {
		t.Fatalf("unlocked title edit: %v", err)
	}
	want("unlocked title edit", "", model.SortKey("Third"))
	if err := st.EditItemFields(ctx, pid, map[string]string{"title": "Fourth", "title_sort": "Fourth, The"},
		user, model.LockOf(false), false); err != nil {
		t.Fatalf("combined edit: %v", err)
	}
	want("combined edit", "Fourth, The", model.SortKey("Fourth, The"))
	if err := st.EditItemField(ctx, pid, "title", "Fourth", user, model.LockOf(false), false); err != nil {
		t.Fatalf("title set to itself: %v", err)
	}
	want("title set to itself", "Fourth, The", model.SortKey("Fourth, The"))
	if err := st.EditItemField(ctx, pid, "title_sort", "", user, model.LockOf(false), false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	want("cleared spelling", "", model.SortKey("Fourth"))

	bst, blib := entityFixture(t)
	b := putBook(t, bst, blib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb", title: "Hobbit", author: "Tolkien"})
	if err := bst.EditItemField(ctx, b.ItemPID, "title_sort", "Hobbit, The", user, model.LockOf(false), false); err != nil {
		t.Fatalf("book title_sort: %v", err)
	}
	if sp, k := itemSortCols(t, bst, b.ItemPID); sp != "Hobbit, The" || k != model.SortKey("Hobbit, The") {
		t.Errorf("book title_sort/sort_key = (%q, %q), want the spelling", sp, k)
	}
	if err := bst.EditItemField(ctx, b.ItemPID, "title", "The Hobbit", user, model.LockOf(false), false); err != nil {
		t.Fatalf("book title: %v", err)
	}
	if sp, _ := itemSortCols(t, bst, b.ItemPID); sp != "" {
		t.Errorf("book title edit kept the spelling %q", sp)
	}
	assertVerifyClean(t, st)
	assertVerifyClean(t, bst)
}

// TestEditAlbumSortMatrix: album_sort is a track's stated album spelling, which an album
// edit drops unless it is locked, and the album's derived spelling follows the edit.
func TestEditAlbumSortMatrix(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t) // album "One"
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	albumSort := func() string {
		return scalarStr(t, st, "SELECT album_sort FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?", string(pid))
	}
	if err := st.EditItemField(ctx, pid, "album_sort", "Uno", user, model.LockOf(true), false); err != nil {
		t.Fatalf("edit album_sort: %v", err)
	}
	if got := albumSort(); got != "Uno" {
		t.Errorf("album_sort = %q, want Uno", got)
	}
	if sp, k := entitySpellingOf(t, st, "album", "One"); sp != "Uno" || k != model.SortKey("Uno") {
		t.Errorf("album spelling/key = %q/%q, want the member's", sp, k)
	}
	if err := st.EditItemField(ctx, pid, "album", "Two", user, model.LockOf(false), false); err != nil {
		t.Fatalf("album edit over a locked spelling: %v", err)
	}
	if got := albumSort(); got != "Uno" {
		t.Errorf("album edit over a locked spelling: album_sort = %q, want it kept", got)
	}
	if err := st.UnlockField(ctx, pid, "album_sort"); err != nil {
		t.Fatal(err)
	}
	if err := st.EditItemField(ctx, pid, "album", "Three", user, model.LockOf(false), false); err != nil {
		t.Fatalf("unlocked album edit: %v", err)
	}
	if got := albumSort(); got != "" {
		t.Errorf("unlocked album edit: album_sort = %q, want it dropped", got)
	}
	assertVerifyClean(t, st)
}

// TestScanKeepsLockedTitleSpellings: a forced rescan keeps a locked title spelling against
// the file's, and a locked title keeps its spelling beside it.
func TestScanKeepsLockedTitleSpellings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	rescan := func(st *Store, lib *model.Library, content, spelling string) {
		t.Helper()
		in := trackSpecInput(lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: content, title: "Disk Title",
			titleSort: spelling, artist: "A", album: "Al", albumSort: "Disk Album"})
		in.PreserveLocks = true
		if _, err := st.PutScannedTrack(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	st, lib := entityFixture(t)
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "Song", artist: "A", album: "Al"})
	if err := st.EditItemFields(ctx, tr.ItemPID, map[string]string{"title_sort": "Curated", "album_sort": "Curated Album"},
		user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	rescan(st, lib, "c2", "Disk Spelling")
	if sp, _ := itemSortCols(t, st, tr.ItemPID); sp != "Curated" {
		t.Errorf("locked title_sort after a forced rescan = %q, want Curated", sp)
	}
	if got := scalarStr(t, st, "SELECT album_sort FROM track"); got != "Curated Album" {
		t.Errorf("locked album_sort after a forced rescan = %q, want Curated Album", got)
	}

	st2, lib2 := entityFixture(t)
	tr2 := putTrack(t, st2, lib2.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "Song", artist: "A", album: "Al"})
	if err := st2.EditItemField(ctx, tr2.ItemPID, "title", "Curated Title", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	rescan(st2, lib2, "c2", "Disk Spelling")
	if sp, k := itemSortCols(t, st2, tr2.ItemPID); sp != "" || k != model.SortKey("Curated Title") {
		t.Errorf("locked title rescan = (%q, %q), want no spelling and the curated title's key", sp, k)
	}
	assertVerifyClean(t, st)
	assertVerifyClean(t, st2)
}

// TestTitleAndAlbumSortFieldsAndView: title_sort orders by the key the item collates by
// and album_sort by its album's, and the item view carries both spellings as stated.
func TestTitleAndAlbumSortFieldsAndView(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, s := range []struct{ title, titleSort, album, albumSort string }{
		{"Alpha", "Zz Alpha", "Mid", "Mid"}, {"Beta", "", "Zebra", "Aardvark"}, {"Gamma", "", "Last", "Zz Last"},
	} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + "/1.flac", essence: "e" + n, content: "c" + n, title: s.title,
			titleSort: s.titleSort, artist: "X", albumArt: "X", album: s.album, albumSort: s.albumSort})
	}
	order := func(field string) string {
		t.Helper()
		items, err := st.QueryItems(ctx, query.New(query.EntityItems).OrderBy(field, false).Build(), "")
		if err != nil {
			t.Fatalf("order by %s: %v", field, err)
		}
		return strings.Join(titlesOf(items), ",")
	}
	if got := order("title_sort"); got != "Beta,Gamma,Alpha" {
		t.Errorf("by title_sort = %s, want Beta,Gamma,Alpha", got)
	}
	if got := order("album_sort"); got != "Beta,Alpha,Gamma" {
		t.Errorf("by album_sort = %s, want Beta (aardvark), Alpha (mid), Gamma (zz last)", got)
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "Alpha").Build(), "")
	if err != nil || len(items) != 1 || items[0].TitleSort != "Zz Alpha" || items[0].AlbumSort != "Mid" {
		t.Errorf("view = %+v (err %v), want TitleSort Zz Alpha and AlbumSort Mid", items, err)
	}
}

// TestARescanRederivesAnUnlockedTitleSpelling: a rescan whose file states another title
// spelling takes it over an unlocked edit and retires the edit's provenance, so the row no
// longer describes a value the item does not hold.
func TestARescanRederivesAnUnlockedTitleSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "Song", artist: "A", album: "Al"})
	if err := st.EditItemField(ctx, tr.ItemPID, "title_sort", "Edited", model.Attribution{Source: model.SourceUser},
		model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c2", title: "Song", titleSort: "Disk",
		artist: "A", album: "Al", preserveLocks: true})
	if sp, _ := itemSortCols(t, st, tr.ItemPID); sp != "Disk" {
		t.Errorf("title_sort after the rescan = %q, want the file's", sp)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.field = 'title_sort'`, string(tr.ItemPID)); n != 0 {
		t.Errorf("title_sort provenance rows after the rescan = %d, want the edit's retired", n)
	}
	// A file whose only change is the spelling changes the item's metadata too.
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c3", title: "Song", titleSort: "Other",
		artist: "A", album: "Al", preserveLocks: true})
	if !res.MetadataChanged {
		t.Errorf("a put moving only the title spelling = %+v, want MetadataChanged", res)
	}
}

// TestAnAlbumSortEditTouchesTheAlbumChainOnly: only the album and its release group read
// a track's album spelling, so the edit leaves the artists' and genres' upkeep alone.
func TestAnAlbumSortEditTouchesTheAlbumChainOnly(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t)
	ctx := context.Background()
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	aff := newAffectedRollups()
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := editTrackFieldsTx(ctx, tx, st.log, itemID, []string{"album_sort"}, map[string]string{"album_sort": "Uno"},
			"test", aff)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(aff.albums) != 1 || len(aff.rgs) != 1 || len(aff.artists) != 0 || len(aff.genres) != 0 {
		t.Errorf("affected = %d albums, %d groups, %d artists, %d genres; want the album and its group alone",
			len(aff.albums), len(aff.rgs), len(aff.artists), len(aff.genres))
	}
}

// TestAGenreEditTouchesTheGenresOnly: a genre moves an item between genres and nothing
// else, so the edit leaves its artists', album's and release group's rollups and
// spellings alone, and the item still reads and searches under the new genre.
func TestAGenreEditTouchesTheGenresOnly(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t)
	ctx := context.Background()
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	aff := newAffectedRollups()
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := editTrackFieldsTx(ctx, tx, st.log, itemID, []string{"genre"}, map[string]string{"genre": "Jazz; Blues"},
			"test", aff); err != nil {
			return err
		}
		return maintainRollupsTx(ctx, tx, aff, nowNS())
	}); err != nil {
		t.Fatal(err)
	}
	if len(aff.genres) != 3 || len(aff.artists) != 0 || len(aff.albums) != 0 || len(aff.rgs) != 0 {
		t.Errorf("affected = %d genres, %d artists, %d albums, %d groups; want Rock, Jazz and Blues alone",
			len(aff.genres), len(aff.artists), len(aff.albums), len(aff.rgs))
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_genre ig JOIN genre g ON g.id = ig.genre_id
		WHERE ig.item_id = ? AND g.name IN ('Jazz', 'Blues')`, itemID); n != 2 {
		t.Errorf("item genres = %d of Jazz and Blues, want both", n)
	}
	res, err := st.Search(ctx, "Blues", read.SearchOptions{})
	if err != nil || len(res.Tracks) != 1 {
		t.Errorf("search for the new genre = %+v (err %v), want the track", res, err)
	}
	assertVerifyClean(t, st)
}
