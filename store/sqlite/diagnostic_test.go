package sqlite_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// TestScanDiagnosticsReplacedPerScan verifies the scan owns its origin's rows: a
// rescan that comes back clean clears the stale ones, and re-deriving the same
// diagnostic does not accumulate duplicates.
func TestScanDiagnosticsReplacedPerScan(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	in := input(lib.ID, "/lib/x.wma", "ess-x", "c-x", "Junk")
	in.Diagnostics = []model.FileDiagnostic{{
		Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
	}}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	// The file scope is the grain these assertions are about, so ask for it rather
	// than dumping the catalog and matching on display path.
	forFile := func(t *testing.T) []model.FileDiagnostic {
		t.Helper()
		ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: res.FilePID})
		if err != nil {
			t.Fatalf("FileDiagnostics: %v", err)
		}
		return ds
	}
	if got := forFile(t); len(got) != 1 || got[0].Code != model.DiagUnsupportedFormat {
		t.Fatalf("after scan: %+v", got)
	}

	// Re-deriving the same diagnostic must not double it.
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if got := forFile(t); len(got) != 1 || got[0].Code != model.DiagUnsupportedFormat {
		t.Fatalf("re-derive accumulated rows: %+v", got)
	}

	// A scan that comes back clean clears the scan's own rows.
	clean := input(lib.ID, "/lib/x.wma", "ess-x", "c-x", "Junk")
	if _, err := st.PutScannedTrack(ctx, clean); err != nil {
		t.Fatalf("clean rescan: %v", err)
	}
	if got := forFile(t); len(got) != 0 {
		t.Fatalf("clean rescan left stale rows: %+v", got)
	}
}

// TestDiagnosticsCrossWriterIsolation pins the property that made origin a writer
// identity rather than a phase: each writer replaces only its OWN rows, so a clean
// ReplayGain pass cannot erase what organize found. It is a schema property now (the
// primary key carries origin), but pin it so a refactor cannot quietly undo it.
func TestDiagnosticsCrossWriterIsolation(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}

	if err := st.PutFileDiagnostics(ctx, res.FilePID, model.OriginOrganize, []model.FileDiagnostic{{
		Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
		TagKey: "TRACKNUMBER", Detail: "value-dropped",
	}}); err != nil {
		t.Fatalf("PutFileDiagnostics(organize): %v", err)
	}
	// A clean replaygain pass replaces ITS rows with nothing.
	if err := st.PutFileDiagnostics(ctx, res.FilePID, model.OriginReplayGain, nil); err != nil {
		t.Fatalf("PutFileDiagnostics(replaygain): %v", err)
	}

	got, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: res.FilePID})
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	if len(got) != 1 || got[0].Origin != model.OriginOrganize || got[0].TagKey != "TRACKNUMBER" {
		t.Fatalf("organize row did not survive a clean replaygain pass: %+v", got)
	}
}

// TestDiagnosticsCascadeOnFileDelete verifies the ON DELETE CASCADE reaps rows with
// their file, so a removed file leaves no orphan diagnostics behind.
func TestDiagnosticsCascadeOnFileDelete(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	diagnosed := func(path, essence, content string) model.PID {
		in := input(lib.ID, path, essence, content, path)
		in.Diagnostics = []model.FileDiagnostic{{
			Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "truncated",
		}}
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		return res.FilePID
	}
	gone := diagnosed("/lib/gone.mp3", "ess-g", "c-g")
	// A second diagnosed file the cascade must leave alone. Without it, an over-broad
	// delete (or a cascade that reaped the wrong rows) would look identical to a
	// correct one from a catalog-wide count of zero.
	kept := diagnosed("/lib/kept.mp3", "ess-k", "c-k")

	if err := st.DetachFile(ctx, gone); err != nil {
		t.Fatalf("DetachFile: %v", err)
	}

	// The survivor's rows are intact, asserted by file rather than by count.
	survivors, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: kept})
	if err != nil {
		t.Fatalf("FileDiagnostics(kept): %v", err)
	}
	if len(survivors) != 1 || survivors[0].Code != model.DiagCorruptAudio {
		t.Errorf("the untouched file's diagnostics = %+v, want its one corrupt_audio row", survivors)
	}
	// And nothing else remains, which is what catches an orphan row: its file_id now
	// dangles, so no file-scoped read could ever reach it.
	n, err := st.CountFileDiagnostics(ctx)
	if err != nil {
		t.Fatalf("CountFileDiagnostics: %v", err)
	}
	if n != len(survivors) {
		t.Errorf("catalog holds %d diagnostics, want only the survivor's %d "+
			"(the detached file's rows should have cascaded)", n, len(survivors))
	}
}

