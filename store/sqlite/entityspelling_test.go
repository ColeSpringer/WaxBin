package sqlite

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// entitySpellingOf reads an entity's derived spelling and its key by its display name.
func entitySpellingOf(t *testing.T, st *Store, table, name string) (spelling, key string) {
	t.Helper()
	nameCol, spellCol := "title", "title_sort"
	if table == "artist" {
		nameCol, spellCol = "name", "name_sort"
	}
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT "+spellCol+", sort_key FROM "+table+" WHERE "+nameCol+" = ?", name).Scan(&spelling, &key); err != nil {
		t.Fatalf("read %s %q: %v", table, name, err)
	}
	return spelling, key
}

// facetOrder lists a facet's bucket labels in its order.
func facetOrder(t *testing.T, st *Store, g read.GroupBy) string {
	t.Helper()
	res, err := st.Facet(context.Background(), query.New(query.EntityItems).Build(), g, "", 0, "")
	if err != nil {
		t.Fatalf("facet %s: %v", g, err)
	}
	var out []string
	for _, b := range res.Buckets {
		out = append(out, b.Display)
	}
	return strings.Join(out, ",")
}

// TestAlbumSortFollowsItsMembersSpelling: an album takes the spelling most of its live
// members state, a tie going to the earliest member's and a lone stating member's
// winning alone; the key follows it, the album facet orders by it, and a move emits the
// album's delta.
func TestAlbumSortFollowsItsMembersSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	put := func(n, album, spelling string) {
		t.Helper()
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + album + "/" + n + ".flac", essence: album + n, content: album + n,
			title: "T" + n, artist: "Pink Floyd", albumArt: "Pink Floyd", album: album, albumSort: spelling})
	}
	put("1", "Wall", "Wall, The")
	put("2", "Wall", "")
	put("3", "Wall", "Wall, The")
	put("1", "Meddle", "")
	put("2", "Meddle", "Zz Meddle")
	put("1", "Animals", "Beasts")
	put("2", "Animals", "Animals, The")
	for name, want := range map[string]string{"Wall": "Wall, The", "Meddle": "Zz Meddle", "Animals": "Beasts"} {
		if sp, k := entitySpellingOf(t, st, "album", name); sp != want || k != model.SortKey(want) {
			t.Errorf("album %s spelling/key = %q/%q, want %q and its key", name, sp, k, want)
		}
	}
	if got := facetOrder(t, st, read.GroupAlbum); got != "Animals,Wall,Meddle" {
		t.Errorf("album facet order = %s, want Animals (beasts), Wall (wall, the), Meddle (zz meddle)", got)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	put("3", "Animals", "Animals, The")
	if sp, _ := entitySpellingOf(t, st, "album", "Animals"); sp != "Animals, The" {
		t.Errorf("after a second stating member, Animals spelling = %q, want the majority", sp)
	}
	animals := scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Animals'")
	if !slices.Contains(changesSince(t, st, seq), [2]string{"album", animals}) {
		t.Errorf("no album delta for the moved spelling: %v", changesSince(t, st, seq))
	}
	assertVerifyClean(t, st)
}

// TestReleaseGroupSortFollowsItsAlbums: a release group's spelling is the one its albums'
// members state, across editions in folders of their own.
func TestReleaseGroupSortFollowsItsAlbums(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, folder := range []string{"cd", "remaster", "remaster"} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + folder + "/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Wall", albumSort: "Wall, The"})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE title = 'Wall'"); n != 2 {
		t.Fatalf("albums = %d, want two editions", n)
	}
	if sp, k := entitySpellingOf(t, st, "release_group", "Wall"); sp != "Wall, The" || k != model.SortKey("Wall, The") {
		t.Errorf("release group spelling/key = %q/%q, want Wall, The", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestArtistSortFollowsArtistSortTags: an artist takes the spelling its credits naming it
// alone state, so the facet and EntityPage order Elvis Costello under C.
func TestArtistSortFollowsArtistSortTags(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, a := range []struct{ name, spelling string }{
		{"Elvis Costello", "Costello, Elvis"}, {"David Bowie", "Bowie, David"}, {"Dire Straits", ""},
	} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: a.name, albumArt: a.name, album: "Al" + n, artistSort: a.spelling})
	}
	if sp, k := entitySpellingOf(t, st, "artist", "Elvis Costello"); sp != "Costello, Elvis" || k != model.SortKey("Costello, Elvis") {
		t.Errorf("Elvis Costello spelling/key = %q/%q", sp, k)
	}
	if got := facetOrder(t, st, read.GroupArtist); got != "David Bowie,Elvis Costello,Dire Straits" {
		t.Errorf("artist facet order = %s, want Bowie, Costello, Dire Straits", got)
	}
	page, err := st.EntityPage(ctx, read.EntityArtist, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range page.Entities {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "David Bowie,Elvis Costello,Dire Straits" {
		t.Errorf("EntityPage order = %s, want Bowie, Costello, Dire Straits", got)
	}
	assertVerifyClean(t, st)
}

// TestACollaborationCreditSpellsNoArtist: a spelling of a credit naming two artists spells
// neither of them, whether it is the track credit or the album-artist credit.
func TestACollaborationCreditSpellsNoArtist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Under Pressure",
		artist: "Queen; David Bowie", artistSort: "Queen & Bowie, David", album: "Hot Space"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Other",
		artist: "Freddie Mercury", albumArt: "Queen; David Bowie", albumArtistSort: "Queen & Bowie, David", album: "Duets"})
	for _, name := range []string{"Queen", "David Bowie"} {
		if sp, k := entitySpellingOf(t, st, "artist", name); sp != "" || k != model.SortKey(name) {
			t.Errorf("%s spelling/key = %q/%q, want none and the name's key", name, sp, k)
		}
	}
	assertVerifyClean(t, st)
}

