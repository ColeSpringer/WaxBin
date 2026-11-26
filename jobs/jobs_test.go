package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

func newManager(t *testing.T) (*jobs.Manager, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{
		Path:   testcatalog.Seed(t, filepath.Join(t.TempDir(), "jobs.db")),
		Owner:  "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return jobs.NewManager(st, "test", slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func TestRunHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, _ := newManager(t)

	job, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, h *jobs.Handle) error {
		return h.Heartbeat(ctx, 0.5, "halfway")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if job.State != model.JobDone {
		t.Fatalf("state = %s, want done", job.State)
	}
}

// TestRunReturnsErrorOnPanic verifies a panicking job is recovered into a
// CodeInternal error (not propagated), recorded as failed, and the lease frees.
func TestRunReturnsErrorOnPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)

	job, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(context.Context, *jobs.Handle) error {
		panic("boom")
	})
	if err == nil {
		t.Fatal("expected panic to be returned as an error")
	}
	if !waxerr.Is(err, waxerr.CodeInternal) {
		t.Fatalf("want CodeInternal, got %v (code %s)", err, waxerr.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("panic detail not in error: %v", err)
	}
	if job == nil || job.State != model.JobFailed {
		t.Fatalf("job not marked failed: %+v", job)
	}
	if !strings.Contains(job.Error, "boom") {
		t.Fatalf("panic detail not recorded on job: %q", job.Error)
	}

	// The failure is persisted, and the lease was released.
	list, err := m.List(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].State != model.JobFailed {
		t.Fatalf("job not persisted as failed: %+v", list)
	}
	ok, err := st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "next", AcquiredAt: 1, HeartbeatAt: 1})
	if err != nil || !ok {
		t.Fatalf("lease not released after panic: ok=%v err=%v", ok, err)
	}
}

// TestRunRecordsTheTarget: the job row names what a job targeted, and a whole-catalog
// job names nothing.
func TestRunRecordsTheTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	noop := func(context.Context, *jobs.Handle) error { return nil }
	scoped, err := m.Run(ctx, jobs.Spec{Kind: "enrich", Scope: "enrich", TargetType: "item", TargetPID: "01TARGET"}, noop)
	if err != nil {
		t.Fatalf("scoped run: %v", err)
	}
	whole, err := m.Run(ctx, jobs.Spec{Kind: "enrich", Scope: "enrich"}, noop)
	if err != nil {
		t.Fatalf("whole run: %v", err)
	}
	got, err := st.JobByPID(ctx, scoped.PID)
	if err != nil || got.TargetType != "item" || got.TargetPID != "01TARGET" {
		t.Fatalf("scoped job = %+v (err %v), want item:01TARGET", got, err)
	}
	got, err = st.JobByPID(ctx, whole.PID)
	if err != nil || got.TargetType != "" || got.TargetPID != "" {
		t.Fatalf("whole-catalog job = %+v (err %v), want no target", got, err)
	}
}

// TestRunRecordsACancel: a run whose context is canceled and that returns the cancel
// ends canceled, not failed, with a message saying it was cut short.
func TestRunRecordsACancel(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	job, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, _ *jobs.Handle) error {
		cancel()
		return waxerr.FromContext("scan.Scan", ctx.Err(), waxerr.CodeIO)
	})
	if !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("err = %v, want the cancel returned", err)
	}
	if job.State != model.JobCanceled || job.Error != "interrupted before it finished" {
		t.Fatalf("job = %s %q, want canceled and interrupted", job.State, job.Error)
	}
	list, err := m.List(context.Background(), 1)
	if err != nil || len(list) != 1 || list[0].State != model.JobCanceled {
		t.Fatalf("persisted = %+v (err %v), want canceled", list, err)
	}

	// A job that hands back its context's raw error was canceled all the same.
	ctx, cancel = context.WithCancel(context.Background())
	job, _ = m.Run(ctx, jobs.Spec{Kind: "enrich", Scope: "enrich"}, func(ctx context.Context, _ *jobs.Handle) error {
		cancel()
		return ctx.Err()
	})
	if job.State != model.JobCanceled {
		t.Errorf("raw cancel = %s, want canceled", job.State)
	}

	// A cancel error from a context that was not canceled is a failure like any other.
	job, _ = m.Run(context.Background(), jobs.Spec{Kind: "scan", Scope: "scan"}, func(context.Context, *jobs.Handle) error {
		return waxerr.New(waxerr.CodeCanceled, "inner", "a sub-operation gave up")
	})
	if job.State != model.JobFailed {
		t.Errorf("inner cancel = %s, want failed", job.State)
	}
}