// diagFixture is the seeded diagnostic corpus the filter tests slice: three tracks
// across two libraries plus a two-part audiobook, carrying five rows from two
// writers with mixed codes and severities.
type diagFixture struct {
	st        *sqlite.Store
	lib, lib2 *model.Library
	fileA     model.PID // /lib/a.wma, one unsupported_format row
	fileB     model.PID // /lib/b.mp3, the one file two writers both recorded against
	fileC     model.PID // /lib2/c.mp3, the only row in the second library
	bookItem  model.PID // the two-part audiobook
	bookPart2 model.PID // its second (non-primary) part, the file carrying a diagnostic
	bookPart1 model.PID // its primary part, which carries none
}

// diagnosticQueryFixture builds the corpus. The audiobook is what makes the item
// scope worth testing: its only diagnostic sits on the non-primary part, so an API
// that resolved an item to its representative file alone would return nothing.
func diagnosticQueryFixture(t *testing.T) diagFixture {
	t.Helper()
	ctx := context.Background()
	st, lib := openTestStore(t)
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}

	scanDiag := func(libID int64, path, essence, content string, ds ...model.FileDiagnostic) model.PID {
		in := input(libID, path, essence, content, path)
		in.Diagnostics = ds
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		return res.FilePID
	}
	fa := scanDiag(lib.ID, "/lib/a.wma", "e-a", "c-a", model.FileDiagnostic{
		Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
	})
	fb := scanDiag(lib.ID, "/lib/b.mp3", "e-b", "c-b", model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "truncated",
	})
	fc := scanDiag(lib2.ID, "/lib2/c.mp3", "e-c", "c-c", model.FileDiagnostic{
		Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
	})
	if err := st.PutFileDiagnostics(ctx, fb, model.OriginEdit, []model.FileDiagnostic{{
		Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: "read-only mount",
	}}); err != nil {
		t.Fatalf("edit diagnostics: %v", err)
	}

	// A two-part audiobook (both parts share one BookKey, so they join one item),
	// with the diagnostic on the second part only.
	bookPart := func(path, essence string, pos int, ds ...model.FileDiagnostic) model.PutScannedBookInput {
		return model.PutScannedBookInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
				Kind: model.FileAudio, Size: 10, MTimeNS: 1,
				ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindBook, State: model.StatePresent, Title: "Split Book",
				SortKey:     model.SortKey("Split Book"),
				IdentityKey: identity.BookKey("", "", "Book Author", "Split Book", ""),
			},
			Book: model.Book{
				Author: "Book Author", AuthorSort: model.SortKey("Book Author"),
				Authors: []string{"Book Author"},
			},
			Position:    pos,
			Diagnostics: ds,
		}
	}
	p1, err := st.PutScannedBook(ctx, bookPart("/lib/split-1.m4b", "e-bk1", 1))
	if err != nil {
		t.Fatalf("book part 1: %v", err)
	}
	p2, err := st.PutScannedBook(ctx, bookPart("/lib/split-2.m4b", "e-bk2", 2, model.FileDiagnostic{
		Code: model.DiagTagWriteLost, Severity: model.SeverityWarn, TagKey: "SUBTITLE", Detail: "value-dropped",
	}))
	if err != nil {
		t.Fatalf("book part 2: %v", err)
	}
	if p2.ItemPID != p1.ItemPID {
		t.Fatalf("book parts joined different items (%s vs %s); the item scope needs one item with two files",
			p1.ItemPID, p2.ItemPID)
	}
	return diagFixture{
		st: st, lib: lib, lib2: lib2, fileA: fa, fileB: fb, fileC: fc,
		bookItem: p1.ItemPID, bookPart1: p1.FilePID, bookPart2: p2.FilePID,
	}
}