// TestAnAuthorsSpellingOrdersTheArtist: the author sort a sole author's books state spells
// the artist the artist facet files them under.
func TestAnAuthorsSpellingOrdersTheArtist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putBook(t, st, lib.ID, bookSpec{path: "/lib/a.m4b", essence: "ea", content: "ca", title: "Earthsea",
		author: "Ursula K. Le Guin", authorSort: "Le Guin, Ursula K."})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb", title: "Frankenstein",
		author: "Mary Shelley"})
	if sp, _ := entitySpellingOf(t, st, "artist", "Ursula K. Le Guin"); sp != "Le Guin, Ursula K." {
		t.Errorf("author spelling = %q, want the books' author sort", sp)
	}
	if got := facetOrder(t, st, read.GroupArtist); got != "Ursula K. Le Guin,Mary Shelley" {
		t.Errorf("artist facet order = %s, want Le Guin before Shelley", got)
	}
	assertVerifyClean(t, st)
}

// TestCuratedSortOutranksTheMembers covers the curated sort against the members': a
// curated sort keys the album whatever its members state, clearing it falls back to
// their spelling rather than the title, and members that all drop the spelling leave
// the album keyed by its title.
func TestCuratedSortOutranksTheMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := func(spelling, content string) trackSpec {
		return trackSpec{path: "/lib/w/1.flac", essence: "ew", content: content, title: "T",
			artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Wall", albumSort: spelling}
	}
	putTrack(t, st, lib.ID, spec("Wall, The", "c1"))
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Wall'"))
	edit := func(v string) {
		t.Helper()
		if _, err := st.EditEntityFields(ctx, model.MergeAlbum, album, map[string]string{"sort": v},
			model.Attribution{Source: model.SourceUser}, model.LockOn, true); err != nil {
			t.Fatalf("edit sort %q: %v", v, err)
		}
	}
	edit("Zzz")
	putTrack(t, st, lib.ID, spec("Other, The", "c2"))
	if sp, k := entitySpellingOf(t, st, "album", "Wall"); sp != "Other, The" || k != model.SortKey("Zzz") {
		t.Errorf("curated: spelling/key = %q/%q, want the members' spelling kept and the curated key", sp, k)
	}
	assertVerifyClean(t, st)
	edit("")
	if _, k := entitySpellingOf(t, st, "album", "Wall"); k != model.SortKey("Other, The") {
		t.Errorf("cleared: key = %q, want the members' spelling's", k)
	}
	assertVerifyClean(t, st)
	putTrack(t, st, lib.ID, spec("", "c3"))
	if sp, k := entitySpellingOf(t, st, "album", "Wall"); sp != "" || k != model.SortKey("Wall") {
		t.Errorf("no member states one: spelling/key = %q/%q, want none and the title's key", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestDeferredPutsSettleTheSpellings: a scan's deferred puts leave a moved spelling for the
// settle, as they leave the counts, and the settle moves it and the key together.
func TestDeferredPutsSettleTheSpellings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, sp := range []string{"Beasts", "Animals, The", "Animals, The"} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/an/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Animals", albumSort: sp, deferRollups: true})
	}
	if sp, _ := entitySpellingOf(t, st, "album", "Animals"); sp != "Beasts" {
		t.Errorf("before the settle, spelling = %q, want the first member's, taken as the put created the album", sp)
	}
	if _, err := st.SettleRollups(ctx); err != nil {
		t.Fatal(err)
	}
	if sp, k := entitySpellingOf(t, st, "album", "Animals"); sp != "Animals, The" || k != model.SortKey("Animals, The") {
		t.Errorf("after the settle, spelling/key = %q/%q, want the majority's", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestVerifyChecksTheDerivedSpellings: a derived spelling knocked off its members is drift,
// which RefreshRollups repairs and counts.
func TestVerifyChecksTheDerivedSpellings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "T",
		artist: "Elvis Costello", artistSort: "Costello, Elvis", album: "Armed Forces", albumSort: "Forces, Armed"})
	for _, q := range []string{"UPDATE album SET title_sort = 'stale'", "UPDATE artist SET name_sort = ''"} {
		if _, err := st.wdb().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.SortSpellingDrift != 2 || rep.Consistent() {
		t.Fatalf("verify = %+v (err %v), want two spellings drifted and inconsistent", rep, err)
	}
	fix, err := st.RefreshRollups(ctx)
	if err != nil || fix.SortSpellings != 2 {
		t.Fatalf("RefreshRollups = %+v (err %v), want two spellings rewritten", fix, err)
	}
	assertVerifyClean(t, st)
	if again, err := st.RefreshRollups(ctx); err != nil || again.SortSpellings != 0 {
		t.Errorf("second RefreshRollups = %+v (err %v), want nothing to rewrite", again, err)
	}
}

// TestAJointAuthorCreditSpellsNoAuthor: the author sort of a book two authors wrote spells
// the pair, so neither author takes it.
func TestAJointAuthorCreditSpellsNoAuthor(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	in := bookSpecInput(lib.ID, bookSpec{path: "/lib/a.m4b", essence: "ea", content: "ca", title: "Good Omens",
		author: "Neil Gaiman", authorSort: "Gaiman, Neil & Pratchett, Terry"})
	in.Book.Authors = []string{"Neil Gaiman", "Terry Pratchett"}
	if _, err := st.PutScannedBook(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Neil Gaiman", "Terry Pratchett"} {
		if sp, _ := entitySpellingOf(t, st, "artist", name); sp != "" {
			t.Errorf("%s spelling = %q, want none from a joint credit", name, sp)
		}
	}
	assertVerifyClean(t, st)
}

// TestSpellingWritersOutsideAPutRefreshTheArtist: following an ARTISTSORT write-back and a
// credit edit that drops a spelling both leave the artist's derived spelling where its
// members now state it.
func TestSpellingWritersOutsideAPutRefreshTheArtist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Alison",
		artist: "Elvis Costello", album: "My Aim Is True"})
	// A second track keeps the artist standing once the first one's credit moves away.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Watching",
		artist: "Elvis Costello", album: "This Year's Model"})
	artist := entityPIDByName(t, st, "artist", "name", "Elvis Costello")
	if err := st.FollowArtistSortWrite(ctx, artist, []model.PID{tr.FilePID}, "Costello, Elvis"); err != nil {
		t.Fatal(err)
	}
	if sp, k := entitySpellingOf(t, st, "artist", "Elvis Costello"); sp != "Costello, Elvis" || k != model.SortKey("Costello, Elvis") {
		t.Errorf("after the write-back follow, spelling/key = %q/%q", sp, k)
	}
	assertVerifyClean(t, st)
	if _, _, err := st.SetItemCredits(ctx, tr.ItemPID, model.RoleArtist, []string{"Declan MacManus"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false, false); err != nil {
		t.Fatal(err)
	}
	if sp, k := entitySpellingOf(t, st, "artist", "Elvis Costello"); sp != "" || k != model.SortKey("Elvis Costello") {
		t.Errorf("after the stating credit moved away, spelling/key = %q/%q, want none", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestABookLeavingItsAuthorRefreshesTheAuthorsSpelling: a book that stops being a live
// member of its author, trashed or retagged to another author, takes its author sort with
// it, as a track does its artist's.
func TestABookLeavingItsAuthorRefreshesTheAuthorsSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	trashed := putBook(t, st, lib.ID, bookSpec{path: "/lib/a.m4b", essence: "ea", content: "ca", title: "Earthsea",
		author: "Ursula K. Le Guin", authorSort: "Le Guin, Ursula K."})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb", title: "Lathe",
		author: "Ursula K. Le Guin"})
	trashItemFile(t, st, trashed)
	if sp, _ := entitySpellingOf(t, st, "artist", "Ursula K. Le Guin"); sp != "" {
		t.Errorf("after the trash, author spelling = %q, want none", sp)
	}
	assertVerifyClean(t, st)

	retag := bookSpec{path: "/lib/c.m4b", essence: "ec", content: "cc", title: "Dune",
		author: "Frank Herbert", authorSort: "Herbert, Frank"}
	putBook(t, st, lib.ID, retag)
	putBook(t, st, lib.ID, bookSpec{path: "/lib/d.m4b", essence: "ed", content: "cd", title: "Other", author: "Frank Herbert"})
	retag.author, retag.authorSort, retag.content = "Brian Herbert", "", "cc2"
	putBook(t, st, lib.ID, retag)
	if sp, _ := entitySpellingOf(t, st, "artist", "Frank Herbert"); sp != "" {
		t.Errorf("after the retag, the old author's spelling = %q, want none", sp)
	}
	assertVerifyClean(t, st)
}

