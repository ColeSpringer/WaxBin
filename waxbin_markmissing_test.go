package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// stateOf reads one item's lifecycle state, failing the test on a read error rather
// than dereferencing the nil view a failed Get returns.
func stateOf(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) model.ItemState {
	t.Helper()
	got, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get(%s): %v", pid, err)
	}
	return got.State
}

// TestMarkMissingVerifiesBeforeWriting walks the verification the facade adds over
// the store's state rule: a file that is really on disk is reported back instead of
// recorded, a deleted file marks, and a repeat is idempotent.
func TestMarkMissingVerifiesBeforeWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	path := filepath.Join(root, "album", "one.mp3")
	writeFile(t, path, testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	pid := items[0].PID

	// The bytes are there, so the catalog was right and the caller's failure is
	// something else. Nothing is written.
	outcome, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("mark with the file present: %v", err)
	}
	if outcome != model.OutcomeFilesPresent {
		t.Errorf("outcome = %q, want files-present", outcome)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a files-present answer must not write)", st)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outcome, err = lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("mark after deletion: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("outcome = %q, want marked", outcome)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StateMissing {
		t.Errorf("state = %q, want missing", st)
	}

	outcome, err = lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if outcome != model.OutcomeAlreadyMissing {
		t.Errorf("re-mark outcome = %q, want already-missing", outcome)
	}
}

// TestMarkMissingAdmitsDeletedFolder is the case the mount gate is deliberately not
// tightened for: a user removing a whole album folder is the ordinary genuine
// deletion, and a gate on each file's parent directory would refuse exactly that,
// teaching callers to reach for Force and skip every guard.
func TestMarkMissingAdmitsDeletedFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	album := filepath.Join(root, "album")
	writeFile(t, filepath.Join(album, "one.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	if err := os.RemoveAll(album); err != nil {
		t.Fatal(err)
	}
	outcome, err := lib.MarkMissing(ctx, items[0].PID, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("a deleted album folder must still mark: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("outcome = %q, want marked", outcome)
	}
}

// TestMarkMissingRefusesUnreachableRoot pins the failure the gate exists for: with
// the library root gone the files' absence proves nothing, so the call is refused
// rather than recording a whole library as deleted. Force is the override.
func TestMarkMissingRefusesUnreachableRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mount")
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "album", "one.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	pid := items[0].PID

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("unreachable root = %v, want CodeIO", err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a refused call must not write)", st)
	}

	outcome, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{Force: true})
	if err != nil {
		t.Fatalf("forced mark: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("forced outcome = %q, want marked", outcome)
	}
}

// TestMarkMissingMultiFileBookRefusesPartialView covers a book whose parts disagree:
// one is genuinely deleted and the other's storage is unreadable. The answer is a
// refusal rather than a decision made from the half of the book it could see.
func TestMarkMissingMultiFileBookRefusesPartialView(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the unreadable part cannot be staged")
	}
	if runtime.GOOS == "windows" {
		// Chmod 0 on a directory is a no-op there; staging an unreadable part would
		// need a deny ACE. The product path is reachable on Windows (an access-denied
		// stat maps to CodeIO), only the fixture cannot stage it.
		t.Skip("directory permissions cannot make a part unreadable on Windows")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "in", "p1.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Sanderson", "Mistborn", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "vault", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Sanderson", "Mistborn", 2, testaudio.AudioWithSeed(0x55)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1 (two parts grouped)", len(books))
	}
	pid := books[0].PID

	// One part deleted outright, the other behind a directory that cannot be
	// traversed, so its stat fails with something other than "does not exist".
	if err := os.Remove(filepath.Join(root, "in", "p1.m4b")); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(root, "vault")
	if err := os.Chmod(vault, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vault, 0o755) })

	if _, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("unreadable part = %v, want CodeIO", err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a refused call must not write)", st)
	}
}

