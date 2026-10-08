package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// sortCols reads one item's spelling and key columns from its subtype row.
func sortCols(t *testing.T, st *Store, pid model.PID, table, spelling, key string) (string, string) {
	t.Helper()
	var sp, k string
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT "+spelling+", "+key+" FROM "+table+" x JOIN playable_item pi ON pi.id = x.item_id WHERE pi.pid = ?",
		string(pid)).Scan(&sp, &k); err != nil {
		t.Fatalf("read %s.%s: %v", table, spelling, err)
	}
	return sp, k
}

// TestSortSpellingsKeepTheirCase: the sort spelling a file states is what the item view
// shows, case and all, and the key beside it is its folded form; with no spelling stated
// the view shows none and the key folds what the item sorts by instead (the artist, else
// the album artist; the composer; the author).
func TestSortSpellingsKeepTheirCase(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	stated := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Prelude",
		artist: "The Beatles", artistSort: "Beatles, The", album: "A",
		composer: "J. S. Bach", composerSort: "Bach, Johann Sebastian",
	}).ItemPID
	derived := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "Largo",
		artist: "Antonín Dvořák", album: "A", composer: "Antonín Dvořák",
	}).ItemPID
	albumArtistOnly := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/3.flac", essence: "e3", content: "c3", title: "Third",
		albumArt: "The Album Artist", album: "A",
	}).ItemPID
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "The Dispossessed",
		author: "Ursula K. Le Guin", authorSort: "Le Guin, Ursula K.",
	}).ItemPID
	plainBook := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/2.m4b", essence: "be2", content: "bc2", title: "Kindred", author: "Octavia E. Butler",
	}).ItemPID

	v, err := st.ItemByPID(ctx, stated)
	if err != nil {
		t.Fatal(err)
	}
	if v.ComposerSort != "Bach, Johann Sebastian" {
		t.Errorf("view ComposerSort = %q, want the file's spelling", v.ComposerSort)
	}
	if sp, k := sortCols(t, st, stated, "track", "artist_sort", "artist_sort_key"); sp != "Beatles, The" || k != "beatles, the" {
		t.Errorf("artist sort = (%q, %q), want the spelling and its key", sp, k)
	}
	if sp, k := sortCols(t, st, stated, "track", "composer_sort", "composer_sort_key"); sp != "Bach, Johann Sebastian" || k != "bach, johann sebastian" {
		t.Errorf("composer sort = (%q, %q), want the spelling and its key", sp, k)
	}

	if v, err := st.ItemByPID(ctx, derived); err != nil || v.ComposerSort != "" {
		t.Errorf("view ComposerSort with no spelling = %q (err %v), want none", v.ComposerSort, err)
	}
	if sp, k := sortCols(t, st, derived, "track", "composer_sort", "composer_sort_key"); sp != "" || k != model.SortKey("Antonín Dvořák") {
		t.Errorf("derived composer sort = (%q, %q), want no spelling and the composer's key", sp, k)
	}
	if sp, k := sortCols(t, st, derived, "track", "artist_sort", "artist_sort_key"); sp != "" || k != model.SortKey("Antonín Dvořák") {
		t.Errorf("derived artist sort = (%q, %q), want no spelling and the artist's key", sp, k)
	}
	if _, k := sortCols(t, st, albumArtistOnly, "track", "artist_sort", "artist_sort_key"); k != model.SortKey("The Album Artist") {
		t.Errorf("artist sort key of a track with only an album artist = %q, want the album artist's", k)
	}

	if v, err := st.ItemByPID(ctx, book); err != nil || v.AuthorSort != "Le Guin, Ursula K." {
		t.Errorf("view AuthorSort = %q (err %v), want the file's spelling", v.AuthorSort, err)
	}
	if sp, k := sortCols(t, st, book, "book", "author_sort", "author_sort_key"); sp != "Le Guin, Ursula K." || k != "le guin, ursula k." {
		t.Errorf("author sort = (%q, %q), want the spelling and its key", sp, k)
	}
	if sp, k := sortCols(t, st, plainBook, "book", "author_sort", "author_sort_key"); sp != "" || k != model.SortKey("Octavia E. Butler") {
		t.Errorf("derived author sort = (%q, %q), want no spelling and the author's key", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestSortFieldsOrderByTheirKeys: composer_sort and author_sort order by the folded key,
// which a byte order of the spellings would not give: an uppercase spelling would sort
// ahead of every lowercase one, and an unstated one ahead of them all.
func TestSortFieldsOrderByTheirKeys(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, c := range []struct{ title, composer, spelling string }{
		{"Albinoni", "Tomaso Albinoni", ""},
		{"Bach", "J. S. Bach", "bach, Johann"},
		{"Zappa", "Frank Zappa", "Zappa, Frank"},
	} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/c/" + c.title + ".flac", essence: "e" + c.title, content: "c" + c.title,
			title: c.title, artist: "A", album: "A", composer: c.composer, composerSort: c.spelling, trackNo: i + 1,
		})
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityTracks).OrderBy("composer_sort", false).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Title)
	}
	if len(got) != 3 || got[0] != "Bach" || got[1] != "Albinoni" || got[2] != "Zappa" {
		t.Errorf("composer_sort order = %v, want Bach (bach), Albinoni (tomaso), Zappa (zappa)", got)
	}

	for _, b := range []struct{ title, author, spelling string }{
		{"Kindred", "Octavia E. Butler", ""},
		{"Earthsea", "Ursula K. Le Guin", "le Guin, Ursula"},
		{"Dune", "Frank Herbert", "Herbert, Frank"},
	} {
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/b/" + b.title + ".m4b", essence: "b" + b.title, content: "b" + b.title,
			title: b.title, author: b.author, authorSort: b.spelling,
		})
	}
	books, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").
		OrderBy("author_sort", false).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for _, it := range books {
		got = append(got, it.Title)
	}
	if len(got) != 3 || got[0] != "Dune" || got[1] != "Earthsea" || got[2] != "Kindred" {
		t.Errorf("author_sort order = %v, want Dune (herbert), Earthsea (le guin), Kindred (octavia)", got)
	}
}

