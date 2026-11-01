package waxbin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
)

// copyLibrary scans a root holding one recording twice: a/1.mp3 tagged "Original" and a
// byte-identical audio copy b/1.mp3 tagged "Retagged". The walk reaches a/ first, so it
// is the primary. It returns the library, the item and both files' pids and paths.
type copyFixture struct {
	lib                *waxbin.Library
	item               model.PID
	orig, copy         model.PID
	origPath, copyPath string
}

func newCopyFixture(t *testing.T) copyFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	f := copyFixture{origPath: filepath.Join(root, "a", "1.mp3"), copyPath: filepath.Join(root, "b", "1.mp3")}
	writeFile(t, f.origPath, testaudio.BuildMP3("Original", "Artist", "Album", 1))
	writeFile(t, f.copyPath, testaudio.BuildMP3("Retagged", "Artist", "Album", 1))
	f.lib = openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := f.lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := f.lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want the copies on one item", len(items), err)
	}
	f.item = items[0].PID
	for _, r := range copyRoles(t, f.lib, f.item) {
		switch {
		case r.Role == "primary" && string(r.Path) == f.origPath:
			f.orig = r.FilePID
		case r.Role == "alternate" && string(r.Path) == f.copyPath:
			f.copy = r.FilePID
		}
	}
	if f.orig == "" || f.copy == "" {
		t.Fatalf("edges = %+v, want a/ primary and b/ alternate", copyRoles(t, f.lib, f.item))
	}
	return f
}

func copyRoles(t *testing.T, lib *waxbin.Library, item model.PID) []model.ItemFileRef {
	t.Helper()
	refs, err := lib.ItemFiles(context.Background(), item)
	if err != nil {
		t.Fatalf("item files: %v", err)
	}
	return refs
}

func titleOf(t *testing.T, lib *waxbin.Library, item model.PID) string {
	t.Helper()
	it, err := lib.Get(context.Background(), item)
	if err != nil {
		t.Fatalf("get %s: %v", item, err)
	}
	return it.Title
}

func deleteFiles(t *testing.T, lib *waxbin.Library, files ...model.PID) {
	t.Helper()
	ctx := context.Background()
	plan, err := lib.PlanDeleteFiles(ctx, files, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != len(files) {
		t.Fatalf("plan = %+v, want %d pending actions", plan.Actions, len(files))
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil || rep.Trashed != len(files) {
		t.Fatalf("apply = %+v (err %v), want %d trashed", rep, err, len(files))
	}
}

// TestCopyTrashedPrimaryPromotesAndRestoreRejoins: trashing the primary file promotes
// the copy and re-reads it, so the item's title is the copy's; restoring the original
// brings it back as an alternate rather than taking the item over.
func TestCopyTrashedPrimaryPromotesAndRestoreRejoins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	deleteFiles(t, f.lib, f.orig)
	if got := titleOf(t, f.lib, f.item); got != "Retagged" {
		t.Errorf("title after trashing the primary = %q, want the promoted copy's Retagged", got)
	}
	refs := copyRoles(t, f.lib, f.item)
	if len(refs) != 1 || refs[0].FilePID != f.copy || refs[0].Role != "primary" {
		t.Fatalf("edges = %+v, want the copy alone as primary", refs)
	}

	entries, err := f.lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v), want one entry", entries, err)
	}
	if err := f.lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := titleOf(t, f.lib, f.item); got != "Retagged" {
		t.Errorf("title after the restore = %q, want Retagged kept", got)
	}
	roles := map[string]string{}
	for _, r := range copyRoles(t, f.lib, f.item) {
		roles[string(r.Path)] = r.Role
	}
	if roles[f.copyPath] != "primary" || roles[f.origPath] != "alternate" {
		t.Errorf("edges after the restore = %v, want the copy primary and the restored file alternate", roles)
	}
}

func TestCopyTrashedAlternateLeavesTheItem(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	deleteFiles(t, f.lib, f.copy)
	if got := titleOf(t, f.lib, f.item); got != "Original" {
		t.Errorf("title = %q, want Original", got)
	}
	if refs := copyRoles(t, f.lib, f.item); len(refs) != 1 || refs[0].FilePID != f.orig {
		t.Errorf("edges = %+v, want the original alone", refs)
	}
}

