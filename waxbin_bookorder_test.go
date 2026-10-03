package waxbin_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// readingOrder returns the file names of the book titled title, in reading order.
func readingOrder(t *testing.T, ctx context.Context, lib *waxbin.Library, title string) []string {
	t.Helper()
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Where("title", query.OpIs, title).Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books titled %q = %d (err %v), want 1", title, len(books), err)
	}
	files, err := lib.ItemFiles(ctx, books[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f.DisplayPath))
	}
	return names
}

// organizeMoves returns the moves an organize plan would make.
func organizeMoves(t *testing.T, ctx context.Context, lib *waxbin.Library) []string {
	t.Helper()
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	var moves []string
	for _, a := range plan.Actions {
		if !a.Skip {
			moves = append(moves, filepath.Base(a.Src)+" -> "+a.RelDst)
		}
	}
	return moves
}

// mixedPart writes a part of "Mix Book" whose place is its track tag, or its file name
// when track is 0.
func mixedPart(t *testing.T, path, title string, track int, seed byte) {
	t.Helper()
	writeFile(t, path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Artist: "Author", Album: "Mix Book",
		Track: track, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(seed)}))
}

// TestImportedMixedBookKeepsItsReadingOrder: a book mixing parts its tags number with
// parts only their names place (two with no number, an epilogue) imports in its reading
// order and keeps it through organize and a forced rescan, the numbers in its new file
// names never read back as places.
func TestImportedMixedBookKeepsItsReadingOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	mixedPart(t, filepath.Join(staging, "Mix", "u1.mp3"), "", 0, 1)
	mixedPart(t, filepath.Join(staging, "Mix", "u2.mp3"), "", 0, 2)
	mixedPart(t, filepath.Join(staging, "Mix", "t1.mp3"), "Gamma", 1, 3)
	mixedPart(t, filepath.Join(staging, "Mix", "t2.mp3"), "Delta", 2, 4)
	mixedPart(t, filepath.Join(staging, "Mix", "Epilogue.mp3"), "", 0, 5)
	importAll(t, ctx, lib, staging, 5)
	want := []string{"Mix Book - 01.mp3", "Mix Book - 02.mp3", "Mix Book - 03.mp3", "Mix Book - 04.mp3", "Mix Book - 05.mp3"}
	if got := readingOrder(t, ctx, lib, "Mix Book"); !slices.Equal(got, want) {
		t.Fatalf("reading order after the import = %v, want %v", got, want)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := readingOrder(t, ctx, lib, "Mix Book"); !slices.Equal(got, want) {
		t.Errorf("reading order after a forced rescan = %v, want %v", got, want)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the rescan would move %v", moves)
	}
}

// TestOrganizedBookKeepsItsOrderOnRescan: a book organize named by reading order keeps
// that order when its files are read again, though their new names carry numbers its
// untagged parts never had.
func TestOrganizedBookKeepsItsOrderOnRescan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	mixedPart(t, filepath.Join(root, "in", "u1.mp3"), "Alpha", 0, 1)
	mixedPart(t, filepath.Join(root, "in", "u2.mp3"), "Beta", 0, 2)
	mixedPart(t, filepath.Join(root, "in", "t1.mp3"), "Gamma", 1, 3)
	mixedPart(t, filepath.Join(root, "in", "t2.mp3"), "Delta", 2, 4)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Errored != 0 {
		t.Fatalf("ApplyOrganize: %+v, %v", rep, err)
	}
	want := readingOrder(t, ctx, lib, "Mix Book")
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := readingOrder(t, ctx, lib, "Mix Book"); !slices.Equal(got, want) {
		t.Errorf("reading order after a forced rescan = %v, want %v as organize named it", got, want)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the rescan would move %v", moves)
	}
}

// TestSeriesNamedSingleFileBookKeepsItsName: a single-file book whose file name ends in
// a number ("Mistborn - 03", "Catch - 22") is not a numbered part, so organize and an
// import name it for its book alone.
func TestSeriesNamedSingleFileBookKeepsItsName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, acq := t.TempDir(), t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	writeFile(t, filepath.Join(root, "in", "Mistborn - 03.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Artist: "Brandon Sanderson", Album: "The Hero of Ages", TXXX: narrated, Audio: testaudio.AudioWithSeed(1)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 1 || filepath.Base(moves[0]) != "The Hero of Ages.mp3" {
		t.Errorf("organize moves = %v, want The Hero of Ages.mp3", moves)
	}
	src := filepath.Join(acq, "Catch - 22.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Joseph Heller", Album: "Catch-22",
		TXXX: narrated, Audio: testaudio.AudioWithSeed(2)}))
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if a := res.Plan.Actions[0]; filepath.Base(a.RelDst) != "Catch-22.mp3" {
		t.Errorf("import planned %s, want Catch-22.mp3", a.RelDst)
	}
}

