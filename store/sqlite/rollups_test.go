package sqlite

import (
	"context"
	"testing"
)

// artistTrackCount reads a stored rollup directly (no recompute) so the test
// proves the maintained value, not a fresh aggregation.
func rollupTrackCount(t *testing.T, st *Store, table, joinTable, nameCol, name string) int {
	t.Helper()
	q := "SELECT r.track_count FROM " + table + " r JOIN " + joinTable +
		" e ON e.id = r." + idColFor(table) + " WHERE e." + nameCol + " = ?"
	var n int
	if err := st.read.QueryRowContext(context.Background(), q, name).Scan(&n); err != nil {
		t.Fatalf("read %s for %q: %v", table, name, err)
	}
	return n
}

func idColFor(table string) string {
	switch table {
	case "artist_rollup":
		return "artist_id"
	case "genre_rollup":
		return "genre_id"
	default:
		return "id"
	}
}

func TestRollupsMaintainedOnWrite(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two tracks of one artist; rollups must be correct WITHOUT a manual refresh.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/R/A/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Radiohead", album: "A", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/R/A/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Radiohead", album: "A", genre: "Rock", durationMS: 200,
	})

	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Radiohead"); got != 2 {
		t.Errorf("artist rollup track_count = %d, want 2 (maintained on write)", got)
	}
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 2 {
		t.Errorf("genre rollup track_count = %d, want 2", got)
	}
	// No drift, and no manual RefreshRollups was called.
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Fatalf("derived data inconsistent after maintained writes: %+v (err %v)", rep, err)
	}
}

func TestRetagDecrementsOldGenreRollup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "stable", content: "c1", title: "Song",
		artist: "X", album: "Al", genre: "Rock", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 1 {
		t.Fatalf("Rock rollup = %d, want 1", got)
	}
	// Retag (same essence, content changed) to a different genre.
	spec.content, spec.genre = "c2", "Jazz"
	putTrack(t, st, lib.ID, spec)

	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 0 {
		t.Errorf("old genre Rock rollup = %d, want 0 after retag", got)
	}
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Jazz"); got != 1 {
		t.Errorf("new genre Jazz rollup = %d, want 1", got)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("derived data inconsistent after retag: %+v (err %v)", rep, err)
	}
}

func TestOrphanDeleteDecrementsRollup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Re-key the single file's essence so the prior item is orphaned and deleted.
	spec := trackSpec{
		path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First",
		artist: "Solo", album: "Al", genre: "Rock", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	spec.essence, spec.content, spec.title = "e2", "c2", "Second"
	putTrack(t, st, lib.ID, spec)

	// The artist still has exactly one track (the re-keyed item), not two.
	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Solo"); got != 1 {
		t.Errorf("artist rollup after orphan delete = %d, want 1", got)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("derived data inconsistent after orphan delete: %+v (err %v)", rep, err)
	}
}
