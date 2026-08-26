package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// TestGCOrphans verifies the manual orphan sweep leaves referenced entities intact,
// respects the grace window, and then deletes a childless album -> release_group ->
// artist chain.
func TestGCOrphans(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album", year: 2001,
	})

	// While the track is present, nothing is orphaned.
	rep, err := st.GCOrphans(ctx, 0)
	if err != nil {
		t.Fatalf("gc (present): %v", err)
	}
	if rep.Total() != 0 {
		t.Fatalf("gc with a present track deleted %d entities, want 0", rep.Total())
	}
	artistsBefore := scalarInt(t, st, "SELECT COUNT(*) FROM artist")
	if artistsBefore == 0 {
		t.Fatal("fixture created no artist entity")
	}

	// Delete the item (cascades its track), orphaning the artist/album/release_group.
	if _, err := st.write.ExecContext(ctx, "DELETE FROM playable_item WHERE pid = ?", string(r.ItemPID)); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	// A long grace window records candidates but deletes nothing yet.
	rep, err = st.GCOrphans(ctx, time.Hour.Nanoseconds())
	if err != nil {
		t.Fatalf("gc (grace): %v", err)
	}
	if rep.Total() != 0 {
		t.Fatalf("gc within grace deleted %d entities, want 0", rep.Total())
	}
	if rep.Pending == 0 {
		t.Fatal("gc within grace recorded no pending candidates")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != artistsBefore {
		t.Fatalf("artist deleted within grace: %d, want %d", n, artistsBefore)
	}

	// grace=0 sweeps the now-confirmed orphans, children-first.
	rep, err = st.GCOrphans(ctx, 0)
	if err != nil {
		t.Fatalf("gc (sweep): %v", err)
	}
	if rep.Artists == 0 || rep.Albums == 0 || rep.ReleaseGroups == 0 {
		t.Fatalf("sweep report = %+v, want album+release_group+artist deleted", rep)
	}
	for _, tbl := range []string{"artist", "album", "release_group"} {
		if n := scalarInt(t, st, "SELECT COUNT(*) FROM "+tbl); n != 0 {
			t.Errorf("%s rows after sweep = %d, want 0", tbl, n)
		}
	}
	// The candidate table is emptied as entities are swept.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM orphan_candidate"); n != 0 {
		t.Errorf("orphan_candidate rows after sweep = %d, want 0", n)
	}
}

// TestOrphanRGSweepDropsAuxMarker: the aux-art backfill marker is keyed by the release
// group's id under its own entity_type, so the sweep's delete (which keys on the
// entity's own type) misses it. Release-group rowids are reused, and a marker left
// behind would silently suppress the backfill for whatever new group inherits the id.
func TestOrphanRGSweepDropsAuxMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	var rgID int64
	var rgPID string
	if err := st.read.QueryRowContext(ctx, "SELECT id, pid FROM release_group").Scan(&rgID, &rgPID); err != nil {
		t.Fatalf("resolve release group: %v", err)
	}
	if err := st.ApplyReleaseGroupAuxArt(ctx,
		model.ReleaseGroupAuxArt{ReleaseGroupID: rgID, PID: model.PID(rgPID)}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	// Through the cascade helper, as the real prune path does: a raw DELETE strands the
	// item's FTS row and the verify below would fail for an unrelated reason.
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(r.ItemPID)))
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := deleteItemCascade(ctx, tx, itemID)
		return err
	}); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	if _, err := st.GCOrphans(ctx, 0); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 0 {
		t.Fatalf("release groups after sweep = %d, want 0", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art'"); n != 0 {
		t.Errorf("aux_art marker rows after sweep = %d, want 0", n)
	}
	assertVerifyClean(t, st)
}

// TestGCOrphansEmitsDeltas confirms a swept entity emits a change_log delete.
func TestGCOrphansEmitsDeltas(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/Al/1.flac", essence: "e1", content: "c1", title: "T", artist: "A", album: "Al",
	})
	if _, err := st.write.ExecContext(ctx, "DELETE FROM playable_item WHERE pid = ?", string(r.ItemPID)); err != nil {
		t.Fatal(err)
	}
	before, _ := st.LatestChangeSeq(ctx)
	if _, err := st.GCOrphans(ctx, 0); err != nil {
		t.Fatalf("gc: %v", err)
	}
	after, _ := st.LatestChangeSeq(ctx)
	if after <= before {
		t.Error("orphan GC emitted no change_log deltas")
	}
	// The deltas are typed deletes for the swept entity kinds.
	var artistDeletes int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM change_log WHERE entity_type = 'artist' AND op = 'delete'").Scan(&artistDeletes); err != nil {
		t.Fatal(err)
	}
	if artistDeletes == 0 {
		t.Error("no artist delete delta emitted")
	}
	_ = model.OpDelete
}
