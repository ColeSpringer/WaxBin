package analyze

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/peaks"
)

// cheapSignal is a plain sine, fast to synthesize for the long fixtures where a
// rich multi-tone signal would dominate the test's runtime. It is non-silent, so
// loudness gates and peaks register.
func cheapSignal(n int) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(0.3 * math.Sin(float64(i)*0.03))
	}
	return s
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore is an in-memory analyze.Store: it exercises the pass without SQLite,
// serving a fixed file set through the keyset cursor and recording every
// PutAnalysis so a test can assert what was (and was not) stamped.
type fakeStore struct {
	files  []*model.File
	puts   map[model.PID]model.AnalysisInput
	putErr error
}

func newFakeStore(files ...*model.File) *fakeStore {
	return &fakeStore{files: files, puts: map[model.PID]model.AnalysisInput{}}
}

func (s *fakeStore) CountFilesNeedingAnalysis(context.Context, int) (int, error) {
	return len(s.files), nil
}

func (s *fakeStore) FilesNeedingAnalysis(_ context.Context, _ int, afterRelPath []byte, afterID int64, limit int) ([]*model.File, error) {
	sorted := append([]*model.File(nil), s.files...)
	sort.Slice(sorted, func(i, j int) bool {
		if c := bytes.Compare(sorted[i].RelPath, sorted[j].RelPath); c != 0 {
			return c < 0
		}
		return sorted[i].ID < sorted[j].ID
	})
	var out []*model.File
	for _, f := range sorted {
		c := bytes.Compare(f.RelPath, afterRelPath)
		if c > 0 || (c == 0 && f.ID > afterID) {
			out = append(out, f)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) PutAnalysis(_ context.Context, in model.AnalysisInput) error {
	if s.putErr != nil {
		return s.putErr
	}
	s.puts[in.Fingerprint.FilePID] = in
	return nil
}

// pureGoAnalyzer builds an Analyzer forced onto the pure-Go fingerprint backend,
// so the tests exercise WaxFlow's decode path and stay host-independent whether or
// not fpcalc is installed (the field override is legal from this in-package test).
func pureGoAnalyzer(t *testing.T, store Store) *Analyzer {
	t.Helper()
	a := New(store, nil, discardLog())
	a.fpAlgo = fingerprint.AlgoVersion
	a.version = effectiveVersion(a.fpAlgo)
	return a
}

// writeFixture writes data under dir and returns the model.File the store serves.
func writeFixture(t *testing.T, dir, name string, id int64, data []byte) *model.File {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return &model.File{
		ID:          id,
		PID:         model.PID("pid-" + name),
		Path:        []byte(p),
		DisplayPath: name,
		RelPath:     []byte(name),
		EssenceHash: "essence-" + name,
	}
}

// TestRunAllFormats is the test that proves the migration: every one of WaxFlow's
// eight formats decodes, fingerprints, and measures on a single host with no
// external binaries. An aac/aac-lc vocabulary slip would surface here loudly as a
// skip, not silently in production.
func TestRunAllFormats(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 4*time.Second)
	formats := []struct{ format, container, ext string }{
		{"wav", "", "wav"},
		{"aiff", "", "aiff"},
		{"flac", "", "flac"},
		{"alac", "", "m4a"},
		{"mp3", "", "mp3"},
		{"aac", "", "m4a"},
		{"opus", "", "opus"},
		{"vorbis", "", "ogg"},
	}
	var files []*model.File
	for i, fc := range formats {
		data := testaudio.EncodeAs(t, fc.format, fc.container, rate, sig)
		files = append(files, writeFixture(t, dir, fc.format+"."+fc.ext, int64(i), data))
	}
	store := newFakeStore(files...)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Analyzed != len(formats) || res.Skipped != 0 || res.Errored != 0 {
		t.Fatalf("Run = {Analyzed:%d Skipped:%d Errored:%d}, want {%d 0 0}",
			res.Analyzed, res.Skipped, res.Errored, len(formats))
	}
	for _, f := range files {
		in, ok := store.puts[f.PID]
		if !ok {
			t.Errorf("%s: never stamped", f.DisplayPath)
			continue
		}
		if in.Loudness == nil {
			t.Errorf("%s: no loudness stored", f.DisplayPath)
		}
		if in.Peaks == nil {
			t.Errorf("%s: no peaks stored", f.DisplayPath)
		}
		if in.Fingerprint.AlgoVersion != fingerprint.AlgoVersion {
			t.Errorf("%s: algo = %d, want pure-Go %d", f.DisplayPath, in.Fingerprint.AlgoVersion, fingerprint.AlgoVersion)
		}
	}
}

// TestRunUnsupportedSkipped: an input this build cannot decode (random bytes) is
// skipped and never stamped, so a future WaxFlow can pick it up.
func TestRunUnsupportedSkipped(t *testing.T) {
	dir := t.TempDir()
	rnd := make([]byte, 16384)
	for i := range rnd {
		rnd[i] = byte(i*7 + 3)
	}
	f := writeFixture(t, dir, "random.mp3", 0, rnd)
	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 1 || res.Analyzed != 0 || res.Errored != 0 {
		t.Fatalf("Run = {Analyzed:%d Skipped:%d Errored:%d}, want {0 1 0}", res.Analyzed, res.Skipped, res.Errored)
	}
	if _, ok := store.puts[f.PID]; ok {
		t.Error("an unsupported file was stamped; it must be skipped and retried later")
	}
}

// TestRunCorruptErrored: a recognized container whose bytes are truncated mid-
// stream (an MP4 whose sample data runs past EOF) is a stream-phase failure. It
// must land in Errored so audit sees it, NOT in Skipped where it would be retried
// forever, and it must not be stamped.
func TestRunCorruptErrored(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 4*time.Second)
	enc := testaudio.EncodeAs(t, "aac", "", rate, sig)
	trunc := append([]byte(nil), enc[:len(enc)*60/100]...)
	f := writeFixture(t, dir, "corrupt.m4a", 0, trunc)
	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errored != 1 || res.Analyzed != 0 || res.Skipped != 0 {
		t.Fatalf("Run = {Analyzed:%d Skipped:%d Errored:%d}, want {0 0 1}", res.Analyzed, res.Skipped, res.Errored)
	}
	if _, ok := store.puts[f.PID]; ok {
		t.Error("a corrupt file was stamped; it must land in Errored, not be committed")
	}
}

