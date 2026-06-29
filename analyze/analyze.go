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

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/model"
)

// Store is the persistence the analyze pass needs (a focused port satisfied by
// store/sqlite).
type Store interface {
	FilesNeedingAnalysis(ctx context.Context, algoVersion int, afterRelPath []byte, afterID int64, limit int) ([]*model.File, error)
	CountFilesNeedingAnalysis(ctx context.Context, algoVersion int) (int, error)
	PutFingerprint(ctx context.Context, in model.FingerprintInput) error
}

// Analyzer runs the analyze pass over a catalog.
type Analyzer struct {
	store Store
	dec   *decode.Registry
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
	return &Analyzer{store: store, dec: dec, log: log}
}

// Result tallies an analyze run.
type Result struct {
	Analyzed int // fingerprints computed and stored
	Skipped  int // no decoder for the file's codec (doctor reports coverage)
	Errored  int // decode/store failures (logged, not fatal)
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
	total, err := a.store.CountFilesNeedingAnalysis(ctx, fingerprint.AlgoVersion)
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
		files, err := a.store.FilesNeedingAnalysis(ctx, fingerprint.AlgoVersion, afterRelPath, afterID, batchSize)
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

// analyzeFile decodes one file (capped), fingerprints it, and stores the result.
// A codec with no decoder is skipped (not an error), so a no-ffmpeg build still
// completes the pass over the formats it can decode.
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
	in := model.FingerprintInput{
		FilePID:        f.PID,
		EssenceHash:    f.EssenceHash,
		AlgoVersion:    fingerprint.AlgoVersion,
		DurationBucket: fingerprint.DurationBucket(durForBucket),
		FP:             fingerprint.Pack(fp.Sub),
		Terms:          fingerprint.IndexTerms(fp.Sub, fingerprint.DefaultIndexTerms),
	}
	if err := a.store.PutFingerprint(ctx, in); err != nil {
		return err
	}
	res.Analyzed++
	return nil
}
