// Package jobs runs mutating background work under a scoped advisory lease and a
// tracked job row. A lease guarantees at most one mutating job per scope;
// heartbeats prove liveness; flock-based reclaim on Open (in store/sqlite) turns
// jobs orphaned by a crash into "crashed" without any PID check.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Manager creates and supervises jobs for a single write owner.
type Manager struct {
	store model.JobStore
	owner string
	log   *slog.Logger
}

// NewManager builds a job manager bound to a store and owner identity.
func NewManager(store model.JobStore, owner string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: store, owner: owner, log: log}
}

// Handle is the live job context passed to a job function for progress updates.
type Handle struct {
	mgr *Manager
	job *model.Job
}

// JobPID is the public id of the running job.
func (h *Handle) JobPID() model.PID { return h.job.PID }

// Heartbeat records progress (0..1) and a status message, proving liveness.
func (h *Handle) Heartbeat(ctx context.Context, progress float64, msg string) error {
	h.job.Progress = progress
	h.job.Message = msg
	h.job.HeartbeatAt = time.Now().UnixNano()
	return h.mgr.store.Heartbeat(ctx, h.job.ID, h.job.HeartbeatAt, progress, msg)
}

// SetResult attaches a JSON result summary to the job, persisted when the job is
// finalized. A server-run job records its result here so a client tailing the job
// row (which did not run the work in-process) can render the same outcome a local
// run prints. It is set in memory; Run's finalize writes it out with the terminal
// state.
func (h *Handle) SetResult(result string) { h.job.Result = result }

// Run acquires the lease for scope, creates a running job, invokes fn, then
// finalizes the job (done/failed) and releases the lease. It returns
// CodeConflict if the scope is already leased. A panic from fn is recovered,
// recorded as a failed job, and returned as a CodeInternal error rather than
// propagating to the caller. Finalization uses a cancel-free context so a
// canceled or panicked run still records its terminal state and frees the lease.
func (m *Manager) Run(ctx context.Context, kind, scope string, fn func(context.Context, *Handle) error) (job *model.Job, err error) {
	release, aerr := m.acquire(ctx, scope, "jobs.Run")
	if aerr != nil {
		return nil, aerr
	}
	defer release()

	cleanup := context.WithoutCancel(ctx)
	now := time.Now().UnixNano()
	job = &model.Job{
		Kind: kind, Scope: scope, State: model.JobRunning, Owner: m.owner,
		StartedAt: now, HeartbeatAt: now,
	}
	if cerr := m.store.CreateJob(ctx, job); cerr != nil {
		return nil, cerr
	}

	// Recover a panic from fn into a returned error (and a failed job) instead of
	// letting it crash the caller. The lease-release defer above still runs.
	defer func() {
		if r := recover(); r != nil {
			err = waxerr.New(waxerr.CodeInternal, "jobs.Run", fmt.Sprintf("panic: %v", r))
			m.finalize(cleanup, job, kind, model.JobFailed, err.Error())
		}
	}()

	runErr := fn(ctx, &Handle{mgr: m, job: job})
	if runErr != nil {
		m.finalize(cleanup, job, kind, model.JobFailed, runErr.Error())
	} else {
		m.finalize(cleanup, job, kind, model.JobDone, "")
	}
	return job, runErr
}

// RunLeased runs fn holding scope's lease with no job row, returning CodeConflict if
// the scope is already leased. It is Run's preamble without the tracking: a verb that
// is one step inside a user action wants the mutual exclusion but not a row in
// `waxbin jobs`, and a bulk pass calling a per-item verb would otherwise push a row
// per item through the job log.
//
// A batch pass worth showing in the log uses Run instead. The lease is released on
// every path, including a panic, which propagates rather than being converted the way
// Run converts it: there is no job row to record it against.
func (m *Manager) RunLeased(ctx context.Context, scope string, fn func(context.Context) error) error {
	release, err := m.acquire(ctx, scope, "jobs.RunLeased")
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// leaseWaitStep and leaseWaitTries bound RunLeasedWait's retry, at 30s total. Sized
// against the podcast scope's slowest holders, which are bulk unlink passes over a
// subscription set rather than the brief per-episode verbs; a pass larger than the
// budget still loses, which RunLeasedWait's doc states rather than pretending away.
const (
	leaseWaitStep  = 250 * time.Millisecond
	leaseWaitTries = 120
)

// RunLeasedWait is RunLeased with a bounded retry on a busy scope, for the one caller
// whose work is already paid for by the time it needs the lease (a finished download's
// commit tail) and would otherwise be discarded.
//
// The bound is real: a holder that outlasts it still yields CodeConflict and the
// caller still loses its work. That is the trade against hanging behind a pass whose
// own duration is unbounded.
//
// It is unsuitable for a scope whose holders run for minutes, such as fs-mutate, where
// the backoff would only add latency before the same CodeConflict.
func (m *Manager) RunLeasedWait(ctx context.Context, scope string, fn func(context.Context) error) error {
	var err error
	for try := 0; try < leaseWaitTries; try++ {
		var release func()
		release, err = m.acquire(ctx, scope, "jobs.RunLeasedWait")
		if err == nil {
			defer release()
			return fn(ctx)
		}
		if !waxerr.Is(err, waxerr.CodeConflict) {
			return err
		}
		select {
		case <-ctx.Done():
			return waxerr.FromContext("jobs.RunLeasedWait", ctx.Err(), waxerr.CodeIO)
		case <-time.After(leaseWaitStep):
		}
	}
	return err
}

// acquire takes scope's lease and returns the release func, or CodeConflict. The one
// lease-taking path, shared by all three runners, so the acquire/release pairing and
// the cancel-free release context cannot drift between them.
func (m *Manager) acquire(ctx context.Context, scope, op string) (func(), error) {
	now := time.Now().UnixNano()
	lease := &model.Lease{Scope: scope, Owner: m.owner, AcquiredAt: now, HeartbeatAt: now}
	ok, err := m.store.AcquireLease(ctx, lease)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, waxerr.New(waxerr.CodeConflict, op, "another job holds scope "+scope)
	}
	cleanup := context.WithoutCancel(ctx)
	return func() {
		if rerr := m.store.ReleaseLease(cleanup, scope, m.owner); rerr != nil {
			m.log.Warn("releasing lease", "scope", scope, "err", rerr)
		}
	}, nil
}

// finalize stamps a job's terminal state and persists it (best-effort).
func (m *Manager) finalize(ctx context.Context, job *model.Job, kind string, state model.JobState, errMsg string) {
	fin := time.Now().UnixNano()
	job.HeartbeatAt, job.FinishedAt, job.Progress = fin, fin, 1
	job.State, job.Error = state, errMsg
	if err := m.store.UpdateJob(ctx, job); err != nil {
		m.log.Warn("finalizing job", "kind", kind, "err", err)
	}
}

// List returns recent jobs, newest first.
func (m *Manager) List(ctx context.Context, limit int) ([]*model.Job, error) {
	return m.store.ListJobs(ctx, limit)
}
