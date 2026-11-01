package sqlite_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// plannedTrash is a catalog holding one track at dir/Album/song.mp3 whose trash move has
// been planned and not yet made.
type plannedTrash struct {
	st       *sqlite.Store
	db, dir  string
	src, dst string
	item     model.PID
	file     model.PID
	pending  model.TrashPending
}

func planTrashFixture(t *testing.T) *plannedTrash {
	t.Helper()
	ctx := context.Background()
	f := &plannedTrash{dir: t.TempDir()}
	f.db = testcatalog.Seed(t, filepath.Join(t.TempDir(), "c.db"))
	st, lib := openRootedStore(t, f.dir, f.db, "owner-a")
	f.st = st
	f.src = filepath.Join(f.dir, "Album", "song.mp3")
	f.dst = filepath.Join(f.dir, model.TrashDirName, "01ENTRY", "song.mp3")
	if err := os.MkdirAll(filepath.Dir(f.src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedTrack(ctx, input(lib.ID, f.src, "sha256:E", "audio", "Song"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	f.item, f.file = r.ItemPID, r.FilePID
	f.pending = model.TrashPending{PID: model.NewPID(), FilePID: r.FilePID, Reason: "dedup",
		TrashPath: []byte(f.dst), TrashDisplay: f.dst, JobPID: "JOB"}
	if err := st.PlanTrash(ctx, f.pending); err != nil {
		t.Fatalf("plan trash: %v", err)
	}
	return f
}

// move does what the trash's move does to the file on disk.
func (f *plannedTrash) move(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.src, f.dst); err != nil {
		t.Fatal(err)
	}
}

// reopen closes the store as a crash leaves it and opens it again, which runs recovery.
func (f *plannedTrash) reopen(t *testing.T) *sqlite.Store {
	t.Helper()
	if err := f.st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return f.reopenAfterClose(t)
}

// reopenAfterClose opens the closed catalog again, which runs recovery.
func (f *plannedTrash) reopenAfterClose(t *testing.T) *sqlite.Store {
	t.Helper()
	st, _ := openRootedStore(t, f.dir, f.db, "owner-b")
	return st
}

func pendingRows(t *testing.T, db string) int {
	t.Helper()
	var n int
	if err := roConn(t, db).QueryRow("SELECT COUNT(*) FROM trash_pending").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// checkFinished checks that recovery wrote the trash row the move would have: the entry
// under the pending row's pid with what it planned, and the item archived.
func (f *plannedTrash) checkFinished(t *testing.T, st *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	e, err := st.ActiveTrashByPID(ctx, f.pending.PID)
	if err != nil {
		t.Fatalf("trash row: %v", err)
	}
	if e.ItemPID != f.item || e.OrigDisplay != f.src || string(e.OrigPath) != f.src || e.TrashDisplay != f.dst ||
		e.Size != int64(len("audio")) || e.Reason != "dedup" {
		t.Errorf("trash row = %+v, want the planned move of %s", e, f.src)
	}
	it, err := st.ItemByPID(ctx, f.item)
	if err != nil || it.State != model.StateArchived {
		t.Errorf("item = %+v (err %v), want it archived", it, err)
	}
	if n := pendingRows(t, f.db); n != 0 {
		t.Errorf("%d pending rows left, want none", n)
	}
}

// TestTrashFileTakesOverItsPendingRow: the journal row takes the pending row's pid and the
// pending row goes in the same write.
func TestTrashFileTakesOverItsPendingRow(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if n := pendingRows(t, f.db); n != 1 {
		t.Fatalf("%d pending rows after the plan, want 1", n)
	}
	f.move(t)
	res, err := f.st.TrashFile(context.Background(), model.TrashFileInput{PID: f.pending.PID, FilePID: f.file, Reason: "dedup",
		TrashPath: []byte(f.dst), TrashDisplay: f.dst})
	if err != nil || res.TrashPID != f.pending.PID {
		t.Fatalf("TrashFile = %+v (err %v), want the pending pid %s", res, err, f.pending.PID)
	}
	f.checkFinished(t, f.st)
}

// TestAbortTrashDropsThePendingRow: a move that did not happen leaves no record, and a
// plan for a file the catalog does not hold is refused.
func TestAbortTrashDropsThePendingRow(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if err := f.st.AbortTrash(context.Background(), f.pending.PID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if n := pendingRows(t, f.db); n != 0 {
		t.Fatalf("%d pending rows after the abort, want none", n)
	}
	err := f.st.PlanTrash(context.Background(), model.TrashPending{PID: model.NewPID(), FilePID: "NOSUCHFILE",
		TrashPath: []byte(f.dst), TrashDisplay: f.dst})
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("plan for an unknown file: err = %v, want CodeNotFound", err)
	}
}

// TestRecoverTrashFinishesAMoveWhoseRowNeverLanded: a crash after the move and before the
// journal row leaves the file in the trash and a pending row; the next open finishes it.
func TestRecoverTrashFinishesAMoveWhoseRowNeverLanded(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	f.move(t)
	st := f.reopen(t)
	f.checkFinished(t, st)
	if _, err := st.FileByPath(context.Background(), []byte(f.src)); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("file row: err = %v, want it detached", err)
	}
}

// TestRecoverTrashFinishesWithoutTheFileRow: with the file row gone meanwhile, the journal
// row is built from the pending row alone.
func TestRecoverTrashFinishesWithoutTheFileRow(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if _, err := f.st.DetachFile(context.Background(), f.file); err != nil {
		t.Fatalf("detach: %v", err)
	}
	f.move(t)
	st := f.reopen(t)
	f.checkFinished(t, st)
}

// TestRecoverTrashDropsAPendingRowWhoseMoveNeverHappened: a crash before the move leaves
// the file where it was; recovery drops the row and the catalog keeps the file. So it does
// when neither copy is there, the file row left for a scan to mark missing.
func TestRecoverTrashDropsAPendingRowWhoseMoveNeverHappened(t *testing.T) {
	t.Parallel()
	for _, gone := range []bool{false, true} {
		f := planTrashFixture(t)
		if gone {
			if err := os.Remove(f.src); err != nil {
				t.Fatal(err)
			}
		}
		st := f.reopen(t)
		ctx := context.Background()
		if n := pendingRows(t, f.db); n != 0 {
			t.Errorf("gone %v: %d pending rows left, want none", gone, n)
		}
		if _, err := st.ActiveTrashByPID(ctx, f.pending.PID); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("gone %v: trash row err = %v, want none written", gone, err)
		}
		if _, err := st.FileByPath(ctx, []byte(f.src)); err != nil {
			t.Errorf("gone %v: file row: %v, want it kept", gone, err)
		}
	}
}

// TestRecoverTrashRollsBackWhenBothCopiesMatch: a whole copy in the trash beside its
// original comes from a move across filesystems cut off before its source went, one whose
// source could not be removed, or a stranded file copied back by hand to restore it. The
// copy holds nothing the original does not, so it goes and the original stays live, which
// never undoes a restore.
func TestRecoverTrashRollsBackWhenBothCopiesMatch(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := f.reopen(t)
	if n := pendingRows(t, f.db); n != 0 {
		t.Errorf("%d pending rows left, want none", n)
	}
	if !fileOnDisk(f.src) || fileOnDisk(f.dst) || fileOnDisk(filepath.Dir(f.dst)) {
		t.Errorf("original kept %v, trash copy kept %v, entry kept %v; want the original alone", fileOnDisk(f.src), fileOnDisk(f.dst), fileOnDisk(filepath.Dir(f.dst)))
	}
	if _, err := st.ActiveTrashByPID(context.Background(), f.pending.PID); err == nil {
		t.Error("a trash row was written, want the move rolled back")
	}
	if it, err := st.ItemByPID(context.Background(), f.item); err != nil || it.State == model.StateArchived {
		t.Errorf("item = %+v (err %v), want it live", it, err)
	}
}

// TestRecoverTrashDropsACopyCutShort: a crash in the middle of that copy leaves a trash copy
// that is the start of the original; the copy goes and the file stays.
func TestRecoverTrashDropsACopyCutShort(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst, []byte("aud"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := f.reopen(t)
	if n := pendingRows(t, f.db); n != 0 {
		t.Errorf("%d pending rows left, want none", n)
	}
	if !fileOnDisk(f.src) || fileOnDisk(f.dst) || fileOnDisk(filepath.Dir(f.dst)) {
		t.Errorf("original kept %v, short copy kept %v, entry kept %v; want the original alone", fileOnDisk(f.src), fileOnDisk(f.dst), fileOnDisk(filepath.Dir(f.dst)))
	}
	if _, err := st.FileByPath(context.Background(), []byte(f.src)); err != nil {
		t.Errorf("file row: %v, want it kept", err)
	}
}

// TestRecoverTrashLeavesAMismatchedPairAndLogsIt: two copies that differ are left for the
// operator, named in a warning, and the row stays for the next open.
func TestRecoverTrashLeavesAMismatchedPairAndLogsIt(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: f.db, Owner: "owner-b",
		Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if n := pendingRows(t, f.db); n != 1 {
		t.Errorf("%d pending rows, want the row kept", n)
	}
	if !fileOnDisk(f.src) || !fileOnDisk(f.dst) {
		t.Errorf("original kept %v, trash copy kept %v; want both left", fileOnDisk(f.src), fileOnDisk(f.dst))
	}
	if text := logged.String(); !strings.Contains(text, "WARN") || !strings.Contains(text, f.src) || !strings.Contains(text, f.dst) {
		t.Errorf("log = %q, want a warning naming both copies", text)
	}
}

// TestRecoverTrashKeepsARowWhoseRootIsAway: with the library's root away (a drive not
// mounted), neither copy can be seen, which says nothing of the move, so the record stays
// until an open finds the root and settles it.
func TestRecoverTrashKeepsARowWhoseRootIsAway(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	f.move(t)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	away := f.dir + ".away"
	if err := os.Rename(f.dir, away); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: f.db, Owner: "owner-b"})
	if err != nil {
		t.Fatalf("open with the root away: %v", err)
	}
	if n := pendingRows(t, f.db); n != 1 {
		t.Errorf("%d pending rows with the root away, want the record kept", n)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(away, f.dir); err != nil {
		t.Fatal(err)
	}
	f.checkFinished(t, f.reopenAfterClose(t))
}

// TestRecoverTrashKeepsARowItCannotSettle: a pair recovery cannot read is left for the
// operator, named in a warning, rather than refusing the open.
func TestRecoverTrashKeepsARowItCannotSettle(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A folder where the original was: both are there, and the original cannot be read.
	if err := os.Remove(f.src); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.src, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: f.db, Owner: "owner-b",
		Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatalf("open with a pair recovery cannot read: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if n := pendingRows(t, f.db); n != 1 {
		t.Errorf("%d pending rows, want the record kept", n)
	}
	if text := logged.String(); !strings.Contains(text, "WARN") || !strings.Contains(text, f.dst) {
		t.Errorf("log = %q, want a warning naming the trash copy", text)
	}
}

// pendingPaths reads the one pending row's original and trash paths, raw and shown.
func pendingPaths(t *testing.T, db string) (orig, origShown, trashed, trashShown string) {
	t.Helper()
	var o, tr []byte
	if err := roConn(t, db).QueryRow("SELECT orig_path, orig_display, trash_path, trash_display FROM trash_pending").
		Scan(&o, &origShown, &tr, &trashShown); err != nil {
		t.Fatal(err)
	}
	return string(o), origShown, string(tr), trashShown
}

// TestARootMoveCarriesAPendingTrashMove: a root relocated while a trash move waits takes
// the pending row's paths along, so the next open finds the file in the moved trash and
// finishes the move instead of dropping the row and leaving the file there unrecorded.
func TestARootMoveCarriesAPendingTrashMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := planTrashFixture(t)
	f.move(t)
	libs, err := f.st.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatalf("libraries = %v (err %v)", libs, err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(f.dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RelocateLibraryRoot(ctx, libs[0].PID, moved); err != nil {
		t.Fatal(err)
	}
	wantOrig := filepath.Join(moved, "Album", "song.mp3")
	wantTrash := filepath.Join(moved, model.TrashDirName, "01ENTRY", "song.mp3")
	if o, os_, tr, ts := pendingPaths(t, f.db); o != wantOrig || os_ != wantOrig || tr != wantTrash || ts != wantTrash {
		t.Errorf("pending paths = %s, %s, %s, %s; want them under %s", o, os_, tr, ts, moved)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := openRootedStore(t, moved, f.db, "owner-b")
	e, err := st.ActiveTrashByPID(ctx, f.pending.PID)
	if err != nil {
		t.Fatalf("trash row after the reopen: %v, want the move finished", err)
	}
	if string(e.OrigPath) != wantOrig || e.TrashDisplay != wantTrash || !fileOnDisk(wantTrash) {
		t.Errorf("trash row = %+v, want it at %s from %s", e, wantTrash, wantOrig)
	}
}

// TestARespelledFolderCarriesAPendingTrashMove: a folder renamed between two spellings
// respells the pending row's original path below it, as it does a trash row's.
func TestARespelledFolderCarriesAPendingTrashMove(t *testing.T) {
	t.Parallel()
	f := planTrashFixture(t)
	from, to := filepath.Join(f.dir, "Album"), filepath.Join(f.dir, "album")
	if _, err := f.st.RespellFolder(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(to, "song.mp3")
	if o, os_, tr, _ := pendingPaths(t, f.db); o != want || os_ != want || tr != f.dst {
		t.Errorf("pending paths = %s, %s, %s; want the original at %s and the trash path kept", o, os_, tr, want)
	}
}