// TestCopyDeletingTheItemTrashesEveryFile: deleting the item plans and trashes its copy
// too, leaving it archived.
func TestCopyDeletingTheItemTrashesEveryFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	plan, err := f.lib.PlanDeletePIDs(ctx, []model.PID{f.item}, model.DeleteTrash)
	if err != nil || plan.Pending() != 2 {
		t.Fatalf("plan = %+v (err %v), want both files", plan, err)
	}
	if rep, err := f.lib.ApplyDelete(ctx, plan); err != nil || rep.Trashed != 2 {
		t.Fatalf("apply = %+v (err %v), want two trashed", rep, err)
	}
	if entries, err := f.lib.Trash(ctx, false, 0); err != nil || len(entries) != 2 {
		t.Errorf("trash = %+v (err %v), want two entries", entries, err)
	}
	if st := stateOf(t, ctx, f.lib, f.item); st != model.StateArchived {
		t.Errorf("state = %q, want archived", st)
	}
}

// TestMarkMissingPromotesOrDropsCopies: mark-missing on an item whose primary is gone
// and copy present promotes the copy (re-read, so its title shows); on one whose copy
// is gone it drops the copy's row.
func TestMarkMissingPromotesOrDropsCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	if err := os.Remove(f.copyPath); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.lib.MarkMissing(ctx, f.item, waxbin.MarkMissingOptions{})
	if err != nil || outcome != model.OutcomeDropped {
		t.Fatalf("mark-missing with the copy gone = %q (err %v), want dropped", outcome, err)
	}
	if refs := copyRoles(t, f.lib, f.item); len(refs) != 1 || refs[0].FilePID != f.orig {
		t.Errorf("edges = %+v, want the original alone", refs)
	}

	g := newCopyFixture(t)
	if err := os.Remove(g.origPath); err != nil {
		t.Fatal(err)
	}
	outcome, err = g.lib.MarkMissing(ctx, g.item, waxbin.MarkMissingOptions{})
	if err != nil || outcome != model.OutcomePromoted {
		t.Fatalf("mark-missing with the primary gone = %q (err %v), want promoted", outcome, err)
	}
	if st := stateOf(t, ctx, g.lib, g.item); st != model.StatePresent {
		t.Errorf("state = %q, want present", st)
	}
	if got := titleOf(t, g.lib, g.item); got != "Retagged" {
		t.Errorf("title = %q, want the promoted copy's Retagged", got)
	}
	if outcome, err := g.lib.MarkMissing(ctx, g.item, waxbin.MarkMissingOptions{}); err != nil || outcome != model.OutcomeFilesPresent {
		t.Errorf("repeat = %q (err %v), want files-present", outcome, err)
	}
}

// TestCopyRescanPromotesWhenThePrimaryVanishes: a plain rescan that finds the primary
// gone promotes the copy and re-reads it, instead of marking the item missing.
func TestCopyRescanPromotesWhenThePrimaryVanishes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	if err := os.Remove(f.origPath); err != nil {
		t.Fatal(err)
	}
	res, err := f.lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if res.Total.Missing != 0 {
		t.Errorf("missing = %d, want 0", res.Total.Missing)
	}
	if st := stateOf(t, ctx, f.lib, f.item); st != model.StatePresent {
		t.Errorf("state = %q, want present", st)
	}
	if got := titleOf(t, f.lib, f.item); got != "Retagged" {
		t.Errorf("title = %q, want the promoted copy's Retagged", got)
	}
	if refs := copyRoles(t, f.lib, f.item); len(refs) != 1 || refs[0].FilePID != f.copy || refs[0].Role != "primary" {
		t.Errorf("edges = %+v, want the copy alone as primary", refs)
	}
}

