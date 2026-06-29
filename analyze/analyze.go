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

// Version is the combined analysis version stored on each file. It includes the
// fingerprint, loudness, and peaks versions; changing any component makes prior
// analysis stale. Each component gets three decimal digits (0-999).
const Version = fingerprint.AlgoVersion*1_000_000 + loudness.AnalysisVersion*1_000 + peaks.Version

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
	store Store
	dec   *decode.Registry
	caps  caps.Caps
	log   *slog.Logger
}

// New builds an Analyzer. A nil registry uses decode.Default() (pure-Go WAV plus
// ffmpeg when present).
func New(store Store, dec *decode.Registry, log *slog.Logger) *Analyzer {
	if dec == nil {
		dec = decode.Default()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Analyzer{store: store, dec: dec, caps: caps.Detect(), log: log}
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
	total, err := a.store.CountFilesNeedingAnalysis(ctx, Version)
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
		files, err := a.store.FilesNeedingAnalysis(ctx, Version, afterRelPath, afterID, batchSize)
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
	dec, ok := a.dec.For(f.Codec)
	if !ok {
		res.Skipped++
		return nil
	}
	pcm, err := dec.Decode(ctx, string(f.Path), fingerprint.MaxAnalyze)
	if err != nil {
		return err
	}
	fp := fingerprint.Compute(pcm)

	// Bucket by the full track length from tags; fall back to the (possibly
	// capped) analyzed length only when the tag duration is unknown.
	durForBucket := f.DurationMS
	if durForBucket <= 0 {
		durForBucket = fp.DurationMS
	}
	in := model.AnalysisInput{
		AnalysisVersion: Version,
		Fingerprint: model.FingerprintInput{
			FilePID:        f.PID,
			EssenceHash:    f.EssenceHash,
			AlgoVersion:    fingerprint.AlgoVersion,
			DurationBucket: fingerprint.DurationBucket(durForBucket),
			FP:             fingerprint.Pack(fp.Sub),
			Terms:          fingerprint.IndexTerms(fp.Sub, fingerprint.DefaultIndexTerms),
		},
	}
	in.Loudness, in.Peaks = a.measure(ctx, f, dec)

	if err := a.store.PutAnalysis(ctx, in); err != nil {
		return err
	}
	res.Analyzed++
	if in.Loudness != nil {
		res.LoudnessMeasured++
	}
	return nil
}

// measure computes loudness and peaks, preferring ffmpeg's whole-file ebur128
// path and falling back to a bounded pure-Go BS.1770 pass. Either result may be
// nil after a transient failure.
func (a *Analyzer) measure(ctx context.Context, f *model.File, dec decode.Decoder) (*model.LoudnessData, *model.PeaksData) {
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

	// Pure-Go fallback: one bounded decode feeds both R128 and the waveform.
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