// TestOrganizeNamesPartsByTheirTaggedTotal: the part total a book's files are tagged with
// pads their numbers and numbers a lone first part, for organize as for the import, so a
// long book imported a part at a time, and a book's first part imported before the rest,
// are named the same by both.
func TestOrganizeNamesPartsByTheirTaggedTotal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging, acq := t.TempDir(), t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i := 1; i <= 2; i++ {
		writeFile(t, filepath.Join(staging, "Long", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Artist: "Author", Album: "Long Book", Track: i, TrackTotal: 120, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i))}))
	}
	importAll(t, ctx, lib, staging, 2)
	if got, want := readingOrder(t, ctx, lib, "Long Book"), []string{"Long Book - 001.mp3", "Long Book - 002.mp3"}; !slices.Equal(got, want) {
		t.Errorf("long book parts = %v, want %v", got, want)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the long book's import would move %v", moves)
	}
	for i := 1; i <= 2; i++ {
		src := filepath.Join(acq, "part"+string(rune('0'+i))+".mp3")
		writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author", Album: "Two Book", Track: i,
			TrackTotal: 2, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(10 + i))}))
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
		if err != nil {
			t.Fatalf("ImportAcquired part %d: %v", i, err)
		}
		if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
			t.Fatalf("ApplyImport part %d: %+v, %v", i, rep, err)
		}
		if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
			t.Errorf("organize after part %d of Two Book would move %v", i, moves)
		}
	}
}

// TestBookEditKeepsItsPartTotal: a book reads its part total apart from a track's total,
// and an edit rewrites the book row from what it holds, the part total included.
func TestBookEditKeepsItsPartTotal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "in", "part1.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author",
		Album: "Two Book", Track: 1, TrackTotal: 2, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(books) != 1 || books[0].PartTotal != 2 || books[0].TrackTotal != 0 {
		t.Fatalf("books = %+v (err %v), want one with a part total of 2 and no track total", books, err)
	}
	if err := lib.EditField(ctx, books[0].PID, "publisher", "Press", waxbin.EditOptions{}); err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if v, err := lib.Get(ctx, books[0].PID); err != nil || v.PartTotal != 2 {
		t.Errorf("after the edit = %+v (err %v), want the part total kept", v, err)
	}
}

// TestOrganizeRenumbersABookInOneRun: renaming a book's parts onto each other's names,
// a chain (parts at places 2 to 4 named 01 to 03 by an older rule) or a swap (two parts
// trading numbers), lands in one run with nothing left over.
func TestOrganizeRenumbersABookInOneRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	part := func(rel string, track int, seed byte) {
		writeFile(t, filepath.Join(root, "Author", rel), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author",
			Album: filepath.Base(filepath.Dir(rel))[:5], Track: track, TXXX: narrated, Audio: testaudio.AudioWithSeed(seed)}))
	}
	part(filepath.Join("Chain {Reader}", "Chain - 01.mp3"), 2, 1)
	part(filepath.Join("Chain {Reader}", "Chain - 02.mp3"), 3, 2)
	part(filepath.Join("Chain {Reader}", "Chain - 03.mp3"), 4, 3)
	part(filepath.Join("Swaps {Reader}", "Swaps - 01.mp3"), 2, 4)
	part(filepath.Join("Swaps {Reader}", "Swaps - 02.mp3"), 1, 5)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	if plan.Pending() != 5 {
		t.Errorf("planned moves = %d, want one per part that moves", plan.Pending())
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Errored != 0 || rep.Moved != 5 {
		t.Fatalf("ApplyOrganize: %+v, %v", rep, err)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after one run would still move %v", moves)
	}
	if got, want := readingOrder(t, ctx, lib, "Chain"), []string{"Chain - 02.mp3", "Chain - 03.mp3", "Chain - 04.mp3"}; !slices.Equal(got, want) {
		t.Errorf("Chain = %v, want %v", got, want)
	}
	if got, want := readingOrder(t, ctx, lib, "Swaps"), []string{"Swaps - 01.mp3", "Swaps - 02.mp3"}; !slices.Equal(got, want) {
		t.Errorf("Swaps = %v, want %v", got, want)
	}
}

// TestCaseOnlyRenameMovesOnce: a part whose new path differs from its own only by case
// waits on no other move, so it moves once rather than through a parked name.
func TestCaseOnlyRenameMovesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i := 1; i <= 2; i++ {
		writeFile(t, filepath.Join(root, "author", "Tome {Reader}", "Tome - 0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(
			testaudio.MP3Spec{Artist: "Author", Album: "Tome", Track: i, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i))}))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	seq, _ := lib.LatestChangeSeq(ctx)
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Errored != 0 || rep.Moved != 2 {
		t.Fatalf("ApplyOrganize: %+v, %v", rep, err)
	}
	changes, err := lib.Changes(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, c := range changes {
		if c.EntityType == "file" {
			files++
		}
	}
	if files != 2 {
		t.Errorf("file changes = %d, want one move per part", files)
	}
}
