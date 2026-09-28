package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// putAnalyzed stores an analysis the way the analyze pass does, with what its decode
// observed about the bytes.
func putAnalyzed(t *testing.T, st *Store, filePID model.PID, essence string, observed bool, ds ...model.FileDiagnostic) {
	t.Helper()
	if err := st.PutAnalysis(context.Background(), model.AnalysisInput{
		AnalysisVersion: 1,
		Fingerprint:     model.FingerprintInput{FilePID: filePID, EssenceHash: essence, AlgoVersion: 1, FP: []byte{}},
		Observed:        observed,
		Diagnostics:     ds,
	}); err != nil {
		t.Fatalf("put analysis: %v", err)
	}
}

// originRows lists one writer's diagnostics for a file as code/severity/detail.
func originRows(t *testing.T, st *Store, filePID model.PID, origin model.DiagnosticOrigin) []string {
	t.Helper()
	ds, err := st.FileDiagnostics(context.Background(), model.DiagnosticFilter{FilePID: filePID, Origin: origin})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	var out []string
	for _, d := range ds {
		out = append(out, string(d.Code)+"/"+string(d.Severity)+"/"+d.Detail)
	}
	return out
}

// TestPutAnalysisOwnsTheAnalyzeOrigin: a decode that looked at the bytes replaces what
// the analyze origin recorded for the file, and a clean one clears it, while the scan's
// rows belong to another writer and stay. A decode that never looked (an unsupported
// input, an IO error, a cancel) leaves the prior verdict standing.
func TestPutAnalysisOwnsTheAnalyzeOrigin(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	if err := st.PutFileDiagnostics(ctx, r.FilePID, model.OriginScan, []model.FileDiagnostic{{
		Code: model.DiagCorruptAudio, Severity: model.SeverityWarn, Detail: "truncated audio",
	}}); err != nil {
		t.Fatalf("scan diagnostics: %v", err)
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	scanRow := "corrupt_audio/warn/truncated audio"

	putAnalyzed(t, st, r.FilePID, "e1", true, model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityWarn, Detail: "lost sync"})
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); !same(got, "corrupt_audio/warn/lost sync") {
		t.Errorf("analyze rows after a damaged decode = %q", got)
	}

	putAnalyzed(t, st, r.FilePID, "e1", false)
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); !same(got, "corrupt_audio/warn/lost sync") {
		t.Errorf("analyze rows after a decode that never looked = %q, want the prior verdict", got)
	}

	putAnalyzed(t, st, r.FilePID, "e1", true, model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "bad frame"})
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); !same(got, "corrupt_audio/error/bad frame") {
		t.Errorf("analyze rows after a failed decode = %q, want the new verdict alone", got)
	}

	putAnalyzed(t, st, r.FilePID, "e1", true)
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); len(got) != 0 {
		t.Errorf("analyze rows after a clean decode = %q, want none", got)
	}
	if got := originRows(t, st, r.FilePID, model.OriginScan); !same(got, scanRow) {
		t.Errorf("scan rows = %q, want the scan's own row untouched", got)
	}
}

// TestPutAnalysisLeavesTheDiagnosticStampAlone: only the scan says a file's
// diagnostics were derived under the current rules, so an analyze verdict must not
// mark a file the scan never derived as current.
func TestPutAnalysisLeavesTheDiagnosticStampAlone(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	if _, err := st.write.ExecContext(ctx, "UPDATE file SET diag_version = 0 WHERE pid = ?", string(r.FilePID)); err != nil {
		t.Fatal(err)
	}
	putAnalyzed(t, st, r.FilePID, "e1", true, model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityWarn, Detail: "lost sync"})
	stale, total, err := st.DiagnosticCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stale != 1 || total != 1 {
		t.Errorf("coverage after an analyze verdict = %d stale of %d, want 1 of 1", stale, total)
	}
}

// TestFilesDurationMismatch: a file whose header states a length its decoded audio
// does not have is reported, once the gap passes both two seconds and two percent of
// the header's length. A file with no header duration has nothing to compare, and one
// whose waveform predates its current audio says nothing about it.
func TestFilesDurationMismatch(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	file := func(name string, headerMS int64, decodedFrames int64) model.PID {
		t.Helper()
		r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + name, essence: "e-" + name, content: "c-" + name,
			title: name, artist: "X", album: "Al", durationMS: headerMS})
		putPeaks(t, st, r.FilePID, "e-"+name, []byte{1, 0}, decodedFrames, 8000)
		return r.FilePID
	}
	short := file("a-short.mp3", 60_000, 800_000)  // header 60 s, decoded 100 s
	file("b-close.flac", 180_000, 1_452_000)       // 181.5 s: under the 2 s floor
	file("c-long.flac", 600_000, 4_880_000)        // 610 s: under 2% of 10 min
	file("d-noheader.mp3", 0, 800_000)             // nothing to compare
	file("e-stale.mp3", 60_000, 800_000)           // its waveform goes stale below
	long := file("f-long.mp3", 300_000, 1_600_000) // header 300 s, decoded 200 s
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/e-stale.mp3", essence: "e-new", content: "c-new",
		title: "e-stale.mp3", artist: "X", album: "Al", durationMS: 60_000})

	got, total, err := st.FilesDurationMismatch(ctx, 10)
	if err != nil {
		t.Fatalf("FilesDurationMismatch: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("mismatches = %+v (total %d), want a-short and f-long", got, total)
	}
	if got[0].FilePID != short || got[0].DisplayPath != "/lib/a-short.mp3" || got[0].HeaderMS != 60_000 || got[0].DecodedMS != 100_000 {
		t.Errorf("first = %+v, want a-short at 60000 ms header, 100000 ms decoded", got[0])
	}
	if got[1].FilePID != long || got[1].HeaderMS != 300_000 || got[1].DecodedMS != 200_000 {
		t.Errorf("second = %+v, want f-long at 300000 ms header, 200000 ms decoded", got[1])
	}

	got, total, err = st.FilesDurationMismatch(ctx, 1)
	if err != nil || total != 2 || len(got) != 1 || got[0].FilePID != short {
		t.Errorf("sample of 1 = %+v (total %d, err %v), want a-short of 2", got, total, err)
	}
}

