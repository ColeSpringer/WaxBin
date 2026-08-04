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

// TestRecentEpisodesList pins the cross-show publication ordering the list exists
// for: episodes from every feed interleave by pub date, newest first, and the two
// populations the spec excludes stay out (an undated episode, which the NULL filter
// drops so the statement can drive off episode_pubdate, and every non-episode, which
// has no ep row at all).
func TestRecentEpisodesList(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/t.flac", essence: "et", content: "ct",
		title: "A Track", artist: "X", album: "Al"})
	putFeedSpec(t, st, "http://cast.example/one", false,
		epSpec{title: "One-Old", pubNS: 1_000_000_000},
		epSpec{title: "One-New", pubNS: 4_000_000_000})
	putFeedSpec(t, st, "http://cast.example/two", false,
		epSpec{title: "Two-Mid", pubNS: 2_000_000_000},
		epSpec{title: "Two-Newest", pubNS: 5_000_000_000},
		epSpec{title: "Two-Undated", pubNS: 0})

	want := []string{"Two-Newest", "One-New", "Two-Mid", "One-Old"}
	full := drainBrowse(t, st, read.ListRecentEpisodes, read.BrowseOptions{}, 50)
	if strings.Join(full, ",") != strings.Join(want, ",") {
		t.Errorf("recent-episodes = %v, want %v (newest published first, across shows)", full, want)
	}
	// Cursor tiling: paging two at a time must reassemble the same list, with
	// drainBrowse asserting no row is duplicated or skipped across the boundaries.
	tiled := drainBrowse(t, st, read.ListRecentEpisodes, read.BrowseOptions{}, 2)
	if strings.Join(tiled, ",") != strings.Join(want, ",") {
		t.Errorf("recent-episodes paged by 2 = %v, want %v", tiled, want)
	}
}

// TestRecentEpisodesPlanUsesPubDateIndex is why the spec excludes undated episodes
// instead of coalescing them: this has to be a keyset over an index, not a full sort.
//
// The second assertion rests on a substring distinction, so read it before loosening
// it: the plan says "USE TEMP B-TREE FOR LAST TERM OF ORDER BY", which does not
// contain "USE TEMP B-TREE FOR ORDER BY". Only the pid tiebreak is block-sorted; a
// bare "FOR ORDER BY" would mean the whole result set was.
func TestRecentEpisodesPlanUsesPubDateIndex(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/t.flac", essence: "et", content: "ct",
		title: "A Track", artist: "X", album: "Al"})
	putFeedSpec(t, st, "http://cast.example/one", false,
		epSpec{title: "One-Old", pubNS: 1_000_000_000},
		epSpec{title: "One-New", pubNS: 4_000_000_000})
	putFeedSpec(t, st, "http://cast.example/two", false,
		epSpec{title: "Two-Mid", pubNS: 2_000_000_000})

	inProgress := query.New(query.EntityItems).
		Where("position_ms", query.OpGt, 0).
		Where("finished", query.OpIs, 0).Build()
	for _, tc := range []struct {
		name string
		opt  read.BrowseOptions
	}{
		{"head", read.BrowseOptions{}},
		{"cursor-bound", read.BrowseOptions{Cursor: read.EncodeCursor("4000000000", "01ARZ3NDEKTSV4RRFFQ69G5FAV")}},
		{"in-progress filter", read.BrowseOptions{Query: inProgress}},
	} {
		stmt, args, err := st.browseStmt(ctx, read.ListRecentEpisodes, tc.opt, 50, "test")
		if err != nil {
			t.Fatalf("%s: browse stmt: %v", tc.name, err)
		}
		plan := explainPlan(t, st, stmt, args...)
		if !strings.Contains(plan, "episode_pubdate") {
			t.Errorf("%s does not drive off episode_pubdate:\n%s", tc.name, plan)
		}
		if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("%s sorts the whole result set instead of walking the index:\n%s", tc.name, plan)
		}
	}

	// The control that makes the assertion above mean anything. recently-added has no
	// index to walk (playable_item indexes kind, sort_key, and the partial
	// identity_key, not created_at), so it scans and full-sorts, and its plan carries
	// the bare "USE TEMP B-TREE FOR ORDER BY" verbatim. Without this, the !Contains
	// check on recent-episodes could be passing because SQLite never emits that exact
	// wording at all rather than because the new list avoids it.
	//
	// A failure here is informative either way: recently-added gained an index and is
	// no longer the vocabulary's floor (update this and the comment), or SQLite
	// reworded its plan output and the check above needs the new string.
	stmt, args, err := st.browseStmt(ctx, read.ListRecentlyAdded, read.BrowseOptions{}, 50, "test")
	if err != nil {
		t.Fatalf("recently-added stmt: %v", err)
	}
	if plan := explainPlan(t, st, stmt, args...); !strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("recently-added no longer emits the bare full-sort string, so the "+
			"recent-episodes assertion above is no longer known to discriminate:\n%s", plan)
	}
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
	// A book carries a real release year, so it ranks here beside the tracks: the
	// scope is "kinds that have a release year", not "music".
	putBook(t, st, lib.ID, bookSpec{path: "/lib/bk.m4b", essence: "ebk", content: "cbk",
		title: "Book2015", author: "Author", year: 2015})
	// An episode is absent however recent, because its year is derived from its
	// publication date. It would outrank every track while being ordered meaninglessly
	// against its own siblings; recent-episodes orders those at full precision.
	putFeedSpec(t, st, "http://cast.example/f", false, epSpec{title: "Ep2010", pubNS: 1_000_000_000, year: 2010})

	order := drainBrowse(t, st, read.ListNewest, read.BrowseOptions{}, 2)
	// Newest release first; the undated track (year 0) sorts last.
	want := []string{"New", "Book2015", "Mid", "Old", "Undated"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("newest order = %v, want %v", order, want)
	}
	// The same episode is reachable through by-year, which is a filter rather than an
	// ordering. That asymmetry is deliberate; see the two specs.
	if got := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{Year: 2010}, 5); strings.Join(got, ",") != "Ep2010" {
		t.Errorf("by-year 2010 = %v, want the episode newest excludes", got)
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

// TestBrowseRecentlyPlayed pins the list in-progress is most easily confused with:
// membership is a play, ordering is last_played_at, and a checkpoint alone does not
// put an item here. That last one is the gap in-progress exists to fill.
func TestBrowseRecentlyPlayed(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	for _, title := range []string{"One", "Two", "Three"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		ids[title] = res.ItemPID
	}
	for _, title := range []string{"One", "Three", "Two"} {
		if err := st.MarkPlayed(ctx, "", ids[title], false); err != nil {
			t.Fatalf("mark played %s: %v", title, err)
		}
	}
	order := drainBrowse(t, st, read.ListRecentlyPlayed, read.BrowseOptions{}, 2)
	if want := "Two,Three,One"; strings.Join(order, ",") != want {
		t.Errorf("recently-played order = %v, want %v", order, want)
	}

	// A checkpoint stamps last_progress_at, not last_played_at, so it does not join
	// this list.
	four := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Four.flac", essence: "eFour",
		content: "cFour", title: "Four", artist: "X", album: "Al"})
	if err := st.SetProgress(ctx, "", four.ItemPID, 60_000); err != nil {
		t.Fatalf("set progress: %v", err)
	}
	if order := drainBrowse(t, st, read.ListRecentlyPlayed, read.BrowseOptions{}, 5); strings.Join(order, ",") != "Two,Three,One" {
		t.Errorf("recently-played after a checkpoint = %v, want the play order unchanged", order)
	}
}