// TestLeaseHeldIsTyped: a held scope is a conflict whose reason says so, whose cause
// names the scope, and whose text reads as it always has.
func TestLeaseHeldIsTyped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	if ok, err := st.AcquireLease(ctx, &model.Lease{Scope: "fs-mutate", Owner: "other", AcquiredAt: 1, HeartbeatAt: 1}); err != nil || !ok {
		t.Fatalf("hold the scope: ok=%v err=%v", ok, err)
	}
	err := m.RunLeased(ctx, "fs-mutate", func(context.Context) error { return nil })
	var held *jobs.LeaseHeldError
	if !waxerr.Is(err, waxerr.CodeConflict) || waxerr.ReasonOf(err) != jobs.ReasonLeaseHeld || !jobs.IsLeaseHeld(err) {
		t.Fatalf("held scope = %v (%s %q), want a conflict whose reason is lease_held", err, waxerr.CodeOf(err), waxerr.ReasonOf(err))
	}
	if !errors.As(err, &held) || held.Scope != "fs-mutate" {
		t.Fatalf("cause = %+v, want a LeaseHeldError naming fs-mutate", held)
	}
	if err.Error() != "jobs.RunLeased: another job holds scope fs-mutate" {
		t.Fatalf("text = %q, want the sentence unchanged", err.Error())
	}
	if jobs.IsLeaseHeld(waxerr.New(waxerr.CodeConflict, "store", "owned elsewhere")) || jobs.IsLeaseHeld(nil) {
		t.Fatal("another conflict, or no error, reads as a held lease")
	}
}

// conflictingStore refuses every lease with a conflict that is not a held scope, the
// shape of a write-ownership refusal.
type conflictingStore struct {
	model.JobStore
	calls atomic.Int32
}

func (s *conflictingStore) AcquireLease(context.Context, *model.Lease) (bool, error) {
	s.calls.Add(1)
	return false, waxerr.New(waxerr.CodeConflict, "store.AcquireLease", "the catalog is owned elsewhere")
}

// TestRunLeasedWaitDoesNotRetryOtherConflicts: only a held lease is waited out; any other
// conflict comes back at once without the work running.
func TestRunLeasedWaitDoesNotRetryOtherConflicts(t *testing.T) {
	t.Parallel()
	_, st := newManager(t)
	cs := &conflictingStore{JobStore: st}
	m := jobs.NewManager(cs, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ran := false
	err := m.RunLeasedWait(ctx, "podcast-fs", func(context.Context) error { ran = true; return nil })
	if !waxerr.Is(err, waxerr.CodeConflict) || jobs.IsLeaseHeld(err) || ran || cs.calls.Load() != 1 {
		t.Fatalf("err = %v (%s), ran %v, %d tries; want the conflict back after one try", err, waxerr.CodeOf(err), ran, cs.calls.Load())
	}
}

// TestRunLeasedWaitRetriesAHeldLease: a held scope is waited out and the work runs once
// the holder lets go.
func TestRunLeasedWaitRetriesAHeldLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	if ok, err := st.AcquireLease(ctx, &model.Lease{Scope: "podcast-fs", Owner: "other", AcquiredAt: 1, HeartbeatAt: 1}); err != nil || !ok {
		t.Fatalf("hold the scope: ok=%v err=%v", ok, err)
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = st.ReleaseLease(ctx, "podcast-fs", "other") })
	ran := false
	if err := m.RunLeasedWait(ctx, "podcast-fs", func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("wait = %v, ran %v; want the work run once the lease freed", err, ran)
	}
}

// TestAFailedJobKeepsItsErrorClass: a job's row keeps the class and reason of the error
// that ended it beside its message, so a client tailing it can answer with the same class;
// a panic is internal and a cancel canceled, and a job that finished carries none.
func TestAFailedJobKeepsItsErrorClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	stored := func(fn func(context.Context, *jobs.Handle) error) *model.Job {
		t.Helper()
		job, _ := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, fn)
		got, err := st.JobByPID(ctx, job.PID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	locked := stored(func(context.Context, *jobs.Handle) error {
		return waxerr.New(waxerr.CodeLocked, "test", "item is locked").WithReason("kind_locked")
	})
	if locked.State != model.JobFailed || locked.ErrorCode != waxerr.CodeLocked || locked.ErrorReason != "kind_locked" || locked.Error == "" {
		t.Errorf("failed job = %+v, want its class and reason kept", locked)
	}
	if p := stored(func(context.Context, *jobs.Handle) error { panic("boom") }); p.ErrorCode != waxerr.CodeInternal || p.ErrorReason != "" {
		t.Errorf("panicked job = %+v, want internal", p)
	}
	cctx, cancel := context.WithCancel(ctx)
	job, _ := m.Run(cctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, _ *jobs.Handle) error {
		cancel()
		return waxerr.FromContext("test", ctx.Err(), waxerr.CodeIO)
	})
	if c, err := st.JobByPID(ctx, job.PID); err != nil || c.State != model.JobCanceled || c.ErrorCode != waxerr.CodeCanceled {
		t.Errorf("canceled job = %+v (err %v), want canceled", c, err)
	}
	if ok := stored(func(context.Context, *jobs.Handle) error { return nil }); ok.ErrorCode != "" || ok.ErrorReason != "" {
		t.Errorf("done job = %+v, want no class", ok)
	}
}

