package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// undoTrack writes one tagged track of the album "Night Moves" by The Foobars.
func undoTrack(t *testing.T, path, title string, n int, extra ...testaudio.TXXXFrame) {
	t.Helper()
	writeFile(t, path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: title, Artist: "The Foobars", Album: "Night Moves", Track: n,
		TXXX: extra, Audio: testaudio.AudioWithSeed(byte(n)),
	}))
}

// lastOrganizeJob returns the newest organize job's pid from the history.
func lastOrganizeJob(t *testing.T, ctx context.Context, lib *waxbin.Library) model.PID {
	t.Helper()
	batches, err := lib.OrganizeHistory(ctx, 1)
	if err != nil || len(batches) != 1 {
		t.Fatalf("organize history = %+v (err %v), want the newest batch", batches, err)
	}
	return batches[0].JobPID
}

// TestUndoOrganizeMovesTheFilesBack: an undo moves every file an organize moved back
// where it was, its own sidecar and the folder's companions with it, under a job of its
// own whose journal runs the other way; a later read keeps the album the files belong to.
func TestUndoOrganizeMovesTheFilesBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inbox := filepath.Join(root, "Inbox")
	undoTrack(t, filepath.Join(inbox, "one.mp3"), "Midnight Drive", 1)
	undoTrack(t, filepath.Join(inbox, "two.mp3"), "Neon Rain", 2)
	writeFile(t, filepath.Join(inbox, "one.lrc"), []byte("[00:01.00]words\n"))
	writeFile(t, filepath.Join(inbox, "cover.jpg"), []byte("jpeg"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	albumBefore := onlyAlbumPID(t, ctx, lib)

	rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil || rr.Report.Moved != 2 {
		t.Fatalf("organize = %+v (err %v), want two moves", rr, err)
	}
	organized := filepath.Join(root, "The Foobars", "Night Moves")
	if !fileExists(filepath.Join(organized, "01 - Midnight Drive.lrc")) || !fileExists(filepath.Join(organized, "cover.jpg")) {
		t.Fatal("the organize did not carry the sidecar and the cover")
	}
	job := lastOrganizeJob(t, ctx, lib)
	// A retag of both read in the new folder re-keys the album there and keeps it.
	undoTrack(t, filepath.Join(organized, "01 - Midnight Drive.mp3"), "Midnight Drive", 1, testaudio.TXXXFrame{Desc: "MOOD", Value: "early"})
	undoTrack(t, filepath.Join(organized, "02 - Neon Rain.mp3"), "Neon Rain", 2, testaudio.TXXXFrame{Desc: "MOOD", Value: "early"})
	scanLib(t, ctx, lib)
	if got := onlyAlbumPID(t, ctx, lib); got != albumBefore {
		t.Fatalf("album after the organize and a read = %s, want %s", got, albumBefore)
	}

	rep, err := lib.UndoOrganize(ctx, job)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if rep.Moved != 2 || rep.Held != 0 || rep.Errored != 0 {
		t.Fatalf("undo report = %+v, want both files moved back", rep)
	}
	for _, name := range []string{"one.mp3", "two.mp3", "one.lrc", "cover.jpg"} {
		if !fileExists(filepath.Join(inbox, name)) {
			t.Errorf("%s is not back in the inbox", name)
		}
	}
	if _, err := os.Stat(organized); !os.IsNotExist(err) {
		t.Errorf("the organized folder is still there (err %v)", err)
	}
	jobs, err := lib.Jobs(ctx, 1)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "organize-undo" || jobs[0].State != model.JobDone {
		t.Fatalf("newest job = %+v (err %v), want the finished undo", jobs, err)
	}
	batches, err := lib.OrganizeHistory(ctx, 10)
	if err != nil || len(batches) != 2 || batches[0].JobPID != jobs[0].PID || batches[0].Committed != 2 ||
		batches[1].JobPID != job || batches[1].Committed != 2 || batches[0].Kind != "organize-undo" {
		t.Fatalf("history = %+v (err %v), want the undo above the organize, two moves each", batches, err)
	}

	// Read back in the inbox after another retag, the album re-keys there and is still the
	// same album.
	undoTrack(t, filepath.Join(inbox, "one.mp3"), "Midnight Drive", 1, testaudio.TXXXFrame{Desc: "MOOD", Value: "late"})
	undoTrack(t, filepath.Join(inbox, "two.mp3"), "Neon Rain", 2, testaudio.TXXXFrame{Desc: "MOOD", Value: "late"})
	scanLib(t, ctx, lib)
	if got := onlyAlbumPID(t, ctx, lib); got != albumBefore {
		t.Errorf("album after the undo and a read = %s, want %s", got, albumBefore)
	}

	// Undoing the undo is a redo, and an unknown job is refused before any job starts.
	if rep, err := lib.UndoOrganize(ctx, jobs[0].PID); err != nil || rep.Moved != 2 {
		t.Errorf("redo = %+v (err %v), want both moved again", rep, err)
	}
	if _, err := lib.UndoOrganize(ctx, model.NewPID()); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("undo of an unknown job = %v, want CodeNotFound", err)
	}
}

