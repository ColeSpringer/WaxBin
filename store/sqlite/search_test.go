package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

func TestSearchGroupsAndMatches(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Paranoid Android", artist: "Radiohead", album: "OK Computer", albumArt: "Radiohead"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "Karma Police", artist: "Radiohead", album: "OK Computer", albumArt: "Radiohead"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "Bohemian Rhapsody", artist: "Queen", album: "A Night at the Opera", albumArt: "Queen"})

	res, err := st.Search(ctx, "radiohead", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Two tracks, one artist, one album for the Radiohead query.
	if len(res.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(res.Tracks))
	}
	if len(res.Artists) != 1 || res.Artists[0].Title != "Radiohead" {
		t.Errorf("artists = %+v, want [Radiohead]", res.Artists)
	}
	if len(res.Albums) != 1 || res.Albums[0].Title != "OK Computer" {
		t.Errorf("albums = %+v, want [OK Computer]", res.Albums)
	}
	if res.Albums[0].PID == "" || res.Artists[0].PID == "" {
		t.Error("artist/album hits must carry their entity pid for drilldown")
	}
}

// TestSearchTitleOutranksArtist verifies BM25 field weighting: a track whose
// title contains the term ranks above one that only matches via an artist/album
// column.
func TestSearchTitleOutranksArtist(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One fixture has "Mercury" as the title.
	titleHit := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Mercury", artist: "The Planets", album: "Holst"})
	// The other has "Mercury" as the artist.
	artistHit := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Killer Queen", artist: "Mercury", album: "Sheer Heart"})

	res, err := st.Search(ctx, "mercury", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Tracks) < 2 {
		t.Fatalf("tracks = %d, want >= 2", len(res.Tracks))
	}
	if res.Tracks[0].PID != model.PID(titleHit.ItemPID) {
		t.Errorf("top track = %s (%q), want the title match %s",
			res.Tracks[0].PID, res.Tracks[0].Title, titleHit.ItemPID)
	}
	if res.Tracks[0].Score >= res.Tracks[1].Score {
		t.Errorf("title hit score %v should be lower (better) than artist hit %v",
			res.Tracks[0].Score, res.Tracks[1].Score)
	}
	_ = artistHit
}

func TestSearchEmptyAndPunctuationQuery(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Hello", artist: "X", album: "Al"})

	// A query that tokenizes to nothing returns an empty (non-error) result.
	res, err := st.Search(ctx, "   !!! ", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search punctuation: %v", err)
	}
	if !res.Empty() {
		t.Errorf("punctuation-only query should be empty, got %+v", res)
	}

	// FTS operator words are neutralized by lowercasing, so "OR" is a plain term,
	// not a syntax error.
	if _, err := st.Search(ctx, "OR AND NOT", read.SearchOptions{}); err != nil {
		t.Errorf("operator-word query should not error: %v", err)
	}
}

func TestFTSMatchQuery(t *testing.T) {
	cases := map[string]string{
		"Beatles":     "beatles*",
		"AC/DC":       "ac* dc*",
		"  hello  ":   "hello*",
		"!!!":         "",
		"Sgt. Pepper": "sgt* pepper*",
		"OR":          "or*", // lowercased: a plain term, not the FTS operator
	}
	for in, want := range cases {
		if got := ftsMatchQuery(in); got != want {
			t.Errorf("ftsMatchQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