// TestCheckpointSavesTheResultWhileRunning: a checkpoint is the job row's result while the
// job runs, the result it finishes with unless SetResult replaces it, and writes no change
// row (a tailer reads the row; a change consumer has nothing to refresh).
func TestCheckpointSavesTheResultWhileRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	job, err := m.Run(ctx, jobs.Spec{Kind: "set-kind", Scope: "set-kind"}, func(ctx context.Context, h *jobs.Handle) error {
		seq, err := st.LatestChangeSeq(ctx)
		if err != nil {
			return err
		}
		if err := h.Checkpoint(ctx, map[string]int{"reads": 1}); err != nil {
			return err
		}
		row, err := st.JobByPID(ctx, h.JobPID())
		if err != nil {
			return err
		}
		if row.State != model.JobRunning || row.Result != `{"reads":1}` {
			t.Errorf("row during the run = %+v, want the checkpoint", row)
		}
		if after, err := st.LatestChangeSeq(ctx); err != nil || after != seq {
			t.Errorf("change seq %d -> %d (err %v), want no change row", seq, after, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.JobByPID(ctx, job.PID); err != nil || row.Result != `{"reads":1}` {
		t.Errorf("finished row = %+v (err %v), want the checkpoint kept", row, err)
	}
}

// TestSetParamsRecordsTheJobsRequest: a job writes what it was asked to do to its row once
// it knows it, with no change row.
func TestSetParamsRecordsTheJobsRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	job, err := m.Run(ctx, jobs.Spec{Kind: "set-kind", Scope: "set-kind"}, func(ctx context.Context, h *jobs.Handle) error {
		seq, err := st.LatestChangeSeq(ctx)
		if err != nil {
			return err
		}
		if err := h.SetParams(ctx, map[string]any{"kind": "book"}); err != nil {
			return err
		}
		if after, err := st.LatestChangeSeq(ctx); err != nil || after != seq {
			t.Errorf("change seq %d -> %d (err %v), want no change row", seq, after, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.JobByPID(ctx, job.PID); err != nil || row.Params != `{"kind":"book"}` {
		t.Errorf("row = %+v (err %v), want the params kept", row, err)
	}
}

// TestCancelEndsTheJobOnRequest: a cancel by pid ends the job canceled with the request's
// reason, keeps the progress it reached, and Run returns that class and reason around the
// work's own error.
func TestCancelEndsTheJobOnRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	started := make(chan model.PID, 1)
	var workErr error
	type ran struct {
		job *model.Job
		err error
	}
	done := make(chan ran, 1)
	go func() {
		job, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, h *jobs.Handle) error {
			if err := h.Heartbeat(ctx, 0.4, "walking"); err != nil {
				return err
			}
			started <- h.JobPID()
			<-ctx.Done()
			workErr = waxerr.FromContext("scan.Scan", ctx.Err(), waxerr.CodeIO)
			return workErr
		})
		done <- ran{job, err}
	}()
	pid := <-started
	if !m.Cancel(pid) {
		t.Fatal("Cancel = false for a running job")
	}
	r := <-done
	if !waxerr.Is(r.err, waxerr.CodeCanceled) || waxerr.ReasonOf(r.err) != jobs.ReasonCancelRequested || !errors.Is(r.err, workErr) {
		t.Fatalf("Run = %v (%s %q), want canceled on request around the work's error", r.err, waxerr.CodeOf(r.err), waxerr.ReasonOf(r.err))
	}
	row, err := st.JobByPID(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != model.JobCanceled || row.Error != "canceled on request" || row.ErrorCode != waxerr.CodeCanceled ||
		row.ErrorReason != jobs.ReasonCancelRequested || row.Progress != 0.4 {
		t.Fatalf("row = %+v, want canceled on request at 40%%", row)
	}
	if m.Cancel(pid) {
		t.Error("Cancel = true for a finished job")
	}
	if m.Cancel("01UNKNOWNJOBPID0000000000") {
		t.Error("Cancel = true for a job that never ran")
	}
}

// TestACallersCancelIsNotARequest: a run its caller's context ends keeps the interrupted
// text and no reason, and returns the work's error as it was.
func TestACallersCancelIsNotARequest(t *testing.T) {
	t.Parallel()
	m, st := newManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	var workErr error
	job, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, _ *jobs.Handle) error {
		cancel()
		workErr = waxerr.FromContext("scan.Scan", ctx.Err(), waxerr.CodeIO)
		return workErr
	})
	if err != workErr || waxerr.ReasonOf(err) == jobs.ReasonCancelRequested {
		t.Fatalf("Run = %v, want the work's own error", err)
	}
	row, rerr := st.JobByPID(context.Background(), job.PID)
	if rerr != nil || row.Error != "interrupted before it finished" || row.ErrorReason != "" {
		t.Fatalf("row = %+v (err %v), want interrupted with no reason", row, rerr)
	}
}

// TestAJobKeepsItsProgressUnlessDone: a job that fails or is canceled lists the progress
// it reached; only a finished one reads 100%.
func TestAJobKeepsItsProgressUnlessDone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, st := newManager(t)
	progressOf := func(ctx context.Context, end func(context.Context) error) float64 {
		t.Helper()
		job, _ := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, h *jobs.Handle) error {
			if err := h.Heartbeat(ctx, 0.25, "a quarter"); err != nil {
				return err
			}
			return end(ctx)
		})
		row, err := st.JobByPID(context.Background(), job.PID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Progress
	}
	if p := progressOf(ctx, func(context.Context) error { return waxerr.New(waxerr.CodeIO, "test", "disk gone") }); p != 0.25 {
		t.Errorf("failed job progress = %v, want 0.25", p)
	}
	cctx, cancel := context.WithCancel(ctx)
	if p := progressOf(cctx, func(ctx context.Context) error { cancel(); return ctx.Err() }); p != 0.25 {
		t.Errorf("canceled job progress = %v, want 0.25", p)
	}
	if p := progressOf(ctx, func(context.Context) error { return nil }); p != 1 {
		t.Errorf("done job progress = %v, want 1", p)
	}
}

