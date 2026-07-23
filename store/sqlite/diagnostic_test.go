package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

func diagsFor(t *testing.T, ds []model.FileDiagnostic, path string) []model.FileDiagnostic {
	t.Helper()
	var out []model.FileDiagnostic
	for _, d := range ds {
		if d.DisplayPath == path {
			out = append(out, d)
		}
	}
	return out
}

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
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	if got := diagsFor(t, ds, "/lib/x.wma"); len(got) != 1 || got[0].Code != model.DiagUnsupportedFormat {
		t.Fatalf("after scan: %+v", got)
	}

	// Re-deriving the same diagnostic must not double it.
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	ds, _ = st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if got := diagsFor(t, ds, "/lib/x.wma"); len(got) != 1 {
		t.Fatalf("re-derive accumulated rows: %+v", got)
	}

	// A scan that comes back clean clears the scan's own rows.
	clean := input(lib.ID, "/lib/x.wma", "ess-x", "c-x", "Junk")
	if _, err := st.PutScannedTrack(ctx, clean); err != nil {
		t.Fatalf("clean rescan: %v", err)
	}
	ds, _ = st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if got := diagsFor(t, ds, "/lib/x.wma"); len(got) != 0 {
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

	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	got := diagsFor(t, ds, "/lib/a.mp3")
	if len(got) != 1 || got[0].Origin != model.OriginOrganize || got[0].TagKey != "TRACKNUMBER" {
		t.Fatalf("organize row did not survive a clean replaygain pass: %+v", got)
	}
}

// TestDiagnosticsCascadeOnFileDelete verifies the ON DELETE CASCADE reaps rows with
// their file, so a removed file leaves no orphan diagnostics behind.
func TestDiagnosticsCascadeOnFileDelete(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	in := input(lib.ID, "/lib/gone.mp3", "ess-g", "c-g", "Gone")
	in.Diagnostics = []model.FileDiagnostic{{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "truncated",
	}}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.DetachFile(ctx, res.FilePID); err != nil {
		t.Fatalf("DetachFile: %v", err)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	if got := diagsFor(t, ds, "/lib/gone.mp3"); len(got) != 0 {
		t.Fatalf("diagnostics survived their file: %+v", got)
	}
}

// diagnosticQueryFixture catalogs three files across two libraries and records
// diagnostics from two writers with mixed codes and severities, the corpus the
// filter tests slice.
func diagnosticQueryFixture(t *testing.T) (*sqlite.Store, *model.Library, *model.Library) {
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
	scanDiag(lib.ID, "/lib/a.wma", "e-a", "c-a", model.FileDiagnostic{
		Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
	})
	fb := scanDiag(lib.ID, "/lib/b.mp3", "e-b", "c-b", model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "truncated",
	})
	scanDiag(lib2.ID, "/lib2/c.mp3", "e-c", "c-c", model.FileDiagnostic{
		Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
	})
	if err := st.PutFileDiagnostics(ctx, fb, model.OriginEdit, []model.FileDiagnostic{{
		Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: "read-only mount",
	}}); err != nil {
		t.Fatalf("edit diagnostics: %v", err)
	}
	return st, lib, lib2
}

// TestFileDiagnosticsFilters verifies each filter dimension and their
// conjunction, and that the zero filter still returns the full dump.
func TestFileDiagnosticsFilters(t *testing.T) {
	st, lib, lib2 := diagnosticQueryFixture(t)
	ctx := context.Background()

	all, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("zero filter: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("zero filter rows = %d, want the full dump of 4", len(all))
	}

	cases := []struct {
		name   string
		filter model.DiagnosticFilter
		want   int
	}{
		{"origin", model.DiagnosticFilter{Origin: model.OriginEdit}, 1},
		{"code", model.DiagnosticFilter{Code: model.DiagUnsupportedFormat}, 2},
		{"severity", model.DiagnosticFilter{Severity: model.SeverityError}, 1},
		{"library", model.DiagnosticFilter{LibraryPID: lib2.PID}, 1},
		{"combined", model.DiagnosticFilter{Origin: model.OriginScan, LibraryPID: lib.PID}, 2},
		{"combined-empty", model.DiagnosticFilter{Code: model.DiagCorruptAudio, LibraryPID: lib2.PID}, 0},
	}
	for _, tc := range cases {
		ds, err := st.FileDiagnostics(ctx, tc.filter)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(ds) != tc.want {
			t.Errorf("%s: rows = %d, want %d (%+v)", tc.name, len(ds), tc.want, ds)
		}
	}

	if _, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{LibraryPID: "nope"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown library = %v, want CodeNotFound", err)
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

// TestFileDiagnosticsPaging verifies limit/offset windows tile the stable order
// exactly: concatenating pages reproduces the full dump.
func TestFileDiagnosticsPaging(t *testing.T) {
	st, _, _ := diagnosticQueryFixture(t)
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
	st, lib, _ := diagnosticQueryFixture(t)
	ctx := context.Background()

	counts, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(counts) != 3 {
		t.Fatalf("buckets = %+v, want 3 (error corrupt, warn unsynced, info unsupported x2)", counts)
	}
	if counts[0].Severity != model.SeverityError || counts[0].Code != model.DiagCorruptAudio || counts[0].Count != 1 {
		t.Errorf("first bucket = %+v, want the error band first", counts[0])
	}
	if counts[1].Severity != model.SeverityWarn || counts[1].Code != model.DiagTagWriteUnsynced {
		t.Errorf("second bucket = %+v, want the warn band", counts[1])
	}
	if counts[2].Severity != model.SeverityInfo || counts[2].Count != 2 {
		t.Errorf("third bucket = %+v, want info unsupported_format x2", counts[2])
	}

	scoped, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{LibraryPID: lib.PID})
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
