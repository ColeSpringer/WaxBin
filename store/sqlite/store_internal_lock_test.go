package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestAWriteWaitsOutAMomentaryWriteLock: a write transaction that reads before it writes
// waits for a write lock another connection holds a moment, as a reader does while it
// rereads a torn WAL header, instead of failing with SQLITE_BUSY: a deferred transaction
// that already reads is refused the lock at once, with no busy wait. The lock is held
// until the write was tried, or for a moment if it never starts, so a transaction that
// does not wait meets the lock every time.
func TestAWriteWaitsOutAMomentaryWriteLock(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	other, err := sql.Open(driverName, "file:"+st.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	tried := make(chan struct{})
	released := make(chan error, 1)
	go func() {
		select {
		case <-tried:
		case <-time.After(300 * time.Millisecond):
		}
		_, err := conn.ExecContext(ctx, "ROLLBACK")
		released <- err
	}()
	err = st.writeTx(ctx, func(tx *sql.Tx) error {
		defer close(tried)
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM library").Scan(&n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO secret(key, value, updated_at) VALUES ('k', 'v', 1)")
		return err
	})
	if rerr := <-released; rerr != nil {
		t.Fatalf("release the write lock: %v", rerr)
	}
	if err != nil {
		t.Errorf("write beside a momentary write lock: %v, want it to wait", err)
	}
}