// TestAJobThatFinishesDespiteACancelIsDone: work that returns nil after a cancel landed
// finished, and its row says so.
func TestAJobThatFinishesDespiteACancelIsDone(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	job, err := m.Run(context.Background(), jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, h *jobs.Handle) error {
		m.Cancel(h.JobPID())
		<-ctx.Done()
		return nil
	})
	if err != nil || job.State != model.JobDone {
		t.Fatalf("Run = %v, state %s; want done", err, job.State)
	}
}

// blockedCreateStore holds CreateJob until release closes, telling entered the pid the
// job was given.
type blockedCreateStore struct {
	model.JobStore
	entered chan model.PID
	release chan struct{}
}

func (s *blockedCreateStore) CreateJob(ctx context.Context, j *model.Job) error {
	s.entered <- j.PID
	<-s.release
	return s.JobStore.CreateJob(ctx, j)
}

// TestCancelBeforeTheRowExists: a job is cancelable from the moment it has a pid, before
// its row is written; Cancel does not wait on that write, and the job ends canceled on
// request under the pid it was given.
func TestCancelBeforeTheRowExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, st := newManager(t)
	bs := &blockedCreateStore{JobStore: st, entered: make(chan model.PID, 1), release: make(chan struct{})}
	m := jobs.NewManager(bs, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() {
		_, err := m.Run(ctx, jobs.Spec{Kind: "scan", Scope: "scan"}, func(ctx context.Context, _ *jobs.Handle) error {
			<-ctx.Done()
			return waxerr.FromContext("scan.Scan", ctx.Err(), waxerr.CodeIO)
		})
		done <- err
	}()
	pid := <-bs.entered
	if pid == "" {
		t.Fatal("the job reached its insert with no pid")
	}
	canceled := make(chan bool, 1)
	go func() { canceled <- m.Cancel(pid) }()
	select {
	case ok := <-canceled:
		if !ok {
			t.Fatal("Cancel = false while the row is being written")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel waited on the job's insert")
	}
	close(bs.release)
	if err := <-done; waxerr.ReasonOf(err) != jobs.ReasonCancelRequested {
		t.Fatalf("Run = %v, want canceled on request", err)
	}
	if row, err := st.JobByPID(ctx, pid); err != nil || row.State != model.JobCanceled || row.ErrorReason != jobs.ReasonCancelRequested {
		t.Fatalf("row = %+v (err %v), want canceled on request under the given pid", row, err)
	}
}
