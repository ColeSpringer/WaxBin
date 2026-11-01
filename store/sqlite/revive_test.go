package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestAPutSaysWhenItBringsAnItemBack: a put that returns a missing item to present says so
// (Revived), whether the file is the item's primary, a copy of it, a book's part or a rip,
// and a put over a present item does not.
func TestAPutSaysWhenItBringsAnItemBack(t *testing.T) {
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
	markMissing := func(files ...model.PID) {
		t.Helper()
		if _, err := st.MarkFilesMissing(ctx, files); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what string, res *model.ScanItemResult, want bool) {
		t.Helper()
		if res.Revived != want {
			t.Errorf("%s: revived %v, want %v (%+v)", what, res.Revived, want, res)
		}
	}

	track := trackSpec{path: onDisk("t.mp3"), essence: "ET", content: "CT", title: "Track", artist: "A", album: "Al"}
	first := putTrack(t, st, lib.ID, track)
	check("a new track", first, false)
	check("a present track read again", putTrack(t, st, lib.ID, track), false)
	markMissing(first.FilePID)
	check("a missing track read again", putTrack(t, st, lib.ID, track), true)

	cp := track
	cp.path = onDisk("t copy.mp3")
	copied := putTrack(t, st, lib.ID, cp)
	if !copied.AttachedAsCopy {
		t.Fatalf("copy put = %+v, want an alternate", copied)
	}
	markMissing(first.FilePID, copied.FilePID)
	check("a copy read again beside its primary", putTrack(t, st, lib.ID, cp), true)

	// With the primary gone too, the copy takes the item over: it already backed it, so it
	// brings it back without joining it.
	markMissing(first.FilePID, copied.FilePID)
	if err := os.Remove(track.path); err != nil {
		t.Fatal(err)
	}
	took := putTrack(t, st, lib.ID, cp)
	check("a copy taking its vanished primary's place", took, true)
	if took.Joined || took.AttachedAsCopy {
		t.Errorf("the copy taking over = %+v, want it neither joined nor a copy", took)
	}

	book := bookSpec{path: onDisk("b.mp3"), essence: "EB", content: "CB", title: "Book", author: "Author"}
	part := putBook(t, st, lib.ID, book)
	markMissing(part.FilePID)
	check("a missing book's part read again", putBook(t, st, lib.ID, book), true)

	rip := vtrackSpecInput(lib.ID, onDisk("rip.flac"), "ER", "CR", 600000, [][2]int64{{0, 7500}, {7500, 0}})
	ripped, err := st.PutScannedVirtualTracks(ctx, rip)
	if err != nil {
		t.Fatal(err)
	}
	check("a new rip", ripped, false)
	markMissing(ripped.FilePID)
	again, err := st.PutScannedVirtualTracks(ctx, rip)
	if err != nil {
		t.Fatal(err)
	}
	check("a missing rip read again", again, true)
}

// TestFileOccupantsSayWhatHoldsAPath: the files at a display path, folded for case, and
// the files holding an audio essence each name their item, its state, their essence and
// whether they are on disk.
func TestFileOccupantsSayWhatHoldsAPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := entityFixture(t)
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	here := filepath.Join(root, "Album", "01 Song.mp3")
	if err := os.MkdirAll(filepath.Dir(here), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(here, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := putTrack(t, st, lib.ID, trackSpec{path: here, essence: "E1", content: "C1", title: "Song", artist: "A", album: "Album"})
	gone := putTrack(t, st, lib.ID, trackSpec{path: filepath.Join(root, "Album", "02 Gone.mp3"), essence: "E2", content: "C2",
		title: "Gone", artist: "A", album: "Album"})
	if _, err := st.MarkFilesMissing(ctx, []model.PID{gone.FilePID}); err != nil {
		t.Fatal(err)
	}
	occ, err := st.FileOccupants(ctx, filepath.Join(root, "ALBUM", "01 song.MP3"))
	if err != nil || len(occ) != 1 {
		t.Fatalf("occupants by a case-folded path = %+v (err %v), want the one file", occ, err)
	}
	if o := occ[0]; o.DisplayPath != here || string(o.Path) != here || o.FilePID != kept.FilePID || o.ItemPID != kept.ItemPID ||
		o.Kind != model.KindTrack || o.State != model.StatePresent || o.Essence != "E1" || !o.OnDisk || !o.Reachable {
		t.Errorf("occupant = %+v, want the file on disk, its present item and essence", o)
	}
	occ, err = st.FilesByEssence(ctx, "E2")
	if err != nil || len(occ) != 1 {
		t.Fatalf("occupants of E2 = %+v (err %v), want the gone file", occ, err)
	}
	if o := occ[0]; o.FilePID != gone.FilePID || o.ItemPID != gone.ItemPID || o.State != model.StateMissing || o.OnDisk || !o.Reachable {
		t.Errorf("occupant = %+v, want the gone file of a missing item under a root that is there", o)
	}
	if occ, err := st.FilesByEssence(ctx, "E9"); err != nil || len(occ) != 0 {
		t.Errorf("occupants of an unknown essence = %+v (err %v), want none", occ, err)
	}
}
