// Package analyze owns the PCM-decoding pass behind the scan/analyze boundary.
// Scanning never decodes PCM; this pass computes audio-derived data such as the
// internal grouping fingerprint and its index terms. The pass is a resumable
// background job keyed by essence and algorithm version, so a file is analyzed
// once and re-analyzed only when its essence changes or the algorithm changes.
// An input this build cannot decode is skipped and retried on a future run,
// when a later WaxFlow may decode it; a corrupt one is reported as an error, so
// audit sees it, rather than being buried as a silent skip and retried forever.
package analyze

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/internal/caps"
	"github.com/colespringer/waxbin/loudness"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/peaks"
)

// effectiveVersion combines a fingerprint algorithm with the loudness and peaks
// versions into the single value stamped on a file. Changing any component makes
// prior analysis stale. Each non-fingerprint component gets three decimal digits
// (0-999); the fingerprint algorithm is multiplied by a million, so the pure-Go
// (1) and Chromaprint (100) backends yield distinct numbers and never collide.
//
// Nothing here tracks WaxFlow. A decoder that starts producing different samples
// for a codec it already handled invalidates every measurement of that codec, and
// this value will not notice, so read WaxFlow's decoder-version changes on every
// waxflow bump and bump loudness.AnalysisVersion when one of them moved.
//
// Folding format.DecoderVersion in per codec would automate that, and it is
// deliberately not done: it would key catalog codec strings to WaxFlow codec IDs,
// the vocabulary sync decode.Coverage refuses for the same reason. The failure
// mode is a whole format silently never re-analyzing.
func effectiveVersion(fpAlgo int) int {
	return fpAlgo*1_000_000 + loudness.AnalysisVersion*1_000 + peaks.Version
}

// Version is the combined analysis version for the pure-Go fingerprint backend,
// the baseline when fpcalc is absent. The analyzer stamps effectiveVersion(algo)
// per file, swapping in the Chromaprint algorithm when fpcalc is present, so this
// is a single derived definition rather than a re-encoded formula.
var Version = effectiveVersion(fingerprint.AlgoVersion)

// Store is the persistence the analyze pass needs (a focused port satisfied by
// store/sqlite).
type Store interface {
	FilesNeedingAnalysis(ctx context.Context, algoVersion int, afterRelPath []byte, afterID int64, limit int) ([]*model.File, error)
	CountFilesNeedingAnalysis(ctx context.Context, algoVersion int) (int, error)
	PutAnalysis(ctx context.Context, in model.AnalysisInput) error
}

// Analyzer runs the analyze pass over a catalog.
type Analyzer struct {
	store   Store
	eng     *decode.Engine
	caps    caps.Caps
	log     *slog.Logger
	fpAlgo  int // fingerprint backend: pure-Go, or Chromaprint when fpcalc is present
	version int // combined analysis version stamped on each file this run
}

// New builds an Analyzer. A nil engine uses decode.New(log) (pure-Go WaxFlow
// decode for every codec WaxBin can tag-read). The fingerprint backend is chosen
// from detected capabilities: fpcalc (Chromaprint) when present, else the pure-Go
// fingerprint.
func New(store Store, eng *decode.Engine, log *slog.Logger) *Analyzer {
	if log == nil {
		log = slog.Default()
	}
	if eng == nil {
		eng = decode.New(log)
	}
	c := caps.Detect()
	fpAlgo := fingerprint.AlgoVersion
	if c.Fpcalc {
		fpAlgo = fingerprint.ChromaprintAlgoVersion
	}
	return &Analyzer{store: store, eng: eng, caps: c, log: log, fpAlgo: fpAlgo, version: effectiveVersion(fpAlgo)}
}

