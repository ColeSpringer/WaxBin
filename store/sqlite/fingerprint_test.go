package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// putFile inserts an audio file row (no entities) for fingerprint tests.
func putFile(t *testing.T, st *Store, libID int64, path, essence string, durationMS int64) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(path),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, Codec: "pcm",
			ContentHash: "c" + essence, EssenceHash: essence, DurationMS: durationMS,
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: essence,
			SortKey: essence, IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: "A"},
	})
	if err != nil {
		t.Fatalf("put file %s: %v", path, err)
	}
	return res.FilePID
}

func TestFingerprintCandidatesSharedTerms(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	a := putFile(t, st, lib.ID, "/lib/a.wav", "ea", 200000)
	b := putFile(t, st, lib.ID, "/lib/b.wav", "eb", 200000) // same duration bucket as a
	c := putFile(t, st, lib.ID, "/lib/c.wav", "ec", 999000) // different bucket

	bucketAB := int64(200000 / 2000)
	bucketC := int64(999000 / 2000)
	mustPut := func(pid model.PID, bucket int64, terms []int64) {
		if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
			FilePID: pid, EssenceHash: string(pid), AlgoVersion: 1,
			DurationBucket: bucket, FP: []byte{1, 2, 3, 4}, Terms: terms,
		}}); err != nil {
			t.Fatalf("put fingerprint: %v", err)
		}
	}
	mustPut(a, bucketAB, []int64{10, 20, 30, 40, 50})
	mustPut(b, bucketAB, []int64{10, 20, 30, 40, 99}) // shares 4 terms with a
	mustPut(c, bucketC, []int64{10, 20, 30, 40, 50})  // identical terms but other bucket

	cands, err := st.FingerprintCandidates(ctx, a, 4)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 (b only; c is in another bucket)", len(cands))
	}
	if cands[0].FilePID != b || cands[0].SharedTerms != 4 {
		t.Errorf("candidate = %+v, want file %s with 4 shared terms", cands[0], b)
	}
	if cands[0].ItemPID == "" {
		t.Error("candidate should carry the item pid it backs")
	}

	// Raising the threshold past the shared count drops the candidate.
	if cands, _ := st.FingerprintCandidates(ctx, a, 5); len(cands) != 0 {
		t.Errorf("threshold 5 should exclude b (shares 4), got %d", len(cands))
	}
	// The candidate's fingerprint vector is returned for in-process verification.
	if cands, _ := st.FingerprintCandidates(ctx, a, 4); len(cands) == 1 && len(cands[0].FP) == 0 {
		t.Error("candidate should carry its fingerprint vector (FP)")
	}
}

func TestFingerprintCandidatesNeighborBucket(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	q := putFile(t, st, lib.ID, "/lib/q.wav", "eq", 0)
	near := putFile(t, st, lib.ID, "/lib/near.wav", "en", 0)
	far := putFile(t, st, lib.ID, "/lib/far.wav", "ef", 0)

	terms := []int64{1, 2, 3, 4, 5}
	put := func(pid model.PID, bucket int64) {
		if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
			FilePID: pid, EssenceHash: string(pid), AlgoVersion: 1,
			DurationBucket: bucket, FP: []byte{9}, Terms: terms,
		}}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	put(q, 100)
	put(near, 101) // one bucket away -> still a candidate (boundary tolerance)
	put(far, 103)  // three buckets away -> excluded

	cands, err := st.FingerprintCandidates(ctx, q, 3)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].FilePID != near {
		t.Fatalf("expected only the ±1-bucket neighbor, got %+v", cands)
	}
}

func TestFilesNeedingAnalysisLifecycle(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putFile(t, st, lib.ID, "/lib/a.wav", "ea", 200000)

	need, err := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100)
	if err != nil {
		t.Fatalf("need: %v", err)
	}
	if len(need) != 1 {
		t.Fatalf("a fresh file should need analysis, got %d", len(need))
	}

	if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, MeasureCompleted: true,
		Fingerprint: model.FingerprintInput{
			FilePID: a, EssenceHash: "ea", AlgoVersion: 1, DurationBucket: 1, FP: []byte{1}, Terms: []int64{1},
		}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if need, _ := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100); len(need) != 0 {
		t.Errorf("analyzed file should not need analysis at the same version, got %d", len(need))
	}
	// A newer algorithm version makes prior analysis stale.
	if need, _ := st.FilesNeedingAnalysis(ctx, 2, nil, 0, 100); len(need) != 1 {
		t.Errorf("a version bump should restage the file, got %d", len(need))
	}
}

