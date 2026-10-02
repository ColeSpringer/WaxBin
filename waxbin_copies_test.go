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
// readOnlyPrimary read-only, and returns the library, the item, and the file pids.
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

// TestTitleSortClearFollowsThePrimary: a title write-back clears TITLESORT from every file
// it reaches. The catalog follows the primary, whose tags own the item: when a copy in a
// read-only library refuses the clear, the catalog drops TITLESORT and the copy owes the
// clear; when the primary refuses, the catalog keeps it and the copy that dropped it owes
// it back.
func TestTitleSortClearFollowsThePrimary(t *testing.T) {
	ctx := context.Background()
	hasTitleSort := func(lib *waxbin.Library, pid model.PID) bool {
		t.Helper()
		tags, err := lib.ItemTags(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(tags, func(it model.ItemTag) bool { return it.Key == "TITLESORT" })
	}
	for _, readOnlyPrimary := range []bool{false, true} {
		lib, pid, primary, copy := titleSortCopies(t, readOnlyPrimary)
		err := lib.EditFields(ctx, pid, map[string]string{"title": "New"}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
		var wbErr *waxbin.WriteBackError
		if !errors.As(err, &wbErr) || len(wbErr.Failures) != 1 {
			t.Fatalf("edit (read-only primary %v) = %v, want one refusal", readOnlyPrimary, err)
		}
		if got := hasTitleSort(lib, pid); got != readOnlyPrimary {
			t.Errorf("read-only primary %v: catalog keeps TITLESORT = %v, want %v", readOnlyPrimary, got, readOnlyPrimary)
		}
		owed := owedKeys(t, lib, copy)
		if !slices.Contains(owed, "tag.TITLESORT") {
			t.Errorf("read-only primary %v: the copy owes %v, want tag.TITLESORT among them", readOnlyPrimary, owed)
		}
		if got := owedKeys(t, lib, primary); !readOnlyPrimary && len(got) != 0 {
			t.Errorf("the primary owes %v, want nothing once the catalog dropped TITLESORT", got)
		}
	}
}

// TestCopyWriteBackReachesTheCopy: a written-back edit lands in the copy too, so the
// copy's tags do not drift from the item they back.
func TestCopyWriteBackReachesTheCopy(t *testing.T) {
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
