package waxbin_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// libraryPIDFor returns the pid of the library whose root is root.
func libraryPIDFor(t *testing.T, ctx context.Context, lib *waxbin.Library, root string) model.PID {
	t.Helper()
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.DisplayRoot == root {
			return l.PID
		}
	}
	t.Fatalf("no library at %s", root)
	return ""
}

// setReadOnly flags or clears a library, failing the test unless the answer is the
// library with the flag as asked.
func setReadOnly(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID, ro bool) {
	t.Helper()
	got, err := lib.SetLibraryReadOnly(ctx, pid, ro)
	if err != nil || got == nil || got.PID != pid || got.ReadOnly != ro {
		t.Fatalf("set read-only %v = %+v (err %v), want the library with the flag set", ro, got, err)
	}
}

// TestReadOnlyLibraryRefusesTheTagWriteBack: an edit in a read-only library lands in
// the catalog and leaves the file alone, reported and queued for review with its value
// still owed; once the flag clears, the next edit writes the file and pays it.
func TestReadOnlyLibraryRefusesTheTagWriteBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Album", Track: 1, BPM: "90"}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), true)

	err := lib.EditFields(ctx, pid, map[string]string{"bpm": "128"}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
	var wb *waxbin.WriteBackError
	if !errors.As(err, &wb) || len(wb.Failures) != 1 || wb.Failures[0].Path != src ||
		!strings.Contains(wb.Failures[0].Reason, "read-only library") {
		t.Fatalf("edit in a read-only library = %v, want the file refused as read-only", err)
	}
	if v, err := lib.Get(ctx, pid); err != nil || v.BPM != 128 {
		t.Fatalf("catalog BPM = %d (err %v), want the edit to stand", v.BPM, err)
	}
	if fm, err := meta.NewReader().Read(ctx, src); err != nil || fm.Tags.BPM != 90 {
		t.Fatalf("on-disk BPM = %d (err %v), want the file untouched", fm.Tags.BPM, err)
	}
	ds, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginEdit, Code: model.DiagTagWriteUnsynced})
	if err != nil || len(ds) != 1 || !strings.Contains(ds[0].Detail, "read-only library") {
		t.Fatalf("edit diagnostics = %+v (err %v), want the refusal queued for review", ds, err)
	}
	if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"bpm"}) {
		t.Fatalf("owed after the refusal = %v, want [bpm]", got)
	}

	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), false)
	if err := lib.EditFields(ctx, pid, map[string]string{"bpm": "130"}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true, Force: true}); err != nil {
		t.Fatalf("edit once writable: %v", err)
	}
	if fm, err := meta.NewReader().Read(ctx, src); err != nil || fm.Tags.BPM != 130 {
		t.Fatalf("on-disk BPM = %d (err %v), want the edit written", fm.Tags.BPM, err)
	}
	if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
		t.Fatalf("owed after the write landed = %v, want none", got)
	}
}

// TestReadOnlyLibraryRefusesTheCustomTagWriteBack: a custom tag set with write-back in a
// read-only library lands in the catalog alone, with the refusal queued for review and the
// value owed, and once the flag clears the same set writes the file and pays it.
func TestReadOnlyLibraryRefusesTheCustomTagWriteBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Album"}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), true)
	mood := func() []string {
		t.Helper()
		fm, err := meta.NewReader().Read(ctx, src)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return fm.Tags.Custom["MOOD"]
	}

	_, _, err := lib.SetItemTag(ctx, pid, "MOOD", []string{"calm"}, waxbin.TagEditOptions{Lock: model.LockOn, WriteBack: true})
	var wb *waxbin.WriteBackError
	if !errors.As(err, &wb) || len(wb.Failures) != 1 || !strings.Contains(wb.Failures[0].Reason, "read-only library") {
		t.Fatalf("set in a read-only library = %v, want the file refused as read-only", err)
	}
	if got := mood(); got != nil {
		t.Fatalf("MOOD on disk = %v, want the file untouched", got)
	}
	ds, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginEdit, Code: model.DiagTagWriteUnsynced})
	if err != nil || len(ds) != 1 || !strings.Contains(ds[0].Detail, "read-only library") {
		t.Fatalf("edit diagnostics = %+v (err %v), want the refusal queued for review", ds, err)
	}
	if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"tag.MOOD"}) {
		t.Fatalf("owed after the refusal = %v, want [tag.MOOD]", got)
	}

	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), false)
	if _, _, err := lib.SetItemTag(ctx, pid, "MOOD", []string{"calm"}, waxbin.TagEditOptions{Lock: model.LockOn, WriteBack: true, Force: true}); err != nil {
		t.Fatalf("set once writable: %v", err)
	}
	if got := mood(); !slices.Equal(got, []string{"calm"}) {
		t.Fatalf("MOOD on disk = %v, want [calm] written", got)
	}
	if got := owedOn(t, ctx, lib, pid); got != nil {
		t.Fatalf("owed after the write landed = %v, want none", got)
	}
	if ds, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginEdit, Code: model.DiagTagWriteUnsynced}); err != nil || len(ds) != 0 {
		t.Errorf("drift after the write landed = %+v (err %v), want it cleared", ds, err)
	}
}