// TestNeedsAnalysisTracksMeasurementCompletion pins the discriminator between the
// two ways a file ends up with no loudness row. Silence measures perfectly well and
// stores nothing, so it must settle; a measuring decode that fell over stores
// nothing either, and must come back. Before measured_essence the store could not
// tell them apart, so one of the two was always handled wrong.
func TestNeedsAnalysisTracksMeasurementCompletion(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	silent := putFile(t, st, lib.ID, "/lib/silent.wav", "es", 200000)
	broken := putFile(t, st, lib.ID, "/lib/broken.wv", "eb", 200000)

	put := func(pid model.PID, essence string, completed bool) {
		t.Helper()
		if err := st.PutAnalysis(ctx, model.AnalysisInput{
			AnalysisVersion: 1, MeasureCompleted: completed,
			Fingerprint: model.FingerprintInput{
				FilePID: pid, EssenceHash: essence, AlgoVersion: 1, DurationBucket: 1,
				FP: []byte{1}, Terms: []int64{1},
			},
		}); err != nil {
			t.Fatalf("put %s: %v", pid, err)
		}
	}
	// Both store a fingerprint and no loudness. Only the second one's measurement ran.
	put(silent, "es", true)
	put(broken, "eb", false)

	need, err := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100)
	if err != nil {
		t.Fatalf("need: %v", err)
	}
	if len(need) != 1 || need[0].PID != broken {
		got := make([]model.PID, len(need))
		for i, f := range need {
			got[i] = f.PID
		}
		t.Fatalf("restaged %v, want only the file whose measure failed (%s)", got, broken)
	}

	// A later run that does measure it settles the file.
	put(broken, "eb", true)
	if need, _ := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100); len(need) != 0 {
		t.Errorf("a completed measurement should settle the file, got %d restaged", len(need))
	}

	// New audio in the same file: the old measurement covers essence that is gone.
	if _, err := st.write.ExecContext(ctx,
		"UPDATE file SET essence_hash = ? WHERE pid = ?", "eb2", string(broken)); err != nil {
		t.Fatalf("re-essence: %v", err)
	}
	if need, _ := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100); len(need) != 1 {
		t.Errorf("a changed essence should restage the file, got %d", len(need))
	}
}

