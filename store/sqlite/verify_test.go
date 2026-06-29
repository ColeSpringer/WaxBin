package sqlite

import (
	"context"
	"testing"
)

func seedTwoTracks(t *testing.T, st *Store, libID int64) {
	t.Helper()
	putTrack(t, st, libID, trackSpec{
		path: "/lib/r/1.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Radiohead", album: "OK Computer", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, libID, trackSpec{
		path: "/lib/r/2.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Radiohead", album: "OK Computer", genre: "Rock", durationMS: 250,
	})
}

func TestVerifyDerivedCleanAfterRefresh(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("clean catalog reported drift: %+v", rep)
	}
}

func TestVerifyDetectsStaleRollups(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)
	// No RefreshRollups: the artist/genre/release-group rollup rows are missing,
	// which the check must flag as drift (a missed maintenance path).
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Consistent() {
		t.Fatal("missing rollup rows should report as drift")
	}
	if rep.ArtistRollupDrift == 0 || rep.GenreRollupDrift == 0 || rep.ReleaseGroupRollupDrift == 0 {
		t.Errorf("expected drift in all three rollups, got %+v", rep)
	}

	// Refreshing repairs it.
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rep, _ := st.VerifyDerived(ctx); !rep.Consistent() {
		t.Fatalf("refresh did not clear drift: %+v", rep)
	}
}

func TestVerifyDetectsCorruptedRollup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Corrupt a stored rollup directly; the recompute no longer agrees.
	if _, err := st.write.ExecContext(ctx, "UPDATE artist_rollup SET track_count = 99"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.ArtistRollupDrift == 0 {
		t.Errorf("a corrupted artist rollup was not detected: %+v", rep)
	}
}

func TestVerifyDetectsSortKeyDrift(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)
	_ = st.RefreshRollups(ctx)
	if _, err := st.write.ExecContext(ctx, "UPDATE artist SET sort_key = 'WRONG' WHERE name='Radiohead'"); err != nil {
		t.Fatalf("corrupt sort key: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.SortKeyDrift == 0 {
		t.Errorf("a corrupted sort key was not detected: %+v", rep)
	}
}

func TestVerifyDetectsFTSGap(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)
	_ = st.RefreshRollups(ctx)
	if _, err := st.write.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = (SELECT MIN(id) FROM playable_item)"); err != nil {
		t.Fatalf("delete fts: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.ItemsMissingFTS == 0 {
		t.Errorf("a missing FTS row was not detected: %+v", rep)
	}
}
