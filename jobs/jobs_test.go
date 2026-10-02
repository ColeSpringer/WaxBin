package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

func newManager(t *testing.T) (*jobs.Manager, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{
		Path:   filepath.Join(t.TempDir(), "jobs.db"),
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
