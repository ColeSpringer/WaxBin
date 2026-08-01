package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// drainBrowse pages a discovery list to exhaustion, returning the item titles in
// order and asserting no row is duplicated or skipped across page boundaries.
func drainBrowse(t *testing.T, st *Store, list read.DiscoveryList, opt read.BrowseOptions, pageSize int) []string {
	t.Helper()
	ctx := context.Background()
	seen := map[model.PID]bool{}
	var order []string
	opt.Limit = pageSize
	opt.Cursor = ""
	for i := 0; ; i++ {
		page, err := st.BrowsePage(ctx, list, opt)
		if err != nil {
			t.Fatalf("browse %s: %v", list, err)
		}
		for _, it := range page.Items {
			if seen[it.PID] {
				t.Fatalf("browse %s returned %s on more than one page", list, it.PID)
			}
			seen[it.PID] = true
			order = append(order, it.Title)
		}
		if !page.HasMore {
			break
		}
		opt.Cursor = page.Next
		if i > 100 {
			t.Fatalf("browse %s did not terminate", list)
		}
	}
	return order
}

func TestBrowseAlphabetical(t *testing.T) {
	st, lib := entityFixture(t)
	for _, title := range []string{"Echo", "Alpha", "Delta", "Bravo", "Charlie"} {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
	}
	order := drainBrowse(t, st, read.ListAlphabetical, read.BrowseOptions{}, 2)
	want := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("alphabetical order = %v, want %v", order, want)
	}
}

func TestBrowseNewest(t *testing.T) {
	st, lib := entityFixture(t)
	specs := []struct {
		title string
		year  int
	}{{"Old", 1990}, {"Mid", 2005}, {"New", 2020}, {"Undated", 0}}
	for _, s := range specs {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + s.title + ".flac", essence: "e" + s.title, content: "c" + s.title, title: s.title, artist: "X", album: "Al", year: s.year})
	}
	order := drainBrowse(t, st, read.ListNewest, read.BrowseOptions{}, 2)
	// Newest release first; the undated track (year 0) sorts last.
	want := []string{"New", "Mid", "Old", "Undated"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("newest order = %v, want %v", order, want)
	}
}

func TestBrowseRandomStableSeed(t *testing.T) {
	st, lib := entityFixture(t)
	for _, c := range "ABCDEFGHIJ" {
		s := string(c)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + s + ".flac", essence: "e" + s, content: "c" + s, title: s, artist: "X", album: "Al"})
	}

	first := drainBrowse(t, st, read.ListRandom, read.BrowseOptions{Seed: 42}, 3)
	if len(first) != 10 {
		t.Fatalf("random seed 42 covered %d items, want 10", len(first))
	}
	// The same seed must reproduce the same paginated order exactly. WaxBin has no
	// server-side session, so the seed is what keeps a shuffle stable across pages.
	again := drainBrowse(t, st, read.ListRandom, read.BrowseOptions{Seed: 42}, 4)
	if strings.Join(first, ",") != strings.Join(again, ",") {
		t.Errorf("seed 42 was not stable:\n  %v\n  %v", first, again)
	}
	// A different seed should (with 10 items) produce a different order.
	other := drainBrowse(t, st, read.ListRandom, read.BrowseOptions{Seed: 7}, 3)
	if strings.Join(first, ",") == strings.Join(other, ",") {
		t.Errorf("seed 7 produced the same order as seed 42 (%v); shuffle is not seed-sensitive", first)
	}
	// And a shuffle must not be the plain collation order.
	if strings.Join(first, ",") == "A,B,C,D,E,F,G,H,I,J" {
		t.Errorf("seed 42 returned collation order, not a shuffle: %v", first)
	}
}