// TestNameLookupsReadTheNameNotTheSpelling: a spelling keys an entity's order and nothing
// else, so the duplicate-artist audit still pairs "The Beatles" with "Beatles", and search
// still finds an artist, an album and a title by their names when each states a spelling.
func TestNameLookupsReadTheNameNotTheSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Help",
		artist: "The Beatles", artistSort: "Beatles, The", album: "Help!"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Yesterday",
		artist: "Beatles", album: "Help!"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "Perfect",
		artist: "Ed Sheeran", albumArt: "Ed Sheeran", album: "÷", albumSort: "Divide",
		mbReleaseGroup: "e0000000-0000-4000-8000-000000000001", mbRelease: "e0000000-0000-4000-8000-000000000002"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/d.flac", essence: "ed", content: "cd", title: "!!!",
		titleSort: "Chk Chk Chk", artist: "Band", album: "Bangs"})
	for i := 0; i < 3; i++ {
		n := string(rune('e' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "Beatles Cover " + n, artist: "Singer " + n, album: "Covers " + n})
	}

	sets, err := st.DuplicateArtists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paired := false
	for _, s := range sets {
		var names []string
		for _, m := range s.Members {
			names = append(names, m.Name)
		}
		slices.Sort(names)
		paired = paired || slices.Equal(names, []string{"Beatles", "The Beatles"})
	}
	if !paired {
		t.Errorf("duplicate artists = %+v, want The Beatles paired with Beatles", sets)
	}

	res, err := st.Search(ctx, "beatles", read.SearchOptions{MaxCandidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Artists, func(h read.SearchHit) bool { return h.Title == "The Beatles" }) {
		t.Errorf("search beatles past the cap = %+v, want The Beatles by its name", res.Artists)
	}
	if res, err := st.Search(ctx, "÷", read.SearchOptions{}); err != nil || len(res.Albums) != 1 || res.Albums[0].Title != "÷" {
		t.Errorf("search ÷ = %+v (err %v), want the album by its title", res, err)
	}
	if res, err := st.Search(ctx, "!!!", read.SearchOptions{}); err != nil || len(res.Tracks) != 1 || res.Tracks[0].Title != "!!!" {
		t.Errorf("search !!! = %+v (err %v), want the track by its title", res, err)
	}
}

