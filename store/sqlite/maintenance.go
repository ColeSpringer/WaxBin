package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/waxerr"
)

// Vacuum compacts the database file, reclaiming space freed by deletes and
// defragmenting pages. It runs outside a transaction (VACUUM cannot run inside
// one) on the serialized write connection, so it takes the write lock. WAL mode
// is preserved across the vacuum.
func (s *Store) Vacuum(ctx context.Context) error {
	const op = "store.Vacuum"
	if s.readOnly || s.write == nil {
		return waxerr.New(waxerr.CodeUnsupported, op, "library opened read-only")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return waxerr.New(waxerr.CodeUnsupported, op, "store is closed")
	}
	if _, err := s.write.ExecContext(ctx, "VACUUM"); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

// IntegrityCheck runs SQLite's own PRAGMA integrity_check and returns the
// problems it reports. A healthy database returns a single "ok"; any other
// message is a real corruption finding. It is read-only.
func (s *Store) IntegrityCheck(ctx context.Context) ([]string, error) {
	const op = "store.IntegrityCheck"
	rows, err := s.read.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// PruneChangeLog trims the change_log to its newest keep rows, returning how many
// were deleted. Consumers that fall behind the retained horizon must full-resync
// (the documented delta-sync contract); keeping a bounded tail stops the log from
// growing without limit. keep must be positive.
func (s *Store) PruneChangeLog(ctx context.Context, keep int) (int, error) {
	const op = "store.PruneChangeLog"
	if keep <= 0 {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "keep must be positive")
	}
	var deleted int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM change_log WHERE seq < (
				SELECT MIN(seq) FROM (SELECT seq FROM change_log ORDER BY seq DESC LIMIT ?))`,
			keep)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		deleted, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(deleted), nil
}