// TestBrowseInProgress pins the "where was I" list: membership is a resume position
// on an unfinished item and ordering is the last playback write.
func TestBrowseInProgress(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	for _, title := range []string{"One", "Two", "Three", "Done", "Untouched"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		ids[title] = res.ItemPID
	}
	for _, title := range []string{"One", "Three", "Two"} {
		if err := st.SetProgress(ctx, "", ids[title], 60_000); err != nil {
			t.Fatalf("set progress %s: %v", title, err)
		}
	}
	// A finished item with a non-zero position is excluded; an untouched one has no
	// row to be excluded by.
	if err := st.SetProgress(ctx, "", ids["Done"], 90_000); err != nil {
		t.Fatalf("set progress Done: %v", err)
	}
	if err := st.MarkPlayed(ctx, "", ids["Done"], true); err != nil {
		t.Fatalf("mark Done finished: %v", err)
	}

	want := "Two,Three,One"
	if order := drainBrowse(t, st, read.ListInProgress, read.BrowseOptions{}, 5); strings.Join(order, ",") != want {
		t.Errorf("in-progress = %v, want %v", order, want)
	}
	// Paging reassembles the same order.
	if order := drainBrowse(t, st, read.ListInProgress, read.BrowseOptions{}, 2); strings.Join(order, ",") != want {
		t.Errorf("in-progress paged at 2 = %v, want %v", order, want)
	}

	// The regression that motivates a dedicated column rather than reusing
	// updated_at: starring the oldest entry must not move it.
	if _, err := st.SetStar(ctx, "", ids["One"], true, nil); err != nil {
		t.Fatalf("star One: %v", err)
	}
	if order := drainBrowse(t, st, read.ListInProgress, read.BrowseOptions{}, 5); strings.Join(order, ",") != want {
		t.Errorf("in-progress after starring the tail = %v, want %v (a star must not reorder)", order, want)
	}
	rating := 80
	if _, err := st.SetRating(ctx, "", ids["One"], &rating, nil); err != nil {
		t.Fatalf("rate One: %v", err)
	}
	if order := drainBrowse(t, st, read.ListInProgress, read.BrowseOptions{}, 5); strings.Join(order, ",") != want {
		t.Errorf("in-progress after rating the tail = %v, want %v (a rating must not reorder)", order, want)
	}
}