// TestAnAbsentRootIsUnreachableToScanAndMarkMissing: with a library root gone, a scan
// reports it unreachable and marks nothing missing, as MarkMissing refuses to; once the
// root is back, a scan finds every item still present.
func TestAnAbsentRootIsUnreachableToScanAndMarkMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mount")
	writeFile(t, filepath.Join(root, "album", "one.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	pid := items[0].PID
	away := root + ".away"
	if err := os.Rename(root, away); err != nil {
		t.Fatal(err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil || !res.Total.RootUnreachable || res.Total.Missing != 0 {
		t.Fatalf("scan with the root away = %+v (err %v), want it unreachable and nothing missing", res, err)
	}
	if _, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) {
		t.Errorf("mark-missing with the root away = %v, want CodeIO", err)
	}
	if err := os.Rename(away, root); err != nil {
		t.Fatal(err)
	}
	if res, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil || res.Total.RootUnreachable || res.Total.Missing != 0 {
		t.Fatalf("scan once the root is back = %+v (err %v), want nothing missing", res, err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present throughout", st)
	}
}

// TestARevivedFileIsAChange: a scan that finds a missing item's file again counts it as
// an update rather than as unchanged, since the item is playable again.
func TestARevivedFileIsAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "album", "one.mp3")
	data := testaudio.BuildMP3("One", "Artist", "Album", 1)
	writeFile(t, path, data)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if outcome, err := lib.MarkMissing(ctx, items[0].PID, waxbin.MarkMissingOptions{}); err != nil || outcome != model.OutcomeMarked {
		t.Fatalf("mark missing = %q (err %v)", outcome, err)
	}
	writeFile(t, path, data)
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if res.Total.ItemsUpdated != 1 || res.Total.Unchanged != 0 {
		t.Errorf("rescan = %+v, want the revived file counted as an update", res.Total)
	}
	if st := stateOf(t, ctx, lib, items[0].PID); st != model.StatePresent {
		t.Errorf("state = %q, want present", st)
	}
}

// missingImport is a managed library holding one imported track whose file is gone, and
// the bytes it had, staged again for an import that would restore it.
type missingImport struct {
	lib       *waxbin.Library
	item      model.PID
	file      model.PID
	path      string
	data      []byte
	staging   string
	stagedSrc string
}

func newMissingImport(t *testing.T) missingImport {
	t.Helper()
	ctx := context.Background()
	f := missingImport{data: testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(61))}
	f.lib = openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	first := t.TempDir()
	writeFile(t, filepath.Join(first, "song.mp3"), f.data)
	importAll(t, ctx, f.lib, first, 1)
	items, err := f.lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want the import", len(items), err)
	}
	f.item, f.path = items[0].PID, items[0].DisplayPath
	files, err := f.lib.ItemFiles(ctx, f.item)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %+v (err %v)", files, err)
	}
	f.file = files[0].FilePID
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if outcome, err := f.lib.MarkMissing(ctx, f.item, waxbin.MarkMissingOptions{}); err != nil || outcome != model.OutcomeMarked {
		t.Fatalf("mark missing = %q (err %v)", outcome, err)
	}
	f.staging = t.TempDir()
	f.stagedSrc = filepath.Join(f.staging, "song.mp3")
	writeFile(t, f.stagedSrc, f.data)
	return f
}

// restored checks that the item is present again on its own file at its old path.
func (f missingImport) restored(t *testing.T, rep *inbox.Report) {
	t.Helper()
	ctx := context.Background()
	if rep.Imported != 1 || len(rep.Files) != 1 {
		t.Fatalf("report = %+v, want the file imported", rep)
	}
	if got := rep.Files[0]; got.Verdict != scan.VerdictRevived || got.ItemPID != f.item {
		t.Errorf("outcome = %+v, want it to have revived %s", got, f.item)
	}
	if st := stateOf(t, ctx, f.lib, f.item); st != model.StatePresent {
		t.Errorf("state = %q, want present", st)
	}
	if files, err := f.lib.ItemFiles(ctx, f.item); err != nil || len(files) != 1 || files[0].FilePID != f.file || files[0].DisplayPath != f.path {
		t.Errorf("files = %+v (err %v), want the item's own file back at %s", files, err, f.path)
	}
}

