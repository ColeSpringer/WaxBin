package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestActiveTrashForItemsGroupsByItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	trashed := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "af1")
	// A second item, catalogued but never trashed, must be absent rather than
	// mapped to an empty slice.
	untouched := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b.mp3", essence: "bf1", content: "bf1c", title: "B", artist: "A"})
	unknown := model.NewPID()

	got, err := st.ActiveTrashForItems(ctx, []model.PID{trashed.ItemPID, untouched.ItemPID, unknown})
	if err != nil {
		t.Fatalf("ActiveTrashForItems: %v", err)
	}
	entries, ok := got[trashed.ItemPID]
	if !ok || len(entries) != 1 {
		t.Fatalf("trashed item = %v (present %v), want one entry", entries, ok)
	}
	if entries[0].PID != trashed.PID || entries[0].Size != trashed.Size {
		t.Errorf("entry = %+v, want the journal row %+v", entries[0], trashed)
	}
	if _, ok := got[untouched.ItemPID]; ok {
		t.Error("an item with nothing restorable is present in the map")
	}
	if _, ok := got[unknown]; ok {
		t.Error("an unknown pid is present in the map")
	}

	empty, err := st.ActiveTrashForItems(ctx, nil)
	if err != nil || empty != nil {
		t.Fatalf("empty input = (%v, %v), want (nil, nil)", empty, err)
	}
}

// TestActiveTrashForItemsMultiPartBook is the case a boolean cannot answer: the
// caller needs both trash pids to restore a partly-trashed book.
func TestActiveTrashForItemsMultiPartBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	book := putBookPart(t, st, lib.ID, "/lib/book/p1.m4b", "bk1", "e1", 0)
	part2 := putBookPart(t, st, lib.ID, "/lib/book/p2.m4b", "bk1", "e2", 1)
	part3 := putBookPart(t, st, lib.ID, "/lib/book/p3.m4b", "bk1", "e3", 2)

	var want []model.PID
	for _, p := range []struct {
		filePID model.PID
		dest    string
	}{{part2.FilePID, "/lib/t/p2.m4b"}, {part3.FilePID, "/lib/t/p3.m4b"}} {
		tpid, err := st.TrashFile(ctx, model.TrashFileInput{
			FilePID: p.filePID, TrashPath: []byte(p.dest), TrashDisplay: p.dest})
		if err != nil {
			t.Fatalf("TrashFile: %v", err)
		}
		want = append(want, tpid)
	}

	got, err := st.ActiveTrashForItems(ctx, []model.PID{book.ItemPID})
	if err != nil {
		t.Fatalf("ActiveTrashForItems: %v", err)
	}
	entries := got[book.ItemPID]
	if len(entries) != 2 {
		t.Fatalf("got %d entries for the book, want 2", len(entries))
	}
	// Newest first.
	if entries[0].TrashedAt < entries[1].TrashedAt {
		t.Errorf("entries are not newest first: %d then %d", entries[0].TrashedAt, entries[1].TrashedAt)
	}
	seen := map[model.PID]bool{entries[0].PID: true, entries[1].PID: true}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("trash entry %s missing from the result", w)
		}
	}
}

// TestActiveTrashForItemsExcludesRestored pins the absent-includeRestored decision:
// a restored entry is not restorable, so its item drops out of the map entirely.
func TestActiveTrashForItemsExcludesRestored(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "rf1")

	if err := st.MarkTrashRestored(ctx, entry.PID); err != nil {
		t.Fatalf("MarkTrashRestored: %v", err)
	}
	got, err := st.ActiveTrashForItems(ctx, []model.PID{entry.ItemPID})
	if err != nil {
		t.Fatalf("ActiveTrashForItems: %v", err)
	}
	if _, ok := got[entry.ItemPID]; ok {
		t.Errorf("a restored entry is still reported as restorable: %+v", got)
	}
}

// TestActiveTrashForItemsDedupsInput pins why uniquePIDs is not optional: a
// repeated pid would otherwise have its entries appended twice.
func TestActiveTrashForItemsDedupsInput(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "df1")

	got, err := st.ActiveTrashForItems(ctx, []model.PID{entry.ItemPID, entry.ItemPID, entry.ItemPID})
	if err != nil {
		t.Fatalf("ActiveTrashForItems: %v", err)
	}
	if n := len(got[entry.ItemPID]); n != 1 {
		t.Errorf("a pid passed three times returned %d entries, want 1", n)
	}
}

// TestActiveTrashForItemsChunkBoundary crosses the idBatchSize (500) IN-clause
// boundary, and passes the empty pid that would match every edge-less journal row.
func TestActiveTrashForItemsChunkBoundary(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	entry := trashOneFile(t, st, lib.ID, "/lib/a.mp3", "cf1")

	req := make([]model.PID, 0, 501)
	for i := 0; i < 500; i++ {
		req = append(req, model.NewPID())
	}
	req = append(req, entry.ItemPID, "")

	got, err := st.ActiveTrashForItems(ctx, req)
	if err != nil {
		t.Fatalf("ActiveTrashForItems: %v", err)
	}
	if n := len(got[entry.ItemPID]); n != 1 {
		t.Errorf("real pid in the second chunk returned %d entries, want 1", n)
	}
	if _, ok := got[""]; ok {
		t.Error("the empty pid matched journal rows; it must be skipped")
	}
}