// TestFileDiagnosticsFilters verifies each filter dimension and their
// conjunction, and that the zero filter still returns the full dump.
func TestFileDiagnosticsFilters(t *testing.T) {
	fx := diagnosticQueryFixture(t)
	st := fx.st
	ctx := context.Background()

	all, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("zero filter: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("zero filter rows = %d, want the full dump of 5", len(all))
	}

	// Each case asserts the rows themselves, keyed by (file pid, code), not merely
	// how many came back: a count alone would pass while selecting different rows.
	type row struct {
		file model.PID
		code model.DiagnosticCode
	}
	// Every case states its expected rows outright. Nothing is asserted by count
	// alone: a count matches while selecting entirely different rows, which is the
	// failure the display-path helper this test replaced could not catch either.
	cases := []struct {
		name   string
		filter model.DiagnosticFilter
		want   []row
	}{
		{"origin", model.DiagnosticFilter{Origin: model.OriginEdit},
			[]row{{fx.fileB, model.DiagTagWriteUnsynced}}},
		// Ordered by display path, so /lib/a.wma precedes /lib2/c.mp3.
		{"code", model.DiagnosticFilter{Code: model.DiagUnsupportedFormat},
			[]row{{fx.fileA, model.DiagUnsupportedFormat}, {fx.fileC, model.DiagUnsupportedFormat}}},
		{"severity", model.DiagnosticFilter{Severity: model.SeverityError},
			[]row{{fx.fileB, model.DiagCorruptAudio}}},
		{"library", model.DiagnosticFilter{LibraryPID: fx.lib2.PID},
			[]row{{fx.fileC, model.DiagUnsupportedFormat}}},
		// Both rows share a path, so they order by origin: edit before scan.
		{"file", model.DiagnosticFilter{FilePID: fx.fileB},
			[]row{{fx.fileB, model.DiagTagWriteUnsynced}, {fx.fileB, model.DiagCorruptAudio}}},
		// The case a FilePID-only API gets wrong: the book's only diagnostic sits on
		// its non-primary part, so resolving the item to its representative file would
		// return nothing here.
		{"item", model.DiagnosticFilter{ItemPID: fx.bookItem},
			[]row{{fx.bookPart2, model.DiagTagWriteLost}}},
		{"file + item", model.DiagnosticFilter{FilePID: fx.bookPart2, ItemPID: fx.bookItem},
			[]row{{fx.bookPart2, model.DiagTagWriteLost}}},
		// They AND like every other pair, so a file that does not back that item
		// selects nothing.
		{"file + item disjoint", model.DiagnosticFilter{FilePID: fx.fileB, ItemPID: fx.bookItem}, []row{}},
		// The book's primary part carries no rows of its own.
		{"file, primary book part", model.DiagnosticFilter{FilePID: fx.bookPart1}, []row{}},
		{"combined-empty", model.DiagnosticFilter{Code: model.DiagCorruptAudio, LibraryPID: fx.lib2.PID}, []row{}},
	}
	for _, tc := range cases {
		ds, err := st.FileDiagnostics(ctx, tc.filter)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := make([]row, 0, len(ds))
		for _, d := range ds {
			got = append(got, row{d.FilePID, d.Code})
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: rows = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// The scan-origin conjunction now spans both kinds: two tracks under lib plus the
	// book part.
	scanned, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginScan, LibraryPID: fx.lib.PID})
	if err != nil {
		t.Fatalf("scan + library: %v", err)
	}
	if len(scanned) != 3 {
		t.Errorf("scan + library rows = %d, want 3 (%+v)", len(scanned), scanned)
	}

	for name, bad := range map[string]model.DiagnosticFilter{
		"library": {LibraryPID: "nope"},
		"file":    {FilePID: "nope"},
		"item":    {ItemPID: "nope"},
	} {
		if _, err := st.FileDiagnostics(ctx, bad); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("unknown %s = %v, want CodeNotFound", name, err)
		}
		if _, err := st.DiagnosticSummary(ctx, bad); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("summary unknown %s = %v, want CodeNotFound", name, err)
		}
	}

	// A typo in an enum dimension fails closed rather than matching nothing.
	for name, bad := range map[string]model.DiagnosticFilter{
		"origin":   {Origin: "scam"},
		"code":     {Code: "tag_write_unsynched"},
		"severity": {Severity: "warning"},
	} {
		if _, err := st.FileDiagnostics(ctx, bad); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("unknown %s = %v, want CodeInvalid", name, err)
		}
		if _, err := st.DiagnosticSummary(ctx, bad); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("summary unknown %s = %v, want CodeInvalid", name, err)
		}
	}
}

