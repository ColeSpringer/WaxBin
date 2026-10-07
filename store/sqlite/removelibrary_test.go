package sqlite

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRemoveLibraryDetachesInBatches: a library removed two files at a time comes out as
// one removed whole. A book whose parts straddle a batch is archived, a track whose copy
// sits in another library keeps it as its primary and hands it back to be re-read, the
// other tracks are archived, the derived data stays consistent, and the row goes with one
// library delta.
func TestRemoveLibraryDetachesInBatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := entityFixture(t)
	g, k := t.TempDir(), t.TempDir()
	gone, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(g), DisplayRoot: g, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(k), DisplayRoot: k, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	one := putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "1.mp3")), essence: "sha256:E1",
		content: "sha256:C1", title: "One", artist: "Artist", album: "Album", trackNo: 1})
	two := putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "2.mp3")), essence: "sha256:E2",
		content: "sha256:C2", title: "Two", artist: "Artist", album: "Album", trackNo: 2})
	moved := putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "3.mp3")), essence: "sha256:E3",
		content: "sha256:C3", title: "Three", artist: "Artist", album: "Album", trackNo: 3})
	copied := putTrack(t, st, kept.ID, trackSpec{path: realFile(t, filepath.Join(k, "3.mp3")), essence: "sha256:E3",
		content: "sha256:C3K", title: "Three", artist: "Artist", album: "Album", trackNo: 3})
	if copied.ItemPID != moved.ItemPID || !copied.AttachedAsCopy {
		t.Fatalf("copy = %+v, want an alternate of %s", copied, moved.ItemPID)
	}
	part1 := putBook(t, st, gone.ID, bookSpec{path: realFile(t, filepath.Join(g, "b1.m4b")), essence: "sha256:B1",
		content: "sha256:BC1", title: "Book", author: "Author", position: 1})
	part2 := putBook(t, st, gone.ID, bookSpec{path: realFile(t, filepath.Join(g, "b2.m4b")), essence: "sha256:B2",
		content: "sha256:BC2", title: "Book", author: "Author", position: 2})
	if part1.ItemPID != part2.ItemPID {
		t.Fatalf("parts landed on %s and %s, want one book", part1.ItemPID, part2.ItemPID)
	}
	seq := latestSeq(t, st)

	var beats []int
	rep, promoted, err := st.removeLibrary(ctx, gone.PID, 2, func(done, total int) error {
		if total != 5 {
			t.Errorf("beat total = %d, want 5", total)
		}
		beats = append(beats, done)
		return nil
	})
	if err != nil {
		t.Fatalf("remove library: %v", err)
	}
	if rep.FilesDetached != 5 || rep.ItemsArchived != 3 || rep.TrashRowsDropped != 0 || rep.Root != g {
		t.Errorf("report = %+v, want 5 files, 3 items archived, no trash at %s", rep, g)
	}
	if !slices.Equal(beats, []int{2, 4, 5}) {
		t.Errorf("beats = %v, want one per batch of two", beats)
	}
	for _, pid := range []model.PID{one.ItemPID, two.ItemPID, part1.ItemPID} {
		if s := itemState(t, st, pid); s != string(model.StateArchived) {
			t.Errorf("item %s = %s, want archived", pid, s)
		}
	}
	if s := itemState(t, st, moved.ItemPID); s != string(model.StatePresent) {
		t.Errorf("the item with a copy elsewhere = %s, want present", s)
	}
	refs, err := st.ItemFiles(ctx, moved.ItemPID)
	if err != nil || len(refs) != 1 || refs[0].FilePID != copied.FilePID || refs[0].Role != primaryRole {
		t.Errorf("its files = %+v (err %v), want the copy as its primary", refs, err)
	}
	if len(promoted) != 1 || promoted[0].FilePID != copied.FilePID {
		t.Errorf("promoted = %+v, want the copy handed back", promoted)
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.PID == gone.PID {
			t.Errorf("library %s is still registered", gone.PID)
		}
	}
	deletes, files := 0, 0
	for _, ch := range changesSince(t, st, seq) {
		switch {
		case ch == [2]string{"library", string(gone.PID)}:
			deletes++
		case ch[0] == "file":
			files++
		}
	}
	if deletes != 1 || files != 5 {
		t.Errorf("deltas: %d library, %d file; want 1 and 5", deletes, files)
	}
	derived, err := st.VerifyDerived(ctx)
	if err != nil || !derived.Consistent() {
		t.Errorf("derived data after the removal = %+v (err %v), want consistent", derived, err)
	}
}

