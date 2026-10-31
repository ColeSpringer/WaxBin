package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// TestStatsExcludesGhostEntities verifies a ghost entity left behind by a retag
// (an artist no longer backing any track) is not counted, so the totals stay
// consistent with the Facet-derived top lists.
func TestStatsExcludesGhostEntities(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Scan with a typo'd artist, then fix the tag (content change, same item).
	spec := trackSpec{path: "/lib/a/1.flac", essence: "stable", content: "c1", title: "Song", artist: "Beetles", album: "Al"}
	putTrack(t, st, lib.ID, spec)
	spec.content, spec.artist = "c2", "Beatles"
	putTrack(t, st, lib.ID, spec)

	// Both artist rows exist (the ghost is not garbage-collected yet)...
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 2 {
		t.Fatalf("raw artist rows = %d, want 2 (ghost lingers)", n)
	}
	// ...but Stats counts only the one that actually backs a track.
	s, err := st.Stats(ctx, "", 10)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.Artists != 1 {
		t.Errorf("Stats.Artists = %d, want 1 (ghost excluded)", s.Artists)
	}
	if len(s.TopArtists) != 1 || s.TopArtists[0].Display != "Beatles" {
		t.Errorf("top artists = %+v, want only Beatles", s.TopArtists)
	}
}

// TestYearInReviewLeavesTheTrashOut: the recap agrees with the catalog summary, so a
// trashed item is no part of its plays, its new arrivals or its top lists.
func TestYearInReviewLeavesTheTrashOut(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "Kept",
		artist: "Stays", album: "Here", genre: "Rock", durationMS: 240000})
	gone := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2", title: "Gone",
		artist: "Leaves", album: "There", genre: "Jazz", durationMS: 240000})
	created := int64(scalarInt(t, st, "SELECT created_at FROM playable_item WHERE title = 'Kept'"))
	uid := defaultUserID(t, st)
	for i, title := range []string{"Kept", "Gone", "Gone"} {
		if _, err := st.wdb().ExecContext(ctx,
			"INSERT INTO play_session(pid, user_id, item_id, started_at, ended_at, ms_played, client) VALUES (?,?,?,?,?,?,'test')",
			string(model.NewPID()), uid, itemID(t, st, title), created+int64(i), created+int64(i)+120000, 120000); err != nil {
			t.Fatal(err)
		}
	}
	trashItemFile(t, st, gone)

	yr, err := st.YearInReview(ctx, "", time.Unix(0, created).UTC().Year(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if yr.Sessions != 1 || yr.MinutesPlayed != 2 || yr.TracksPlayed != 1 || yr.NewInLibrary != 1 {
		t.Errorf("sessions %d, minutes %d, tracks %d, new %d; want the kept item's 1, 2, 1, 1",
			yr.Sessions, yr.MinutesPlayed, yr.TracksPlayed, yr.NewInLibrary)
	}
	if len(yr.TopTracks) != 1 || yr.TopTracks[0].Title != "Kept" || len(yr.TopArtists) != 1 ||
		yr.TopArtists[0].Display != "Stays" || len(yr.TopGenres) != 1 || yr.TopGenres[0].Display != "Rock" {
		t.Errorf("top lists = %+v / %+v / %+v, want the kept item's alone", yr.TopTracks, yr.TopArtists, yr.TopGenres)
	}
}

// TestStatsLeaveTheTrashOut: a trashed item is no part of the summary, its counts, its
// top lists or the entities only it backed.
func TestStatsLeaveTheTrashOut(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "Kept",
		artist: "Stays", album: "Here", genre: "Rock", year: 2001})
	gone := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2", title: "Gone",
		artist: "Leaves", album: "There", genre: "Jazz", year: 1999})
	trashed := putBook(t, st, lib.ID, bookSpec{path: "/lib/c.m4b", essence: "be", content: "bc", title: "Tome", author: "Writer"})
	kept := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title = 'Kept'"))
	for _, pid := range []model.PID{kept, gone.ItemPID} {
		if err := st.MarkPlayed(ctx, "", pid, true, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetStar(ctx, "", pid, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	trashItemFile(t, st, gone)
	trashItemFile(t, st, trashed)

	s, err := st.Stats(ctx, "", 10)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.Items != 1 || s.Books != 0 || s.Artists != 1 || s.Albums != 1 || s.ReleaseGroups != 1 || s.Genres != 1 {
		t.Errorf("counts items %d books %d artists %d albums %d groups %d genres %d, want 1 0 1 1 1 1",
			s.Items, s.Books, s.Artists, s.Albums, s.ReleaseGroups, s.Genres)
	}
	if len(s.TopArtists) != 1 || s.TopArtists[0].Display != "Stays" {
		t.Errorf("top artists = %+v, want only Stays", s.TopArtists)
	}
	if len(s.TopGenres) != 1 || s.TopGenres[0].Display != "Rock" {
		t.Errorf("top genres = %+v, want only Rock", s.TopGenres)
	}
	if len(s.ByYear) != 1 || s.ByYear[0].Display != "2001" {
		t.Errorf("by year = %+v, want only 2001", s.ByYear)
	}
	if p := s.Play; p.TotalPlays != 1 || p.Starred != 1 || len(p.MostPlayed) != 1 || p.MostPlayed[0].PID != kept {
		t.Errorf("play stats = %+v, want the kept item's play and star alone", p)
	}
}
