package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// foldKeys lists the keys an entity type's folds hold.
func foldKeys(t *testing.T, st *Store, et model.MergeEntity) map[string]model.PID {
	t.Helper()
	page, err := st.EntityFolds(context.Background(), et, "", 0)
	if err != nil {
		t.Fatalf("EntityFolds(%s): %v", et, err)
	}
	out := map[string]model.PID{}
	for _, f := range page.Folds {
		out[f.Key] = f.EntityPID
	}
	return out
}

// TestASeriesMergeSurvivesARescan: the loser's spelling of a merged series keeps resolving
// to the survivor, so a read of the file that still says "BK Sagas" leaves the book on
// "BK Saga" instead of minting the loser again.
func TestASeriesMergeSurvivesARescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := bookSpec{
		path: "/lib/b/5.m4b", essence: "be5", content: "bc5", title: "Fifth", author: "BK Author",
		series: "BK Sagas", seq: "5",
	}
	putBook(t, st, lib.ID, spec)
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "First", author: "BK Author",
		series: "BK Saga", seq: "1",
	})
	survivor := entityPIDByName(t, st, "series", "name", "BK Saga")
	loser := entityPIDByName(t, st, "series", "name", "BK Sagas")
	rep, err := st.MergeEntity(ctx, model.MergeSeries, survivor, loser)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Folds, []string{"bk sagas"}) {
		t.Errorf("MergeReport.Folds = %v, want the loser's key", rep.Folds)
	}
	// Changed bytes make the put rewrite the book, series and all.
	spec.content = "bc5b"
	putBook(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series"); n != 1 {
		t.Errorf("series after the rescan = %d, want the survivor alone", n)
	}
	if got := scalarStr(t, st, `SELECT s.pid FROM book b JOIN series s ON s.id = b.series_id
		JOIN playable_item pi ON pi.id = b.item_id WHERE pi.title = 'Fifth'`); got != string(survivor) {
		t.Errorf("the rescanned book's series = %s, want the survivor %s", got, survivor)
	}
	if got := foldKeys(t, st, model.MergeSeries); !reflect.DeepEqual(got, map[string]model.PID{"bk sagas": survivor}) {
		t.Errorf("series folds = %v", got)
	}
	assertVerifyClean(t, st)
}

// TestAnArtistMergeSurvivesARescan, with the fold chain a second merge makes and the
// unfold that lets the next rescan bring the loser back.
func TestAnArtistMergeSurvivesARescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	beatles := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "One"}
	putTrack(t, st, lib.ID, beatles)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "Two"})
	fab := trackSpec{path: "/lib/c/1.flac", essence: "e3", content: "c3", title: "Three", artist: "Fab Four", album: "Three"}
	putTrack(t, st, lib.ID, fab)
	the := entityPIDByName(t, st, "artist", "name", "The Beatles")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, the, entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	// A retag of another field changes the bytes, so the put resolves the entities again.
	beatles.content = "c1b"
	putTrack(t, st, lib.ID, beatles)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name = 'Beatles'"); n != 0 {
		t.Errorf("the rescan minted the merged artist again (%d rows)", n)
	}
	if got := scalarStr(t, st, `SELECT a.pid FROM track t JOIN artist a ON a.id = t.artist_id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'One'`); got != string(the) {
		t.Errorf("the rescanned track's artist = %s, want the survivor %s", got, the)
	}

	// Merging the survivor in turn carries its folds along.
	fabPID := entityPIDByName(t, st, "artist", "name", "Fab Four")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, fabPID, the); err != nil {
		t.Fatal(err)
	}
	if got := foldKeys(t, st, model.MergeArtist); !reflect.DeepEqual(got, map[string]model.PID{"beatles": fabPID, "the beatles": fabPID}) {
		t.Errorf("artist folds after the second merge = %v, want both keys on Fab Four", got)
	}
	beatles.content = "c1c"
	putTrack(t, st, lib.ID, beatles)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Errorf("artists after the chained rescan = %d, want Fab Four alone", n)
	}

	// Unfolding forgets the spelling, a key named twice once, and the next rescan creates
	// it again.
	if err := st.UnfoldEntity(ctx, model.MergeArtist, "beatles", "beatles"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnfoldEntity(ctx, model.MergeArtist, "beatles"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unfolding a key twice = %v, want CodeNotFound", err)
	}
	beatles.content = "c1d"
	putTrack(t, st, lib.ID, beatles)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name = 'Beatles'"); n != 1 {
		t.Errorf("Beatles rows after the unfold and a rescan = %d, want it back", n)
	}
	assertVerifyClean(t, st)
}