// Result tallies an analyze run.
type Result struct {
	Analyzed         int // fingerprints computed and stored
	LoudnessMeasured int // files that also got a ReplayGain measurement
	Skipped          int // input WaxFlow cannot decode (retried on a future run)
	Errored          int // decode/store failures (logged, not fatal)
	// MeasureFailed counts files that were stamped with a fingerprint but whose
	// best-effort loudness/peaks decode failed: a damaged tail past the
	// fingerprinted head, an undecodable input on an fpcalc host, or a transient
	// read glitch. The file still groups on its fingerprint, but its measurement is
	// left unstamped, so the next run tries again. A permanently damaged tail
	// therefore reports here every run instead of freezing unmeasured and unseen.
	MeasureFailed int
	// ReplayGainTagsWritten counts files whose computed ReplayGain was written back
	// to disk (only when the write-back toggle is on; the facade fills this).
	ReplayGainTagsWritten int
	// ReplayGainTagsFailed counts files whose write-back errored (a read-only file, a
	// vanished path). It is not fatal, since the measurement is in the catalog either
	// way, but a run where every write failed must not read as one with nothing to write.
	ReplayGainTagsFailed int
	// ReplayGainTagsUnrepresented counts files whose gain never reached the disk
	// because the file could not hold it: a value the tag format dropped, or a
	// container WaxLabel cannot write at all. Neither is worth retrying, and the
	// first can even report as a landed no-op, so both are counted apart from
	// failures.
	ReplayGainTagsUnrepresented int
}

// Heartbeat reports progress; it may be nil.
type Heartbeat func(progress float64, msg string) error

const batchSize = 200

// Run analyzes every file whose fingerprint is missing or stale until none
// remain. It is resumable: each file is committed independently, so an
// interrupted run resumes where it left off. Files this build cannot decode are
// counted as skipped and tried again on a future run (e.g. once a later WaxFlow
// covers the format), without blocking the pass.
func (a *Analyzer) Run(ctx context.Context, hb Heartbeat) (*Result, error) {
	res := &Result{}
	// A one-shot total taken up front lets the heartbeat report a real ratio.
	// The single-writer model means the needing-analysis set only shrinks during
	// the run (analyzed files drop out), so processed/total stays monotonic.
	total, err := a.store.CountFilesNeedingAnalysis(ctx, a.version)
	if err != nil {
		return res, err
	}
	progress := func() float64 {
		done := res.Analyzed + res.Skipped + res.Errored
		if total <= 0 || done >= total {
			return 1
		}
		return float64(done) / float64(total)
	}

	// Keyset cursor over (rel_path, id): each batch resumes strictly after the
	// last file seen, so files skipped for lack of a decoder are stepped over
	// rather than re-fetched, and decodable files later in the order are always
	// reached. The cursor strictly advances, so the loop always terminates.
	var afterRelPath []byte
	var afterID int64
	for {
		files, err := a.store.FilesNeedingAnalysis(ctx, a.version, afterRelPath, afterID, batchSize)
		if err != nil {
			return res, err
		}
		if len(files) == 0 {
			break
		}
		for _, f := range files {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if err := a.analyzeFile(ctx, f, res); err != nil {
				// A canceled context now propagates out of analyzeFile (measure and
				// fingerprintFile surface it rather than swallowing it), so stop the
				// run cleanly instead of counting the rest of the page as errors.
				if ctx.Err() != nil {
					return res, err
				}
				a.log.Warn("analyze file", "path", f.DisplayPath, "err", err)
				res.Errored++
			}
			if hb != nil && (res.Analyzed+res.Skipped+res.Errored)%25 == 0 {
				if err := hb(progress(), "analyzed "+strconv.Itoa(res.Analyzed)+" files"); err != nil {
					return res, err
				}
			}
		}
		last := files[len(files)-1]
		afterRelPath, afterID = last.RelPath, last.ID
	}
	if hb != nil {
		_ = hb(1, "analyzed "+strconv.Itoa(res.Analyzed)+" files")
	}
	return res, nil
}