// TestSortSpellingEditsRefoldTheKey: an explicit sort edit keeps the literal and folds
// the key from it; clearing it folds the key from the name again; a name edit drops a
// spelling it no longer describes unless the spelling is locked; and a credit edit does
// the same.
func TestSortSpellingEditsRefoldTheKey(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "The Beatles", artistSort: "Beatles, The", album: "A",
		composer: "Ludwig van Beethoven", composerSort: "Beethoven, Ludwig van",
	}).ItemPID
	if err := st.EditItemField(ctx, tr, "composer_sort", "van Beethoven, Ludwig", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "van Beethoven, Ludwig" || k != "van beethoven, ludwig" {
		t.Errorf("edited composer sort = (%q, %q), want the literal and its key", sp, k)
	}
	if v, err := st.ItemByPID(ctx, tr); err != nil || v.ComposerSort != "van Beethoven, Ludwig" {
		t.Errorf("view ComposerSort = %q (err %v), want the literal", v.ComposerSort, err)
	}
	// A composer edit leaves a locked spelling, and its key, alone.
	if err := st.EditItemField(ctx, tr, "composer", "L. v. Beethoven", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "van Beethoven, Ludwig" || k != "van beethoven, ludwig" {
		t.Errorf("locked composer sort after a composer edit = (%q, %q), want it kept", sp, k)
	}
	// Unlocked, a composer edit drops the spelling and the key follows the composer.
	if err := st.UnlockField(ctx, tr, "composer_sort"); err != nil {
		t.Fatal(err)
	}
	if err := st.EditItemField(ctx, tr, "composer", "Franz Schubert", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "" || k != model.SortKey("Franz Schubert") {
		t.Errorf("composer sort after an unlocked composer edit = (%q, %q), want no spelling and the composer's key", sp, k)
	}
	// Setting and then clearing a spelling: the key follows each.
	if err := st.EditItemField(ctx, tr, "composer_sort", "Schubert, Franz", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if _, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); k != "schubert, franz" {
		t.Errorf("composer sort key after a spelling edit = %q", k)
	}
	if err := st.EditItemField(ctx, tr, "composer_sort", "", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "" || k != model.SortKey("Franz Schubert") {
		t.Errorf("cleared composer sort = (%q, %q), want no spelling and the composer's key", sp, k)
	}
	// An artist edit drops the stated artist spelling.
	if err := st.EditItemField(ctx, tr, "artist", "The Rolling Stones", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "artist_sort", "artist_sort_key"); sp != "" || k != model.SortKey("The Rolling Stones") {
		t.Errorf("artist sort after an artist edit = (%q, %q), want no spelling and the artist's key", sp, k)
	}
	// A composer credit edit drops an unlocked spelling as the plain edit does.
	if err := st.EditItemField(ctx, tr, "composer_sort", "Schubert, F.", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetItemCredits(ctx, tr, model.RoleComposer, []string{"Clara Schumann"}, user, model.LockOf(false), false, false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "" || k != model.SortKey("Clara Schumann") {
		t.Errorf("composer sort after a credit edit = (%q, %q), want no spelling and the credit's key", sp, k)
	}
	if _, _, err := st.SetItemCredits(ctx, tr, model.RoleArtist, []string{"The Who"}, user, model.LockOf(false), false, false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, tr, "track", "artist_sort", "artist_sort_key"); sp != "" || k != model.SortKey("The Who") {
		t.Errorf("artist sort after a credit edit = (%q, %q), want no spelling and the credit's key", sp, k)
	}

	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "Dune",
		author: "Frank Herbert", authorSort: "Herbert, Frank",
	}).ItemPID
	if err := st.EditItemField(ctx, bk, "author_sort", "HERBERT, Frank", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, bk, "book", "author_sort", "author_sort_key"); sp != "HERBERT, Frank" || k != "herbert, frank" {
		t.Errorf("edited author sort = (%q, %q), want the literal and its key", sp, k)
	}
	if err := st.EditItemField(ctx, bk, "author", "Brian Herbert", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, bk, "book", "author_sort", "author_sort_key"); sp != "HERBERT, Frank" || k != "herbert, frank" {
		t.Errorf("locked author sort after an author edit = (%q, %q), want it kept", sp, k)
	}
	if err := st.UnlockField(ctx, bk, "author_sort"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetItemCredits(ctx, bk, model.RoleAuthor, []string{"Kevin J. Anderson"}, user, model.LockOf(false), false, false); err != nil {
		t.Fatal(err)
	}
	if sp, k := sortCols(t, st, bk, "book", "author_sort", "author_sort_key"); sp != "" || k != model.SortKey("Kevin J. Anderson") {
		t.Errorf("author sort after a credit edit = (%q, %q), want no spelling and the credit's key", sp, k)
	}
	assertVerifyClean(t, st)
}

// TestASortSpellingOutlivesANameSetToItself: an edit or a credit edit that sets a name to
// the value it already holds keeps the sort spelling beside it, since the name it spells
// did not change.
func TestASortSpellingOutlivesANameSetToItself(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "The Beatles", artistSort: "Beatles, The", album: "A",
		composer: "Ludwig van Beethoven", composerSort: "Beethoven, Ludwig van",
	}).ItemPID
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "The Dispossessed",
		author: "Ursula K. Le Guin", authorSort: "Le Guin, Ursula K.",
	}).ItemPID
	check := func(when string) {
		t.Helper()
		for _, c := range []struct {
			pid              model.PID
			table, col, want string
		}{
			{tr, "track", "artist_sort", "Beatles, The"},
			{tr, "track", "composer_sort", "Beethoven, Ludwig van"},
			{book, "book", "author_sort", "Le Guin, Ursula K."},
		} {
			if sp, _ := sortCols(t, st, c.pid, c.table, c.col, c.col+"_key"); sp != c.want {
				t.Errorf("%s after %s = %q, want %q kept", c.col, when, sp, c.want)
			}
		}
	}
	if err := st.EditItemFields(ctx, tr, map[string]string{"artist": "The Beatles", "composer": "Ludwig van Beethoven"},
		user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if err := st.EditItemField(ctx, book, "author", "Ursula K. Le Guin", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	check("an edit to the same names")
	for _, c := range []struct {
		pid   model.PID
		role  model.ContributorRole
		names []string
	}{
		{tr, model.RoleArtist, []string{"The Beatles"}},
		{tr, model.RoleComposer, []string{"Ludwig van Beethoven"}},
		{book, model.RoleAuthor, []string{"Ursula K. Le Guin"}},
	} {
		if _, _, err := st.SetItemCredits(ctx, c.pid, c.role, c.names, user, model.LockOf(false), false, false); err != nil {
			t.Fatal(err)
		}
	}
	check("a credit edit to the same names")
	assertVerifyClean(t, st)
}

// TestCreditsSavedAgainKeepTheirDisplayAndSpelling: a credit edit naming the people the
// role already names, the artists it credits in order or, with none, those its display
// spells, leaves the display and its sort spelling as the file gave them ("Simon &
// Garfunkel", "Lennon/McCartney"); one naming others rewrites both.
func TestCreditsSavedAgainKeepTheirDisplayAndSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", album: "A",
		artist: "Simon & Garfunkel", artists: []string{"Simon", "Garfunkel"}, artistSort: "Simon and Garfunkel",
		composer: "Lennon/McCartney", composerSort: "Lennon and McCartney",
	}).ItemPID
	set := func(role model.ContributorRole, names ...string) {
		t.Helper()
		if _, _, err := st.SetItemCredits(ctx, tr, role, names, user, model.LockOf(false), false, false); err != nil {
			t.Fatal(err)
		}
	}
	view := func() *model.ItemView {
		t.Helper()
		v, err := st.ItemByPID(ctx, tr)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	set(model.RoleArtist, "Simon", "Garfunkel")
	set(model.RoleComposer, "Lennon", "McCartney")
	if v := view(); v.Artist != "Simon & Garfunkel" || v.ArtistSort != "Simon and Garfunkel" ||
		v.Composer != "Lennon/McCartney" || v.ComposerSort != "Lennon and McCartney" {
		t.Errorf("after saving the same names: artist (%q, %q), composer (%q, %q), want both as the file gave them",
			v.Artist, v.ArtistSort, v.Composer, v.ComposerSort)
	}
	set(model.RoleArtist, "Simon")
	if v := view(); v.Artist != "Simon" || v.ArtistSort != "" {
		t.Errorf("after crediting another set: artist (%q, %q), want the new display and no spelling", v.Artist, v.ArtistSort)
	}
	assertVerifyClean(t, st)
}