// TestUndoOrganizeHoldsWhatChangedSince: a file that moved again, one whose old place is
// taken, one gone from disk, and one the catalog no longer holds are held with their
// reasons while the rest go back.
func TestUndoOrganizeHoldsWhatChangedSince(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inbox := filepath.Join(root, "Inbox")
	titles := []string{"Ok", "Moved", "Taken", "Gone", "Dropped"}
	for i, title := range titles {
		undoTrack(t, filepath.Join(inbox, title+".mp3"), title, i+1)
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil || rr.Report.Moved != 5 {
		t.Fatalf("organize = %+v (err %v), want five moves", rr, err)
	}
	job := lastOrganizeJob(t, ctx, lib)
	organized := filepath.Join(root, "The Foobars", "Night Moves")

	// Moved: renamed on disk and read again, so the catalog follows it elsewhere.
	if err := os.MkdirAll(filepath.Join(root, "Elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(organized, "02 - Moved.mp3"), filepath.Join(root, "Elsewhere", "moved.mp3")); err != nil {
		t.Fatal(err)
	}
	scanLib(t, ctx, lib)
	// Taken: another file now sits where it came from.
	writeFile(t, filepath.Join(inbox, "Taken.mp3"), []byte("someone else"))
	// Gone: deleted behind the catalog's back.
	if err := os.Remove(filepath.Join(organized, "04 - Gone.mp3")); err != nil {
		t.Fatal(err)
	}
	// Dropped: deleted outright, so its journal row no longer names a file.
	dropped := itemPIDByTitle(t, ctx, lib, "Dropped")
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{dropped}, model.DeletePermanent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatal(err)
	}

	rep, err := lib.UndoOrganize(ctx, job)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if rep.Moved != 1 || !fileExists(filepath.Join(inbox, "Ok.mp3")) {
		t.Errorf("undo moved %d (Ok back: %v), want the one file nothing touched", rep.Moved, fileExists(filepath.Join(inbox, "Ok.mp3")))
	}
	codes := map[organize.HoldCode]int{}
	for _, h := range rep.Holds {
		codes[h.Code]++
		if h.Reason == "" {
			t.Errorf("hold %+v carries no reason", h)
		}
	}
	want := map[organize.HoldCode]int{
		organize.HoldMovedSince: 1, organize.HoldOccupied: 1, organize.HoldMissing: 1, organize.HoldUncataloged: 1,
	}
	if len(codes) != len(want) {
		t.Errorf("hold codes = %v, want %v", codes, want)
	}
	for c, n := range want {
		if codes[c] != n {
			t.Errorf("holds %s = %d, want %d (all %+v)", c, codes[c], n, rep.Holds)
		}
	}
}

// TestPruneOrganizeJournal: rows past the age go, unless their job is still running, and a
// negative age is refused.
func TestPruneOrganizeJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	undoTrack(t, filepath.Join(root, "Inbox", "one.mp3"), "Midnight Drive", 1)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	if _, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, err := lib.PruneOrganizeJournal(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("prune past an hour = %d (err %v), want nothing that young", n, err)
	}
	if n, err := lib.PruneOrganizeJournal(ctx, -time.Hour); !waxerr.Is(err, waxerr.CodeInvalid) || n != 0 {
		t.Fatalf("prune past a negative age = %d (err %v), want CodeInvalid", n, err)
	}
	if n, err := lib.PruneOrganizeJournal(ctx, 0); err != nil || n != 1 {
		t.Fatalf("prune of everything settled = %d (err %v), want the one row", n, err)
	}
	if batches, err := lib.OrganizeHistory(ctx, 10); err != nil || len(batches) != 0 {
		t.Errorf("history after the prune = %+v (err %v), want none", batches, err)
	}
}

