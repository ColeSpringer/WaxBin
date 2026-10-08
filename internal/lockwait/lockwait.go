// Package lockwait waits out a write lock that is changing hands. Both ends of a
// maintenance hand-off use it, the CLI taking the lock a server has just released and the
// server taking it back, since either can still see the lock held for a moment after the
// other let go of it under a heavy checkpoint or a loaded filesystem.
package lockwait

import (
	"context"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

// Retry runs try until it returns anything but a CodeConflict, waiting between attempts
// with a backoff that doubles from 5ms up to 250ms, for at most sixty waits (about 14
// seconds). A lock still held after that returns try's conflict, which names the owner; a
// ctx that ends meanwhile returns CodeCanceled under op.
func Retry(ctx context.Context, op string, try func() error) error {
	const maxWaits = 60
	const maxBackoff = 250 * time.Millisecond
	backoff := 5 * time.Millisecond
	for wait := 0; ; wait++ {
		err := try()
		if !waxerr.Is(err, waxerr.CodeConflict) || wait >= maxWaits {
			return err
		}
		select {
		case <-ctx.Done():
			return waxerr.FromContext(op, ctx.Err(), waxerr.CodeConflict)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