// TestCopyOfABookPartIsNotOrganizedAsAPart: organize lays out a book's parts only, so a
// copy of one of them is neither renumbered into the book folder nor moved at all.
func TestCopyOfABookPartIsNotOrganizedAsAPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "in", "p1.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Sanderson", "Mistborn", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "in", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Sanderson", "Mistborn", 2, testaudio.AudioWithSeed(0x55)))
	writeFile(t, filepath.Join(root, "spare", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Sanderson", "Mistborn", 2, testaudio.AudioWithSeed(0x55)))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %d (err %v), want 1", len(books), err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 2 {
		t.Fatalf("planned moves = %+v, want the two parts only", plan.Actions)
	}
	for _, a := range plan.Actions {
		if strings.Contains(a.Src, "spare") {
			t.Errorf("the copy is planned: %+v", a)
		}
	}
}

// titleSortCopies scans one recording twice, in a managed root and an in-place one, both
// files carrying TITLESORT "Old Sort", flags the root holding the side named by
// readOnlyPrimary read-only, and returns the library, the item, and the primary and copy
// file pids.
func titleSortCopies(t *testing.T, readOnlyPrimary bool) (*waxbin.Library, model.PID, model.PID, model.PID) {
	t.Helper()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	pa, pb := filepath.Join(rootA, "song.mp3"), filepath.Join(rootB, "song.mp3")
	for _, p := range []string{pa, pb} {
		writeFile(t, p, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Old", Artist: "Band", Album: "Album"}))
		if _, err := meta.NewWriter().Apply(ctx, p, []meta.TagEdit{{Key: "TITLESORT", Values: []string{"Old Sort"}}}); err != nil {
			t.Fatalf("stage TITLESORT: %v", err)
		}
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: rootB, Mode: model.ModeInPlace},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Old")
	var primary, copy model.PID
	for _, r := range copyRoles(t, lib, pid) {
		switch {
		case r.Role == "primary" && string(r.Path) == pa:
			primary = r.FilePID
		case r.Role == "alternate" && string(r.Path) == pb:
			copy = r.FilePID
		}
	}
	if primary == "" || copy == "" {
		t.Fatalf("edges = %+v, want the managed file primary", copyRoles(t, lib, pid))
	}
	ro := rootB
	if readOnlyPrimary {
		ro = rootA
	}
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, ro), true)
	return lib, pid, primary, copy
}

// owedKeys returns the tag_write_owed keys a file carries.
func owedKeys(t *testing.T, lib *waxbin.Library, file model.PID) []string {
	t.Helper()
	ds, err := lib.FileDiagnostics(context.Background(), model.DiagnosticFilter{FilePID: file, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, d := range ds {
		keys = append(keys, d.TagKey)
	}
	return keys
}

// TestTitleSortClearReachesEveryFileItCan: a title edit drops the catalog's title spelling
// whichever file refuses its write-back, the TITLESORT clear lands with the title on every
// file that takes the write, and a file in a read-only library owes the title, with which
// the clear rides when it is paid.
func TestTitleSortClearReachesEveryFileItCan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, readOnlyPrimary := range []bool{false, true} {
		lib, pid, primary, copy := titleSortCopies(t, readOnlyPrimary)
		err := lib.EditFields(ctx, pid, map[string]string{"title": "New"}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
		var wbErr *waxbin.WriteBackError
		if !errors.As(err, &wbErr) || len(wbErr.Failures) != 1 {
			t.Fatalf("edit (read-only primary %v) = %v, want one refusal", readOnlyPrimary, err)
		}
		if v, err := lib.Get(ctx, pid); err != nil || v.TitleSort != "" {
			t.Errorf("read-only primary %v: catalog title_sort = %q (err %v), want it dropped", readOnlyPrimary, v.TitleSort, err)
		}
		refused, landed := copy, primary
		if readOnlyPrimary {
			refused, landed = primary, copy
		}
		if got := owedKeys(t, lib, refused); !slices.Equal(got, []string{"title"}) {
			t.Errorf("read-only primary %v: the refusing file owes %v, want [title]", readOnlyPrimary, got)
		}
		if got := owedKeys(t, lib, landed); len(got) != 0 {
			t.Errorf("read-only primary %v: the landed file owes %v, want nothing", readOnlyPrimary, got)
		}
	}
}

// TestCopyWriteBackReachesTheCopy: a written-back edit lands in the copy too, so the
// copy's tags do not drift from the item they back.
func TestCopyWriteBackReachesTheCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	if err := f.lib.EditFields(ctx, f.item, map[string]string{"genre": "Jazz"}, waxbin.EditOptions{WriteBack: true}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	for _, p := range []string{f.origPath, f.copyPath} {
		fm, err := meta.NewReader().Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.Genre != "Jazz" {
			t.Errorf("%s genre = %q, want Jazz", p, fm.Tags.Genre)
		}
	}
}

// TestMultiRootScanJobReportsTheWhole: a scan over two roots, the last one empty, ends
// with one message for the whole run, and its stored result is the total with each
// root's run beside it, named by library.
func TestMultiRootScanJobReportsTheWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	for i := range 3 {
		writeFile(t, filepath.Join(rootA, fmt.Sprintf("%d.mp3", i)),
			testaudio.BuildMP3WithAudio(fmt.Sprintf("T%d", i), "Artist", "Album", i+1, testaudio.AudioWithSeed(byte(40+i))))
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: rootB, Mode: model.ModeInPlace},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	jobPID, err := lib.StartScan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("start scan: %v", err)
	}
	job := waitForJobDone(t, ctx, lib, jobPID)
	if job.Message != "scanned 3 files in 2 libraries" {
		t.Errorf("message = %q, want the whole run's", job.Message)
	}
	var res struct {
		scan.Result
		Runs []scan.Result
	}
	if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
		t.Fatalf("decode %q: %v", job.Result, err)
	}
	if res.ItemsCreated != 3 || len(res.Runs) != 2 {
		t.Fatalf("result = %+v, want 3 created over two runs", res)
	}
	for i, root := range []string{rootA, rootB} {
		if r := res.Runs[i]; r.LibraryPID != libraryPIDFor(t, ctx, lib, root) || r.LibraryName != root {
			t.Errorf("run %d = %+v, want library %s", i, r, root)
		}
	}
}

