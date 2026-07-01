// Package analyze owns the PCM-decoding pass behind the scan/analyze boundary.
// Scanning never decodes PCM; this pass computes audio-derived data such as the
// internal grouping fingerprint and its index terms. The pass is a resumable
// background job keyed by essence and algorithm version, so a file is analyzed
// once and re-analyzed only when its essence changes or the algorithm changes.
// Files whose codec this build cannot decode are reported, not failed.
package analyze

import (
	"context"
	"log/slog"
	"strconv"
	"time"

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
// (0-999); the fingerprint algorithm occupies the millions place, so the pure-Go
// (1) and Chromaprint (100) backends yield distinct numbers and never collide.
func effectiveVersion(fpAlgo int) int {
	return fpAlgo*1_000_000 + loudness.AnalysisVersion*1_000 + peaks.Version
}

// Version is the combined analysis version for the pure-Go fingerprint backend,
// the baseline when fpcalc is absent. The analyzer stamps effectiveVersion(algo)
// per file, swapping in the Chromaprint algorithm when fpcalc is present, so this
// is a single derived definition rather than a re-encoded formula.
var Version = effectiveVersion(fingerprint.AlgoVersion)

// loudnessMaxDecode bounds the pure-Go fallback so a long file cannot exhaust
// memory. Longer tracks use a prefix for loudness. The ffmpeg path streams the
// whole file and does not use this cap.
const loudnessMaxDecode = 15 * time.Minute

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
	dec     *decode.Registry
	caps    caps.Caps
	log     *slog.Logger
	fpAlgo  int // fingerprint backend: pure-Go, or Chromaprint when fpcalc is present
	version int // combined analysis version stamped on each file this run
}

// New builds an Analyzer. A nil registry uses decode.Default() (pure-Go WAV plus
// ffmpeg when present). The fingerprint backend is chosen from detected
// capabilities: fpcalc (Chromaprint) when present, else the pure-Go fingerprint.
func New(store Store, dec *decode.Registry, log *slog.Logger) *Analyzer {
	if dec == nil {
		dec = decode.Default()
	}
	if log == nil {
		log = slog.Default()
	}
	c := caps.Detect()
	fpAlgo := fingerprint.AlgoVersion
	if c.Fpcalc {
		fpAlgo = fingerprint.ChromaprintAlgoVersion
	}
	return &Analyzer{store: store, dec: dec, caps: c, log: log, fpAlgo: fpAlgo, version: effectiveVersion(fpAlgo)}
}

// Result tallies an analyze run.
type Result struct {
	Analyzed         int // fingerprints computed and stored
	LoudnessMeasured int // files that also got a ReplayGain measurement
	Skipped          int // no decoder for the file's codec (doctor reports coverage)
	Errored          int // decode/store failures (logged, not fatal)
}

// Heartbeat reports progress; it may be nil.
type Heartbeat func(progress float64, msg string) error

const batchSize = 200

// Run analyzes every file whose fingerprint is missing or stale until none
// remain. It is resumable: each file is committed independently, so an
// interrupted run resumes where it left off. Files with no available decoder are
// counted as skipped and tried again on a future run (e.g. after ffmpeg is
// installed), without blocking the pass.
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

