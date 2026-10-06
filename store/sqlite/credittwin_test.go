package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

var userAttr = model.Attribution{Source: model.SourceUser}

// provenanceLocked reads an item field's provenance lock, -1 when the row is absent.
func provenanceLocked(t *testing.T, st *Store, pid model.PID, field string) int {
	t.Helper()
	if provenanceRows(t, st, pid, field) == 0 {
		return -1
	}
	return scalarInt(t, st, `SELECT fp.locked FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE pi.pid = ? AND fp.field = ?`, string(pid), field)
}

func composerCredits(t *testing.T, st *Store, pid model.PID) []string {
	t.Helper()
	credits, err := st.ItemCredits(context.Background(), pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	var out []string
	for _, c := range credits {
		if c.Role == model.RoleComposer {
			out = append(out, c.Name)
		}
	}
	return out
}

// TestPlainComposerEditSupersedesTheCredit: a plain composer edit that moves the
// composer away from an earlier composer credit drops that credit, rows and provenance
// alike, as a scan re-deriving the composer does.
func TestPlainComposerEditSupersedesTheCredit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"})
	if _, _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, res.ItemPID, "composer", "John Williams", userAttr, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	if got := composerCredits(t, st, res.ItemPID); len(got) != 0 {
		t.Errorf("composer credits = %v, want none once the composer moved away from them", got)
	}
	if provenanceRows(t, st, res.ItemPID, model.CreditField(model.RoleComposer)) != 0 ||
		provenanceRows(t, st, res.ItemPID, "composer") != 1 {
		t.Error("want the plain composer row describing the column and the credit row retired")
	}
	assertVerifyClean(t, st)
}

// TestPlainComposerEditKeepsACreditItRepeats: retyping the text the credit already wrote
// leaves the credit standing, since its list still describes the column.
func TestPlainComposerEditKeepsACreditItRepeats(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"})
	if _, _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleComposer, []string{"Hans Zimmer", "Lorne Balfe"},
		userAttr, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, res.ItemPID, "composer", "Hans Zimmer; Lorne Balfe", userAttr, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	if got := composerCredits(t, st, res.ItemPID); len(got) != 2 {
		t.Errorf("composer credits = %v, want both kept", got)
	}
	if provenanceRows(t, st, res.ItemPID, model.CreditField(model.RoleComposer)) != 1 {
		t.Error("the credit row was retired though its list still describes the column")
	}
}

// TestCreditEditSupersedesThePlainField: the other direction, a credit edit retiring the
// plain row whose value no longer describes the column.
func TestCreditEditSupersedesThePlainField(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"})
	if err := st.EditItemField(ctx, res.ItemPID, "composer", "John Williams", userAttr, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if provenanceRows(t, st, res.ItemPID, "composer") != 0 ||
		provenanceRows(t, st, res.ItemPID, model.CreditField(model.RoleComposer)) != 1 {
		t.Error("want the credit row describing the column and the plain row retired")
	}
}

// TestTwinLocksHoldBothWays: a column a credit shares with its plain field is locked
// through either spelling, so a write through the other is refused unless forced, and
// a forced write that leaves the lock as it was keeps the column locked. That holds for
// plain edits, credit edits and an artist rename's credits, on tracks and books.
func TestTwinLocksHoldBothWays(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	credit := model.CreditField(model.RoleComposer)
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, tr, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set locked composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, tr, "composer", "John Williams", userAttr, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("plain edit over a locked credit: err = %v, want CodeLocked", err)
	}
	if err := st.EditItemField(ctx, tr, "composer", "John Williams", userAttr, model.LockUnchanged, true); err != nil {
		t.Fatalf("forced plain edit: %v", err)
	}
	if provenanceLocked(t, st, tr, "composer") != 1 || provenanceLocked(t, st, tr, credit) != -1 {
		t.Errorf("after the forced edit: composer lock %d, credit row %d; want the lock carried onto the plain row",
			provenanceLocked(t, st, tr, "composer"), provenanceLocked(t, st, tr, credit))
	}
	if _, _, err := st.SetItemCredits(ctx, tr, model.RoleComposer, []string{"Howard Shore"},
		userAttr, model.LockUnchanged, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("credit edit over a locked plain field: err = %v, want CodeLocked", err)
	}
	if _, _, err := st.SetItemCredits(ctx, tr, model.RoleComposer, []string{"Howard Shore"},
		userAttr, model.LockUnchanged, true, false); err != nil {
		t.Fatalf("forced credit edit: %v", err)
	}
	if provenanceLocked(t, st, tr, credit) != 1 || provenanceLocked(t, st, tr, "composer") != -1 {
		t.Errorf("after the forced credit edit: credit lock %d, composer row %d; want the lock carried onto the credit",
			provenanceLocked(t, st, tr, credit), provenanceLocked(t, st, tr, "composer"))
	}

	// The artist twin on a track.
	art := putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Song", artist: "Someone", album: "Al"}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, art, model.RoleArtist, []string{"Someone"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set locked artist credit: %v", err)
	}
	if err := st.EditItemField(ctx, art, "artist", "Someone Else", userAttr, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("artist edit over a locked artist credit: err = %v, want CodeLocked", err)
	}

	// The author and narrator twins on a book.
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "bc1",
		title: "Book", author: "Writer", narrators: []string{"Reader"}}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, book, model.RoleAuthor, []string{"Writer"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set locked author credit: %v", err)
	}
	if err := st.EditItemField(ctx, book, "author", "Other Writer", userAttr, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("author edit over a locked author credit: err = %v, want CodeLocked", err)
	}
	if err := st.LockField(ctx, book, "narrator"); err != nil {
		t.Fatalf("lock narrator: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, book, model.RoleNarrator, []string{"Other Reader"},
		userAttr, model.LockUnchanged, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("narrator credit over a locked narrator: err = %v, want CodeLocked", err)
	}

	// An artist rename rewrites the credits naming the artist, so a plain field locked
	// beside one of them refuses it too.
	ren := putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "Other Theme", artist: "Orchestra", album: "Al"}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, ren, model.RoleComposer, []string{"Rename Me"},
		userAttr, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if err := st.LockField(ctx, ren, "composer"); err != nil {
		t.Fatalf("lock composer: %v", err)
	}
	artistPID := model.PID(scalarStr(t, st, "SELECT pid FROM artist WHERE name = 'Rename Me'"))
	if _, err := st.RenameEntity(ctx, model.MergeArtist, artistPID, map[string]string{"name": "Renamed"},
		userAttr, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("rename over a locked composer field: err = %v, want CodeLocked", err)
	}
}

