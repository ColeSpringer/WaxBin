package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
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

	if err := st.SetStar(ctx, "", ids["One"], true); err != nil {
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
	facet, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupGenre, "")
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
