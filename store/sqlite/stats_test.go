package sqlite

import (
	"context"
	"testing"
)

// TestStatsExcludesGhostEntities verifies a ghost entity left behind by a retag
// (an artist no longer backing any track) is not counted, so the totals stay
// consistent with the Facet-derived top lists.
func TestStatsExcludesGhostEntities(t *testing.T) {
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