// TestRunCanceledDoesNotCommit: a canceled run stops cleanly and stamps nothing,
// so no file is frozen as "analyzed, no loudness" and every file is retried.
func TestRunCanceledDoesNotCommit(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 2*time.Second)
	f := writeFixture(t, dir, "a.wav", 0, testaudio.EncodeWAV16(rate, sig))
	store := newFakeStore(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := pureGoAnalyzer(t, store).Run(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if res.Errored != 0 {
		t.Errorf("Errored = %d, want 0 (cancellation must not inflate errors)", res.Errored)
	}
	if len(store.puts) != 0 {
		t.Error("a canceled run committed an analysis; it must stamp nothing")
	}
}

// TestMeasureCanceled: measure yields to a canceled context by returning an error
// rather than a partial measurement, so analyzeFile aborts before PutAnalysis.
func TestMeasureCanceled(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 2*time.Second)
	f := writeFixture(t, dir, "a.wav", 0, testaudio.EncodeWAV16(rate, sig))
	a := pureGoAnalyzer(t, newFakeStore(f))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ld, pk, err := a.measure(ctx, f)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("measure err = %v, want context.Canceled", err)
	}
	if ld != nil || pk != nil {
		t.Error("measure returned data alongside a cancellation")
	}
}

// TestRunLongFilePeaks: a track over fifteen minutes gets a stored waveform. The
// old pure-Go path skipped peaks past a 15-minute decode cap; the streamed
// Accumulator has no such cap, so the waveform is stored whole.
func TestRunLongFilePeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("long-file fixture is slow")
	}
	dir := t.TempDir()
	const rate = 8000
	const seconds = 16 * 60 // 16 minutes, past the old 15-minute peaks cap
	f := writeFixture(t, dir, "long.wav", 0, testaudio.EncodeWAV16(rate, cheapSignal(rate*seconds)))
	f.DurationMS = int64(seconds) * 1000
	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Analyzed != 1 {
		t.Fatalf("Analyzed = %d, want 1", res.Analyzed)
	}
	in := store.puts[f.PID]
	if in.Peaks == nil {
		t.Fatal("no waveform stored for a >15-minute track; the whole-file stream should store one")
	}
	if in.Loudness == nil {
		t.Error("no loudness stored for a >15-minute track")
	}
}

