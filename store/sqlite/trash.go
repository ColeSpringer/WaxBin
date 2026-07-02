package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// detachedFile carries the columns the trash journal needs about a file being
// removed from the catalog.
type detachedFile struct {
	id          int64
	libraryID   sql.NullInt64
	path        []byte
	display     string
	size        int64
	primaryItem model.PID
}

// TrashFile drops a file's catalog row (after the file has been moved into the
// trash on disk) and records the undo journal row, all in one transaction. The
// logical item is preserved, becoming archived when it loses its last file.
// Returns the new trash entry's pid.
func (s *Store) TrashFile(ctx context.Context, in model.TrashFileInput) (model.PID, error) {
	const op = "store.TrashFile"
	tpid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		d, err := detachFileTx(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO trash
			(pid, library_id, item_pid, orig_path, orig_display, trash_path, trash_display, reason, size, trashed_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			string(tpid), d.libraryID, string(d.primaryItem), d.path, d.display,
			in.TrashPath, in.TrashDisplay, reasonOr(in.Reason), d.size, nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return tpid, nil
}

// DetachFile drops a file's catalog row without an undo journal (pruning or
// explicit permanent deletion; the caller removes the file from disk). The
// logical item is preserved and archived if it loses its last file.
func (s *Store) DetachFile(ctx context.Context, filePID model.PID) error {
	const op = "store.DetachFile"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := detachFileTx(ctx, tx, filePID, op)
		return err
	})
}

// detachFileTx deletes a file row (cascading its item_file edges and analysis
// rows), archives every item left with no files, and writes the change_log rows.
// It returns the detached file's metadata for the trash journal.
func detachFileTx(ctx context.Context, tx *sql.Tx, filePID model.PID, op string) (*detachedFile, error) {
	var d detachedFile
	err := tx.QueryRowContext(ctx,
		"SELECT id, library_id, path, display_path, size FROM file WHERE pid = ?", string(filePID)).
		Scan(&d.id, &d.libraryID, &d.path, &d.display, &d.size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(filePID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// The primary item (for reporting) and every item linked to this file, captured
	// before the cascade removes the edges.
	if err := tx.QueryRowContext(ctx,
		`SELECT pi.pid FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		 WHERE itf.file_id = ? AND itf.role = 'primary' LIMIT 1`, d.id).Scan(&d.primaryItem); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	itemIDs, err := itemIDsForFile(ctx, tx, d.id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// The rollups' total_duration_ms sums from the file row, so dropping the file
	// must refresh the touched entities or `db verify` would see drift. The track
	// rows survive (the item is archived, not deleted), so the recompute keeps the
	// track counts and only sheds the now-absent duration.
	affected := newAffectedRollups()
	for _, iid := range itemIDs {
		if err := affected.collect(ctx, tx, iid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM file WHERE id = ?", d.id); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	now := nowNS()
	if !affected.empty() {
		if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	for _, iid := range itemIDs {
		has, err := itemHasAnyFile(ctx, tx, iid)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if has {
			// A surviving multi-file book that lost a part must promote a primary (or
			// it reads back headless), refresh its denormalized total duration, and
			// emit an item update because its part count/duration/chapters changed, so a
			// change_log consumer must refresh it (symmetric with the attach side). Its
			// rollups were already recomputed above.
			if err := ensurePrimary(ctx, tx, iid); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := refreshBookDuration(ctx, tx, iid); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			var pid model.PID
			if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id=?", iid).Scan(&pid); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", pid, model.OpUpdate); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			continue
		}
		var pid model.PID
		if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id=?", iid).Scan(&pid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET state=?, updated_at=? WHERE id=?",
			string(model.StateArchived), now, iid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "item", pid, model.OpUpdate); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if err := appendChange(ctx, tx, "file", filePID, model.OpDelete); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return &d, nil
}

// itemIDsForFile returns the distinct item ids linked to a file by any role.
func itemIDsForFile(ctx context.Context, tx *sql.Tx, fileID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT item_id FROM item_file WHERE file_id = ?", fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const trashCols = `pid, item_pid, orig_path, orig_display, trash_path, trash_display,
	reason, size, trashed_at, COALESCE(restored_at, 0)`

// TrashEntries lists trash journal rows, newest first. includeRestored controls
// whether already-restored rows are returned (limit 0 = no cap).
func (s *Store) TrashEntries(ctx context.Context, includeRestored bool, limit int) ([]model.TrashEntry, error) {
	const op = "store.TrashEntries"
	q := "SELECT " + trashCols + " FROM trash"
	if !includeRestored {
		q += " WHERE restored_at IS NULL"
	}
	q += " ORDER BY trashed_at DESC"
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
	var out []model.TrashEntry
	for rows.Next() {
		e, err := scanTrashEntry(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ActiveTrashByPID returns an un-restored trash entry, or CodeNotFound.
func (s *Store) ActiveTrashByPID(ctx context.Context, trashPID model.PID) (*model.TrashEntry, error) {
	const op = "store.ActiveTrashByPID"
	row := s.read.QueryRowContext(ctx,
		"SELECT "+trashCols+" FROM trash WHERE pid = ? AND restored_at IS NULL", string(trashPID))
	e, err := scanTrashEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no active trash entry: "+string(trashPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return e, nil
}

// MarkTrashRestored marks an entry restored. It is a no-op (CodeNotFound) if the
// entry is missing or already restored, so a double restore is rejected.
func (s *Store) MarkTrashRestored(ctx context.Context, trashPID model.PID) error {
	const op = "store.MarkTrashRestored"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			"UPDATE trash SET restored_at=? WHERE pid=? AND restored_at IS NULL", nowNS(), string(trashPID))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no active trash entry: "+string(trashPID))
		}
		return nil
	})
}

// DeleteTrashRow removes a trash journal row (after its file has been permanently
// removed from disk by an empty-trash pass).
func (s *Store) DeleteTrashRow(ctx context.Context, trashPID model.PID) error {
	const op = "store.DeleteTrashRow"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE pid = ?", string(trashPID))
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

func scanTrashEntry(sc rowScanner) (*model.TrashEntry, error) {
	var e model.TrashEntry
	var pid, itemPID string
	if err := sc.Scan(&pid, &itemPID, &e.OrigPath, &e.OrigDisplay, &e.TrashPath, &e.TrashDisplay,
		&e.Reason, &e.Size, &e.TrashedAt, &e.RestoredAt); err != nil {
		return nil, err
	}
	e.PID = model.PID(pid)
	e.ItemPID = model.PID(itemPID)
	return &e, nil
}

func reasonOr(reason string) string {
	if reason == "" {
		return "user"
	}
	return reason
}