// TestBrowseInProgressBoundaries pins the three write/membership edges, including
// the documented sharp one. A test asserting the behaviour is what stops someone
// "fixing" it later without reading the contract on read.ListInProgress.
func TestBrowseInProgressBoundaries(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	ids := map[string]model.PID{}
	for _, title := range []string{"Reset", "Finished", "Stale"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		ids[title] = res.ItemPID
		if err := st.SetProgress(ctx, "", res.ItemPID, 60_000); err != nil {
			t.Fatalf("set progress %s: %v", title, err)
		}
	}

	// Checkpointing back to 0 stamps the write and drops the item off the list.
	if err := st.SetProgress(ctx, "", ids["Reset"], 0); err != nil {
		t.Fatalf("reset progress: %v", err)
	}
	// MarkPlayed(finished=true) stamps it, and finished keeps it off.
	if err := st.MarkPlayed(ctx, "", ids["Finished"], true); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	// MarkPlayed(finished=false) on a stale non-zero position keeps it on the list, at
	// the head. Nothing here clears a resume position, so a client that scrobbles a
	// fully-played track without passing finished pins it until something resets it.
	if err := st.MarkPlayed(ctx, "", ids["Stale"], false); err != nil {
		t.Fatalf("mark played: %v", err)
	}

	order := drainBrowse(t, st, read.ListInProgress, read.BrowseOptions{}, 5)
	if strings.Join(order, ",") != "Stale" {
		t.Errorf("in-progress = %v, want just [Stale]", order)
	}
	for _, pid := range []model.PID{ids["Reset"], ids["Finished"], ids["Stale"]} {
		ps, err := st.PlayStateFor(ctx, "", pid)
		if err != nil {
			t.Fatalf("play state: %v", err)
		}
		if ps.LastProgressAt == 0 {
			t.Errorf("%s has no last_progress_at; all three writes stamp it", pid)
		}
	}
}

// TestBrowseInProgressPlanUsesProgressIndex mirrors
// TestRecentEpisodesPlanUsesPubDateIndex, control included: the head and the
// cursor-bound page must both drive off play_state_progress rather than sorting the
// whole set.
func TestBrowseInProgressPlanUsesProgressIndex(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, title := range []string{"One", "Two"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al"})
		if err := st.SetProgress(ctx, "", res.ItemPID, 60_000); err != nil {
			t.Fatalf("set progress: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		opt  read.BrowseOptions
	}{
		{"head", read.BrowseOptions{}},
		{"cursor-bound", read.BrowseOptions{Cursor: read.EncodeCursor("4000000000", "01ARZ3NDEKTSV4RRFFQ69G5FAV")}},
	} {
		stmt, args, err := st.browseStmt(ctx, read.ListInProgress, tc.opt, 50, "test")
		if err != nil {
			t.Fatalf("%s: browse stmt: %v", tc.name, err)
		}
		plan := explainPlan(t, st, stmt, args...)
		if !strings.Contains(plan, "play_state_progress") {
			t.Errorf("%s does not drive off play_state_progress:\n%s", tc.name, plan)
		}
		// Not tightened past the bare string: this plan legitimately carries
		// "USE TEMP B-TREE FOR LAST TERM OF ORDER BY" for the pi.pid tiebreak, as
		// recently-played and starred already do.
		if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("%s sorts the whole result set instead of walking the index:\n%s", tc.name, plan)
		}
	}
}

func TestBrowseByYearAndByGenre(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al", genre: "Rock", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Y", album: "Bl", genre: "Jazz", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "Z", album: "Cl", genre: "Rock", year: 1990})
	// The episode side of the same three-way year coalesce newest uses: an episode
	// visible at 2000 there must be reachable here, or the two lists disagree about
	// what year an item is from.
	putFeedSpec(t, st, "http://cast.example/f", false, epSpec{title: "Ep", pubNS: 1_000_000_000, year: 2000})

	byYear := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{Year: 2000}, 5)
	if strings.Join(byYear, ",") != "A,B,Ep" {
		t.Errorf("by-year 2000 = %v, want [A B Ep]", byYear)
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

// TestEpisodeScopeAcrossReadSurfaces pins one rule, not three: a query field matches
// what the row displays and so finds episodes, a browse list spans every kind unless
// its ordering is kind-specific, and a music facet excludes them. Year is asserted
// beside artist because year is the pair people re-litigate, and seeing artist behave
// identically shows it is the design.
func TestEpisodeScopeAcrossReadSurfaces(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", albumArt: "X", album: "Al", year: 2000})
	// putFeedSpec titles every show "Cast", which the artist/album fields coalesce.
	putFeedSpec(t, st, "http://cast.example/f", false,
		epSpec{title: "Ep2000", pubNS: 1_000_000_000, year: 2000},
		epSpec{title: "Ep2025", pubNS: 2_000_000_000, year: 2025})

	// Fields find episodes.
	if n := countWhere(t, st, "artist", query.OpIs, "Cast"); n != 2 {
		t.Errorf("artist field over the show title = %d, want both episodes", n)
	}
	if n := countWhere(t, st, "year", query.OpIs, 2025); n != 1 {
		t.Errorf("year field at 2025 = %d, want the episode", n)
	}

	// Browse lists span kinds.
	if got := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{Year: 2000}, 10); len(got) != 2 {
		t.Errorf("by-year 2000 = %v, want the track and the episode", got)
	}
	if got := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{Year: 2025}, 10); len(got) != 1 {
		t.Errorf("by-year 2025 = %v, want the one episode", got)
	}
	// And a caller that wants one kind asks for it, which is what `browse --kind` does.
	scoped := drainBrowse(t, st, read.ListByYear, read.BrowseOptions{
		Year:  2000,
		Query: query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build(),
	}, 10)
	if strings.Join(scoped, ",") != "A" {
		t.Errorf("by-year 2000 scoped to tracks = %v, want just the track", scoped)
	}

	// Music facets exclude episodes, for year and artist alike.
	for _, tc := range []struct {
		group   read.GroupBy
		present string // a bucket the music dimension must have
		count   int
		absent  string // a bucket only an episode could produce
	}{
		{read.GroupYear, "2000", 1, "2025"},
		{read.GroupArtist, "X", 1, "Cast"},
	} {
		res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), tc.group, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", tc.group, err)
		}
		if b, ok := bucketByDisplay(res, tc.present); !ok || b.Count != tc.count {
			t.Errorf("%s facet %q bucket = %+v, want count %d (music only)",
				tc.group, tc.present, b, tc.count)
		}
		if b, ok := bucketByDisplay(res, tc.absent); ok {
			t.Errorf("%s facet gained an episode-only bucket %q (%+v); the dimension is music-scoped",
				tc.group, tc.absent, b)
		}
	}
}

