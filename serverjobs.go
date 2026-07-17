package waxbin

import (
	"context"
	"encoding/json"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the server-run-job seam. A long-running mutating pass
// (scan/analyze/enrich/organize) starts asynchronously and its progress is
// followed through the job row. A server that holds the write lock runs the pass
// in its own process and stays fully available: the proxy dispatches a Start*
// here, returns the job PID, and the client tails the read-only job row. The
// server is never closed for one of these jobs, which is what keeps an embedding
// host such as WaxDeck serving while a CLI-submitted scan runs.

// jobFn is the body of a job, sharing the exact work between the synchronous
// facade method and its asynchronous Start* variant.
type jobFn = func(context.Context, *jobs.Handle) error

// startJob starts a job and returns its PID as soon as the job row exists, then
// keeps running it in the background under ctx (a server passes its own lifetime
// context, so the job outlives the client request and is canceled only on server
// shutdown). A failure to even start the job (a scope already leased, or a create
// error) is returned synchronously.
func (l *Library) startJob(ctx context.Context, kind, scope string, work jobFn) (model.PID, error) {
	if l.ReadOnly() {
		return "", waxerr.New(waxerr.CodeUnsupported, "waxbin.startJob", "a background job requires a read-write library")
	}
	type outcome struct {
		pid model.PID
		err error
	}
	ch := make(chan outcome, 1)
	// Track the goroutine so Close drains it: a shutdown must let the job finalize
	// against the still-open store rather than close it out from under the job. Add
	// before the goroutine starts so a concurrent Close cannot miss it.
	l.jobsWG.Add(1)
	go func() {
		defer l.jobsWG.Done()
		ran := false
		_, err := l.jobs.Run(ctx, kind, scope, func(jctx context.Context, h *jobs.Handle) error {
			// The job row now exists: hand its PID back so the submit call can return
			// while the work runs on.
			ran = true
			ch <- outcome{pid: h.JobPID()}
			return work(jctx, h)
		})
		// Run returned without ever invoking fn: a lease conflict or a create error.
		if !ran {
			ch <- outcome{err: err}
		}
	}()
	res := <-ch
	return res.pid, res.err
}

// jsonResult marshals a job's result summary for storage on the job row. A marshal
// failure yields an empty result rather than failing the job, which has already
// done its work by the time the summary is recorded; the (near-impossible, since
// these are plain data structs) failure is logged so a silent empty summary is
// diagnosable rather than mysterious to a tailer.
func (l *Library) jsonResult(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		l.log.Warn("marshaling job result summary; tailer will see no summary", "err", err)
		return ""
	}
	return string(b)
}

// Job returns a job by public id, the read a client uses to tail a server-run job
// it submitted: progress and status while running, the terminal state, and the
// JSON Result summary once done.
func (l *Library) Job(ctx context.Context, pid model.PID) (*model.Job, error) {
	return l.store.JobByPID(ctx, pid)
}

// --- scan ---