// TestRemoveLibraryStopsBetweenBatchesAndFinishesOnARerun: a removal canceled between
// batches leaves the library registered with the files it had not reached and the items
// it had archived archived, and a second call finishes the job.
func TestRemoveLibraryStopsBetweenBatchesAndFinishesOnARerun(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	g := t.TempDir()
	gone, err := st.EnsureLibrary(context.Background(), &model.Library{Root: []byte(g), DisplayRoot: g, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	var items []model.PID
	for i, title := range []string{"One", "Two", "Three"} {
		r := putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, title+".mp3")), essence: "sha256:E" + title,
			content: "sha256:C" + title, title: title, artist: "Artist", album: "Album", trackNo: i + 1})
		items = append(items, r.ItemPID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rep, _, err := st.removeLibrary(ctx, gone.PID, 2, func(int, int) error { cancel(); return nil })
	if !waxerr.Is(err, waxerr.CodeCanceled) || rep == nil || rep.FilesDetached != 2 || rep.ItemsArchived != 2 {
		t.Fatalf("canceled removal = %+v, %v; want CodeCanceled after the first batch of two", rep, err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file WHERE library_id = ?", gone.ID); n != 1 {
		t.Errorf("files left = %d, want the one the cancel kept it from", n)
	}
	if s := itemState(t, st, items[2]); s != string(model.StatePresent) {
		t.Errorf("the unreached item = %s, want present", s)
	}

	rep, _, err = st.removeLibrary(context.Background(), gone.PID, 2, nil)
	if err != nil || rep.FilesDetached != 1 || rep.ItemsArchived != 1 {
		t.Fatalf("rerun = %+v, %v; want the last file detached", rep, err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM library WHERE id = ?", gone.ID); n != 0 {
		t.Errorf("the library is still registered after the rerun")
	}
	for _, pid := range items {
		if s := itemState(t, st, pid); s != string(model.StateArchived) {
			t.Errorf("item %s = %s, want archived", pid, s)
		}
	}
}

// TestRemoveLibraryTakesAFileThatLandsBetweenBatches: a file that joins the library while
// the removal runs, as a promoted file re-read by a mark-missing can, is detached and its
// item archived like the rest rather than cascading away with the library row.
func TestRemoveLibraryTakesAFileThatLandsBetweenBatches(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	g := t.TempDir()
	gone, err := st.EnsureLibrary(context.Background(), &model.Library{Root: []byte(g), DisplayRoot: g, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"One", "Two"} {
		putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, title+".mp3")), essence: "sha256:E" + title,
			content: "sha256:C" + title, title: title, artist: "Artist", album: "Album", trackNo: i + 1})
	}
	var late model.PID
	rep, _, err := st.removeLibrary(context.Background(), gone.PID, 2, func(done, total int) error {
		if done > total {
			t.Errorf("progress = %d of %d files", done, total)
		}
		if late == "" {
			late = putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "Late.mp3")), essence: "sha256:ELate",
				content: "sha256:CLate", title: "Late", artist: "Artist", album: "Album", trackNo: 3}).ItemPID
		}
		return nil
	})
	if err != nil || rep.FilesDetached != 3 || rep.ItemsArchived != 3 {
		t.Fatalf("removal = %+v, %v; want the late file detached too", rep, err)
	}
	if s := itemState(t, st, late); s != string(model.StateArchived) {
		t.Errorf("the late item = %s, want archived", s)
	}
}