func TestBrowseMostPlayedAndStarred(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	for _, title := range []string{"One", "Two", "Three"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		ids[title] = res.ItemPID
	}
	// Two plays for Two, three for Three, none for One.
	for i := 0; i < 2; i++ {
		if err := st.MarkPlayed(ctx, "", ids["Two"], false); err != nil {
			t.Fatalf("mark played: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := st.MarkPlayed(ctx, "", ids["Three"], false); err != nil {
			t.Fatalf("mark played: %v", err)
		}
	}
	order := drainBrowse(t, st, read.ListMostPlayed, read.BrowseOptions{}, 2)
	want := []string{"Three", "Two"} // One never played, excluded
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("most-played order = %v, want %v", order, want)
	}

	if _, err := st.SetStar(ctx, "", ids["One"], true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	starred := drainBrowse(t, st, read.ListStarred, read.BrowseOptions{}, 5)
	if strings.Join(starred, ",") != "One" {
		t.Errorf("starred = %v, want [One]", starred)
	}
}

func TestBrowseByYearAndByGenre(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al", genre: "Rock", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Y", album: "Bl", genre: "Jazz", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "Z", album: "Cl", genre: "Rock", year: 1990})

	byYear := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{Year: 2000}, 5)
	if strings.Join(byYear, ",") != "A,B" {
		t.Errorf("by-year 2000 = %v, want [A B]", byYear)
	}
	if _, err := st.BrowsePage(ctx, read.ListByYear, read.BrowseOptions{}); err == nil {
		t.Error("by-year without a year should error")
	}

	// Resolve the Rock genre pid via the facet, then browse it.
	facet, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupGenre, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	var rockPID model.PID
	for _, b := range facet.Buckets {
		if b.Display == "Rock" {
			rockPID = b.EntityPID
		}
	}
	if rockPID == "" {
		t.Fatal("no Rock genre pid")
	}
	byGenre := drainBrowse(t, st, read.ListByGenre, read.BrowseOptions{GenrePID: rockPID}, 5)
	if strings.Join(byGenre, ",") != "A,C" {
		t.Errorf("by-genre Rock = %v, want [A C]", byGenre)
	}
}

// TestBrowseZeroQueryUnchanged pins the compatibility half of the browse filter: a
// zero Query leaves every list byte-for-byte what it was, and in particular still
// ignores UserPID on a list that does not read play_state. Only a filtered browse
// validates that pid, so an unfiltered one cannot start erroring on a value it never
// looked at.
func TestBrowseZeroQueryUnchanged(t *testing.T) {
	st, lib := entityFixture(t)
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
	}
	plain := drainBrowse(t, st, read.ListAlphabetical, read.BrowseOptions{}, 2)
	if strings.Join(plain, ",") != "Alpha,Bravo,Charlie" {
		t.Fatalf("unfiltered alphabetical = %v", plain)
	}
	withBogusUser := drainBrowse(t, st, read.ListAlphabetical, read.BrowseOptions{UserPID: "no-such-user"}, 2)
	if strings.Join(withBogusUser, ",") != strings.Join(plain, ",") {
		t.Errorf("an unfiltered browse stopped ignoring UserPID: %v vs %v", withBogusUser, plain)
	}
}

// TestBrowseQueryWithoutEntityErrors pins the fail-closed entity rule: a Where with
// no Entity is rejected rather than defaulting to items, because the entity is what
// selects the field whitelist. An unknown field is CodeInvalid too, inherited from
// query.Compile rather than reimplemented here.
func TestBrowseQueryWithoutEntityErrors(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"})

	noEntity := query.Query{Where: query.Cond{Field: "title", Op: query.OpIs, Value: "A"}}
	if _, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Query: noEntity}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("query with no entity = %v, want CodeInvalid", err)
	}
	unknownField := query.New(query.EntityItems).Where("no_such_field", query.OpIs, "x").Build()
	if _, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Query: unknownField}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("query with an unknown field = %v, want CodeInvalid", err)
	}

	// Sorts alone are not a filter. This path ignores them whether or not an entity is
	// named, so a rule doc carrying only sorts browses unfiltered rather than tripping
	// the entity check over a field it never set.
	sortsOnly := query.Query{Sorts: []query.Sort{{Field: "year", Desc: true}}}
	page, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Query: sortsOnly})
	if err != nil {
		t.Fatalf("sorts-only query: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("sorts-only browse returned %d items, want the whole list", len(page.Items))
	}
}

