package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxbin/model"
)

// heldLeaseStore answers every lease request as held by another owner. RunLeasedWait
// calls nothing else on it while the lease stays out of reach.
type heldLeaseStore struct {
	model.JobStore
	tries int
}

func (s *heldLeaseStore) AcquireLease(context.Context, *model.Lease) (bool, error) {
	s.tries++
	return false, nil
}

// TestRunLeasedWaitStopsAtItsLastTry: the try that spends the bound returns its conflict at
// once, so the bound costs one step fewer than it has tries and no cancel can land after
// the last one.
func TestRunLeasedWaitStopsAtItsLastTry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		st := &heldLeaseStore{}
		m := NewManager(st, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
		start := time.Now()
		err := m.RunLeasedWait(context.Background(), "podcast-fs", func(context.Context) error {
			t.Error("the work ran without its lease")
			return nil
		})
		if !IsLeaseHeld(err) || st.tries != leaseWaitTries {
			t.Fatalf("err = %v after %d tries, want the held-lease conflict after %d", err, st.tries, leaseWaitTries)
		}
		if got, want := time.Since(start), (leaseWaitTries-1)*leaseWaitStep; got != want {
			t.Errorf("waited %v, want %v: no step after the last try", got, want)
		}
	})
}
