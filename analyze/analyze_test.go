package analyze

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testfpcalc"
	"github.com/colespringer/waxbin/loudness"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/peaks"
	"github.com/colespringer/waxbin/waxerr"
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
	files            []*model.File
	puts             map[model.PID]model.AnalysisInput
	verdicts         map[model.PID][]model.FileDiagnostic
	putErr           error
	fallbacksCleared int
}

func newFakeStore(files ...*model.File) *fakeStore {
	return &fakeStore{files: files, puts: map[model.PID]model.AnalysisInput{},
		verdicts: map[model.PID][]model.FileDiagnostic{}}
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

func (s *fakeStore) PutDecodeVerdict(_ context.Context, filePID model.PID, _ string, ds []model.FileDiagnostic) error {
	s.verdicts[filePID] = ds
	return nil
}

func (s *fakeStore) ClearFingerprintFallbacks(context.Context) error {
	s.fallbacksCleared++
	return nil
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

	// The waveform's span is the accumulator's own count, and the tap sees every chunk
	// the meter does, so it equals the measured frame count exactly on every format.
	// Lossless formats decode to the signal's length; lossy ones add their priming.
	// Opus decodes at 48 kHz whatever the source rate was.
	eng := decode.New(discardLog())
	for i, f := range files {
		pk := store.puts[f.PID].Peaks
		if pk == nil {
			continue
		}
		m, err := eng.Measure(context.Background(), string(f.Path), nil)
		if err != nil {
			t.Fatalf("%s: measure: %v", f.DisplayPath, err)
		}
		if pk.Frames != m.Frames || pk.SampleRate != m.SampleRate {
			t.Errorf("%s: waveform spans %d frames at %d Hz, the meter measured %d at %d",
				f.DisplayPath, pk.Frames, pk.SampleRate, m.Frames, m.SampleRate)
		}
		wantRate, want := rate, int64(len(sig))
		if formats[i].format == "opus" {
			wantRate, want = 48000, int64(len(sig))*48000/rate
		}
		if pk.SampleRate != wantRate {
			t.Errorf("%s: waveform rate = %d, want %d", f.DisplayPath, pk.SampleRate, wantRate)
		}
		slack := int64(0)
		switch formats[i].format {
		case "mp3", "aac", "opus", "vorbis":
			slack = lossyPriming
		}
		if d := pk.Frames - want; d < -slack || d > slack {
			t.Errorf("%s: waveform spans %d frames, want %d (within %d)", f.DisplayPath, pk.Frames, want, slack)
		}
	}
}

// lossyPriming bounds how far a lossy decode's length may drift from the signal it
// encoded, from the encoder's priming and padding.
const lossyPriming = 600

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
	if ds, ok := store.verdicts[f.PID]; ok {
		t.Errorf("an unsupported file got a verdict %+v; nothing read its bytes", ds)
	}
}

// TestRunOpenPhaseDamageErrored: a file whose magic matched and whose headers are then
// damaged fails at open, and it must still land in Errored so audit can name it,
// rather than in Skipped where it would be retried forever. The pass counts it and
// carries on.
func TestRunOpenPhaseDamageErrored(t *testing.T) {
	dir := t.TempDir()
	f := writeFixture(t, dir, "truncated.flac", 0, []byte("fLaC\x00\x00\x00"))
	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errored != 1 || res.Analyzed != 0 || res.Skipped != 0 {
		t.Fatalf("Run = {Analyzed:%d Skipped:%d Errored:%d}, want {0 0 1}", res.Analyzed, res.Skipped, res.Errored)
	}
	if _, ok := store.puts[f.PID]; ok {
		t.Error("a damaged file was stamped; nothing was measured")
	}
	assertDecodeVerdict(t, store, f)
}