// onlyAlbumPID returns the pid of the catalog's one album.
func onlyAlbumPID(t *testing.T, ctx context.Context, lib *waxbin.Library) model.PID {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) == 0 {
		t.Fatalf("items = %d (err %v)", len(items), err)
	}
	album := items[0].AlbumPID
	for _, it := range items {
		if it.AlbumPID != album {
			t.Fatalf("items span albums %s and %s, want one", album, it.AlbumPID)
		}
	}
	return album
}

// TestUndoOrganizeRestoresTheCompanions: an undo puts back exactly the covers and other
// folder companions its organize carried: a cover it copied out of a folder that kept
// other content goes, a cover it moved returns, and one a pruned album folder handed on
// returns to that folder rather than to the disc folders below it; nothing lands in a
// folder that never held it, and the organized folders go. A redo carries them again.
func TestUndoOrganizeRestoresTheCompanions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	in := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	// Inbox keeps a sub-folder of the same album, so its cover is copied out.
	undoTrack(t, in("Inbox", "one.mp3"), "Midnight Drive", 1)
	undoTrack(t, in("Inbox", "sub", "two.mp3"), "Neon Rain", 2)
	writeFile(t, in("Inbox", "cover.jpg"), []byte("inbox cover"))
	// A disc set whose cover sits above its disc folders.
	for disc, title := range map[int]string{1: "Side One", 2: "Side Two"} {
		writeFile(t, in("Set", "CD"+string(rune('0'+disc)), title+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "The Settlers", Album: "Box", Track: 1, Disc: disc, Audio: testaudio.AudioWithSeed(byte(40 + disc)),
		}))
	}
	writeFile(t, in("Set", "cover.jpg"), []byte("set cover"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil || rr.Report.Moved != 4 {
		t.Fatalf("organize = %+v (err %v), want four moves", rr, err)
	}
	foobars, settlers := in("The Foobars", "Night Moves"), in("The Settlers", "Box")
	if !fileExists(filepath.Join(foobars, "cover.jpg")) || !fileExists(filepath.Join(settlers, "cover.jpg")) {
		t.Fatal("the organize did not carry both covers")
	}
	job := lastOrganizeJob(t, ctx, lib)

	rep, err := lib.UndoOrganize(ctx, job)
	if err != nil || rep.Moved != 4 {
		t.Fatalf("undo = %+v (err %v), want four moves back", rep, err)
	}
	for _, p := range []string{in("Inbox", "one.mp3"), in("Inbox", "sub", "two.mp3"), in("Inbox", "cover.jpg"),
		in("Set", "CD1", "Side One.mp3"), in("Set", "CD2", "Side Two.mp3"), in("Set", "cover.jpg")} {
		if !fileExists(p) {
			t.Errorf("%s is not back", p)
		}
	}
	for _, p := range []string{in("Inbox", "sub", "cover.jpg"), in("Set", "CD1", "cover.jpg"), in("Set", "CD2", "cover.jpg"),
		in("The Foobars"), in("The Settlers")} {
		if fileExists(p) {
			t.Errorf("%s is there, which the tree never held", p)
		}
	}
	if b, err := os.ReadFile(in("Inbox", "cover.jpg")); err != nil || string(b) != "inbox cover" {
		t.Errorf("the inbox cover = %q (err %v), want its own bytes", b, err)
	}

	jobs, err := lib.Jobs(ctx, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v (err %v)", jobs, err)
	}
	if rep, err := lib.UndoOrganize(ctx, jobs[0].PID); err != nil || rep.Moved != 4 {
		t.Fatalf("redo = %+v (err %v), want four moves", rep, err)
	}
	for _, p := range []string{filepath.Join(foobars, "cover.jpg"), filepath.Join(settlers, "cover.jpg"), in("Inbox", "cover.jpg")} {
		if !fileExists(p) {
			t.Errorf("after the redo %s is missing", p)
		}
	}
	if fileExists(in("Set", "cover.jpg")) {
		t.Error("after the redo the set's cover is still in its old folder")
	}
}

// TestUndoLeavesACoverWithAudioThatStays: a cover an organize carried goes back only with
// audio that goes back, so an undo holding every file leaves the cover with them rather
// than in a folder the files never return to.
func TestUndoLeavesACoverWithAudioThatStays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	x := filepath.Join(root, "Inbox", "X")
	undoTrack(t, filepath.Join(x, "a.mp3"), "Midnight Drive", 1)
	undoTrack(t, filepath.Join(x, "b.mp3"), "Neon Rain", 2)
	writeFile(t, filepath.Join(x, "cover.jpg"), []byte("x cover"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	if rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil || rr.Report.Moved != 2 {
		t.Fatalf("organize = %+v (err %v), want two moves", rr, err)
	}
	job := lastOrganizeJob(t, ctx, lib)
	organized := filepath.Join(root, "The Foobars", "Night Moves")
	if !fileExists(filepath.Join(organized, "cover.jpg")) || fileExists(x) {
		t.Fatal("the organize did not carry the cover out of the emptied folder")
	}
	// Another file takes a's old place, and b moves on.
	writeFile(t, filepath.Join(x, "a.mp3"), []byte("someone else"))
	if err := os.MkdirAll(filepath.Join(root, "Elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(organized, "02 - Neon Rain.mp3"), filepath.Join(root, "Elsewhere", "b.mp3")); err != nil {
		t.Fatal(err)
	}
	scanLib(t, ctx, lib)

	rep, err := lib.UndoOrganize(ctx, job)
	if err != nil || rep.Moved != 0 {
		t.Fatalf("undo = %+v (err %v), want every file held", rep, err)
	}
	if !fileExists(filepath.Join(organized, "cover.jpg")) || fileExists(filepath.Join(x, "cover.jpg")) {
		t.Errorf("cover with the held audio %v, in the old folder %v; want it staying with the audio",
			fileExists(filepath.Join(organized, "cover.jpg")), fileExists(filepath.Join(x, "cover.jpg")))
	}
}

// TestUndoPutsACopyBackWhenItsOriginalStays: a folder split between two albums copied its
// cover to one and moved it to the other; when only the copy's audio comes back, the copy
// takes the original's place, so the folder has its cover and the emptied one goes.
func TestUndoPutsACopyBackWhenItsOriginalStays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "Mixed")
	writeFile(t, filepath.Join(src, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Alpha", Artist: "Band A", Album: "First", Track: 1, Audio: testaudio.AudioWithSeed(61)}))
	writeFile(t, filepath.Join(src, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Beta", Artist: "Band B", Album: "Second", Track: 1, Audio: testaudio.AudioWithSeed(62)}))
	writeFile(t, filepath.Join(src, "cover.jpg"), []byte("mixed cover"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	if rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil || rr.Report.Moved != 2 {
		t.Fatalf("organize = %+v (err %v), want two moves", rr, err)
	}
	job := lastOrganizeJob(t, ctx, lib)
	first, second := filepath.Join(root, "Band A", "First"), filepath.Join(root, "Band B", "Second")
	if !fileExists(filepath.Join(first, "cover.jpg")) || !fileExists(filepath.Join(second, "cover.jpg")) {
		t.Fatal("the organize did not give both destinations the cover")
	}
	// Whichever destination took the original, its audio moves on, so only the other's
	// audio, and with it the copy, comes back.
	if err := os.MkdirAll(filepath.Join(root, "Elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	kept := first
	if err := os.Rename(filepath.Join(second, "01 - Beta.mp3"), filepath.Join(root, "Elsewhere", "b.mp3")); err != nil {
		t.Fatal(err)
	}
	scanLib(t, ctx, lib)

	rep, err := lib.UndoOrganize(ctx, job)
	if err != nil || rep.Moved != 1 {
		t.Fatalf("undo = %+v (err %v), want the one file back", rep, err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "cover.jpg")); err != nil || string(b) != "mixed cover" {
		t.Errorf("the folder's cover after the undo = %q (err %v), want it back", b, err)
	}
	if fileExists(kept) {
		t.Errorf("%s is still there, want the emptied destination gone", kept)
	}
}
