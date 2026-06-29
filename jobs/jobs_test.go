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

	job, err := m.Run(ctx, "scan", "scan", func(ctx context.Context, h *jobs.Handle) error {
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

	job, err := m.Run(ctx, "scan", "scan", func(context.Context, *jobs.Handle) error {
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