// TestAMergedInAlbumArtistCreditSpellsTheSurvivor: an album-artist credit naming one
// artist spells that artist after a merge folds it into another, as an artist credit does.
func TestAMergedInAlbumArtistCreditSpellsTheSurvivor(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Help",
		artist: "The Beatles", album: "Help!"})
	spec := trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Something",
		artist: "George Harrison", albumArt: "Beatles", albumArtistSort: "Beatles, The", album: "Abbey Road"}
	putTrack(t, st, lib.ID, spec)
	survivor := entityPIDByName(t, st, "artist", "name", "The Beatles")
	loser := entityPIDByName(t, st, "artist", "name", "Beatles")
	if _, err := st.MergeEntities(ctx, model.MergeArtist, survivor, []model.PID{loser}); err != nil {
		t.Fatal(err)
	}
	spec.content = "cb2"
	putTrack(t, st, lib.ID, spec)
	if sp, _ := entitySpellingOf(t, st, "artist", "The Beatles"); sp != "Beatles, The" {
		t.Errorf("survivor spelling = %q, want the folded credit's", sp)
	}
	assertVerifyClean(t, st)
}

// TestFollowingAnAlbumSortWriteKeepsALockedSpelling: a member whose album_sort is locked
// keeps it when the album's fanned ALBUMSORT is followed, and its file, which now states
// the album's, owes the member's.
func TestFollowingAnAlbumSortWriteKeepsALockedSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := putTrack(t, st, lib.ID, trackSpec{path: "/lib/w/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Wall"})
	two := putTrack(t, st, lib.ID, trackSpec{path: "/lib/w/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Wall"})
	if err := st.EditItemField(ctx, two.ItemPID, "album_sort", "Mine", model.Attribution{Source: model.SourceUser},
		model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	// As though the locked spelling had been written back: the file states it and owes nothing.
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM file_diagnostic WHERE code = ?", string(model.DiagTagWriteOwed)); err != nil {
		t.Fatal(err)
	}
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Wall'"))
	if err := st.FollowAlbumSortWrite(ctx, album, []model.PID{one.FilePID, two.FilePID}, "Zz Wall"); err != nil {
		t.Fatal(err)
	}
	spelling := func(pid model.PID) string {
		return scalarStr(t, st, "SELECT album_sort FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?", string(pid))
	}
	if got := spelling(one.ItemPID); got != "Zz Wall" {
		t.Errorf("unlocked member album_sort = %q, want the followed spelling", got)
	}
	if got := spelling(two.ItemPID); got != "Mine" {
		t.Errorf("locked member album_sort = %q, want it kept", got)
	}
	if keys, err := owedKeysOf(ctx, st, two.FilePID); err != nil || !slices.Equal(keys, []string{"album_sort"}) {
		t.Errorf("locked member's file owes %v (err %v), want album_sort", keys, err)
	}
	if keys, err := owedKeysOf(ctx, st, one.FilePID); err != nil || keys != nil {
		t.Errorf("followed member's file owes %v (err %v), want nothing", keys, err)
	}
	assertVerifyClean(t, st)
}