// TestAnAlbumMergeOfAFolderPairSurvives: two folders of one release are two albums, and
// their merge holds through a rescan of both.
func TestAnAlbumMergeOfAFolderPairSurvives(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first := trackSpec{path: "/lib/Beatles/Help/1.flac", essence: "e1", content: "c1", title: "Help", artist: "The Beatles", album: "Help", trackNo: 1}
	second := trackSpec{path: "/lib/Beatles/Help (2)/2.flac", essence: "e2", content: "c2", title: "Yesterday", artist: "The Beatles", album: "Help", trackNo: 2}
	a := putTrack(t, st, lib.ID, first)
	b := putTrack(t, st, lib.ID, second)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("albums = %d, want one per folder", n)
	}
	survivor := model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(a.ItemPID)))
	loser := model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(b.ItemPID)))
	rep, err := st.MergeEntity(ctx, model.MergeAlbum, survivor, loser)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Folds) != 1 {
		t.Errorf("MergeReport.Folds = %q, want the loser's key", rep.Folds)
	}
	second.content, first.content = "c2b", "c1b"
	putTrack(t, st, lib.ID, second)
	putTrack(t, st, lib.ID, first)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums after the rescan = %d, want the merged one", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release groups after the rescan = %d, want one", n)
	}
	assertVerifyClean(t, st)
}

// TestAFoldedAlbumTakesTheIdentifiersAFileBrings: a file that resolves to an album
// through a fold fills the identifiers the album lacks, as a file hitting its own key does.
func TestAFoldedAlbumTakesTheIdentifiersAFileBrings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first := trackSpec{path: "/lib/Beatles/Help/1.flac", essence: "e1", content: "c1", title: "Help", artist: "The Beatles", album: "Help", trackNo: 1}
	second := trackSpec{path: "/lib/Beatles/Help (2)/2.flac", essence: "e2", content: "c2", title: "Yesterday", artist: "The Beatles", album: "Help", trackNo: 2}
	a := putTrack(t, st, lib.ID, first)
	b := putTrack(t, st, lib.ID, second)
	survivor := model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(a.ItemPID)))
	loser := model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(b.ItemPID)))
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, survivor, loser); err != nil {
		t.Fatal(err)
	}
	second.content, second.barcode, second.label = "c2b", "0123456789012", "Parlophone"
	putTrack(t, st, lib.ID, second)
	if got := scalarStr(t, st, "SELECT COALESCE(barcode,'') || '|' || COALESCE(label,'') FROM album WHERE pid = ?", string(survivor)); got != "0123456789012|Parlophone" {
		t.Errorf("the survivor's barcode and label = %q, want the file's", got)
	}
	assertVerifyClean(t, st)
}