// TestEnrichWriteTagsLeavesAReadOnlyLibraryOwed: an enrichment pass that writes tags
// writes the writable library's files and leaves the read-only one's owed, and the
// first pass after the flag clears writes those.
func TestEnrichWriteTagsLeavesAReadOnlyLibraryOwed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	srcA, srcB := filepath.Join(rootA, "a.mp3"), filepath.Join(rootB, "b.mp3")
	writeFile(t, srcA, testaudio.BuildMP3WithAudio("A", "Band", "One", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, srcB, testaudio.BuildMP3WithAudio("B", "Band", "Two", 1, testaudio.AudioWithSeed(2)))
	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRecording {
				return &enrich.Candidate{Fields: map[string]string{"bpm": "120"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: rootB, Mode: model.ModeManaged, Profile: "waxbin-native"},
		},
		EnrichmentProviders: []enrich.Provider{fields},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	scanLib(t, ctx, lib)
	pidB := libraryPIDFor(t, ctx, lib, rootB)
	setReadOnly(t, ctx, lib, pidB, true)
	bpm := func(path string) int {
		t.Helper()
		fm, err := meta.NewReader().Read(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return fm.Tags.BPM
	}

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TrackFieldsEnriched != 2 || res.Result.TagsWritten != 1 || res.Result.TagsReadOnly != 1 ||
		bpm(srcA) != 120 || bpm(srcB) != 0 {
		t.Fatalf("first pass = %+v with bpm %d/%d, want both filled, only the writable file written, and one held back",
			res.Result, bpm(srcA), bpm(srcB))
	}

	setReadOnly(t, ctx, lib, pidB, false)
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TagsWritten != 1 || res.Result.TagsReadOnly != 0 || bpm(srcB) != 120 {
		t.Fatalf("pass after clearing = %+v with bpm %d, want the owed file written", res.Result, bpm(srcB))
	}
}

// twoLibraries opens two managed libraries with one scanned track each, returning the
// tracks' paths and pids and the library pids, A first. A is mixed and B holds music,
// so an acquired track routes to B.
func twoLibraries(t *testing.T) (lib *waxbin.Library, srcs [2]string, items, libs [2]model.PID) {
	t.Helper()
	ctx := context.Background()
	roots := [2]string{t.TempDir(), t.TempDir()}
	var cfg []config.Root
	for i, root := range roots {
		srcs[i] = filepath.Join(root, "song.mp3")
		writeFile(t, srcs[i], testaudio.BuildMP3WithAudio("Song "+string(rune('A'+i)), "Band", "Album "+string(rune('A'+i)), 1, testaudio.AudioWithSeed(byte(i+1))))
		cfg = append(cfg, config.Root{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"})
	}
	cfg[1].Media = model.MediaMusic
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	for i, root := range roots {
		items[i] = itemPIDByTitle(t, ctx, lib, "Song "+string(rune('A'+i)))
		libs[i] = libraryPIDFor(t, ctx, lib, root)
	}
	return lib, srcs, items, libs
}

// TestReadOnlyLibraryKeepsItsTrash: files trashed from a library that is now read-only
// are neither purged nor restored until the flag clears.
func TestReadOnlyLibraryKeepsItsTrash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, items, libs := twoLibraries(t)
	plan, err := lib.PlanDeletePIDs(ctx, items[:], model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatal(err)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("trash = %+v (err %v), want two entries", entries, err)
	}
	var entryB model.TrashEntry
	for _, e := range entries {
		if e.ItemPID == items[1] {
			entryB = e
		}
	}
	setReadOnly(t, ctx, lib, libs[1], true)

	if _, err := lib.PurgeTrash(ctx, entryB.PID); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("purge from a read-only library = %v, want CodeLocked", err)
	}
	if err := lib.RestoreTrash(ctx, entryB.PID); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("restore into a read-only library = %v, want CodeLocked", err)
	}
	rep, err := lib.EmptyTrash(ctx, waxbin.EmptyTrashOptions{})
	if err != nil || rep.Purged != 1 || rep.SkippedReadOnly != 1 {
		t.Fatalf("empty trash = %+v (err %v), want one purged and one skipped read-only", rep, err)
	}
	if !fileExists(entryB.TrashDisplay) {
		t.Fatal("the read-only library's trashed file was purged")
	}

	setReadOnly(t, ctx, lib, libs[1], false)
	if err := lib.RestoreTrash(ctx, entryB.PID); err != nil {
		t.Fatalf("restore once writable: %v", err)
	}
}

// TestReadOnlyLibraryRefusesDeletes: a sweep plans nothing in a read-only library and
// says so, and naming its item is refused.
func TestReadOnlyLibraryRefusesDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, items, libs := twoLibraries(t)
	setReadOnly(t, ctx, lib, libs[1], true)
	plan, err := lib.PlanDelete(ctx, query.New(query.EntityItems).Build(), model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].ItemPID != items[0] || plan.SkippedReadOnly != 1 {
		t.Fatalf("sweep plan = %+v, want the writable item alone and one skipped read-only", plan)
	}
	if _, err := lib.PlanDeletePIDs(ctx, []model.PID{items[1]}, model.DeleteTrash); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("deleting a read-only library's item = %v, want CodeLocked", err)
	}
}

