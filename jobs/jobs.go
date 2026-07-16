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
	now := time.Now().UnixNano()
	lease := &model.Lease{Scope: scope, Owner: m.owner, AcquiredAt: now, HeartbeatAt: now}
	ok, aerr := m.store.AcquireLease(ctx, lease)
	if aerr != nil {
		return nil, aerr
	}
	if !ok {
		return nil, waxerr.New(waxerr.CodeConflict, "jobs.Run", "another job holds scope "+scope)
	}

	cleanup := context.WithoutCancel(ctx)
	defer func() {
		if rerr := m.store.ReleaseLease(cleanup, scope, m.owner); rerr != nil {
			m.log.Warn("releasing lease", "scope", scope, "err", rerr)
		}
	}()

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