// TestRelocateLibraryRootMovesEveryStoredPath: a relocation leaves no path under the old
// root: the organize journal's moves and the sidecar observations follow the files, so
// an undo or a sidecar check after the move reads where things are now.
func TestRelocateLibraryRootMovesEveryStoredPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := entityFixture(t)
	base := t.TempDir()
	oldRoot, newRoot := filepath.Join(base, "old"), filepath.Join(base, "new")
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(oldRoot), DisplayRoot: oldRoot, Mode: model.ModeManaged})
	if err != nil {
		t.Fatal(err)
	}
	src, dst := filepath.Join(oldRoot, "a.mp3"), filepath.Join(oldRoot, "Artist", "a.mp3")
	in := trackSpecInput(lib.ID, trackSpec{path: src, essence: "sha256:EA", content: "sha256:CA", title: "A", artist: "Artist", album: "Album"})
	in.AuxObservations = []model.AuxObservation{{Kind: "lrc", Path: []byte(filepath.Join(oldRoot, "a.lrc")), Size: 1, MTimeNS: 1}}
	put, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	move := model.RelocateInput{FilePID: put.FilePID, SrcPath: []byte(src), NewPath: []byte(dst), NewDisplayPath: dst,
		NewRelPath: []byte(filepath.Join("Artist", "a.mp3"))}
	jpid, err := st.PlanMove(ctx, move)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMove(ctx, jpid, move); err != nil {
		t.Fatal(err)
	}
	// A move of a file deleted since keeps its journal row with no file behind it.
	gsrc, gdst := filepath.Join(oldRoot, "b.mp3"), filepath.Join(oldRoot, "Artist", "b.mp3")
	gone := putTrack(t, st, lib.ID, trackSpec{path: gsrc, essence: "sha256:EB", content: "sha256:CB", title: "B", artist: "Artist", album: "Album"})
	gmove := model.RelocateInput{FilePID: gone.FilePID, SrcPath: []byte(gsrc), NewPath: []byte(gdst), NewDisplayPath: gdst,
		NewRelPath: []byte(filepath.Join("Artist", "b.mp3"))}
	gjpid, err := st.PlanMove(ctx, gmove)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMove(ctx, gjpid, gmove); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DetachFile(ctx, gone.FilePID); err != nil {
		t.Fatal(err)
	}

	if err := st.RelocateLibraryRoot(ctx, lib.PID, newRoot); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	var gonesrc, gonedst []byte
	if err := st.read.QueryRowContext(ctx, "SELECT src, dst FROM organize_journal WHERE pid = ? AND file_id IS NULL", string(gjpid)).Scan(&gonesrc, &gonedst); err != nil {
		t.Fatal(err)
	}
	if string(gonesrc) != filepath.Join(newRoot, "b.mp3") || string(gonedst) != filepath.Join(newRoot, "Artist", "b.mp3") {
		t.Errorf("the deleted file's journal move = %s -> %s, want it under %s", gonesrc, gonedst, newRoot)
	}
	var jsrc, jdst, aux []byte
	if err := st.read.QueryRowContext(ctx, "SELECT src, dst FROM organize_journal WHERE pid = ?", string(jpid)).Scan(&jsrc, &jdst); err != nil {
		t.Fatal(err)
	}
	if string(jsrc) != filepath.Join(newRoot, "a.mp3") || string(jdst) != filepath.Join(newRoot, "Artist", "a.mp3") {
		t.Errorf("journal move = %s -> %s, want it under %s", jsrc, jdst, newRoot)
	}
	if err := st.read.QueryRowContext(ctx, "SELECT path FROM file_aux_state WHERE kind = 'lrc'").Scan(&aux); err != nil {
		t.Fatal(err)
	}
	if string(aux) != filepath.Join(newRoot, "a.lrc") {
		t.Errorf("sidecar observation = %s, want it under %s", aux, newRoot)
	}
}

// TestRemoveLibraryHandsBackWhatItPromotedWhenStopped: a removal stopped after a batch
// that promoted a copy in another library still hands that copy back to be re-read, so
// its item does not keep the detached file's metadata until some later scan.
func TestRemoveLibraryHandsBackWhatItPromotedWhenStopped(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	g, k := t.TempDir(), t.TempDir()
	ctxBg := context.Background()
	gone, err := st.EnsureLibrary(ctxBg, &model.Library{Root: []byte(g), DisplayRoot: g, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := st.EnsureLibrary(ctxBg, &model.Library{Root: []byte(k), DisplayRoot: k, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "1.mp3")), essence: "sha256:E1",
		content: "sha256:C1", title: "One", artist: "Artist", album: "Album", trackNo: 1})
	copied := putTrack(t, st, kept.ID, trackSpec{path: realFile(t, filepath.Join(k, "1.mp3")), essence: "sha256:E1",
		content: "sha256:C1K", title: "One", artist: "Artist", album: "Album", trackNo: 1})
	putTrack(t, st, gone.ID, trackSpec{path: realFile(t, filepath.Join(g, "2.mp3")), essence: "sha256:E2",
		content: "sha256:C2", title: "Two", artist: "Artist", album: "Album", trackNo: 2})
	ctx, cancel := context.WithCancel(ctxBg)
	_, promoted, err := st.removeLibrary(ctx, gone.PID, 1, func(int, int) error { cancel(); return nil })
	if !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("stopped removal = %v, want CodeCanceled", err)
	}
	if len(promoted) != 1 || promoted[0].FilePID != copied.FilePID {
		t.Errorf("promoted = %+v, want the copy the first batch promoted", promoted)
	}
}
