package sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// foldKeys lists the keys an entity type's folds hold.
func foldKeys(t *testing.T, st *Store, et model.MergeEntity) map[string]model.PID {
	t.Helper()
	folds, err := st.EntityFolds(context.Background(), et)
	if err != nil {
		t.Fatalf("EntityFolds(%s): %v", et, err)
	}
	out := map[string]model.PID{}
	for _, f := range folds {
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
