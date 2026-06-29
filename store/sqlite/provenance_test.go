package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func itemPID(t *testing.T, st *Store) model.PID {
	t.Helper()
	var pid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM playable_item LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("read item pid: %v", err)
	}
	return model.PID(pid)
}

func TestLockUnlockKeepsProvenanceSparse(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "Alp",
	})
	pid := itemPID(t, st)

	// No provenance rows for a freshly scanned (tag-sourced) item.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance"); n != 0 {
		t.Fatalf("provenance rows = %d, want 0 for a tag-only item", n)
	}

	if err := st.LockField(ctx, pid, "title"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	locked, err := st.IsFieldLocked(ctx, pid, "title")
	if err != nil || !locked {
		t.Fatalf("IsFieldLocked = %v (err %v), want true", locked, err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE locked=1"); n != 1 {
		t.Errorf("locked rows = %d, want 1", n)
	}

	// Unlocking a tag-sourced field with no curated value removes the row.
	if err := st.UnlockField(ctx, pid, "title"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance"); n != 0 {
		t.Errorf("provenance rows after unlock = %d, want 0 (sparse)", n)
	}
}

// TestRedundantLockUnlockSilent verifies a no-op lock/unlock emits no change
// delta (unlocking a never-locked field nets to zero and must stay silent).
func TestRedundantLockUnlockSilent(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	pid := itemPID(t, st)

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.UnlockField(ctx, pid, "title"); err != nil { // never locked -> no-op
		t.Fatal(err)
	}
	if seq, _ := st.LatestChangeSeq(ctx); seq != seq0 {
		t.Errorf("unlocking a never-locked field emitted %d spurious deltas", seq-seq0)
	}

	// A real lock emits one delta; re-locking the same field is silent.
	if err := st.LockField(ctx, pid, "title"); err != nil {
		t.Fatal(err)
	}
	seq1, _ := st.LatestChangeSeq(ctx)
	if seq1 == seq0 {
		t.Error("a real lock should emit a delta")
	}
	if err := st.LockField(ctx, pid, "title"); err != nil { // already locked -> no-op
		t.Fatal(err)
	}
	if seq, _ := st.LatestChangeSeq(ctx); seq != seq1 {
		t.Errorf("re-locking an already-locked field emitted %d spurious deltas", seq-seq1)
	}
}

func TestLockRejectsUnknownField(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
	err := st.LockField(context.Background(), itemPID(t, st), "definitely_not_a_field")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for an unknown field, got %v", err)
	}
}

func TestSetFieldProvenanceRespectsLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
	pid := itemPID(t, st)

	// A user edit records source=user with the curated value and survives unlock
	// (it carries a value, so it is not a pure-lock row).
	if err := st.SetFieldProvenance(ctx, pid, "artist", model.SourceUser, "Curated Artist", false); err != nil {
		t.Fatalf("set user provenance: %v", err)
	}
	if err := st.LockField(ctx, pid, "artist"); err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Enrichment must not overwrite a locked field.
	err := st.SetFieldProvenance(ctx, pid, "artist", model.SourceEnrichment, "Wikidata Name", false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("enrichment over a locked field: want CodeConflict, got %v", err)
	}

	rows, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "artist" || rows[0].Source != model.SourceUser || !rows[0].Locked || rows[0].Value != "Curated Artist" {
		t.Fatalf("provenance = %+v, want one locked user-sourced artist row", rows)
	}

	// Unlocking leaves the curated value in place (the row is not pure-lock).
	if err := st.UnlockField(ctx, pid, "artist"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE value='Curated Artist'"); n != 1 {
		t.Errorf("curated value dropped on unlock; want it retained")
	}
}

// TestProvenanceUnknownItem verifies a bogus item pid is reported as NotFound,
// not silently rendered as a clean tag-sourced item.
func TestProvenanceUnknownItem(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	if _, err := st.FieldProvenance(context.Background(), "01J0NONEXISTENT0000000000"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unknown item provenance: want CodeNotFound, got %v", err)
	}
	// A real item with no curated fields still returns an empty (non-error) result.
	if rows, err := st.FieldProvenance(context.Background(), itemPID(t, st)); err != nil || len(rows) != 0 {
		t.Errorf("real item with no provenance: want empty/no-error, got %v / %d rows", err, len(rows))
	}
}

func TestProvenanceCascadesWithItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Re-key the file's essence so the prior item is orphaned and deleted; its
	// provenance row must cascade away with it.
	spec := trackSpec{path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First", artist: "A", album: "Alp"}
	putTrack(t, st, lib.ID, spec)
	if err := st.LockField(ctx, itemPID(t, st), "title"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	spec.essence, spec.content, spec.title = "e2", "c2", "Second"
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance"); n != 0 {
		t.Errorf("orphaned provenance rows = %d, want 0 (cascaded with the item)", n)
	}
}