// TestTwinLockReaders: every reader of an item's locks sees a column locked through
// either spelling, so a guard asking about one spelling honours the other's lock.
func TestTwinLockReaders(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set locked composer credit: %v", err)
	}
	locked, err := st.LockedFields(ctx, pid)
	if err != nil {
		t.Fatalf("locked fields: %v", err)
	}
	if !locked["composer"] || !locked[model.CreditField(model.RoleComposer)] {
		t.Errorf("LockedFields = %v, want both spellings of the composer", locked)
	}
	if ok, err := st.IsFieldLocked(ctx, pid, "composer"); err != nil || !ok {
		t.Errorf("IsFieldLocked(composer) = %v, %v; want true", ok, err)
	}
	if err := st.SetFieldProvenance(ctx, pid, "composer", userAttr, "John Williams", false); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("SetFieldProvenance over the locked credit: err = %v, want CodeConflict", err)
	}
}

// TestEnrichmentFillHonoursTwinLocks: a fill leaves a column alone while it is locked
// through its credit, the user's deliberately empty credit included, rather than filling
// it and moving that lock onto the provider's value.
func TestEnrichmentFillHonoursTwinLocks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"}).ItemPID
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "bc1",
		title: "Book", author: "Writer"}).ItemPID
	for _, c := range []struct {
		pid    model.PID
		role   model.ContributorRole
		field  string
		column string
	}{
		{tr, model.RoleComposer, "composer", "SELECT composer FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?"},
		{book, model.RoleNarrator, "narrator", "SELECT narrator FROM book b JOIN playable_item pi ON pi.id = b.item_id WHERE pi.pid = ?"},
	} {
		if _, _, err := st.SetItemCredits(ctx, c.pid, c.role, nil, userAttr, model.LockOf(true), false, false); err != nil {
			t.Fatalf("clear and lock %s credit: %v", c.role, err)
		}
		itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(c.pid)))
		if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: c.pid, Matched: true,
			Provider: "mb", Fields: map[string]string{c.field: "Somebody Else"}}); err != nil {
			t.Fatalf("fill %s: %v", c.field, err)
		}
		if got := scalarStr(t, st, c.column, string(c.pid)); got != "" {
			t.Errorf("%s = %q, want it left empty under the locked credit", c.field, got)
		}
		if provenanceLocked(t, st, c.pid, model.CreditField(c.role)) != 1 || provenanceRows(t, st, c.pid, c.field) != 0 {
			t.Errorf("%s: want the locked credit row kept and no fill row", c.field)
		}
	}
}

// TestUnlockThroughEitherSpelling: an explicit unlock through one spelling unlocks the
// column, so a twin row the write keeps loses its lock too, and UnlockField on either
// spelling clears both.
func TestUnlockThroughEitherSpelling(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	credit := model.CreditField(model.RoleComposer)
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"}).ItemPID
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set locked composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer", "Hans Zimmer", userAttr, model.LockOf(false), true); err != nil {
		t.Fatalf("unlocking edit: %v", err)
	}
	if provenanceLocked(t, st, pid, credit) != 0 || provenanceLocked(t, st, pid, "composer") != 0 {
		t.Errorf("after an unlocking edit: credit lock %d, composer lock %d; want both unlocked",
			provenanceLocked(t, st, pid, credit), provenanceLocked(t, st, pid, "composer"))
	}
	if err := st.EditItemField(ctx, pid, "composer", "John Williams", userAttr, model.LockUnchanged, false); err != nil {
		t.Errorf("an edit after the unlock was refused: %v", err)
	}

	// The credit spelling unlocks a plain lock the same way.
	if err := st.EditItemField(ctx, pid, "composer", "Howard Shore", userAttr, model.LockOf(true), false); err != nil {
		t.Fatalf("locking edit: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Howard Shore"},
		userAttr, model.LockOf(false), true, false); err != nil {
		t.Fatalf("unlocking credit: %v", err)
	}
	if ok, err := st.IsFieldLocked(ctx, pid, "composer"); err != nil || ok {
		t.Errorf("after an unlocking credit: composer locked = %v, %v; want unlocked", ok, err)
	}

	// UnlockField names one spelling and clears the column.
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Howard Shore"},
		userAttr, model.LockOf(true), false, false); err != nil {
		t.Fatalf("relock credit: %v", err)
	}
	if err := st.UnlockField(ctx, pid, "composer"); err != nil {
		t.Fatalf("unlock composer: %v", err)
	}
	if provenanceLocked(t, st, pid, credit) == 1 {
		t.Error("UnlockField(composer) left the credit locked")
	}
	if err := st.EditItemField(ctx, pid, "composer", "Ennio Morricone", userAttr, model.LockUnchanged, false); err != nil {
		t.Errorf("an edit after UnlockField was refused: %v", err)
	}
}