// TestDiagnosticSummaryScopes pins that the summary inherits the file and item
// dimensions from the shared filter builder: "how bad is this one item" is answered
// here, over the same rows the list returns.
func TestDiagnosticSummaryScopes(t *testing.T) {
	fx := diagnosticQueryFixture(t)
	ctx := context.Background()

	byItem, err := fx.st.DiagnosticSummary(ctx, model.DiagnosticFilter{ItemPID: fx.bookItem})
	if err != nil {
		t.Fatalf("summary by item: %v", err)
	}
	if len(byItem) != 1 || byItem[0].Code != model.DiagTagWriteLost || byItem[0].Count != 1 {
		t.Errorf("summary by item = %+v, want one warn tag_write_lost bucket", byItem)
	}
	byFile, err := fx.st.DiagnosticSummary(ctx, model.DiagnosticFilter{FilePID: fx.fileB})
	if err != nil {
		t.Fatalf("summary by file: %v", err)
	}
	if len(byFile) != 2 {
		t.Errorf("summary by file = %+v, want two buckets (its two writers)", byFile)
	}
}

// TestFileDiagnosticsPaging verifies limit/offset windows tile the stable order
// exactly: concatenating pages reproduces the full dump.
func TestFileDiagnosticsPaging(t *testing.T) {
	st := diagnosticQueryFixture(t).st
	ctx := context.Background()

	all, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var paged []model.FileDiagnostic
	for offset := 0; ; offset += 3 {
		page, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("page at %d: %v", offset, err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
	}
	if len(paged) != len(all) {
		t.Fatalf("paged rows = %d, want %d", len(paged), len(all))
	}
	for i := range all {
		if paged[i] != all[i] {
			t.Errorf("row %d differs across paging: %+v vs %+v", i, paged[i], all[i])
		}
	}

	// Offset without a limit skips into the same order.
	tail, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{Offset: 2})
	if err != nil {
		t.Fatalf("offset-only: %v", err)
	}
	if len(tail) != len(all)-2 || tail[0] != all[2] {
		t.Errorf("offset-only tail = %d rows starting %+v, want %d starting %+v",
			len(tail), tail[0], len(all)-2, all[2])
	}
}

// TestDiagnosticSummary verifies the grouped counts and their most-severe-first
// order, and that the dimensional filters apply to the summary too.
func TestDiagnosticSummary(t *testing.T) {
	fx := diagnosticQueryFixture(t)
	st := fx.st
	ctx := context.Background()

	counts, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// Most severe band first, then origin, then code: error corrupt, the two warn
	// buckets (edit before scan), info unsupported x2.
	if len(counts) != 4 {
		t.Fatalf("buckets = %+v, want 4", counts)
	}
	if counts[0].Severity != model.SeverityError || counts[0].Code != model.DiagCorruptAudio || counts[0].Count != 1 {
		t.Errorf("first bucket = %+v, want the error band first", counts[0])
	}
	if counts[1].Severity != model.SeverityWarn || counts[1].Origin != model.OriginEdit ||
		counts[1].Code != model.DiagTagWriteUnsynced {
		t.Errorf("second bucket = %+v, want the edit-origin warn band", counts[1])
	}
	if counts[2].Severity != model.SeverityWarn || counts[2].Origin != model.OriginScan ||
		counts[2].Code != model.DiagTagWriteLost {
		t.Errorf("third bucket = %+v, want the scan-origin warn band", counts[2])
	}
	if counts[3].Severity != model.SeverityInfo || counts[3].Count != 2 {
		t.Errorf("fourth bucket = %+v, want info unsupported_format x2", counts[3])
	}

	scoped, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{LibraryPID: fx.lib.PID})
	if err != nil {
		t.Fatalf("scoped summary: %v", err)
	}
	for _, c := range scoped {
		if c.Code == model.DiagUnsupportedFormat && c.Count != 1 {
			t.Errorf("scoped unsupported count = %d, want 1 (the /lib2 file excluded)", c.Count)
		}
	}
}

// TestDiagnosticCoverageReflectsScan verifies the coverage count the audit reports:
// a file the scan has derived is not stale, which is what lets "no rows" mean
// "clean, and here is the coverage" rather than "not yet derived".
func TestDiagnosticCoverageReflectsScan(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	if _, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "A")); err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	stale, total, err := st.DiagnosticCoverage(ctx)
	if err != nil {
		t.Fatalf("DiagnosticCoverage: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if stale != 0 {
		t.Errorf("stale = %d, want 0: a full-path scan stamps diag_version", stale)
	}
}