// assertDecodeVerdict checks the pass recorded a corrupt_audio error for a file whose
// decode failed on its bytes, so the audit sees it though nothing was analyzed.
func assertDecodeVerdict(t *testing.T, store *fakeStore, f *model.File) {
	t.Helper()
	ds := store.verdicts[f.PID]
	if len(ds) != 1 || ds[0].Code != model.DiagCorruptAudio || ds[0].Severity != model.SeverityError || ds[0].Detail == "" {
		t.Errorf("verdict for %s = %+v, want one corrupt_audio error with a detail", f.DisplayPath, ds)
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
	assertDecodeVerdict(t, store, f)
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
	ld, pk, _, err := a.measure(ctx, f)
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

	lMono, pMono, _, err := a.measure(context.Background(), fMono)
	if err != nil {
		t.Fatalf("mono measure: %v", err)
	}
	lMulti, pMulti, _, err := a.measure(context.Background(), fMulti)
	if err != nil {
		t.Fatalf("surround measure: %v", err)
	}
	if lMono == nil || lMulti == nil {
		t.Fatal("expected a loudness measurement for both files")
	}
	if pMono == nil || pMulti == nil {
		t.Fatal("expected a waveform for both files")
	}
	if pMono.Frames != int64(len(mono)) || pMulti.Frames != int64(len(mono)) {
		t.Errorf("spans = %d mono, %d surround frames, want %d for both (a frame is every channel's sample)",
			pMono.Frames, pMulti.Frames, len(mono))
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

// TestMeasureRecordsTheSpan: the waveform carries how many frames it divides and at
// what rate, so a reader can place a window on it without the header's duration.
func TestMeasureRecordsTheSpan(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 3*time.Second)
	f := writeFixture(t, dir, "a.wav", 0, testaudio.EncodeWAV16(rate, sig))
	_, pk, _, err := pureGoAnalyzer(t, newFakeStore()).measure(context.Background(), f)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if pk == nil {
		t.Fatal("no waveform")
	}
	if pk.Frames != int64(len(sig)) || pk.SampleRate != rate || pk.DurationMS() != 3000 {
		t.Errorf("span = %d frames at %d Hz (%d ms), want %d at %d (3000 ms)",
			pk.Frames, pk.SampleRate, pk.DurationMS(), len(sig), rate)
	}
}

// TestLateCorruptionKeepsFingerprint: a file clean for well over the fingerprint's
// 120-second bound but truncated past it. The bounded fingerprint decode never
// reaches the rot and succeeds; the whole-file measure does reach it and fails.
// Because loudness/peaks are best-effort and the fingerprint is independent of the
// whole-file read, the file must be STORED with its fingerprint (so it groups and
// converges) and counted in MeasureFailed, not discarded into Errored, which would
// discard the fingerprint too and leave the file with nothing to group on.
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
	fp, err := a.fingerprintFile(context.Background(), f)
	if err != nil || fp.algo == 0 || len(fp.sub) == 0 {
		t.Fatalf("fingerprint of the clean head should succeed: algo=%d sub=%d err=%v", fp.algo, len(fp.sub), err)
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
	if in.MeasureCompleted {
		t.Error("a measure that fell over must not stamp the file as measured; the retry predicate reads that flag")
	}
}

// TestSilenceStampsAsMeasured is the other half of TestLateCorruptionKeepsFingerprint.
// Silence never passes the loudness gate, so it stores no loudness row either, and
// the two used to look identical in the catalog. Its measurement did run to the end
// of the file, so it is stamped and the pass leaves it alone from here.
func TestSilenceStampsAsMeasured(t *testing.T) {
	dir := t.TempDir()
	const rate = 8000
	f := writeFixture(t, dir, "silence.wav", 0, testaudio.EncodeWAV16(rate, make([]float32, rate*2)))

	store := newFakeStore(f)
	res, err := pureGoAnalyzer(t, store).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Analyzed != 1 || res.MeasureFailed != 0 {
		t.Fatalf("Run = {Analyzed:%d MeasureFailed:%d}, want {1 0}: measuring silence is not a failure",
			res.Analyzed, res.MeasureFailed)
	}
	in := store.puts[f.PID]
	if in.Loudness != nil {
		t.Error("silence has no gated loudness to store")
	}
	if !in.MeasureCompleted {
		t.Error("silence measured fine; without the stamp the pass re-decodes it every run")
	}
}

// TestVersionLeavesThePreBumpStampBehind: 1001001 is the combined stamp every
// pure-Go-fingerprinted file carried before the WaxFlow bump. That decoder gave the
// meter half an HE-AAC file and could not open WavPack, Monkey's Audio or WMA at
// all, so those measurements are wrong or missing, and only a stamp that differs
// gets them redone.
func TestVersionLeavesThePreBumpStampBehind(t *testing.T) {
	const preBump = 1_001_001
	if Version == preBump {
		t.Fatalf("Version is still the pre-bump %d; loudness.AnalysisVersion (%d) did not move with the decoder",
			Version, loudness.AnalysisVersion)
	}
}

// TestMeasureSettled pins which measure failures stamp the file. An unsupported
// input settles (nothing this build runs can change the answer, and a decoder
// change re-selects it through AnalysisVersion); anything retryable stays clear.
func TestMeasureSettled(t *testing.T) {
	if !measureSettled(nil) {
		t.Error("a clean measure should settle")
	}
	if !measureSettled(fmt.Errorf("open: %w", decode.ErrUnsupported)) {
		t.Error("an unsupported input should settle rather than re-decode every run")
	}
	if measureSettled(errors.New("read: transient glitch")) {
		t.Error("a retryable failure must leave the stamp clear")
	}
}

// TestPeaksDataNeedsARate: a waveform whose decode reported no rate cannot be placed,
// so it is dropped rather than refused by the store with the fingerprint beside it.
func TestPeaksDataNeedsARate(t *testing.T) {
	p := peaks.Compute(cheapSignal(4000), 10)
	if pk := peaksData(p, 4000, 0, "e"); pk != nil {
		t.Errorf("peaksData with no rate = %+v, want nil", pk)
	}
	if pk := peaksData(p, 4000, 8000, "e"); pk == nil || pk.Frames != 4000 || pk.SampleRate != 8000 {
		t.Errorf("peaksData = %+v, want 4000 frames at 8000 Hz", pk)
	}
}

// TestRunRecordsTheDecodeVerdict: the decode that measures loudness reads every file
// end to end, so what it found wrong with the bytes is kept. A FLAC cut short is
// reported whether the read worked around the cut (a warning) or failed on it (an
// error), and a clean file reports an empty list that clears an older verdict.
func TestRunRecordsTheDecodeVerdict(t *testing.T) {
	dir := t.TempDir()
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 4*time.Second))
	cut := writeFixture(t, dir, "cut.flac", 0, flac[:len(flac)*60/100])
	clean := writeFixture(t, dir, "clean.flac", 1, flac)
	store := newFakeStore(cut, clean)
	if _, err := pureGoAnalyzer(t, store).Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	in, ok := store.puts[cut.PID]
	if !ok {
		t.Fatal("the cut file was not stored")
	}
	if !in.Observed || len(in.Diagnostics) != 1 {
		t.Fatalf("cut file: observed=%v diagnostics=%+v, want one observed verdict", in.Observed, in.Diagnostics)
	}
	d := in.Diagnostics[0]
	if d.Code != model.DiagCorruptAudio || (d.Severity != model.SeverityWarn && d.Severity != model.SeverityError) || d.Detail == "" {
		t.Errorf("cut file verdict = %+v, want corrupt_audio at warn or error with a detail", d)
	}

	in = store.puts[clean.PID]
	if !in.Observed || len(in.Diagnostics) != 0 {
		t.Errorf("clean file: observed=%v diagnostics=%+v, want an observed empty list", in.Observed, in.Diagnostics)
	}
}

