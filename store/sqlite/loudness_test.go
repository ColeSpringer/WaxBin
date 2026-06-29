package sqlite

import (
	"context"
	"math"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// putLoudness stamps a file's loudness via PutAnalysis (the atomic write path).
func putLoudness(t *testing.T, st *Store, filePID model.PID, essence string, gainDB, peak float64) {
	t.Helper()
	if err := st.PutAnalysis(context.Background(), model.AnalysisInput{
		AnalysisVersion: 1,
		Fingerprint:     model.FingerprintInput{FilePID: filePID, EssenceHash: essence, AlgoVersion: 1, FP: []byte{}},
		Loudness:        &model.LoudnessData{IntegratedLUFS: -18 - gainDB, TrackGainDB: gainDB, TrackPeak: peak},
	}); err != nil {
		t.Fatalf("put loudness: %v", err)
	}
}

func TestRefreshAlbumGain(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two tracks of one album (same artist/album/folder).
	r1 := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album", durationMS: 100,
	})
	r2 := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", durationMS: 100,
	})
	putLoudness(t, st, r1.FilePID, "e1", -6.0, 0.8)
	putLoudness(t, st, r2.FilePID, "e2", -12.0, 0.95)

	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatalf("refresh album gain: %v", err)
	}

	l, err := st.LoudnessByItem(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("loudness: %v", err)
	}
	if !l.HasAlbum {
		t.Fatal("album gain not set after refresh")
	}
	// album_gain = -10*log10(mean(10^(6/10), 10^(12/10))) for equal-duration tracks.
	wantGain := -10 * math.Log10((math.Pow(10, 0.6)+math.Pow(10, 1.2))/2)
	if math.Abs(l.AlbumGainDB-wantGain) > 0.01 {
		t.Errorf("album gain = %.3f, want %.3f", l.AlbumGainDB, wantGain)
	}
	if math.Abs(l.AlbumPeak-0.95) > 1e-9 {
		t.Errorf("album peak = %.3f, want 0.95 (loudest track)", l.AlbumPeak)
	}
	// Both tracks of the album share the same album gain/peak.
	l2, _ := st.LoudnessByItem(ctx, r2.ItemPID)
	if math.Abs(l2.AlbumGainDB-l.AlbumGainDB) > 1e-9 {
		t.Errorf("album gain differs across tracks of one album: %.3f vs %.3f", l2.AlbumGainDB, l.AlbumGainDB)
	}
}

func TestReanalyzePreservesAlbumGain(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/B/A/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "B", album: "A", durationMS: 100,
	})
	putLoudness(t, st, r.FilePID, "e1", -6.0, 0.8)
	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// A re-analyze (new track loudness) must not wipe the previously-aggregated
	// album gain; it stays until the next RefreshAlbumGain.
	putLoudness(t, st, r.FilePID, "e1", -7.0, 0.82)
	l, err := st.LoudnessByItem(ctx, r.ItemPID)
	if err != nil {
		t.Fatalf("loudness: %v", err)
	}
	if !l.HasAlbum {
		t.Error("re-analyze cleared album gain; it should persist between aggregations")
	}
}

// TestStaleLoudnessHidden verifies a loudness measured from superseded audio
// (the file was re-encoded but not yet re-analyzed) is not returned: the essence
// no longer matches the file's current essence.
func TestStaleLoudnessHidden(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "Al"}
	r := putTrack(t, st, lib.ID, spec)
	putLoudness(t, st, r.FilePID, "e1", -6.0, 0.8)

	// Current essence matches -> loudness is returned.
	if _, err := st.LoudnessByItem(ctx, r.ItemPID); err != nil {
		t.Fatalf("fresh loudness should be readable: %v", err)
	}
	if n, _ := st.CountLoudness(ctx); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}

	// Re-encode: the file's essence advances to e2 (analyze has not re-run).
	spec.essence, spec.content = "e2", "c2"
	putTrack(t, st, lib.ID, spec)

	if _, err := st.LoudnessByItem(ctx, r.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("stale loudness (old essence) should be hidden, got err %v", err)
	}
	if n, _ := st.CountLoudness(ctx); n != 0 {
		t.Errorf("stale loudness should not count as coverage, got %d", n)
	}
}

