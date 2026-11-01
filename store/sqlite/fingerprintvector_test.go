package sqlite_test

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestLoadFingerprintReadsTheCurrentAnalysis: a file's stored fingerprint comes back with
// the analysis that made it while it describes the file's audio; a file never analyzed, or
// one whose audio changed since, has none.
func TestLoadFingerprintReadsTheCurrentAnalysis(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, _, err := st.LoadFingerprint(ctx, res.FilePID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("before analysis: err = %v, want CodeNotFound", err)
	}
	vec := []uint32{1, 0xFFFFFFFE, 7}
	if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
		FilePID: res.FilePID, EssenceHash: "sha256:E", AlgoVersion: fingerprint.ChromaprintAlgoVersion,
		DurationBucket: 1, FP: fingerprint.Pack(vec), Terms: []int64{7},
	}}); err != nil {
		t.Fatalf("analysis: %v", err)
	}
	blob, analysis, err := st.LoadFingerprint(ctx, res.FilePID)
	if got := fingerprint.Unpack(blob); err != nil || !slices.Equal(got, vec) || analysis != fingerprint.ChromaprintAlgoVersion {
		t.Fatalf("stored vector = %v under %d (err %v), want %v under %d", fingerprint.Unpack(blob), analysis, err, vec, fingerprint.ChromaprintAlgoVersion)
	}
	if _, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E2", "sha256:C2", "Song")); err != nil {
		t.Fatalf("rescan with new audio: %v", err)
	}
	if v, _, err := st.LoadFingerprint(ctx, res.FilePID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("after the audio changed: %v (err %v), want CodeNotFound", v, err)
	}
}

// TestReleaseGroupsNeedingEnrichmentNamesTheRepresentativeFile: the representative file
// the AcoustID fallback fingerprints comes with its pid, from the same row as its path.
func TestReleaseGroupsNeedingEnrichmentNamesTheRepresentativeFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	rgs, err := st.ReleaseGroupsNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 10, true, nil)
	if err != nil || len(rgs) != 1 {
		t.Fatalf("release groups = %+v (err %v), want one", rgs, err)
	}
	if rgs[0].FilePID != res.FilePID || rgs[0].FilePath != "/lib/song.mp3" {
		t.Errorf("representative file = %q at %q, want %q at /lib/song.mp3", rgs[0].FilePID, rgs[0].FilePath, res.FilePID)
	}
}