// TestFoldedKeysNameTheirSurvivors: the lookups by key that plan an import's folders and
// seed a playlist ref read a fold the way a scan does, so a name a merge retired answers
// with its survivor: its name, its title, its tracks and books.
func TestFoldedKeysNameTheirSurvivors(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Beatles/Help/1.flac", essence: "e1", content: "c1", title: "Help", artist: "The Beatles", album: "Help", trackNo: 1})
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Beatles/Help (2)/2.flac", essence: "e2", content: "c2", title: "Yesterday", artist: "Beatles", album: "Help", trackNo: 2})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "Dune", author: "Frank Herbert"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/2.m4b", essence: "be2", content: "bc2", title: "Children of Dune", author: "F. Herbert"})
	albumOf := func(item model.PID) (model.PID, string) {
		t.Helper()
		var pid, key string
		if err := st.rdb().QueryRowContext(ctx, `SELECT al.pid, al.match_key FROM album al JOIN track t ON t.album_id = al.id
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(item)).Scan(&pid, &key); err != nil {
			t.Fatal(err)
		}
		return model.PID(pid), key
	}
	loserAlbum, loserKey := albumOf(b.ItemPID)
	for _, m := range []struct {
		et              model.MergeEntity
		survivor, loser model.PID
	}{
		{model.MergeArtist, entityPIDByName(t, st, "artist", "name", "The Beatles"), entityPIDByName(t, st, "artist", "name", "Beatles")},
		{model.MergeArtist, entityPIDByName(t, st, "artist", "name", "Frank Herbert"), entityPIDByName(t, st, "artist", "name", "F. Herbert")},
	} {
		if _, err := st.MergeEntity(ctx, m.et, m.survivor, m.loser); err != nil {
			t.Fatal(err)
		}
	}
	survivorAlbum := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE pid <> ?", string(loserAlbum)))
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, survivorAlbum, loserAlbum); err != nil {
		t.Fatal(err)
	}

	names, err := st.ArtistNames(ctx, []string{"beatles", "the beatles"})
	if err != nil || names["beatles"] != "The Beatles" || names["the beatles"] != "The Beatles" {
		t.Errorf("ArtistNames = %v (err %v), want both keys naming The Beatles", names, err)
	}
	titles, err := st.AlbumTitles(ctx, []string{loserKey})
	if err != nil || titles[loserKey] != "Help" {
		t.Errorf("AlbumTitles of the merged-away key = %v (err %v), want the survivor's title", titles, err)
	}
	tracks, err := st.ItemsByArtistKey(ctx, "beatles")
	if err != nil || len(tracks) != 2 {
		t.Errorf("ItemsByArtistKey(beatles) = %d items (err %v), want the survivor's two tracks", len(tracks), err)
	}
	books, err := st.ItemsByAuthorKey(ctx, "f herbert")
	if err != nil || len(books) != 2 {
		t.Errorf("ItemsByAuthorKey(f herbert) = %d items (err %v), want the survivor's two books", len(books), err)
	}
}

// TestAReleaseGroupAndGenreMergeSurvive: a merged group's key and a merged genre's
// spelling keep resolving to their survivors, so a rescan mints neither again.
func TestAReleaseGroupAndGenreMergeSurvive(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	help := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Help", artist: "The Beatles", album: "Help", genre: "HipHop"}
	putTrack(t, st, lib.ID, help)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Help Me", artist: "The Beatles", album: "Help Me", genre: "Hip Hop"})
	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup,
		entityPIDByName(t, st, "release_group", "title", "Help Me"), entityPIDByName(t, st, "release_group", "title", "Help")); err != nil {
		t.Fatal(err)
	}
	rep, err := st.MergeEntity(ctx, model.MergeGenre,
		entityPIDByName(t, st, "genre", "name", "Hip Hop"), entityPIDByName(t, st, "genre", "name", "HipHop"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Folds, []string{"genre:hiphop"}) {
		t.Errorf("genre MergeReport.Folds = %v, want the facet and the loser's key", rep.Folds)
	}
	help.content = "c1b"
	putTrack(t, st, lib.ID, help)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release groups after the rescan = %d, want the survivor alone", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM genre"); n != 1 {
		t.Errorf("genres after the rescan = %d, want the survivor alone", n)
	}
	assertVerifyClean(t, st)
}

// TestARenameHoldsThroughItsLocks: a locked rename keeps its member on the renamed
// entity through a scan of its file, which still says the old name, and folds the keys it
// left, so a new file spelled the old way joins the renamed artist and album rather than
// minting them again. An unlocked rename writes no fold and yields to the file, as every
// unlocked edit does.
func TestARenameHoldsThroughItsLocks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	locked := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", albumArt: "Beatles",
		album: "One", trackNo: 1, preserveLocks: true}
	putTrack(t, st, lib.ID, locked)
	artist := entityPIDByName(t, st, "artist", "name", "Beatles")
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	if _, err := st.RenameEntity(ctx, model.MergeArtist, artist, map[string]string{"name": "The Beatles"}, user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if got := foldKeys(t, st, model.MergeArtist); !reflect.DeepEqual(got, map[string]model.PID{"beatles": artist}) {
		t.Errorf("artist folds after a locked rename = %v, want the old key on the renamed artist", got)
	}
	locked.content = "c1b"
	putTrack(t, st, lib.ID, locked)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "Two", artist: "Beatles",
		albumArt: "Beatles", album: "One", trackNo: 2, preserveLocks: true})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Errorf("artists after a file spelled the old way = %d, want the renamed one alone", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums after a file spelled the old way = %d, want the renamed one alone", n)
	}
	if got := scalarStr(t, st, `SELECT al.pid FROM track t JOIN album al ON al.id = t.album_id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'Two'`); got != string(album) {
		t.Errorf("the new file's album = %s, want the renamed %s", got, album)
	}

	unlocked := trackSpec{path: "/lib/b/1.flac", essence: "e3", content: "c3", title: "Three", artist: "Fab Four", album: "Three", preserveLocks: true}
	putTrack(t, st, lib.ID, unlocked)
	if _, err := st.RenameEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "Fab Four"),
		map[string]string{"name": "Wings"}, user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if got := foldKeys(t, st, model.MergeArtist); len(got) != 1 {
		t.Errorf("artist folds after an unlocked rename = %v, want none added", got)
	}
	unlocked.content = "c3b"
	putTrack(t, st, lib.ID, unlocked)
	if got := scalarStr(t, st, `SELECT a.name FROM track t JOIN artist a ON a.id = t.artist_id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'Three'`); got != "Fab Four" {
		t.Errorf("an unlocked rename's member after a rescan names %q, want the file's Fab Four", got)
	}
	assertVerifyClean(t, st)
}

// TestARenameOntoAFoldedKeyMergesIntoItsEntity: a merge's fold counts as a holder of its
// key, so renaming another entity onto that spelling merges it where a scan of the
// spelling would land, and the fold's own entity renamed onto it takes the key back.
func TestARenameOntoAFoldedKeyMergesIntoItsEntity(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "One"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "Two"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c/1.flac", essence: "e3", content: "c3", title: "Three", artist: "Fab Four", album: "Three"})
	the := entityPIDByName(t, st, "artist", "name", "The Beatles")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, the, entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	rep, err := st.RenameEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "Fab Four"),
		map[string]string{"name": "Beatles"}, user, model.LockOf(true), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != model.EntityRenameMerged || rep.MergedInto != the {
		t.Errorf("rename report = %+v, want merged into the fold's entity %s", rep, the)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Errorf("artists after a rename onto a folded key = %d, want the fold's entity alone", n)
	}
	if got := scalarStr(t, st, `SELECT a.pid FROM track t JOIN artist a ON a.id = t.artist_id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'Three'`); got != string(the) {
		t.Errorf("the renamed entity's member names %s, want the fold's %s", got, the)
	}
	// Forced past the lock the first rename left on its member.
	if _, err := st.RenameEntity(ctx, model.MergeArtist, the, map[string]string{"name": "Beatles"}, user, model.LockOf(true), true); err != nil {
		t.Fatal(err)
	}
	if got := scalarStr(t, st, "SELECT match_key FROM artist WHERE pid = ?", string(the)); got != "beatles" {
		t.Errorf("match key after renaming onto its own fold = %q, want beatles", got)
	}
	// The key it took is its own now, and the locked renames folded the keys they left.
	want := map[string]model.PID{"fab four": the, "the beatles": the}
	if got := foldKeys(t, st, model.MergeArtist); !reflect.DeepEqual(got, want) {
		t.Errorf("artist folds after the entity took its folded key = %v, want %v", got, want)
	}
	assertVerifyClean(t, st)
}

// TestAnEditCorrectingAnItemFoldsNothing: an item edit or a credit edit that happens to
// cover all of an artist's tracks renames the artist in place, keeping its pid, but folds
// nothing: it said what those items are, not that the old name is an alias, so an album
// scanned later under the old name is that artist's own again.
func TestAnEditCorrectingAnItemFoldsNothing(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	misfiled := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Prince", album: "Wrong"}).ItemPID
	credited := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Two", artist: "Madonna", album: "Also Wrong"}).ItemPID
	if err := st.EditItemField(ctx, misfiled, "artist", "The Artist", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetItemCredits(ctx, credited, model.RoleArtist, []string{"Cyndi Lauper"}, user, model.LockOf(true), false, false); err != nil {
		t.Fatal(err)
	}
	if got := foldKeys(t, st, model.MergeArtist); len(got) != 0 {
		t.Errorf("artist folds after the corrections = %v, want none", got)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c/1.flac", essence: "e3", content: "c3", title: "Kiss", artist: "Prince", album: "Parade"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/d/1.flac", essence: "e4", content: "c4", title: "Vogue", artist: "Madonna", album: "Erotica"})
	for title, want := range map[string]string{"Kiss": "Prince", "Vogue": "Madonna"} {
		if got := scalarStr(t, st, `SELECT a.name FROM track t JOIN artist a ON a.id = t.artist_id
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = ?`, title); got != want {
			t.Errorf("%s's artist = %q, want %q, its own", title, got, want)
		}
	}
	assertVerifyClean(t, st)
}

// TestAFoldGoesWithItsEntity: once the survivor of a merge is swept as an orphan, nothing
// is left to fold into, and the fold goes too.
func TestAFoldGoesWithItsEntity(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "One"}
	two := trackSpec{path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "Two"}
	putTrack(t, st, lib.ID, one)
	putTrack(t, st, lib.ID, two)
	if _, err := st.MergeEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "The Beatles"),
		entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	if len(foldKeys(t, st, model.MergeArtist)) != 1 {
		t.Fatal("no fold after the merge")
	}
	// Both tracks move to another artist, which leaves the survivor childless.
	one.artist, two.artist = "Someone Else", "Someone Else"
	putTrack(t, st, lib.ID, one)
	putTrack(t, st, lib.ID, two)
	if _, err := st.GCOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_fold"); n != 0 {
		t.Errorf("fold rows after the survivor was swept = %d, want none", n)
	}
}

// TestAMergeRecordsTheLosersSpelling: a merge's fold keeps the spelling it retired, the
// loser's name, and for an album the folder its members sat in, below its library's root.
func TestAMergeRecordsTheLosersSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/One/1.mp3", essence: "e1", content: "c1", title: "One",
		artist: "The Beatles", albumArt: "The Beatles", album: "Hits", genre: "Hip Hop", trackNo: 1})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/Two/2.mp3", essence: "e2", content: "c2", title: "Two",
		artist: "Beatles", albumArt: "Beatles", album: "Greatest", genre: "Hip-Hop Rap", trackNo: 1})
	for _, m := range []struct {
		et               model.MergeEntity
		table, col, s, l string
	}{
		{model.MergeArtist, "artist", "name", "The Beatles", "Beatles"},
		{model.MergeAlbum, "album", "title", "Hits", "Greatest"},
		{model.MergeReleaseGroup, "release_group", "title", "Hits", "Greatest"},
		{model.MergeGenre, "genre", "name", "Hip Hop", "Hip-Hop Rap"},
	} {
		survivor, loser := entityPIDByName(t, st, m.table, m.col, m.s), entityPIDByName(t, st, m.table, m.col, m.l)
		if _, err := st.MergeEntity(ctx, m.et, survivor, loser); err != nil {
			t.Fatalf("merge %s: %v", m.et, err)
		}
		page, err := st.EntityFolds(ctx, m.et, "", 0)
		if err != nil || len(page.Folds) != 1 {
			t.Fatalf("%s folds = %+v (err %v), want one", m.et, page, err)
		}
		f := page.Folds[0]
		if f.Name != m.l || f.EntityPID != survivor {
			t.Errorf("%s fold = %+v, want %q kept on the survivor", m.et, f, m.l)
		}
		wantFolder, wantLib := "", model.PID("")
		if m.et == model.MergeAlbum {
			wantFolder, wantLib = filepath.Join("B", "Two"), lib.PID
		}
		if f.Folder != wantFolder || f.LibraryPID != wantLib {
			t.Errorf("%s fold folder = %q in %q, want %q in %q", m.et, f.Folder, f.LibraryPID, wantFolder, wantLib)
		}
	}
}

// TestALockedRenameRecordsTheOldSpelling: a locked rename's fold keeps the name it left,
// and an album's the folder its members sit in.
func TestALockedRenameRecordsTheOldSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", albumArt: "Beatles",
		album: "One", trackNo: 1, preserveLocks: true})
	artist := entityPIDByName(t, st, "artist", "name", "Beatles")
	if _, err := st.RenameEntity(ctx, model.MergeArtist, artist, map[string]string{"name": "The Beatles"}, user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	page, err := st.EntityFolds(ctx, model.MergeArtist, "", 0)
	if err != nil || len(page.Folds) != 1 || page.Folds[0].Name != "Beatles" || page.Folds[0].EntityName != "The Beatles" {
		t.Fatalf("artist folds = %+v (err %v), want the old spelling kept beside the new name", page, err)
	}
	album := entityPIDByName(t, st, "album", "title", "One")
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, album, map[string]string{"album": "Number One"}, user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	// The artist's rename re-keyed the album, folding the key it left, and the album's own
	// rename folds the next: both keep the title they were read from and the folder.
	page, err = st.EntityFolds(ctx, model.MergeAlbum, "", 0)
	if err != nil || len(page.Folds) != 2 {
		t.Fatalf("album folds = %+v (err %v), want the artist rename's and the title rename's", page, err)
	}
	for _, f := range page.Folds {
		if f.Name != "One" || f.Folder != "a" || f.LibraryPID != lib.PID || f.EntityName != "Number One" {
			t.Errorf("album fold = %+v, want the old title One in folder a of the library", f)
		}
	}
}

// TestEntityFoldsPageInKeyOrder: folds page by key, a cursor resuming after the last, and
// the default page holds them all.
func TestEntityFoldsPageInKeyOrder(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/s.flac", essence: "es", content: "cs", title: "S", artist: "Survivor", album: "S"})
	for i, name := range []string{"Charlie", "Alpha", "Bravo"} {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + name + ".flac", essence: "e" + name, content: "c" + name,
			title: name, artist: name, album: name, trackNo: i + 1})
		if _, err := st.MergeEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "Survivor"),
			entityPIDByName(t, st, "artist", "name", name)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.EntityFolds(ctx, model.MergeArtist, "", 2)
	if err != nil || len(page.Folds) != 2 || page.Folds[0].Key != "alpha" || page.Folds[1].Key != "bravo" || !page.HasMore || page.Next != "bravo" {
		t.Fatalf("first page = %+v (err %v), want alpha and bravo and a cursor at bravo", page, err)
	}
	page, err = st.EntityFolds(ctx, model.MergeArtist, page.Next, 2)
	if err != nil || len(page.Folds) != 1 || page.Folds[0].Key != "charlie" || page.HasMore || page.Next != "" {
		t.Fatalf("second page = %+v (err %v), want charlie alone", page, err)
	}
	if page, err := st.EntityFolds(ctx, model.MergeArtist, "", 0); err != nil || len(page.Folds) != 3 {
		t.Errorf("default page = %+v (err %v), want all three", page, err)
	}
}

// TestUnfoldDropsTheMatchingAlias: the alias a merge kept for the loser's name goes with the
// fold of that name, and the survivor's other aliases stay.
func TestUnfoldDropsTheMatchingAlias(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "The Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Beatles", album: "B"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "Fab Four", album: "C"})
	survivor := entityPIDByName(t, st, "artist", "name", "The Beatles")
	for _, name := range []string{"Beatles", "Fab Four"} {
		if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, entityPIDByName(t, st, "artist", "name", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UnfoldEntity(ctx, model.MergeArtist, "beatles"); err != nil {
		t.Fatal(err)
	}
	info, err := st.EntityByPID(ctx, read.EntityArtist, survivor)
	if err != nil || !reflect.DeepEqual(info.Aliases, []string{"Fab Four"}) {
		t.Fatalf("aliases after the unfold = %+v (err %v), want Fab Four alone", info, err)
	}
}

// TestItemsByEntityKeyedSpellingNamesWhatTheFoldKept: a genre's kept items are told by
// their genre list, leaving the genre's own out, and an artist's are every item it is
// credited on, its own included, since no stored text replays how a file spelled it.
func TestItemsByEntityKeyedSpellingNamesWhatTheFoldKept(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	own := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "The Beatles", album: "A", genre: "Rock"})
	kept := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Beatles", album: "B", genre: "Rock & Roll"})
	byAlbumArtist := putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "Guest",
		albumArt: "Beatles", album: "B", genre: "Rock & Roll; Pop"})
	if _, err := st.MergeEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "The Beatles"),
		entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeGenre, entityPIDByName(t, st, "genre", "name", "Rock"),
		entityPIDByName(t, st, "genre", "name", "Rock & Roll")); err != nil {
		t.Fatal(err)
	}
	want := []model.PID{kept.ItemPID, byAlbumArtist.ItemPID}
	slices.Sort(want)
	every := []model.PID{own.ItemPID, kept.ItemPID, byAlbumArtist.ItemPID}
	slices.Sort(every)
	for _, tc := range []struct {
		et   model.MergeEntity
		key  string
		want []model.PID
	}{
		{model.MergeArtist, "beatles", every},
		{model.MergeGenre, "genre:rock roll", want},
		{model.MergeArtist, "nobody", nil},
	} {
		got, err := st.ItemsByEntityKeyedSpelling(ctx, tc.et, tc.key)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s %q = %v (err %v), want %v", tc.et, tc.key, got, err, tc.want)
		}
	}
}

// TestAPutWithReResolveRelinksAnUnchangedBook: a book read again unchanged keeps the author
// a fold gave it, and a read that re-resolves files it under the author it states.
func TestAPutWithReResolveRelinksAnUnchangedBook(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "First", author: "Frank Herbert"})
	spec := bookSpec{path: "/lib/b/2.m4b", essence: "be2", content: "bc2", title: "Second", author: "F. Herbert"}
	putBook(t, st, lib.ID, spec)
	survivor := entityPIDByName(t, st, "artist", "name", "Frank Herbert")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, entityPIDByName(t, st, "artist", "name", "F. Herbert")); err != nil {
		t.Fatal(err)
	}
	if err := st.UnfoldEntity(ctx, model.MergeArtist, "f herbert"); err != nil {
		t.Fatal(err)
	}
	author := func() string {
		return scalarStr(t, st, `SELECT a.pid FROM book b JOIN artist a ON a.id = b.author_id
			JOIN playable_item pi ON pi.id = b.item_id WHERE pi.title = 'Second'`)
	}
	putBook(t, st, lib.ID, spec)
	if got := author(); got != string(survivor) {
		t.Fatalf("an unchanged read moved the book to %s; the test's premise is gone", got)
	}
	in := bookSpecInput(lib.ID, spec)
	in.ReResolve = true
	if _, err := st.PutScannedBook(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := author(); got == string(survivor) {
		t.Errorf("the re-resolving read left the book on the survivor %s, want the author it states", got)
	}
}

// TestItemsByEntityKeyedSpellingNamesEveryCreditOfAnArtist: an artist a fold kept can sit
// in a joint credit, its own name only part of the text, or in a credit the item's text puts
// second, so an unfold reads every item the entity is credited on in any role.
func TestItemsByEntityKeyedSpellingNamesEveryCreditOfAnArtist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/s.flac", essence: "es", content: "cs", title: "S", artist: "Shawn Carter", album: "S"})
	var want []model.PID
	for i, credit := range []string{"Jay-Z", "Jay-Z feat. Rihanna", "Rihanna feat. Jay-Z"} {
		r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i),
			content: "c" + strconv.Itoa(i), title: "T" + strconv.Itoa(i), artist: credit, album: "A" + strconv.Itoa(i)})
		want = append(want, r.ItemPID)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, entityPIDByName(t, st, "artist", "name", "Shawn Carter"),
		entityPIDByName(t, st, "artist", "name", "Jay-Z")); err != nil {
		t.Fatal(err)
	}
	got, err := st.ItemsByEntityKeyedSpelling(ctx, model.MergeArtist, "jay z")
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range want {
		if !slices.Contains(got, pid) {
			t.Errorf("items = %v, want %v among them", got, pid)
		}
	}
}

// TestCheckFoldsNamesTheKeysNoFoldHolds: the check refuses as UnfoldEntity would, naming
// each key no fold holds once, and forgets nothing.
func TestCheckFoldsNamesTheKeysNoFoldHolds(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "The Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Beatles", album: "B"})
	survivor := entityPIDByName(t, st, "artist", "name", "The Beatles")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckFolds(ctx, model.MergeArtist, "beatles"); err != nil {
		t.Errorf("check of a held key = %v, want nil", err)
	}
	err := st.CheckFolds(ctx, model.MergeArtist, "beatles", "nobody", "nobody")
	if !waxerr.Is(err, waxerr.CodeNotFound) || strings.Count(err.Error(), "nobody") != 1 || strings.Contains(err.Error(), `"beatles"`) {
		t.Errorf("check with an unknown key = %v, want CodeNotFound naming it alone, once", err)
	}
	if err := st.CheckFolds(ctx, model.MergeAlbum, "beatles"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("check of an artist key as an album = %v, want CodeNotFound", err)
	}
	for _, bad := range []error{st.CheckFolds(ctx, "planet", "beatles"), st.CheckFolds(ctx, model.MergeArtist)} {
		if !waxerr.Is(bad, waxerr.CodeInvalid) {
			t.Errorf("check with a bad type or no keys = %v, want CodeInvalid", bad)
		}
	}
	if got := foldKeys(t, st, model.MergeArtist); len(got) != 1 || got["beatles"] != survivor {
		t.Errorf("folds after the checks = %v, want beatles still held", got)
	}
}

// TestPrimaryFilesNamesEachItemsPrimaryInOneRead: each item's primary file comes back with
// its path and library, a book's representative part among them, and a pid naming no item
// is left out.
func TestPrimaryFilesNamesEachItemsPrimaryInOneRead(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "A"})
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.mp3", essence: "eb", content: "cb", title: "B", author: "Y", position: 1})
	got, err := st.PrimaryFiles(ctx, []model.PID{track.ItemPID, book.ItemPID, track.ItemPID, "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("primary files = %v, want the two items'", got)
	}
	for _, c := range []struct {
		res  *model.ScanItemResult
		path string
	}{{track, "/lib/a.flac"}, {book, "/lib/b/1.mp3"}} {
		f := got[c.res.ItemPID]
		if f.FilePID != c.res.FilePID || string(f.Path) != c.path || f.LibraryPID != lib.PID || f.Role != "primary" {
			t.Errorf("primary of %s = %+v, want %s at %s in %s", c.res.ItemPID, f, c.res.FilePID, c.path, lib.PID)
		}
	}
}
