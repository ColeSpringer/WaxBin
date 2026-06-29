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
		if err := st.PutFingerprint(ctx, model.FingerprintInput{
			FilePID: pid, EssenceHash: string(pid), AlgoVersion: 1,
			DurationBucket: bucket, FP: []byte{1, 2, 3, 4}, Terms: terms,
		}); err != nil {
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
		if err := st.PutFingerprint(ctx, model.FingerprintInput{
			FilePID: pid, EssenceHash: string(pid), AlgoVersion: 1,
			DurationBucket: bucket, FP: []byte{9}, Terms: terms,
		}); err != nil {
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

	if err := st.PutFingerprint(ctx, model.FingerprintInput{
		FilePID: a, EssenceHash: "ea", AlgoVersion: 1, DurationBucket: 1, FP: []byte{1}, Terms: []int64{1},
	}); err != nil {
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