// TestBrowseQueryTracksEntityExcludesBooks pins entityPredicate on the browse path.
// Without it an EntityTracks query passes the field-map lookup but never narrows to
// pi.kind='track', so a browse would return books and episodes carrying NULL track
// columns.
func TestBrowseQueryTracksEntityExcludesBooks(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A Track", artist: "X", album: "Al"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb", title: "B Book", author: "Auth", asin: "BX"})

	all := drainBrowse(t, st, read.ListAlphabetical, read.BrowseOptions{}, 5)
	if len(all) != 2 {
		t.Fatalf("unfiltered browse = %v, want the track and the book", all)
	}
	tracksOnly := drainBrowse(t, st, read.ListAlphabetical,
		read.BrowseOptions{Query: query.New(query.EntityTracks).Build()}, 5)
	if strings.Join(tracksOnly, ",") != "A Track" {
		t.Errorf("tracks-entity browse = %v, want just the track", tracksOnly)
	}
}

// TestBrowseRandomScopedIsSubsequence pins that filtering a seeded shuffle keeps the
// shuffle. wb_shuffle depends on nothing outside the row, so a scoped random browse
// must be exactly the unfiltered sequence for the same seed with the out-of-scope
// rows removed, not a different permutation of the scope.
func TestBrowseRandomScopedIsSubsequence(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	inScope := map[string]bool{}
	for i, title := range []string{"One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight"} {
		genre := "Jazz"
		if i%2 == 0 {
			genre = "Rock"
			inScope[title] = true
		}
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al", genre: genre})
	}
	facet, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupGenre, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	var rockPID model.PID
	for _, b := range facet.Buckets {
		if b.Display == "Rock" {
			rockPID = b.EntityPID
		}
	}
	if rockPID == "" {
		t.Fatal("no Rock genre pid")
	}

	const seed = 42
	full := drainBrowse(t, st, read.ListRandom, read.BrowseOptions{Seed: seed}, 3)
	scoped := drainBrowse(t, st, read.ListRandom, read.BrowseOptions{
		Seed:  seed,
		Query: query.New(query.EntityItems).Where("genre_pid", query.OpIs, string(rockPID)).Build(),
	}, 3)

	var want []string
	for _, title := range full {
		if inScope[title] {
			want = append(want, title)
		}
	}
	if len(full) != 8 || len(want) != 4 {
		t.Fatalf("fixture drained %d of 8 items and %d of 4 in scope; the comparison would be vacuous",
			len(full), len(want))
	}
	if strings.Join(scoped, ",") != strings.Join(want, ",") {
		t.Errorf("scoped shuffle = %v, want the induced subsequence %v of the seed-%d shuffle %v",
			scoped, want, seed, full)
	}
}