// TestBrowseKindScoped pins the primitive the CLI's --kind flag rides on: any
// discovery list narrows to one media type through the shared filter engine, with
// the list keeping its own ordering. This is what makes an unscoped newest, which
// spans every kind and so leads with podcast episodes on a podcast-heavy catalog,
// a default rather than the only option.
func TestBrowseKindScoped(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "Newer Track", artist: "X", album: "Al", year: 2010})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb",
		title: "Older Track", artist: "X", album: "Al", year: 1990})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/bk.m4b", essence: "ebk", content: "cbk",
		title: "A Book", author: "Author", year: 2000})
	putFeedSpec(t, st, "http://cast.example/f", false,
		epSpec{title: "An Episode", pubNS: 1_000_000_000, year: 2025})

	// recently-added is the kind-agnostic recency list, so it holds everything and
	// scoping it is exactly what the filter is for.
	if all := drainBrowse(t, st, read.ListRecentlyAdded, read.BrowseOptions{}, 10); len(all) != 4 {
		t.Errorf("unscoped recently-added = %v, want all four items", all)
	}
	for _, tc := range []struct {
		list read.DiscoveryList
		kind string
		want string
	}{
		{read.ListNewest, "track", "Newer Track,Older Track"},
		{read.ListNewest, "book", "A Book"},
		{read.ListRecentlyAdded, "book", "A Book"},
		{read.ListAlphabetical, "episode", "An Episode"},
		// The store answers a combination the list cannot hold with an empty page
		// rather than an error: a filter ANDing to nothing is not a store-level
		// mistake. The CLI is where it fails closed (checkListKind).
		{read.ListNewest, "episode", ""},
	} {
		got := drainBrowse(t, st, tc.list, read.BrowseOptions{
			Query: query.New(query.EntityItems).Where("kind", query.OpIs, tc.kind).Build(),
		}, 10)
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%s scoped to %s = %v, want %q", tc.list, tc.kind, got, tc.want)
		}
	}
	// It composes with any list, not just newest: the ordering stays the list's.
	alpha := drainBrowse(t, st, read.ListAlphabetical, read.BrowseOptions{
		Query: query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build(),
	}, 10)
	if strings.Join(alpha, ",") != "Newer Track,Older Track" {
		t.Errorf("alphabetical --kind track = %v, want collation order over tracks only", alpha)
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
