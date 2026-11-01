package sqlite

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestLineageRecordsAKindChangesFoldsAndMints: a write a kind change makes records under
// its job, in its own transaction, each item it folds away with where it went and each item
// it makes for a file another item held with where that came from, and a write of no job
// records nothing. AbsorbedInto follows a pid's fold by the job, then any later one, to
// where it ended; MintedFrom names what the job made; DropLineage forgets the job's rows
// alone; and a rebuild's stamp renames an item in them.
func TestLineageRecordsAKindChangesFoldsAndMints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := entityFixture(t)
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	onDisk := func(name string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := st.rdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM item_lineage").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	part := func(path, essence string, position int) bookSpec {
		return bookSpec{path: path, essence: essence, content: "c" + essence, title: "Tome", author: "Author", position: position}
	}
	one, two := part(onDisk("1.mp3"), "E1", 1), part(onDisk("2.mp3"), "E2", 2)
	book := putBook(t, st, lib.ID, one).ItemPID
	putBook(t, st, lib.ID, two)
	// A scan's write, of no job, splits part 2 off as a track and records nothing.
	split := putTrack(t, st, lib.ID, trackSpec{path: two.path, essence: "E2", content: "cE2", title: "Two", artist: "Author", album: "Tome"})
	if split.Minted[split.ItemPID] != book || rows() != 0 {
		t.Fatalf("split = %+v with %d lineage rows, want the track minted from the book and nothing recorded", split, rows())
	}
	// Read as the book's part again under a kind change, the track folds into the book.
	const job, other = model.PID("job-1"), model.PID("job-2")
	in := bookSpecInput(lib.ID, two)
	in.Job = job
	if back, err := st.PutScannedBook(ctx, in); err != nil || back.Absorbed[split.ItemPID] != book {
		t.Fatalf("back = %+v (err %v), want the track absorbed into the book", back, err)
	}
	if got, err := st.AbsorbedInto(ctx, job, []model.PID{split.ItemPID, book}); err != nil || !maps.Equal(got, map[model.PID]model.PID{split.ItemPID: book}) {
		t.Errorf("absorbed = %v (err %v), want the track in the book, the standing book absent", got, err)
	}
	if got, err := st.AbsorbedInto(ctx, other, []model.PID{split.ItemPID}); err != nil || len(got) != 0 {
		t.Errorf("absorbed under another job = %v (err %v), want none: that job folded nothing", got, err)
	}

	// A whole file carved into a rip under the other job mints its second track from the
	// whole file's item.
	rip := vtrackSpecInput(lib.ID, onDisk("rip.flac"), "ER", "CR", 600000, [][2]int64{{0, 7500}, {7500, 0}})
	whole := putTrack(t, st, lib.ID, trackSpec{path: string(rip.File.Path), essence: "ER", content: "CR", title: "Whole", artist: "A", album: "Rip"})
	rip.Job = other
	carved, err := st.PutScannedVirtualTracks(ctx, rip)
	if err != nil || len(carved.Minted) != 1 {
		t.Fatalf("carve = %+v (err %v), want one track minted", carved, err)
	}
	var second model.PID
	for p := range carved.Minted {
		second = p
	}
	if got, err := st.MintedFrom(ctx, other); err != nil || !maps.Equal(got, map[model.PID]model.PID{second: whole.ItemPID}) {
		t.Errorf("minted = %v (err %v), want the second track from the whole file's item", got, err)
	}
	if got, err := st.MintedFrom(ctx, job); err != nil || len(got) != 0 {
		t.Errorf("minted by the first job = %v (err %v), want none", got, err)
	}

	// Later folds of where the job's fold went are followed to their end, by whichever job
	// made them.
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		return recordLineageTx(ctx, tx, other, map[model.PID]model.PID{book: second, second: whole.ItemPID}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.AbsorbedInto(ctx, job, nil); err != nil || !maps.Equal(got, map[model.PID]model.PID{split.ItemPID: whole.ItemPID}) {
		t.Errorf("absorbed through later folds = %v (err %v), want the track where the book's chain ends", got, err)
	}
	// A rebuild's stamp renames the item wherever the lineage names it.
	stamp := model.NewPID()
	if ok, err := st.AdoptItemPID(ctx, whole.ItemPID, stamp); err != nil || !ok {
		t.Fatalf("adopt = %v (err %v)", ok, err)
	}
	if got, err := st.AbsorbedInto(ctx, job, nil); err != nil || got[split.ItemPID] != stamp {
		t.Errorf("absorbed after the stamp = %v (err %v), want %s", got, err, stamp)
	}
	if got, err := st.MintedFrom(ctx, other); err != nil || got[second] != stamp {
		t.Errorf("minted after the stamp = %v (err %v), want %s", got, err, stamp)
	}
	// Dropping the job's lineage leaves the other job's.
	if err := st.DropLineage(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got, err := st.AbsorbedInto(ctx, job, nil); err != nil || len(got) != 0 || rows() != 3 {
		t.Errorf("after the drop = %v (err %v) with %d rows, want the job's gone and the other job's three kept", got, err, rows())
	}
}