// TestBrowseQueryUserFieldBindsUserJoin pins which user's play_state a per-user
// filter reads on a per-user list. Both joins bind the same user, so swapping the
// bind order returns observably wrong rows rather than an error: A and B have
// different stars and different play counts here precisely so a mismatch shows up as
// the wrong titles.
func TestBrowseQueryUserFieldBindsUserJoin(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	for _, title := range []string{"One", "Two"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		ids[title] = res.ItemPID
	}
	bob, err := st.CreateUser(ctx, "Bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// The default user played One and starred Two; Bob played both and starred One.
	if err := st.MarkPlayed(ctx, "", ids["One"], false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, "", ids["Two"], true, nil); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"One", "Two"} {
		if err := st.MarkPlayed(ctx, bob.PID, ids[title], false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SetStar(ctx, bob.PID, ids["One"], true, nil); err != nil {
		t.Fatal(err)
	}

	starredQ := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()
	got := drainBrowse(t, st, read.ListMostPlayed,
		read.BrowseOptions{UserPID: bob.PID, Query: starredQ}, 5)
	if strings.Join(got, ",") != "One" {
		t.Errorf("bob's most-played + starred = %v, want [One] (the default user's star is on Two)", got)
	}
	// The default user has played only One, and starred only Two, so the same query
	// for the default user matches nothing.
	if def := drainBrowse(t, st, read.ListMostPlayed, read.BrowseOptions{Query: starredQ}, 5); len(def) != 0 {
		t.Errorf("default user's most-played + starred = %v, want none", def)
	}
}

// TestBrowseQueryBindOrder drives the arg positions the browse statement stitches
// together, in the one arrangement where a misorder is observable.
//
// leadArgs (the filter's user join) and spec.joinArgs (a play list's own join) are
// not that arrangement: both bind the same resolved user id, so swapping them
// changes nothing. What distinguishes is a swap across kinds, so the case here is
// by-genre, whose spec.whereArgs binds a genre rowid, filtered by a query carrying a
// per-user field (leadArgs, a user id) and a tag.<KEY> (compiled args, the canonical
// key ahead of its value). Any cross-kind swap binds a genre id where a user id
// belongs, or a tag key where a genre id belongs, and the rows change.
//
// The second case is the plan's most-played + tag.<KEY>: a list join plus compiled
// tag args, with no user field in the query.
func TestBrowseQueryBindOrder(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	// Rock: One (calm), Two (loud). Jazz: Three (calm).
	for _, tc := range []struct{ title, genre, mood string }{
		{"One", "Rock", "calm"}, {"Two", "Rock", "loud"}, {"Three", "Jazz", "calm"},
	} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + tc.title + ".flac",
			essence: "e" + tc.title, content: "c" + tc.title, title: tc.title,
			artist: "X", album: "Al", genre: tc.genre})
		ids[tc.title] = res.ItemPID
		if _, _, err := st.SetItemTag(ctx, res.ItemPID, "MOOD", []string{tc.mood}, model.SourceUser, false, false); err != nil {
			t.Fatalf("tag %s: %v", tc.title, err)
		}
	}
	bob, err := st.CreateUser(ctx, "Bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Bob starred One and Three; the default user starred Two, so binding the wrong
	// user changes the answer as well.
	for _, title := range []string{"One", "Three"} {
		if _, err := st.SetStar(ctx, bob.PID, ids[title], true, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SetStar(ctx, "", ids["Two"], true, nil); err != nil {
		t.Fatal(err)
	}

	rockPID := model.PID(scalarStr(t, st, "SELECT pid FROM genre WHERE name = ?", "Rock"))
	got := drainBrowse(t, st, read.ListByGenre, read.BrowseOptions{
		GenrePID: rockPID,
		UserPID:  bob.PID,
		Query: query.New(query.EntityItems).
			Where("starred", query.OpIs, 1).
			Where("tag.MOOD", query.OpIs, "calm").Build(),
	}, 5)
	if strings.Join(got, ",") != "One" {
		t.Errorf("by-genre Rock + bob-starred + tag.MOOD=calm = %v, want [One]", got)
	}

	// A play-derived list's own join plus compiled tag args.
	for _, title := range []string{"One", "Two"} {
		if err := st.MarkPlayed(ctx, "", ids[title], false); err != nil {
			t.Fatal(err)
		}
	}
	played := drainBrowse(t, st, read.ListMostPlayed, read.BrowseOptions{
		Query: query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "calm").Build(),
	}, 5)
	if strings.Join(played, ",") != "One" {
		t.Errorf("most-played + tag.MOOD=calm = %v, want [One]", played)
	}
}

// TestBrowseQueryContradictsByYear pins that a query contradicting a named list's own
// filter is an empty page rather than an error. Both predicates simply AND together;
// detecting the contradiction would mean teaching the browse path what each list
// filters on, which is exactly the coupling the shared filter engine avoids.
func TestBrowseQueryContradictsByYear(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", album: "Al", year: 2000})

	got := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{
		Year:  2000,
		Query: query.New(query.EntityItems).Where("year", query.OpIs, 1990).Build(),
	}, 5)
	if len(got) != 0 {
		t.Errorf("by-year 2000 filtered to year 1990 = %v, want an empty page", got)
	}
}