// TestMarkMissingIgnoresACopyOnAnUnpluggedDrive: an item whose primary is on disk is
// files-present even when a copy's library root is absent, and only the copies whose
// root is mounted are settled; the unreachable one stays.
func TestMarkMissingIgnoresACopyOnAnUnpluggedDrive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCopyFixture(t)
	drive := filepath.Join(t.TempDir(), "drive")
	offPath := filepath.Join(drive, "1.mp3")
	writeFile(t, offPath, testaudio.BuildMP3("Original", "Artist", "Album", 1))
	if _, err := f.lib.AddRoot(ctx, config.Root{Path: drive, Mode: model.ModeInPlace}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if refs := copyRoles(t, f.lib, f.item); len(refs) != 3 {
		t.Fatalf("edges = %+v, want the drive's copy attached", refs)
	}
	if err := os.RemoveAll(drive); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.lib.MarkMissing(ctx, f.item, waxbin.MarkMissingOptions{})
	if err != nil || outcome != model.OutcomeFilesPresent {
		t.Fatalf("mark-missing with a copy unplugged = %q (err %v), want files-present", outcome, err)
	}
	if err := os.Remove(f.copyPath); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.lib.MarkMissing(ctx, f.item, waxbin.MarkMissingOptions{})
	if err != nil || outcome != model.OutcomeDropped {
		t.Fatalf("mark-missing with a copy deleted = %q (err %v), want dropped", outcome, err)
	}
	refs := copyRoles(t, f.lib, f.item)
	if len(refs) != 2 || !slices.ContainsFunc(refs, func(r model.ItemFileRef) bool { return string(r.Path) == offPath }) {
		t.Errorf("edges = %+v, want the original and the unplugged copy", refs)
	}
}

// TestLibraryScopedFileOperationsKeepToTheirLibrary: the library field finds an item
// through a copy in that library, but an organize or a delete scoped to it acts only on
// items whose own primary lives there, never moving or deleting another library's files.
func TestLibraryScopedFileOperationsKeepToTheirLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(rootA, "loose", "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, filepath.Join(rootB, "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: rootB, Mode: model.ModeInPlace},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	inB := query.New(query.EntityItems).Where("library", query.OpIs, string(libraryPIDFor(t, ctx, lib, rootB))).Build()
	if items, err := lib.Query(ctx, inB, ""); err != nil || len(items) != 1 {
		t.Fatalf("items in B = %d (err %v), want the item found through its copy", len(items), err)
	}
	plan, err := lib.PlanOrganize(ctx, inB, waxbin.OrganizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("organize of B = %+v, want nothing planned for A's primary", plan.Actions)
	}
	del, err := lib.PlanDelete(ctx, inB, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	if len(del.Actions) != 0 {
		t.Errorf("delete of B = %+v, want nothing planned for an item whose primary is in A", del.Actions)
	}
}

// stageRecording writes one encoding of a recording tagged with its recording id, a song
// by a band on a record, into dir, and returns its path.
func stageRecording(t *testing.T, dir, format string, samples []float32, rec string) string {
	t.Helper()
	path := filepath.Join(dir, "song."+format)
	writeFile(t, path, testaudio.EncodeAs(t, format, "", 22050, samples))
	if _, err := meta.NewWriter().Apply(context.Background(), path, []meta.TagEdit{
		{Key: "TITLE", Values: []string{"Song"}}, {Key: "ARTIST", Values: []string{"Band"}},
		{Key: "ALBUM", Values: []string{"Record"}}, {Key: "TRACKNUMBER", Values: []string{"1"}},
		{Key: "MUSICBRAINZ_TRACKID", Values: []string{rec}},
	}); err != nil {
		t.Fatalf("tag %s: %v", path, err)
	}
	return path
}

