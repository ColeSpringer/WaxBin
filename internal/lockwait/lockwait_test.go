package lockwait

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

func TestRetryStopsAtAnythingButAConflict(t *testing.T) {
	t.Parallel()
	other := errors.New("disk on fire")
	for _, tc := range []struct {
		name   string
		fails  int // conflicts before the answer
		answer error
		calls  int
	}{
		{"free at once", 0, nil, 1},
		{"freed on the third try", 2, nil, 3},
		{"another failure", 0, other, 1},
		{"another failure after a conflict", 1, other, 2},
	} {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			err := Retry(context.Background(), "test", func() error {
				calls++
				if calls <= tc.fails {
					return waxerr.New(waxerr.CodeConflict, "test", "held")
				}
				return tc.answer
			})
			if err != tc.answer || calls != tc.calls {
				t.Errorf("%s: err %v after %d calls, want %v after %d", tc.name, err, calls, tc.answer, tc.calls)
			}
		})
	}
}

// TestRetryKeepsItsCeiling: the wait between attempts never passes its 250ms ceiling, so
// sixty waits on a lock that stays held take no more than fifteen seconds, and the
// conflict that names the owner is what comes back.
func TestRetryKeepsItsCeiling(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := waxerr.New(waxerr.CodeConflict, "test", "held by someone")
		calls := 0
		start := time.Now()
		err := Retry(context.Background(), "test", func() error { calls++; return held })
		if err != held || calls != 61 {
			t.Errorf("a lock that stays held: err %v after %d calls, want the conflict after 61", err, calls)
		}
		if waited := time.Since(start); waited > 15*time.Second {
			t.Errorf("sixty waits took %v, longer than sixty at the 250ms ceiling", waited)
		}
	})
}

func TestRetryEndsWithItsContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := Retry(ctx, "test.op", func() error {
			calls++
			if calls == 3 {
				cancel()
			}
			return waxerr.New(waxerr.CodeConflict, "test", "held")
		})
		if !waxerr.Is(err, waxerr.CodeCanceled) || calls != 3 {
			t.Errorf("canceled while waiting: err %v after %d calls, want CodeCanceled after 3", err, calls)
		}
	})
}