// TestRefreshReportSaysWhichSpellingsMovedAKey: a spelling repair behind a curated sort
// moves no key, so it leaves open page cursors valid, and the report tells the two apart.
func TestRefreshReportSaysWhichSpellingsMovedAKey(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "T",
		artist: "Elvis Costello", artistSort: "Costello, Elvis", album: "Armed Forces", albumSort: "Forces, Armed"})
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Armed Forces'"))
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, album, map[string]string{"sort": "Zzz"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET title_sort = 'stale'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE artist SET name_sort = '', sort_key = ?", model.SortKey("Elvis Costello")); err != nil {
		t.Fatal(err)
	}
	rep, err := st.RefreshRollups(ctx)
	if err != nil || rep.SortSpellings != 2 || rep.SortSpellingKeys != 1 {
		t.Fatalf("RefreshRollups = %+v (err %v), want two spellings rewritten and one key moved", rep, err)
	}
	assertVerifyClean(t, st)
}

// TestASpellingCountsEachMemberOnce: a track the artist both performs and heads as album
// artist is one member, so stating a spelling in both tags gives it one vote, not two.
func TestASpellingCountsEachMemberOnce(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, m := range []struct{ albumArt, artistSort, albumArtistSort string }{
		{"Other", "Costello, Elvis", ""}, {"Other", "Costello, Elvis", ""}, {"Other", "Costello, Elvis", ""},
		{"Elvis Costello", "Elvis Costello", "Elvis Costello"}, {"Elvis Costello", "Elvis Costello", "Elvis Costello"},
	} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n, title: "T" + n,
			artist: "Elvis Costello", albumArt: m.albumArt, album: "Al" + n, artistSort: m.artistSort, albumArtistSort: m.albumArtistSort})
	}
	if sp, _ := entitySpellingOf(t, st, "artist", "Elvis Costello"); sp != "Costello, Elvis" {
		t.Errorf("spelling = %q, want the three members' over the two that state theirs twice", sp)
	}
	assertVerifyClean(t, st)
}

// TestANewEntityTakesItsMembersSpellingAtOnce: a deferred put keys the entities it creates
// from the members they have, by the rule the settle applies, so a stated artist list
// naming one artist twice spells that artist as a list naming it once does, before any
// settle.
func TestANewEntityTakesItsMembersSpellingAtOnce(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "T",
		artist: "Queen; QUEEN", artists: []string{"Queen", "QUEEN"}, albumArt: "Other", album: "Jazz",
		albumSort: "Jazz (album)", artistSort: "Queen (band)", deferRollups: true})
	if sp, k := entitySpellingOf(t, st, "artist", "Queen"); sp != "Queen (band)" || k != model.SortKey("Queen (band)") {
		t.Errorf("before the settle, Queen spelling/key = %q/%q, want the member's", sp, k)
	}
	if sp, _ := entitySpellingOf(t, st, "album", "Jazz"); sp != "Jazz (album)" {
		t.Errorf("before the settle, album spelling = %q, want the member's", sp)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE entity_type = 'artist' AND op = 'update'"); n != 0 {
		t.Errorf("artist update deltas = %d, want the create alone", n)
	}
}