// TestMeasureObserved pins which measure outcomes looked at the audio bytes, so that
// their verdict replaces the one before. Bad bytes did; an input no decoder here
// reads, an unreadable source and a cancel did not.
func TestMeasureObserved(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"clean", nil, true},
		{"bad bytes", waxerr.New(waxerr.CodeInvalid, "decode.Measure", "bad frame"), true},
		{"unsupported", fmt.Errorf("open: %w", decode.ErrUnsupported), false},
		{"unreadable", waxerr.New(waxerr.CodeIO, "decode.Measure", "read failed"), false},
		{"canceled", context.Canceled, false},
	} {
		if got := measureObserved(c.err); got != c.want {
			t.Errorf("%s: measureObserved = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunBucketsByTheDecodedLength: the fingerprint's duration bucket comes from the
// decoded length, so a file whose header understates it still lands beside its
// twins. The header's value is the fallback when the decode yields no waveform.
func TestRunBucketsByTheDecodedLength(t *testing.T) {
	dir := t.TempDir()
	const rate = 8000
	f := writeFixture(t, dir, "short-header.wav", 0, testaudio.EncodeWAV16(rate, cheapSignal(rate*8)))
	f.DurationMS = 4000
	store := newFakeStore(f)
	if _, err := pureGoAnalyzer(t, store).Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	in := store.puts[f.PID]
	if got, want := in.Fingerprint.DurationBucket, fingerprint.DurationBucket(8000); got != want {
		t.Errorf("duration bucket = %d, want %d (8 s decoded, not the header's 4 s)", got, want)
	}
}

// TestRunLeavesAWideLayoutWithoutAVerdict: a healthy file with more channels than this
// build mixes is refused, which says what the build cannot do rather than what the
// bytes are, so it gets no corrupt_audio verdict.
func TestRunLeavesAWideLayoutWithoutAVerdict(t *testing.T) {
	dir := t.TempDir()
	const rate, chans = 8000, 10
	mono := cheapSignal(rate * 2)
	inter := make([]float32, len(mono)*chans)
	for i, v := range mono {
		for c := range chans {
			inter[i*chans+c] = v
		}
	}
	f := writeFixture(t, dir, "wide.wav", 0, testaudio.EncodeWAV16Multi(rate, chans, inter))
	store := newFakeStore(f)
	if _, err := pureGoAnalyzer(t, store).Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if in, ok := store.puts[f.PID]; ok && (in.Observed || len(in.Diagnostics) > 0) {
		t.Errorf("observed=%v diagnostics=%+v, want no verdict on a file the meter refused", in.Observed, in.Diagnostics)
	}
	if v := store.verdicts[f.PID]; len(v) > 0 {
		t.Errorf("decode verdict = %+v, want none", v)
	}
}

// TestRunBucketsADamagedFileByItsHeader: a read that worked around damage decodes less
// than the recording holds, so a cut file keeps its header's length and stays beside the
// intact copy.
func TestRunBucketsADamagedFileByItsHeader(t *testing.T) {
	dir := t.TempDir()
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 8*time.Second))
	f := writeFixture(t, dir, "cut.flac", 0, flac[:len(flac)/2])
	f.DurationMS = 8000
	store := newFakeStore(f)
	if _, err := pureGoAnalyzer(t, store).Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	in := store.puts[f.PID]
	if in.Peaks == nil || len(in.Diagnostics) != 1 || in.Diagnostics[0].Severity != model.SeverityWarn {
		t.Fatalf("peaks=%v diagnostics=%+v, want a waveform read around the damage", in.Peaks != nil, in.Diagnostics)
	}
	if got, want := in.Fingerprint.DurationBucket, fingerprint.DurationBucket(8000); got != want {
		t.Errorf("duration bucket = %d, want %d (the header's 8 s, not the %d ms that decoded)", got, want, in.Peaks.DurationMS())
	}
}

// TestBucketDuration pins the order the fingerprint bucket's length is taken in: the
// decoded length unless the read was damaged, then the header's, then the analyzed
// head's.
func TestBucketDuration(t *testing.T) {
	for _, c := range []struct {
		name               string
		pk                 *model.PeaksData
		damaged            bool
		header, head, want int64
	}{
		{"decoded length first", &model.PeaksData{Frames: 80000, SampleRate: 8000}, false, 4000, 3000, 10000},
		{"header over a damaged read", &model.PeaksData{Frames: 80000, SampleRate: 8000}, true, 4000, 3000, 4000},
		{"header without a waveform", nil, false, 4000, 3000, 4000},
		{"header when the waveform has no rate", &model.PeaksData{Frames: 80000}, false, 4000, 3000, 4000},
		{"analyzed head last", nil, false, 0, 3000, 3000},
	} {
		if got := bucketDuration(c.pk, c.damaged, c.header, c.head); got != c.want {
			t.Errorf("%s: bucketDuration = %d, want %d", c.name, got, c.want)
		}
	}
}

// chromaprintAnalyzer builds an Analyzer on the Chromaprint backend with bin as fpcalc.
func chromaprintAnalyzer(store Store, bin string) *Analyzer {
	a := New(store, nil, discardLog())
	a.fpAlgo = fingerprint.ChromaprintAlgoVersion
	a.caps.Fpcalc, a.caps.FpcalcPath = true, bin
	a.version = effectiveVersion(a.fpAlgo)
	return a
}

// TestRunCountsAFpcalcFallback: a file fpcalc fails on is fingerprinted by the pure-Go
// path, counted, and stored with fpcalc's reason for the analyze origin's
// fingerprint_fallback row; the next run, with fpcalc working, stores the Chromaprint
// fingerprint and no reason, which clears the row.
func TestRunCountsAFpcalcFallback(t *testing.T) {
	ctx := context.Background()
	f := writeFixture(t, t.TempDir(), "a.wav", 1, testaudio.EncodeWAV16(44100, cheapSignal(44100*3)))
	f.DurationMS = 3000
	store := newFakeStore(f)

	res, err := chromaprintAnalyzer(store, testfpcalc.Write(t, "", "ERROR: Could not find any audio stream in the file", 2)).Run(ctx, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	in := store.puts[f.PID]
	if res.Analyzed != 1 || res.FingerprintFallbacks != 1 || res.FingerprintPartialReads != 0 {
		t.Fatalf("result = %+v, want one file analyzed through the fallback", res)
	}
	if in.Fingerprint.AlgoVersion != fingerprint.AlgoVersion || !strings.Contains(in.Fallback, "Could not find any audio stream") {
		t.Fatalf("stored algo %d, fallback %q; want the pure-Go fingerprint with fpcalc's reason", in.Fingerprint.AlgoVersion, in.Fallback)
	}

	res, err = chromaprintAnalyzer(store, testfpcalc.Write(t, `{"duration": 3.00, "fingerprint": [1,2,3,4]}`, "", 0)).Run(ctx, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	in = store.puts[f.PID]
	if res.FingerprintFallbacks != 0 || in.Fingerprint.AlgoVersion != fingerprint.ChromaprintAlgoVersion ||
		in.AnalysisVersion != effectiveVersion(fingerprint.ChromaprintAlgoVersion) || in.Fallback != "" {
		t.Fatalf("second run = %+v, stored algo %d, version %d, fallback %q; want Chromaprint with no fallback",
			res, in.Fingerprint.AlgoVersion, in.AnalysisVersion, in.Fallback)
	}
}

// TestRunKeepsAPartialFpcalcRead: a read error fpcalc reports after fingerprinting the
// whole file keeps its Chromaprint fingerprint, counted apart and with no fallback row.
func TestRunKeepsAPartialFpcalcRead(t *testing.T) {
	ctx := context.Background()
	f := writeFixture(t, t.TempDir(), "a.wav", 1, testaudio.EncodeWAV16(44100, cheapSignal(44100*3)))
	f.DurationMS = 3000
	store := newFakeStore(f)
	bin := testfpcalc.Write(t, `{"duration": 3.00, "fingerprint": [1,2,3,4]}`, "ERROR: Error decoding audio frame (End of file)", 3)

	res, err := chromaprintAnalyzer(store, bin).Run(ctx, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	in := store.puts[f.PID]
	if res.FingerprintPartialReads != 1 || res.FingerprintFallbacks != 0 ||
		in.Fingerprint.AlgoVersion != fingerprint.ChromaprintAlgoVersion || in.Fallback != "" {
		t.Fatalf("result = %+v, stored algo %d, fallback %q; want the partial Chromaprint read kept",
			res, in.Fingerprint.AlgoVersion, in.Fallback)
	}
}

// TestOnlyAPureGoRunClearsFallbacks: a pass with no fpcalc never re-selects a file that
// fell back, since its stamp already names the pure-Go fingerprint, so that pass is what
// drops the fallback rows, while a Chromaprint pass leaves them to each file's analysis.
func TestOnlyAPureGoRunClearsFallbacks(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	if _, err := pureGoAnalyzer(t, store).Run(ctx, nil); err != nil {
		t.Fatalf("pure-Go run: %v", err)
	}
	if store.fallbacksCleared != 1 {
		t.Fatalf("pure-Go run cleared fallbacks %d times, want once", store.fallbacksCleared)
	}
	if _, err := chromaprintAnalyzer(store, "fpcalc-unused").Run(ctx, nil); err != nil {
		t.Fatalf("chromaprint run: %v", err)
	}
	if store.fallbacksCleared != 1 {
		t.Errorf("chromaprint run cleared fallbacks, want them left to each analysis")
	}
}