// TestReuploadOfAMissingFileRestoresItsItem: a missing file's own audio staged again is no
// duplicate of the row its gone file left: the plan says it restores the item, at its old
// path, the import lands, and the item is present again under its pid on its own file, by a
// folder import and by an acquired one alike.
func TestReuploadOfAMissingFileRestoresItsItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMissingImport(t)
	plan, err := f.lib.PlanImport(ctx, waxbin.ImportRequest{Source: f.staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if a := plan.Actions[0]; a.Outcome != inbox.OutcomeImport || a.Restores != f.item || a.Dst != f.path {
		t.Fatalf("action = %+v, want an import restoring %s at %s", a, f.item, f.path)
	}
	rep, err := f.lib.ApplyImport(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	f.restored(t, rep)

	g := newMissingImport(t)
	res, err := g.lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: g.stagedSrc}, model.KindTrack, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if res.Restores != g.item || res.AlreadyPresent || res.Plan.Actions[0].Outcome != inbox.OutcomeImport {
		t.Fatalf("acquired = %+v (action %+v), want an import restoring %s, nothing present", res, res.Plan.Actions[0], g.item)
	}
	rep, err = g.lib.ApplyImport(ctx, res.Plan)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	g.restored(t, rep)
}

// TestReuploadUnderDupAllowIsNotACaseCollision: an import allowed to bring in audio the
// catalog holds is not refused by the row the missing file left at its destination, which
// is the file's own: it restores the item.
func TestReuploadUnderDupAllowIsNotACaseCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMissingImport(t)
	plan, err := f.lib.PlanImport(ctx, waxbin.ImportRequest{Source: f.staging, DupPolicy: model.DupAllow})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if a := plan.Actions[0]; a.Outcome != inbox.OutcomeImport || a.Restores != f.item {
		t.Fatalf("action = %+v, want an import restoring %s", a, f.item)
	}
	rep, err := f.lib.ApplyImport(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	f.restored(t, rep)
}

// TestADifferentSongDoesNotTakeAMissingItemsPath: the row a missing item's gone file left
// still holds its path against other audio, which would take the row over and fold the
// missing item into itself; only the item's own audio takes it back.
func TestADifferentSongDoesNotTakeAMissingItemsPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMissingImport(t)
	writeFile(t, f.stagedSrc, testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(62)))
	for _, dup := range []model.DupPolicy{model.DupSkip, model.DupAllow} {
		plan, err := f.lib.PlanImport(ctx, waxbin.ImportRequest{Source: f.staging, DupPolicy: dup})
		if err != nil {
			t.Fatalf("PlanImport: %v", err)
		}
		if a := plan.Actions[0]; a.Outcome != inbox.OutcomeQuarantine || a.Code != inbox.HoldCataloged || a.Restores != "" ||
			a.Dst != f.path || a.Occupant == nil || a.Occupant.ItemPID != f.item || a.Occupant.FilePID != f.file {
			t.Errorf("%s: action = %+v (occupant %+v), want it held off %s's path", dup, a, a.Occupant, f.item)
		}
	}
}

// TestAudioOnAnUnpluggedDriveIsNoRestore: a file whose library root cannot be reached is not
// gone, only out of sight, so the default DupSkip import of its audio is a duplicate rather
// than a restore of an item that is still present, at plan and at apply.
func TestAudioOnAnUnpluggedDriveIsNoRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ext, managed := filepath.Join(t.TempDir(), "external"), t.TempDir()
	data := testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(91))
	writeFile(t, filepath.Join(ext, "Band", "Record", "01.mp3"), data)
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: []config.Root{
		{Path: ext, Mode: model.ModeInPlace}, {Path: managed, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ext, ext+".unplugged"); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	writeFile(t, filepath.Join(staging, "song.mp3"), data)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatal(err)
	}
	if a := plan.Actions[0]; a.Outcome != inbox.OutcomeDuplicate || a.Restores != "" {
		t.Errorf("action = %+v, want a duplicate of the audio on the unplugged drive", a)
	}
	plan.Actions[0].Outcome, plan.Actions[0].Code, plan.Actions[0].Dst = inbox.OutcomeImport, "", filepath.Join(managed, "song.mp3")
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 0 || rep.Duplicates != 1 {
		t.Errorf("apply = %+v (err %v), want the recheck to hold it a duplicate too", rep, err)
	}
}

// TestAGoneFileOfAnotherKindIsNoRestore: the audio of a missing book's gone file imported as
// a track restores nothing (a track is no book's part): it is held as the catalog's audio.
func TestAGoneFileOfAnotherKindIsNoRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newMissingImport(t)
	res, err := f.lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: f.stagedSrc}, model.KindBook, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Plan.Actions[0]; a.Restores != "" || a.Outcome != inbox.OutcomeDuplicate {
		t.Errorf("action = %+v, want the missing track's audio no restore for a book", a)
	}
}