// TestDeletePlanAppliedAfterTheFlag: a plan built while the library was writable is
// checked again when applied, and its actions there are skipped.
func TestDeletePlanAppliedAfterTheFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, srcs, items, libs := twoLibraries(t)
	plan, err := lib.PlanDeletePIDs(ctx, items[:], model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	setReadOnly(t, ctx, lib, libs[1], true)
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil || rep.Trashed != 1 || rep.Skipped != 1 {
		t.Fatalf("apply = %+v (err %v), want one trashed and one skipped", rep, err)
	}
	if fileExists(srcs[0]) || !fileExists(srcs[1]) {
		t.Fatalf("after apply: A present %v, B present %v, want only B left in place", fileExists(srcs[0]), fileExists(srcs[1]))
	}
}

// TestOrganizeLeavesAReadOnlyLibraryAlone: a plan covers the writable library alone and
// counts the read-only one it passed over.
func TestOrganizeLeavesAReadOnlyLibraryAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, items, libs := twoLibraries(t)
	setReadOnly(t, ctx, lib, libs[1], true)
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].ItemPID != items[0] || plan.ReadOnlyLibraries != 1 {
		t.Fatalf("plan = %+v, want the writable item alone and one read-only library passed over", plan)
	}
}

// TestOrganizePlanAppliedAfterTheFlag: a plan built while the library was writable is
// checked again when applied, and its moves there are skipped.
func TestOrganizePlanAppliedAfterTheFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, srcs, _, libs := twoLibraries(t)
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil || plan.Pending() != 2 {
		t.Fatalf("plan = %+v (err %v), want two moves", plan, err)
	}
	setReadOnly(t, ctx, lib, libs[1], true)
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil || rep.Moved != 1 || rep.Skipped != 1 {
		t.Fatalf("apply = %+v (err %v), want one moved and one skipped", rep, err)
	}
	if fileExists(srcs[0]) || !fileExists(srcs[1]) {
		t.Fatalf("after apply: A in place %v, B in place %v, want only B left", fileExists(srcs[0]), fileExists(srcs[1]))
	}
}

// openMusicAndMixed opens a music root and a mixed root, both managed, returning the
// library and both library pids (music first).
func openMusicAndMixed(t *testing.T) (*waxbin.Library, [2]model.PID) {
	t.Helper()
	ctx := context.Background()
	music, mixed := t.TempDir(), t.TempDir()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic},
			{Path: mixed, Mode: model.ModeManaged, Media: model.MediaMixed},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib, [2]model.PID{libraryPIDFor(t, ctx, lib, music), libraryPIDFor(t, ctx, lib, mixed)}
}