// TestReanalyzeEssenceChangeClearsStaleLoudness verifies that when a file is
// re-analyzed for new audio but loudness fails (nil), the prior measurement is
// cleared rather than left behind, while a same-essence re-analyze keeps it.
func TestReanalyzeEssenceChangeClearsStaleLoudness(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"})
	putLoudness(t, st, r.FilePID, "e1", -6.0, 0.8)

	// Re-analyze at the SAME essence with no loudness (transient failure) keeps it.
	if err := st.PutAnalysis(ctx, model.AnalysisInput{
		AnalysisVersion: 2, Fingerprint: model.FingerprintInput{FilePID: r.FilePID, EssenceHash: "e1", AlgoVersion: 1, FP: []byte{}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM loudness"); n != 1 {
		t.Errorf("same-essence re-analyze with nil loudness dropped the row (%d), want kept", n)
	}

	// Re-analyze at a DIFFERENT essence with no loudness clears the stale row.
	if err := st.PutAnalysis(ctx, model.AnalysisInput{
		AnalysisVersion: 2, Fingerprint: model.FingerprintInput{FilePID: r.FilePID, EssenceHash: "e2", AlgoVersion: 1, FP: []byte{}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM loudness"); n != 0 {
		t.Errorf("essence-change re-analyze with nil loudness kept a stale row (%d), want cleared", n)
	}
}

// TestAlbumGainClearedWhenLeavingAlbum verifies a track retagged out of its album
// loses its album ReplayGain on the next RefreshAlbumGain.
func TestAlbumGainClearedWhenLeavingAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/A/1.flac", essence: "e1", content: "c1", title: "One", artist: "B", album: "A", durationMS: 100})
	r2 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/A/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", album: "A", durationMS: 100})
	putLoudness(t, st, r1.FilePID, "e1", -6.0, 0.8)
	putLoudness(t, st, r2.FilePID, "e2", -6.0, 0.8)
	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.LoudnessByItem(ctx, r1.ItemPID); !l.HasAlbum {
		t.Fatal("album gain not set initially")
	}

	// Retag track 1 as a non-album single (content change -> entities re-resolve,
	// album_id becomes NULL), then refresh.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/A/1.flac", essence: "e1", content: "c1b", title: "One", artist: "B", album: "", durationMS: 100})
	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.LoudnessByItem(ctx, r1.ItemPID); l.HasAlbum {
		t.Errorf("track that left its album kept a stale album gain: %+v", l)
	}
}

// TestRefreshAlbumGainEmitsDeltas verifies an album-ReplayGain change is reported
// on the change feed (so a data_version tailer can invalidate its cache), and a
// second refresh with no change emits nothing.
func TestRefreshAlbumGainEmitsDeltas(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/A/1.flac", essence: "e1", content: "c1", title: "One", artist: "B", album: "A", durationMS: 100})
	r2 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/A/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", album: "A", durationMS: 100})
	putLoudness(t, st, r1.FilePID, "e1", -6.0, 0.8)
	putLoudness(t, st, r2.FilePID, "e2", -12.0, 0.9)

	before, _ := st.LatestChangeSeq(ctx)
	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := st.LatestChangeSeq(ctx)
	if after-before != 2 {
		t.Errorf("album-gain refresh emitted %d deltas, want 2 (one per track)", after-before)
	}

	// A second refresh changes nothing, so emits nothing (no churn).
	before, _ = st.LatestChangeSeq(ctx)
	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.LatestChangeSeq(ctx); after != before {
		t.Errorf("idempotent album-gain refresh emitted %d spurious deltas", after-before)
	}
}

// TestStalePeaksHidden verifies a waveform from superseded audio is not returned
// (essence mismatch), mirroring the loudness freshness check.
func TestStalePeaksHidden(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "Al"}
	r := putTrack(t, st, lib.ID, spec)
	if err := st.PutAnalysis(ctx, model.AnalysisInput{
		AnalysisVersion: 1,
		Fingerprint:     model.FingerprintInput{FilePID: r.FilePID, EssenceHash: "e1", AlgoVersion: 1, FP: []byte{}},
		Peaks:           &model.PeaksData{Version: 1, Buckets: 2, Data: []byte{1, 0, 2, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadPeaks(ctx, r.ItemPID); err != nil {
		t.Fatalf("fresh peaks should load: %v", err)
	}
	// Re-encode: file essence advances; the waveform is now stale.
	spec.essence, spec.content = "e2", "c2"
	putTrack(t, st, lib.ID, spec)
	if _, err := st.LoadPeaks(ctx, r.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("stale waveform should be hidden, got %v", err)
	}
}

func TestLoudnessNotFound(t *testing.T) {
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "T", artist: "A", album: "Al"})
	if _, err := st.LoudnessByItem(context.Background(), r.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("want CodeNotFound for an unanalyzed item, got %v", err)
	}
}