// TestImportReportsATakeoverAsJoined: an imported encoding that outranks the one a
// recording's item holds takes the item over, and the report says the file joined it.
func TestImportReportsATakeoverAsJoined(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	samples := testaudio.RichSignal(22050, 2, testaudio.MusicalPartials, 51)
	const rec = "4e2b1b2a-0000-4000-8000-0000000000d1"
	lossy := t.TempDir()
	stageRecording(t, lossy, "mp3", samples, rec)
	importAll(t, ctx, lib, lossy, 1)
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want the recording", len(items), err)
	}
	lossless := t.TempDir()
	stageRecording(t, lossless, "flac", samples, rec)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: lossless})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || len(rep.Files) != 1 {
		t.Fatalf("ApplyImport: %+v, %v", rep, err)
	}
	if f := rep.Files[0]; f.Verdict != scan.VerdictJoined || f.ItemPID != items[0].PID || f.AttachedAsCopy {
		t.Errorf("flac import = %+v, want it to have joined %s as its primary", f, items[0].PID)
	}
	if refs := copyRoles(t, lib, items[0].PID); len(refs) != 2 || refs[0].Role != "primary" || filepath.Ext(refs[0].DisplayPath) != ".flac" {
		t.Errorf("edges = %+v, want the flac the primary", refs)
	}
}

// TestARipReadReportsItsFolds: a whole-file track a cue sheet then carves into a rip keeps
// its pid on the opening track, and the scan names the track it minted from it; with the
// sheet gone, the scan names the track it folded back into the whole file, which takes the
// opener's pid.
func TestARipReadReportsItsFolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := writeCueRip(t, filepath.Join(root, "rip"), 9, "One", "Two")
	sheet, err := os.ReadFile(filepath.Join(root, "rip", "album.cue"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "rip", "album.cue")); err != nil {
		t.Fatal(err)
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	whole := itemPIDByTitle(t, ctx, lib, "album")
	writeFile(t, filepath.Join(root, "rip", "album.cue"), sheet)
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan with the sheet: %v", err)
	}
	one, two := itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")
	if one != whole || len(res.Total.Created) != 1 || res.Total.Created[two] != whole || len(res.Total.Absorbed) != 0 {
		t.Fatalf("carving: one %s (whole %s), created %v absorbed %v, want Two minted from the whole file's item", one, whole,
			res.Total.Created, res.Total.Absorbed)
	}
	if err := os.Remove(filepath.Join(root, "rip", "album.cue")); err != nil {
		t.Fatal(err)
	}
	res, err = lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan without the sheet: %v", err)
	}
	if len(res.Total.Absorbed) != 1 || res.Total.Absorbed[two] != whole || len(res.Total.Created) != 0 {
		t.Errorf("collapsing: absorbed %v created %v, want Two folded into %s", res.Total.Absorbed, res.Total.Created, whole)
	}
	if items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), ""); err != nil || len(items) != 1 || items[0].PID != whole {
		t.Errorf("items = %+v (err %v), want the whole file under %s", items, err, whole)
	}
	_ = rip
}

// TestAScanThatFailsStillReportsItsFolds: a scan over several libraries that fails on one
// after another folded items away still answers, and records in its job, what it did to
// those items' pids.
func TestAScanThatFailsStillReportsItsFolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, other := t.TempDir(), t.TempDir()
	writeCueRip(t, filepath.Join(root, "rip"), 9, "One", "Two")
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: []config.Root{
		{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}, {Path: other, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	one, two := itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")
	if err := os.Remove(filepath.Join(root, "rip", "album.cue")); err != nil {
		t.Fatal(err)
	}
	// The sub-path lies under the first root, so the second library's scan refuses it.
	res, err := lib.Scan(ctx, waxbin.ScanRequest{SubPath: filepath.Join(root, "rip")})
	if err == nil {
		t.Fatal("scan succeeded, want the second library to refuse the sub-path")
	}
	if res == nil || res.Total.Absorbed[two] != one {
		t.Fatalf("result = %+v, want Two folded back into the whole file under %s", res, one)
	}
	if got := scanJobAbsorbed(t, ctx, lib, res.JobPID); got[two] != one {
		t.Errorf("job result absorbed = %v, want Two in %s", got, one)
	}
}

// scanJobAbsorbed reads the Absorbed map a scan job recorded.
func scanJobAbsorbed(t *testing.T, ctx context.Context, lib *waxbin.Library, job model.PID) map[model.PID]model.PID {
	t.Helper()
	j, err := lib.Job(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	var res scan.Result
	if err := json.Unmarshal([]byte(j.Result), &res); err != nil {
		t.Fatalf("job result %q: %v", j.Result, err)
	}
	return res.Absorbed
}
