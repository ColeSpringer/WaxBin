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

// putPeaks stamps a file's waveform via PutAnalysis (the atomic write path), using
// the data bytes as the distinguishing mark so a read can be traced to a part.
func putPeaks(t *testing.T, st *Store, filePID model.PID, essence string, data []byte) {
	t.Helper()
	if err := st.PutAnalysis(context.Background(), model.AnalysisInput{
		AnalysisVersion: 1,
		Fingerprint:     model.FingerprintInput{FilePID: filePID, EssenceHash: essence, AlgoVersion: 1, FP: []byte{}},
		Peaks:           &model.PeaksData{Version: 1, Buckets: len(data) / 2, Data: data},
	}); err != nil {
		t.Fatalf("put peaks: %v", err)
	}
}

// outOfOrderBook attaches a three-part audiobook out of reading order (part three
// first) and gives each part its own waveform. Attach order is what picks the
// primary, so the fixture separates "the primary part" from "part one", which are
// the two the peaks reads must not conflate.
func outOfOrderBook(t *testing.T, st *Store, libID int64) map[int]*model.ScanItemResult {
	t.Helper()
	parts := map[int]*model.ScanItemResult{}
	for _, pos := range []int{3, 1, 2} {
		e := "dp" + string(rune('0'+pos))
		parts[pos] = putBook(t, st, libID, bookSpec{
			path:    "/lib/b/p" + string(rune('0'+pos)) + ".mp3",
			essence: e, content: "dc" + string(rune('0'+pos)),
			title: "Tome", author: "Auth", asin: "BD", position: pos, durationMS: 1000,
		})
		putPeaks(t, st, parts[pos].FilePID, e, []byte{byte(pos), 0, byte(pos), 0})
	}
	return parts
}

// TestPeaksPrimaryIsNotReadingOrderPartOne pins what Peaks actually answers for on a
// multi-file book, against both rules that pick the primary. The book's parts are
// attached out of order, so the primary is part three (first attached, the
// linkBookFile rule); after that part is detached the answer follows the promotion
// to the lowest-positioned survivor (the ensurePrimary rule), which is part one.
// Neither rule is reading order, which is why the per-part reads exist.
func TestPeaksPrimaryIsNotReadingOrderPartOne(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	parts := outOfOrderBook(t, st, lib.ID)
	book := parts[3].ItemPID

	pk, err := st.LoadPeaks(ctx, book)
	if err != nil {
		t.Fatalf("load peaks: %v", err)
	}
	if pk.Data[0] != 3 {
		t.Errorf("Peaks answered for part %d, want part 3 (attached first, so primary)", pk.Data[0])
	}

	// Re-key part three's file as a music track: the book loses its primary and
	// promotes the lowest-positioned survivor.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/p3.mp3", essence: "trk3", content: "tc3", title: "Song", artist: "Band"})
	pk, err = st.LoadPeaks(ctx, book)
	if err != nil {
		t.Fatalf("load peaks after detach: %v", err)
	}
	if pk.Data[0] != 1 {
		t.Errorf("Peaks answered for part %d after the primary was detached, want part 1 (lowest-positioned survivor)", pk.Data[0])
	}
}

// TestPeaksPerFileAndPerItem is the multi-part waveform surface: every part of a book
// has its own waveform (analysis is per file and never consulted item_file), each is
// reachable by file pid, and the per-item read returns them all in reading order
// rather than the single primary Peaks answers for.
func TestPeaksPerFileAndPerItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	parts := outOfOrderBook(t, st, lib.ID)

	for pos := 1; pos <= 3; pos++ {
		pk, err := st.LoadPeaksForFile(ctx, parts[pos].FilePID)
		if err != nil {
			t.Fatalf("peaks for part %d: %v", pos, err)
		}
		if pk.Data[0] != byte(pos) {
			t.Errorf("part %d file read back part %d's waveform", pos, pk.Data[0])
		}
	}

	got, err := st.LoadPeaksForItem(ctx, parts[1].ItemPID)
	if err != nil {
		t.Fatalf("peaks for item: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("per-item peaks = %d parts, want 3", len(got))
	}
	for i, ip := range got {
		wantPos := i + 1
		if ip.Position != wantPos || ip.Peaks.Data[0] != byte(wantPos) {
			t.Errorf("per-item peaks[%d] = position %d waveform %d, want part %d in reading order",
				i, ip.Position, ip.Peaks.Data[0], wantPos)
		}
		if ip.FilePID != parts[wantPos].FilePID {
			t.Errorf("per-item peaks[%d] file = %s, want part %d's file", i, ip.FilePID, wantPos)
		}
	}
}

// TestPeaksForFileNotFound pins the two absent cases on the per-file read: an unknown
// file pid, and a waveform left over from superseded audio, which the essence gate
// hides exactly as it does for the item-level read (TestStalePeaksHidden). A stale
// part also drops out of the per-item read rather than reporting an old waveform for
// a part that has been re-encoded.
func TestPeaksForFileNotFound(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	parts := outOfOrderBook(t, st, lib.ID)

	if _, err := st.LoadPeaksForFile(ctx, "file-does-not-exist"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown file pid gave %v, want CodeNotFound", err)
	}

	// Re-encode part two: its essence advances, so its stored waveform is stale.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "dp2b", content: "dc2b",
		title: "Tome", author: "Auth", asin: "BD", position: 2, durationMS: 1000,
	})
	if _, err := st.LoadPeaksForFile(ctx, parts[2].FilePID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("stale waveform gave %v, want CodeNotFound", err)
	}
	got, err := st.LoadPeaksForItem(ctx, parts[1].ItemPID)
	if err != nil {
		t.Fatalf("peaks for item: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("per-item peaks = %d parts, want 2 with part two's waveform stale", len(got))
	}
	for _, ip := range got {
		if ip.Position == 2 {
			t.Errorf("per-item peaks returned the stale part two waveform: %+v", ip)
		}
	}
}

// TestItemFilesCarryRole pins the role on ItemFileRef against the primary-selection
// rule it exists to expose: the parts come back in reading order, but the primary is
// the one attached first, so a consumer cannot infer it from position.
func TestItemFilesCarryRole(t *testing.T) {
	st, lib := entityFixture(t)
	parts := outOfOrderBook(t, st, lib.ID)

	refs, err := st.ItemFiles(context.Background(), parts[1].ItemPID)
	if err != nil {
		t.Fatalf("item files: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("item files = %d, want 3", len(refs))
	}
	var primaries int
	for i, r := range refs {
		if r.Position != i+1 {
			t.Errorf("refs[%d] position = %d, want reading order", i, r.Position)
		}
		if r.Role == "primary" {
			primaries++
			if r.Position != 3 {
				t.Errorf("primary is part %d, want part 3 (attached first)", r.Position)
			}
		} else if r.Role != "part" {
			t.Errorf("refs[%d] role = %q, want primary or part", i, r.Role)
		}
	}
	if primaries != 1 {
		t.Errorf("primary edges = %d, want exactly 1", primaries)
	}
}

func TestLoudnessNotFound(t *testing.T) {
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "T", artist: "A", album: "Al"})
	if _, err := st.LoudnessByItem(context.Background(), r.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("want CodeNotFound for an unanalyzed item, got %v", err)
	}
}