// TestVersionBumpDoesNotSettleOnAFailedRemeasure is the freeze this phase exists to
// break, in the one shape that survives a naive fix. A version bump is the only
// thing that re-selects a file whose essence has not changed, so it is the only
// time PutAnalysis runs against a file that already carries a completed
// measurement. If the failing re-measure leaves that old stamp in place, the file
// settles as fully current while its loudness row still holds numbers from the
// decoder the bump was raised to invalidate, and nothing ever looks at it again.
func TestVersionBumpDoesNotSettleOnAFailedRemeasure(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putFile(t, st, lib.ID, "/lib/a.wv", "ea", 200000)

	put := func(version int, completed bool, ld *model.LoudnessData) {
		t.Helper()
		if err := st.PutAnalysis(ctx, model.AnalysisInput{
			AnalysisVersion: version, MeasureCompleted: completed, Loudness: ld,
			Fingerprint: model.FingerprintInput{
				FilePID: a, EssenceHash: "ea", AlgoVersion: 1, DurationBucket: 1,
				FP: []byte{1}, Terms: []int64{1},
			},
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	loudnessRows := func() int {
		t.Helper()
		var n int
		if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM loudness").Scan(&n); err != nil {
			t.Fatalf("count loudness: %v", err)
		}
		return n
	}

	// Measured cleanly under the old decoder, so the file settles at version 1.
	put(1, true, &model.LoudnessData{IntegratedLUFS: -12, TrackGainDB: -6, TrackPeak: 0.9})
	if need, _ := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 100); len(need) != 0 {
		t.Fatalf("a fully measured file should settle, got %d restaged", len(need))
	}

	// The decoder changes, so the version moves and the file comes back.
	if need, _ := st.FilesNeedingAnalysis(ctx, 2, nil, 0, 100); len(need) != 1 {
		t.Fatalf("a version bump should restage the file, got %d", len(need))
	}

	// This time the measuring decode falls over. The fingerprint is stored, the
	// version advances, and the stale loudness row survives because the essence did
	// not change. The file must not be treated as measured at the new version.
	put(2, false, nil)
	if got := loudnessRows(); got != 1 {
		t.Fatalf("loudness rows = %d, want the stale row kept: without it this walk is not the dangerous one", got)
	}
	if need, _ := st.FilesNeedingAnalysis(ctx, 2, nil, 0, 100); len(need) != 1 {
		t.Fatalf("a file carrying a pre-bump measurement settled at the new version; it will never be re-measured")
	}

	// It settles only once a measure actually completes under the new version.
	put(2, true, &model.LoudnessData{IntegratedLUFS: -11, TrackGainDB: -7, TrackPeak: 0.8})
	if need, _ := st.FilesNeedingAnalysis(ctx, 2, nil, 0, 100); len(need) != 0 {
		t.Errorf("a completed re-measure should settle the file, got %d restaged", len(need))
	}
}

// TestVersionBumpClearsLoudnessOnCompletedEmptyRemeasure is the other half of the
// version-bump hole. A re-measure that runs to completion and produces nothing has
// superseded the prior loudness row: leaving the row while the stamp settles the
// file would serve the old decoder's numbers as current forever. Only a measure
// that failed outright may keep the prior row, and that path stays unsettled.
func TestVersionBumpClearsLoudnessOnCompletedEmptyRemeasure(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putFile(t, st, lib.ID, "/lib/a.wv", "ea", 200000)

	put := func(version int, completed bool, ld *model.LoudnessData) {
		t.Helper()
		if err := st.PutAnalysis(ctx, model.AnalysisInput{
			AnalysisVersion: version, MeasureCompleted: completed, Loudness: ld,
			Fingerprint: model.FingerprintInput{
				FilePID: a, EssenceHash: "ea", AlgoVersion: 1, DurationBucket: 1,
				FP: []byte{1}, Terms: []int64{1},
			},
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	loudnessRows := func() int {
		t.Helper()
		var n int
		if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM loudness").Scan(&n); err != nil {
			t.Fatalf("count loudness: %v", err)
		}
		return n
	}

	put(1, true, &model.LoudnessData{IntegratedLUFS: -12, TrackGainDB: -6, TrackPeak: 0.9})
	if got := loudnessRows(); got != 1 {
		t.Fatalf("loudness rows = %d, want 1 after the first measure", got)
	}

	// The new decoder gates this material to no valid measurement. The run
	// completed, so the old row goes and the file settles at the new version.
	put(2, true, nil)
	if got := loudnessRows(); got != 0 {
		t.Fatalf("loudness rows = %d, want the superseded row cleared", got)
	}
	if need, _ := st.FilesNeedingAnalysis(ctx, 2, nil, 0, 100); len(need) != 0 {
		t.Errorf("a completed empty re-measure should settle the file, got %d restaged", len(need))
	}
}

// TestFilesNeedingAnalysisKeyset verifies the (rel_path, id) cursor advances past
// already-seen files, so paging never re-fetches a batch or strands later files.
func TestFilesNeedingAnalysisKeyset(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		putFile(t, st, lib.ID, "/lib/"+name+".wav", "e"+name, 1000)
	}

	seen := map[model.PID]bool{}
	var afterRel []byte
	var afterID int64
	pages := 0
	for {
		batch, err := st.FilesNeedingAnalysis(ctx, 1, afterRel, afterID, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, f := range batch {
			if seen[f.PID] {
				t.Errorf("file %s re-fetched across keyset pages", f.PID)
			}
			seen[f.PID] = true
		}
		last := batch[len(batch)-1]
		afterRel, afterID = last.RelPath, last.ID
		if pages++; pages > 10 {
			t.Fatal("keyset paging did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("keyset paging surfaced %d of 5 files", len(seen))
	}
}