// scanWork runs the scan across libs, accumulating into out. It is shared by the
// synchronous Scan and the asynchronous StartScan.
func (l *Library) scanWork(libs []*model.Library, req ScanRequest, out *ScanResult) jobFn {
	return func(ctx context.Context, h *jobs.Handle) error {
		for _, lib := range libs {
			r, err := l.scanner.Scan(ctx, scan.Request{
				Library: lib, SubPath: req.SubPath, Force: req.Force,
				AdoptStampedPIDs: req.AdoptStampedPIDs, ForceReconcile: req.ForceReconcile,
				IgnoreLocks: req.IgnoreLocks,
			}, func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
			if err != nil {
				return err
			}
			out.Runs = append(out.Runs, *r)
			addResult(&out.Total, r)
		}
		return nil
	}
}

// StartScan submits a scan as a background job and returns its PID immediately.
// The job runs to completion in this process; a client follows it through Job and
// reads the scan.Result summary from the finished job's Result.
func (l *Library) StartScan(ctx context.Context, req ScanRequest) (model.PID, error) {
	libs, err := l.resolveLibraries(ctx, req.LibraryPID)
	if err != nil {
		return "", err
	}
	return l.startJob(ctx, "scan", fsMutateScope, func(jctx context.Context, h *jobs.Handle) error {
		out := &ScanResult{}
		if err := l.scanWork(libs, req, out)(jctx, h); err != nil {
			return err
		}
		h.SetResult(l.jsonResult(out.Total))
		return nil
	})
}

// --- analyze ---

// analyzeWork runs the analyze pass into out. Shared by Analyze and StartAnalyze.
func (l *Library) analyzeWork(writeRG bool, out *AnalyzeResult) jobFn {
	return func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.analyzer.Run(ctx, func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		if r != nil {
			out.Result = *r
		}
		if err != nil {
			return err
		}
		if err := l.store.RefreshAlbumGain(ctx); err != nil {
			return err
		}
		if writeRG {
			c, err := l.writeReplayGainTags(ctx)
			if err != nil {
				return err
			}
			out.Result.ReplayGainTagsWritten = c.written
			out.Result.ReplayGainTagsFailed = c.failed
			out.Result.ReplayGainTagsUnrepresented = c.unrepresented
		}
		return nil
	}
}

// StartAnalyze submits the analyze pass as a background job and returns its PID.
func (l *Library) StartAnalyze(ctx context.Context, opts AnalyzeOptions) (model.PID, error) {
	writeRG := l.opts.WriteReplayGainTags || opts.WriteReplayGainTags
	return l.startJob(ctx, "analyze", "analyze", func(jctx context.Context, h *jobs.Handle) error {
		out := &AnalyzeResult{}
		if err := l.analyzeWork(writeRG, out)(jctx, h); err != nil {
			return err
		}
		h.SetResult(l.jsonResult(out.Result))
		return nil
	})
}

// --- enrich ---

// enrichWork runs the enrichment pass into out. Shared by Enrich and StartEnrich.
func (l *Library) enrichWork(opts EnrichOptions, out *EnrichResult) jobFn {
	return func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.enricher.Run(ctx, enrich.RunOptions{Force: opts.Force, Limit: opts.Limit},
			func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		if r != nil {
			out.Result = *r
		}
		return err
	}
}

// StartEnrich submits the enrichment pass as a background job and returns its PID.
// It refuses (before starting a job) when enrichment is not configured, matching
// the synchronous Enrich.
func (l *Library) StartEnrich(ctx context.Context, opts EnrichOptions) (model.PID, error) {
	if !l.enricher.Enabled() {
		return "", waxerr.New(waxerr.CodeUnsupported, "waxbin.StartEnrich",
			"enrichment needs a MusicBrainz contact (set enrichment.contact / WAXBIN_ENRICH_CONTACT)")
	}
	return l.startJob(ctx, "enrich", "enrich", func(jctx context.Context, h *jobs.Handle) error {
		out := &EnrichResult{}
		if err := l.enrichWork(opts, out)(jctx, h); err != nil {
			return err
		}
		h.SetResult(l.jsonResult(out.Result))
		return nil
	})
}

// --- organize ---

// RunOrganize submits an organize pass as a background job and returns its PID. The
// job plans across the managed libraries and executes the moves in this process, so
// a server stays available while it runs. profileName overrides each library's
// configured profile when non-empty. The client tails the job and reads the
// organize.Report summary from Result.
func (l *Library) RunOrganize(ctx context.Context, q query.Query, profileName string) (model.PID, error) {
	return l.startJob(ctx, "organize", fsMutateScope, func(jctx context.Context, h *jobs.Handle) error {
		plan, err := l.PlanOrganize(jctx, q, profileName)
		if err != nil {
			return err
		}
		rep, err := l.organizer.Execute(jctx, plan, h.JobPID(),
			func(p float64, msg string) error { return h.Heartbeat(jctx, p, msg) })
		if err != nil {
			return err
		}
		if rep != nil {
			// Record the resolved profile alongside the report so a tailing client prints
			// the same profile name the direct path does, not a placeholder.
			h.SetResult(l.jsonResult(organize.RunResult{Profile: plan.Profile, Report: *rep}))
		}
		return nil
	})
}