// TestImportWaitsOnAReadOnlyLibrary: the flag never changes where a file goes. A staged
// track bound for the read-only music root is quarantined in place naming that root
// rather than sent to the writable mixed one, an acquired track is refused the same
// way, and so is naming the read-only root as the target.
func TestImportWaitsOnAReadOnlyLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, libs := openMusicAndMixed(t)
	staging := t.TempDir()
	writeFile(t, filepath.Join(staging, "song.mp3"), testaudio.BuildMP3("Song", "Band", "Album", 1))
	setReadOnly(t, ctx, lib, libs[0], true)

	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("plan import: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Importable() != 0 || !strings.Contains(plan.Actions[0].Reason, string(libs[0])) {
		t.Fatalf("plan = %+v, want the track quarantined naming the read-only music root", plan.Actions)
	}
	if _, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging, LibraryPID: libs[0]}); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("importing into the read-only root = %v, want CodeLocked", err)
	}
	_, err = lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: filepath.Join(staging, "song.mp3")}, model.KindTrack, waxbin.AcquiredMeta{})
	if !waxerr.Is(err, waxerr.CodeLocked) || !strings.Contains(err.Error(), string(libs[0])) {
		t.Fatalf("acquired track = %v, want CodeLocked naming the music root", err)
	}

	setReadOnly(t, ctx, lib, libs[0], false)
	plan, err = lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil || plan.Importable() != 1 || plan.Actions[0].Library.PID != libs[0] {
		t.Fatalf("plan after clearing = %+v (err %v), want the track bound for the music root", plan, err)
	}
	setReadOnly(t, ctx, lib, libs[0], true)
	setReadOnly(t, ctx, lib, libs[1], true)
	if _, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging}); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("import with every root read-only = %v, want CodeLocked", err)
	}
}

// TestImportPlanAppliedAfterTheFlag: a plan built while its target was writable is
// checked again when applied, and its files are quarantined in place.
func TestImportPlanAppliedAfterTheFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	src := filepath.Join(staging, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Song", "Band", "Album", 1))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil || plan.Importable() != 1 {
		t.Fatalf("plan = %+v (err %v), want one importable file", plan, err)
	}
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), true)
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 0 || rep.Quarantined != 1 {
		t.Fatalf("apply = %+v (err %v), want the file quarantined", rep, err)
	}
	if !fileExists(src) {
		t.Fatal("the staged file left staging")
	}
}

// TestImportLeavesAReadOnlyRootsFilesInPlace: an import that would move files out of a
// read-only library quarantines them, at plan time and again when an older plan is
// applied; a copy may still read them, and an acquired episode is refused.
func TestImportLeavesAReadOnlyRootsFilesInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, srcs, _, libs := twoLibraries(t)
	incoming := filepath.Join(filepath.Dir(srcs[0]), "incoming")
	staged := filepath.Join(incoming, "new.mp3")
	writeFile(t, staged, testaudio.BuildMP3WithAudio("New", "Band", "Fresh", 1, testaudio.AudioWithSeed(9)))
	req := waxbin.ImportRequest{Source: incoming, LibraryPID: libs[1]}

	plan, err := lib.PlanImport(ctx, req)
	if err != nil || plan.Importable() != 1 {
		t.Fatalf("plan while writable = %+v (err %v), want one importable file", plan, err)
	}
	setReadOnly(t, ctx, lib, libs[0], true)
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 0 || rep.Quarantined != 1 || !fileExists(staged) {
		t.Fatalf("applying an older move plan = %+v (err %v), want the file quarantined in place", rep, err)
	}

	plan, err = lib.PlanImport(ctx, req)
	if err != nil || plan.Importable() != 0 || !strings.Contains(plan.Actions[0].Reason, "read-only") {
		t.Fatalf("move plan out of a read-only library = %+v (err %v), want the file quarantined", plan.Actions, err)
	}
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: staged}, model.KindTrack, waxbin.AcquiredMeta{})
	if err != nil || res.Plan.Importable() != 0 {
		t.Fatalf("acquired move out of a read-only library = %+v (err %v), want it quarantined", res, err)
	}
	if _, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: staged}, model.KindEpisode, waxbin.AcquiredMeta{ShowTitle: "Show"}); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("acquired episode moved out of a read-only library = %v, want CodeLocked", err)
	}

	req.Copy = true
	plan, err = lib.PlanImport(ctx, req)
	if err != nil || plan.Importable() != 1 {
		t.Fatalf("copy plan = %+v (err %v), want the file importable", plan, err)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 1 || !fileExists(staged) {
		t.Fatalf("copy import = %+v (err %v), want it imported with the source left in place", rep, err)
	}
}