// analyzeFile decodes one file, fingerprints it, measures loudness and peaks, and
// stores the result atomically. An input this build cannot decode is skipped (algo
// 0), so the pass still analyzes every format WaxFlow covers. Loudness and peaks
// are best-effort: if measuring them fails, the fingerprint is still stored so the
// file groups and converges, and the miss is counted. A retryable failure (a
// damaged tail past the fingerprinted head, a transient glitch) leaves the
// measurement stamp clear so the file re-selects; an undecodable input on an
// fpcalc host settles instead, until a decoder change moves AnalysisVersion. Only
// a canceled context stamps nothing.
func (a *Analyzer) analyzeFile(ctx context.Context, f *model.File, res *Result) error {
	sub, algo, fpDurationMS, err := a.fingerprintFile(ctx, f)
	if err != nil {
		return err
	}
	if algo == 0 {
		// Neither fpcalc nor the pure-Go decoder could read this file: nothing to
		// store; skipped and retried later, when a later WaxFlow may decode a format
		// this build cannot. (A short file that produces an empty-but-valid
		// fingerprint keeps its real algo and is stored as before, not skipped.)
		res.Skipped++
		return nil
	}

	// Bucket by the full track length from tags; fall back to the (possibly
	// capped) analyzed length only when the tag duration is unknown.
	durForBucket := f.DurationMS
	if durForBucket <= 0 {
		durForBucket = fpDurationMS
	}
	in := model.AnalysisInput{
		// Stamp the version for the algorithm actually used, not the run's preferred
		// backend. A file that fell back to pure-Go (fpcalc failed on it) then reads as
		// stale on the next run and is retried once fpcalc can handle it, instead of
		// being frozen with a version claiming Chromaprint while it carries an algo-2
		// vector (which the candidate join would never group and never re-analyze).
		AnalysisVersion: effectiveVersion(algo),
		Fingerprint: model.FingerprintInput{
			FilePID:        f.PID,
			EssenceHash:    f.EssenceHash,
			AlgoVersion:    algo,
			DurationBucket: fingerprint.DurationBucket(durForBucket),
			FP:             fingerprint.Pack(sub),
			Terms:          indexTerms(algo, sub),
		},
	}
	// Loudness and peaks are best-effort. The fingerprint already stands (from the
	// bounded decode above, or from fpcalc, independent of this whole-file read), so
	// a failure measuring them must not discard it: the file groups and stays grouped
	// while the measurement is retried. Keep the fingerprint, null the loudness, and
	// count the miss. Whether the stamp clears depends on the failure: a retryable
	// one leaves it clear so the file re-selects, while ErrUnsupported settles it,
	// since on an fpcalc host the fingerprint comes from fpcalc and no decoder in
	// this build can measure the input; re-selecting it would re-decode the file and
	// churn its fingerprint rows on every run for an answer that cannot change until
	// a decoder change moves AnalysisVersion. Only a canceled context aborts before
	// stamping.
	ld, pk, err := a.measure(ctx, f)
	in.MeasureCompleted = measureSettled(err)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		a.log.Warn("analyze: loudness/peaks unavailable; storing fingerprint only",
			"path", f.DisplayPath, "err", err)
		res.MeasureFailed++
		ld, pk = nil, nil
	}
	in.Loudness, in.Peaks = ld, pk

	if err := a.store.PutAnalysis(ctx, in); err != nil {
		return err
	}
	res.Analyzed++
	if in.Loudness != nil {
		res.LoudnessMeasured++
	}
	return nil
}

// fingerprintFile computes a file's grouping fingerprint, preferring fpcalc
// (Chromaprint) when present and falling back to the pure-Go fingerprint. It
// returns the sub-fingerprint vector, the algorithm that produced it (stored so
// grouping never compares incomparable layouts), and the analyzed duration. An
// fpcalc failure on one file is not fatal: it logs and falls back to the pure-Go
// path for that file. An input this build cannot decode returns a nil vector with
// algo 0 (the caller skips and retries it); a corrupt one returns a real error.
func (a *Analyzer) fingerprintFile(ctx context.Context, f *model.File) ([]uint32, int, int64, error) {
	if a.fpAlgo == fingerprint.ChromaprintAlgoVersion {
		sub, durSec, err := fingerprint.ChromaprintRaw(ctx, a.caps.FpcalcPath, string(f.Path), fingerprint.MaxAnalyze)
		if err == nil {
			return sub, fingerprint.ChromaprintAlgoVersion, int64(durSec) * 1000, nil
		}
		if ctx.Err() != nil {
			return nil, 0, 0, ctx.Err()
		}
		a.log.Warn("fpcalc fingerprint failed", "path", f.DisplayPath, "err", err)
		// Fall through to the pure-Go decode for this one file.
	}
	pcm, err := a.eng.Mono(ctx, string(f.Path), fingerprint.InternalRate, fingerprint.MaxAnalyze)
	if err != nil {
		// ErrUnsupported is set only on the open call, so a corrupt-but-recognized
		// file does not land here: skip and retry it on a future run.
		if errors.Is(err, decode.ErrUnsupported) {
			return nil, 0, 0, nil
		}
		if ctx.Err() != nil {
			return nil, 0, 0, ctx.Err()
		}
		// Any other error, mid-stream corruption included, is a real failure that
		// surfaces the file to audit rather than burying it as a silent skip.
		return nil, 0, 0, err
	}
	fp := fingerprint.Compute(pcm)
	return fp.Sub, fingerprint.AlgoVersion, fp.DurationMS, nil
}

