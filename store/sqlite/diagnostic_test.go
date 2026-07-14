package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
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
	ds, err := st.FileDiagnostics(ctx)
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
	ds, _ = st.FileDiagnostics(ctx)
	if got := diagsFor(t, ds, "/lib/x.wma"); len(got) != 1 {
		t.Fatalf("re-derive accumulated rows: %+v", got)
	}

	// A scan that comes back clean clears the scan's own rows.
	clean := input(lib.ID, "/lib/x.wma", "ess-x", "c-x", "Junk")
	if _, err := st.PutScannedTrack(ctx, clean); err != nil {
		t.Fatalf("clean rescan: %v", err)
	}
	ds, _ = st.FileDiagnostics(ctx)
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

	ds, err := st.FileDiagnostics(ctx)
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
	ds, err := st.FileDiagnostics(ctx)
	if err != nil {
		t.Fatalf("FileDiagnostics: %v", err)
	}
	if got := diagsFor(t, ds, "/lib/gone.mp3"); len(got) != 0 {
		t.Fatalf("diagnostics survived their file: %+v", got)
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