// analyzeFile decodes one file, fingerprints it, measures loudness and peaks,
// and stores the result atomically. Files with no decoder are skipped, so a build
// without ffmpeg still analyzes the formats it can decode. Loudness and peaks are
// optional; a failure there should not prevent the fingerprint from being stored.
func (a *Analyzer) analyzeFile(ctx context.Context, f *model.File, res *Result) error {
	dec, hasDec := a.dec.For(f.Codec)
	// fpcalc (the Chromaprint backend) reads the file itself, so it can fingerprint a
	// codec this build cannot decode; only the pure-Go backend needs a decoder to
	// fingerprint. A file with neither an available decoder nor the fpcalc backend is
	// skipped (and retried on a future run, e.g. once ffmpeg is installed).
	chroma := a.fpAlgo == fingerprint.ChromaprintAlgoVersion
	if !hasDec && !chroma {
		res.Skipped++
		return nil
	}

	sub, algo, fpDurationMS, err := a.fingerprintFile(ctx, f, dec, hasDec)
	if err != nil {
		return err
	}
	if algo == 0 {
		// fpcalc could not fingerprint this file and there is no decoder for the
		// pure-Go fallback: nothing to store; skipped and retried later. (A short file
		// that produces an empty-but-valid fingerprint keeps its real algo and is
		// stored as before, not skipped.)
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
		// being frozen with a version claiming Chromaprint while it carries an algo-1
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
	in.Loudness, in.Peaks = a.measure(ctx, f, dec, hasDec)

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
// fpcalc failure on one file is not fatal: it logs and, when a decoder is available,
// falls back to the pure-Go path for that file. When neither fpcalc nor a decoder
// can read the file it returns a nil vector (the caller skips and retries it).
func (a *Analyzer) fingerprintFile(ctx context.Context, f *model.File, dec decode.Decoder, hasDec bool) ([]uint32, int, int64, error) {
	if a.fpAlgo == fingerprint.ChromaprintAlgoVersion {
		sub, durSec, err := fingerprint.ChromaprintRaw(ctx, a.caps.FpcalcPath, string(f.Path), fingerprint.MaxAnalyze)
		if err == nil {
			return sub, fingerprint.ChromaprintAlgoVersion, int64(durSec) * 1000, nil
		}
		if ctx.Err() != nil {
			return nil, 0, 0, ctx.Err()
		}
		a.log.Warn("fpcalc fingerprint failed", "path", f.DisplayPath, "err", err)
		if !hasDec {
			return nil, 0, 0, nil // no decoder for the pure-Go fallback
		}
	}
	pcm, err := dec.Decode(ctx, string(f.Path), fingerprint.MaxAnalyze)
	if err != nil {
		return nil, 0, 0, err
	}
	fp := fingerprint.Compute(pcm)
	return fp.Sub, fingerprint.AlgoVersion, fp.DurationMS, nil
}

// indexTerms builds the inverted-index min-hash terms for the chosen fingerprint
// backend (Chromaprint hashes its wider sub-values; the pure-Go fingerprint packs
// its 15-bit ones).
func indexTerms(algo int, sub []uint32) []int64 {
	if algo == fingerprint.ChromaprintAlgoVersion {
		return fingerprint.ChromaprintTerms(sub, fingerprint.DefaultIndexTerms)
	}
	return fingerprint.IndexTerms(sub, fingerprint.DefaultIndexTerms)
}

// measure computes loudness and peaks, preferring ffmpeg's whole-file ebur128
// path and falling back to a bounded pure-Go BS.1770 pass. Either result may be
// nil after a transient failure.
func (a *Analyzer) measure(ctx context.Context, f *model.File, dec decode.Decoder, hasDec bool) (*model.LoudnessData, *model.PeaksData) {
	path := string(f.Path)
	if a.caps.FFmpeg {
		var ld *model.LoudnessData
		if r, err := loudness.FFmpeg(ctx, a.caps.FFmpegPath, path); err != nil {
			a.log.Warn("loudness ebur128", "path", f.DisplayPath, "err", err)
		} else {
			ld = loudnessData(r)
		}
		var pk *model.PeaksData
		if p, err := peaks.StreamFFmpeg(ctx, a.caps.FFmpegPath, path, peaks.DefaultBuckets); err != nil {
			a.log.Warn("peaks", "path", f.DisplayPath, "err", err)
		} else {
			pk = peaksData(p, f.EssenceHash) // StreamFFmpeg covers the whole file
		}
		return ld, pk
	}

	// The pure-Go fallback needs a decoder. Without one (an fpcalc-fingerprinted file
	// whose codec this build cannot decode, and no ffmpeg) loudness and peaks are
	// unavailable. The fingerprint still stands.
	if !hasDec {
		return nil, nil
	}
	// One bounded decode feeds both R128 and the waveform.
	pcm, err := dec.Decode(ctx, path, loudnessMaxDecode)
	if err != nil {
		a.log.Warn("loudness decode", "path", f.DisplayPath, "err", err)
		return nil, nil
	}
	ld := loudnessData(loudness.R128(pcm))
	// Do not store a waveform from a capped decode. Stretching a prefix across the
	// full scrubber is misleading. Prefix loudness is still useful as an
	// approximation; the ffmpeg path streams the whole file.
	var pk *model.PeaksData
	if f.DurationMS <= 0 || f.DurationMS <= loudnessMaxDecode.Milliseconds() {
		pk = peaksData(peaks.Compute(pcm.Mono(), peaks.DefaultBuckets), f.EssenceHash)
	}
	return ld, pk
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