// indexTerms builds the inverted-index min-hash terms for the chosen fingerprint
// backend. It defers to fingerprint.TermsForAlgo, which owns the algo->terms dispatch,
// so the analyze write path and the cross-catalog resolve probe always derive the same
// terms for a given fingerprint.
func indexTerms(algo int, sub []uint32) []int64 {
	return fingerprint.TermsForAlgo(algo, sub)
}

// measure computes whole-file loudness and a waveform in one streamed decode: the
// engine's meter measures loudness while a tap folds each chunk's mono mix into
// the peaks accumulator, so the waveform costs no second decode and memory stays
// O(1) in the track length.
//
// A non-nil error covers two situations the caller separates by ctx: a canceled
// context (fatal: abort the run, stamp nothing) versus any decode failure (an
// undecodable input, a damaged file, or a transient read glitch). Loudness and
// peaks are best-effort, so the caller absorbs the latter and keeps the
// already-computed fingerprint; only cancellation is fatal to the file.
func (a *Analyzer) measure(ctx context.Context, f *model.File) (*model.LoudnessData, *model.PeaksData, error) {
	acc := peaks.NewAccumulator(peaks.DefaultBuckets)
	// scratch is our own reused mixdown buffer. MixMono returns it (grown as needed)
	// for a real mix but returns ch[0] aliased for mono input, so retain it only for
	// the mix: scratch then never becomes an alias into WaxFlow's chunk buffer that a
	// later mix could overwrite. (Channel count is fixed for a file, so mono and
	// multichannel chunks never interleave in one decode; this just keeps the
	// buffer's ownership plain rather than argued from that invariant.)
	var scratch []float32
	m, err := a.eng.Measure(ctx, string(f.Path), func(ch [][]float32) {
		mono := decode.MixMono(scratch[:0], ch)
		if len(ch) > 1 {
			scratch = mono
		}
		acc.Add(mono)
	})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, nil, cerr // cancellation is fatal: abort the run cleanly
		}
		// Any other failure (ErrUnsupported on an fpcalc host, a damaged file, or a
		// transient read) is a best-effort miss the caller absorbs while keeping the
		// fingerprint; measureSettled decides which of them stamp the file.
		return nil, nil, err
	}
	return loudnessData(loudness.FromMeasurement(m.IntegratedLUFS, m.SamplePeakDB)),
		peaksData(acc.Peaks(), f.EssenceHash), nil
}

// measureSettled reports whether a measure outcome stamps the file as measured for
// its current essence. A clean run settles, and so does ErrUnsupported: no decoder
// in this build can read the input, so retrying cannot change the answer, and the
// file re-selects when a decoder change moves AnalysisVersion. Anything else is
// worth retrying and leaves the stamp clear.
func measureSettled(err error) bool {
	return err == nil || errors.Is(err, decode.ErrUnsupported)
}

// loudnessData converts a loudness.Result to the stored form. It returns nil for
// silent or too-short material that did not produce a gated measurement.
func loudnessData(r loudness.Result) *model.LoudnessData {
	if !r.Valid {
		return nil
	}
	return &model.LoudnessData{
		IntegratedLUFS: r.IntegratedLUFS,
		TrackGainDB:    loudness.TrackGainDB(r.IntegratedLUFS),
		TrackPeak:      r.SamplePeak,
	}
}

// peaksData converts a waveform to the packed stored form, stamped with the
// essence it covers. It returns nil for an empty waveform.
func peaksData(p peaks.Peaks, essence string) *model.PeaksData {
	if len(p.Buckets) == 0 {
		return nil
	}
	return &model.PeaksData{Version: peaks.Version, Buckets: len(p.Buckets), Data: peaks.Pack(p), EssenceHash: essence}
}