// TestMeasureMultichannel exercises the mixdown tap on a wide layout, guarding the
// scratch-buffer handling in measure. A 6-channel file whose channels are all
// identical to a mono reference must yield the same waveform: the amplitude-average
// mixdown of identical channels is that channel. (Loudness is measured by WaxFlow's
// meter on the full multichannel signal with channel weighting, so it is not
// expected to match the mono file and is only checked for presence here.)
func TestMeasureMultichannel(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	mono := testaudio.ReferenceSignal(rate, 3*time.Second)
	inter := make([]float32, len(mono)*6)
	for i, v := range mono {
		for c := 0; c < 6; c++ {
			inter[i*6+c] = v
		}
	}
	fMono := writeFixture(t, dir, "mono.wav", 0, testaudio.EncodeWAV16(rate, mono))
	fMulti := writeFixture(t, dir, "surround.wav", 1, testaudio.EncodeWAV16Multi(rate, 6, inter))
	a := pureGoAnalyzer(t, newFakeStore())

	lMono, pMono, err := a.measure(context.Background(), fMono)
	if err != nil {
		t.Fatalf("mono measure: %v", err)
	}
	lMulti, pMulti, err := a.measure(context.Background(), fMulti)
	if err != nil {
		t.Fatalf("surround measure: %v", err)
	}
	if lMono == nil || lMulti == nil {
		t.Fatal("expected a loudness measurement for both files")
	}
	if pMono == nil || pMulti == nil {
		t.Fatal("expected a waveform for both files")
	}
	mb := peaks.Unpack(pMono.Data).Buckets
	sb := peaks.Unpack(pMulti.Data).Buckets
	if len(mb) != len(sb) {
		t.Fatalf("bucket count mismatch: mono %d, surround %d", len(mb), len(sb))
	}
	var maxDiff float32
	for i := range mb {
		if d := mb[i] - sb[i]; d > maxDiff {
			maxDiff = d
		} else if -d > maxDiff {
			maxDiff = -d
		}
	}
	if maxDiff > 0.005 {
		t.Errorf("6ch-identical waveform differs from the mono mixdown by %.4f; the mixdown is corrupted", maxDiff)
	}
}

// TestLateCorruptionKeepsFingerprint: a file clean for well over the fingerprint's
// 120-second bound but truncated past it. The bounded fingerprint decode never
// reaches the rot and succeeds; the whole-file measure does reach it and fails.
// Because loudness/peaks are best-effort and the fingerprint is independent of the
// whole-file read, the file must be STORED with its fingerprint (so it groups and
// converges) and counted in MeasureFailed, not discarded into Errored, which would
// re-decode the whole file forever without progress.
func TestLateCorruptionKeepsFingerprint(t *testing.T) {
	if testing.Short() {
		t.Skip("late-corruption fixture is a >120s encode")
	}
	dir := t.TempDir()
	const rate = 8000
	// 150s of clean audio, then drop the tail so the cut lands past the 120s the
	// fingerprint reads but within the whole-file measure.
	enc := testaudio.EncodeAs(t, "aac", "", rate, cheapSignal(rate*150))
	trunc := append([]byte(nil), enc[:len(enc)*85/100]...)
	f := writeFixture(t, dir, "late.m4a", 0, trunc)

	// The bounded fingerprint decode reads only the clean first 120s and succeeds.
	a := pureGoAnalyzer(t, newFakeStore(f))
	sub, algo, _, err := a.fingerprintFile(context.Background(), f)
	if err != nil || algo == 0 || len(sub) == 0 {
		t.Fatalf("fingerprint of the clean head should succeed: algo=%d sub=%d err=%v", algo, len(sub), err)
	}
	// The whole-file measure hits the rot, but the run keeps the fingerprint.
	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Analyzed != 1 || res.Errored != 0 || res.MeasureFailed != 1 {
		t.Fatalf("Run = {Analyzed:%d Errored:%d MeasureFailed:%d}, want {1 0 1}",
			res.Analyzed, res.Errored, res.MeasureFailed)
	}
	in, ok := store.puts[f.PID]
	if !ok || len(in.Fingerprint.FP) == 0 {
		t.Fatal("the valid fingerprint was not stored")
	}
	if in.Loudness != nil || in.Peaks != nil {
		t.Error("loudness/peaks should be nil when the whole-file measure failed")
	}
}
