package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// CreateImportBatch inserts a running import batch, assigning its id and pid.
func (s *Store) CreateImportBatch(ctx context.Context, b *model.ImportBatch) error {
	const op = "store.CreateImportBatch"
	b.PID = model.NewPID()
	if b.State == "" {
		b.State = model.ImportRunning
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO import_batch
			(pid, source, library_id, state, imported, duplicates, quarantined, errored, bytes, started_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			string(b.PID), b.Source, nullInt64(b.LibraryID), string(b.State),
			b.Imported, b.Duplicates, b.Quarantined, b.Errored, b.Bytes, b.StartedAt)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		b.ID = id
		return nil
	})
}

// UpdateImportBatch persists a batch's tallies and terminal state.
func (s *Store) UpdateImportBatch(ctx context.Context, b *model.ImportBatch) error {
	const op = "store.UpdateImportBatch"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE import_batch SET
			state=?, imported=?, duplicates=?, quarantined=?, errored=?, bytes=?, finished_at=?
			WHERE id=?`,
			string(b.State), b.Imported, b.Duplicates, b.Quarantined, b.Errored, b.Bytes,
			nullInt64(b.FinishedAt), b.ID)
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// DisplayPathExistsFold reports whether any cataloged file's display path equals
// displayPath case-insensitively. The importer uses it so a file that would land
// at a path differing only by case from an existing one (which coexist on a
// case-sensitive Linux filesystem but collide on Windows/macOS) is quarantined,
// keeping the managed tree portable. NOCASE folds ASCII, covering typical
// filenames; it scans the file table, which is fine for an import-sized batch.
func (s *Store) DisplayPathExistsFold(ctx context.Context, displayPath string) (bool, error) {
	var exists int
	err := s.read.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM file WHERE display_path = ? COLLATE NOCASE)", displayPath).Scan(&exists)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.DisplayPathExistsFold", err)
	}
	return exists == 1, nil
}

// ImportBatches lists import batches, newest first (limit 0 = all).
func (s *Store) ImportBatches(ctx context.Context, limit int) ([]*model.ImportBatch, error) {
	const op = "store.ImportBatches"
	q := `SELECT id, pid, source, COALESCE(library_id,0), state,
		imported, duplicates, quarantined, errored, bytes, started_at, COALESCE(finished_at,0)
		FROM import_batch ORDER BY started_at DESC`
	var args []any
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.ImportBatch
	for rows.Next() {
		var b model.ImportBatch
		var pid, state string
		if err := rows.Scan(&b.ID, &pid, &b.Source, &b.LibraryID, &state,
			&b.Imported, &b.Duplicates, &b.Quarantined, &b.Errored, &b.Bytes, &b.StartedAt, &b.FinishedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		b.PID = model.PID(pid)
		b.State = model.ImportBatchState(state)
		out = append(out, &b)
	}
	return out, rows.Err()
}