// TestImportSourceSpelledAnotherWayStaysInPlace: the caller's spelling of a source
// does not matter. A relative path, or one that reaches the read-only root through a
// symlink, is still a move out of that library.
func TestImportSourceSpelledAnotherWayStaysInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, srcs, _, libs := twoLibraries(t)
	root := filepath.Dir(srcs[0])
	writeFile(t, filepath.Join(root, "incoming", "new.mp3"),
		testaudio.BuildMP3WithAudio("New", "Band", "Fresh", 1, testaudio.AudioWithSeed(9)))
	setReadOnly(t, ctx, lib, libs[0], true)

	spellings := map[string]string{}
	wd, err := os.Getwd()
	if err == nil {
		wd, err = filepath.Rel(wd, filepath.Join(root, "incoming"))
	}
	if err == nil {
		spellings["relative"] = wd
	} else {
		t.Logf("no relative case: %v", err)
	}
	link := filepath.Join(t.TempDir(), "lib")
	if err := os.Symlink(root, link); err == nil {
		spellings["symlinked"] = filepath.Join(link, "incoming")
	} else {
		t.Logf("no symlink case: %v", err)
	}
	for name, src := range spellings {
		plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: src, LibraryPID: libs[1]})
		if err != nil || len(plan.Actions) != 1 || plan.Importable() != 0 {
			t.Fatalf("%s: move plan = %+v (err %v), want the file quarantined", name, plan, err)
		}
		file := waxbin.AcquiredFile{Path: filepath.Join(src, "new.mp3")}
		res, err := lib.ImportAcquired(ctx, file, model.KindTrack, waxbin.AcquiredMeta{})
		if err != nil || res.Plan.Importable() != 0 {
			t.Fatalf("%s: acquired move = %+v (err %v), want it quarantined", name, res, err)
		}
		if _, err := lib.ImportAcquired(ctx, file, model.KindEpisode, waxbin.AcquiredMeta{ShowTitle: "Show"}); !waxerr.Is(err, waxerr.CodeLocked) {
			t.Fatalf("%s: acquired episode = %v, want CodeLocked", name, err)
		}
	}
}

// TestEpisodeFileStaysInAReadOnlyLibrary: the podcast service's own import verb is held
// to the flag too, since a host can call it without going through ImportAcquired. A
// move out of a read-only library is refused and a copy is not.
func TestEpisodeFileStaysInAReadOnlyLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "ep.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ep", "Host", "Show", 1))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   filepath.Join(t.TempDir(), "catalog.db"),
		Roots:    []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Podcasts: config.PodcastConfig{Dir: filepath.Join(t.TempDir(), "podcasts")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	show, err := lib.Podcasts().AddManual(ctx, "Show", podcast.ManualOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), true)

	if _, err := lib.Podcasts().ImportEpisodeFile(ctx, ep.EpisodePID, src, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("moving an episode file out of a read-only library = %v, want CodeLocked", err)
	}
	if !fileExists(src) {
		t.Fatal("the file left the read-only library")
	}
	if _, err := lib.Podcasts().ImportEpisodeFile(ctx, ep.EpisodePID, src, true); err != nil || !fileExists(src) {
		t.Fatalf("copying it = %v, want the copy made and the file left in place", err)
	}
}

// TestRoutedImportNamesTheReadOnlyLibrary: a staged track whose only library is
// read-only is quarantined saying so, not as though no library took tracks.
func TestRoutedImportNamesTheReadOnlyLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	music, books, staging := t.TempDir(), t.TempDir(), t.TempDir()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic},
			{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	writeFile(t, filepath.Join(staging, "song.mp3"), testaudio.BuildMP3("Song", "Band", "Album", 1))
	musicPID := libraryPIDFor(t, ctx, lib, music)
	setReadOnly(t, ctx, lib, musicPID, true)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || !strings.Contains(plan.Actions[0].Reason, "read-only") ||
		!strings.Contains(plan.Actions[0].Reason, string(musicPID)) {
		t.Fatalf("plan = %+v, want the track quarantined naming the read-only music library", plan.Actions)
	}
}

// TestTrashEntryWithNoRecordedLibraryIsMatchedByPath: a journal row that names no
// library is matched to one by its original path, so it is still kept out of a
// read-only library's reach.
func TestTrashEntryWithNoRecordedLibraryIsMatchedByPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Song", "Band", "Album", 1))
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPIDByTitle(t, ctx, lib, "Song")}, model.DeleteTrash)
	if err == nil {
		_, err = lib.ApplyDelete(ctx, plan)
	}
	if err != nil {
		t.Fatal(err)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v)", entries, err)
	}
	rawExec(t, db, "UPDATE trash SET library_id = NULL")
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, root), true)
	if _, err := lib.PurgeTrash(ctx, entries[0].PID); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("purging an entry with no recorded library = %v, want CodeLocked by its path", err)
	}
}