// TestAnalyzeVerdictFollowsTheEssence: an analyze verdict describes the audio it read, so
// once the file's essence moves on (the user replaced the damaged file) the verdict reads
// as absent everywhere a diagnostic is read, and the next analysis clears it even when
// that analysis could not look at the new bytes.
func TestAnalyzeVerdictFollowsTheEssence(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	spec := trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"}
	r := putTrack(t, st, lib.ID, spec)
	putAnalyzed(t, st, r.FilePID, "e1", true, model.FileDiagnostic{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "bad frame"})
	visible := func() (rows, summary, count int) {
		t.Helper()
		ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginAnalyze})
		if err != nil {
			t.Fatal(err)
		}
		sum, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{Origin: model.OriginAnalyze})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range sum {
			summary += c.Count
		}
		n, err := st.CountFileDiagnostics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ds), summary, n
	}
	if rows, summary, count := visible(); rows != 1 || summary != 1 || count != 1 {
		t.Fatalf("current verdict reads %d rows, %d in the summary, %d counted; want 1 each", rows, summary, count)
	}

	spec.essence, spec.content = "e2", "c2"
	putTrack(t, st, lib.ID, spec)
	if rows, summary, count := visible(); rows != 0 || summary != 0 || count != 0 {
		t.Errorf("verdict on replaced audio reads %d rows, %d in the summary, %d counted; want none", rows, summary, count)
	}

	putAnalyzed(t, st, r.FilePID, "e2", false)
	var stored int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM file_diagnostic WHERE origin = 'analyze'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("analyze rows left after an analysis of the new audio = %d, want 0", stored)
	}
}

// TestPutDecodeVerdictRecordsAFailedAnalysis: a file the analyze pass could not decode at
// all still gets its verdict, under the analyze origin and for the audio it read.
func TestPutDecodeVerdictRecordsAFailedAnalysis(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	spec := trackSpec{path: "/lib/a.m4a", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"}
	r := putTrack(t, st, lib.ID, spec)
	if err := st.PutDecodeVerdict(ctx, r.FilePID, "e1", []model.FileDiagnostic{{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "access unit overruns packet"}}); err != nil {
		t.Fatalf("PutDecodeVerdict: %v", err)
	}
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); len(got) != 1 || got[0] != "corrupt_audio/error/access unit overruns packet" {
		t.Errorf("analyze rows = %q, want the decode failure", got)
	}
	spec.essence, spec.content = "e2", "c2"
	putTrack(t, st, lib.ID, spec)
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); len(got) != 0 {
		t.Errorf("analyze rows after the file changed = %q, want none", got)
	}
}

// TestAnUnobservedAnalysisKeepsTheCurrentVerdict: a verdict stamped with the audio the
// file still holds stands until a decode looks at that audio again. An analysis that
// looked at nothing (an IO error in the measure) clears only verdicts on earlier audio,
// however the file's analysis stamp compares.
func TestAnUnobservedAnalysisKeepsTheCurrentVerdict(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.m4a", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	if err := st.PutDecodeVerdict(ctx, r.FilePID, "e1", []model.FileDiagnostic{{
		Code: model.DiagCorruptAudio, Severity: model.SeverityError, Detail: "access unit overruns packet"}}); err != nil {
		t.Fatalf("PutDecodeVerdict: %v", err)
	}
	putAnalyzed(t, st, r.FilePID, "e1", false)
	if got := originRows(t, st, r.FilePID, model.OriginAnalyze); len(got) != 1 {
		t.Errorf("analyze rows after an unobserved analysis of the same audio = %q, want the verdict", got)
	}
}

// TestAStoredDetailIsOneEscapedLine: the store escapes and bounds every writer's detail,
// so a writer handing over raw error text (a failed tag write naming its path) cannot
// put a control sequence or an unbounded message in front of the audit.
func TestAStoredDetailIsOneEscapedLine(t *testing.T) {
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	raw := "open /lib/a\nb.flac: \x1b[2Jdenied" + strings.Repeat("x", 2*model.MaxDetailBytes)
	if err := st.PutFileDiagnostics(context.Background(), r.FilePID, model.OriginEdit, []model.FileDiagnostic{
		{Code: model.DiagTagWriteUnsynced, Detail: raw}}); err != nil {
		t.Fatalf("PutFileDiagnostics: %v", err)
	}
	const prefix = "tag_write_unsynced/warn/"
	got := originRows(t, st, r.FilePID, model.OriginEdit)
	if len(got) != 1 || !strings.HasPrefix(got[0], prefix+`open /lib/a\x0ab.flac: \x1b[2Jdenied`) ||
		len(got[0]) > len(prefix)+model.MaxDetailBytes {
		t.Errorf("stored detail = %.80q (%d bytes), want it escaped and capped", got, len(strings.Join(got, "")))
	}
}
