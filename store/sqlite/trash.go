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
	id        int64
	libraryID sql.NullInt64
	path      []byte
	display   string
	size      int64
	// itemPID is the item the file backed, for reporting and for the delta a later
	// purge emits. It is the primary item when the file has a primary edge, and any
	// linked item otherwise: a multi-file book's non-first parts are 'part' edges, so
	// requiring 'primary' left every trashed book part with a blank item.
	itemPID model.PID
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
			string(tpid), d.libraryID, string(d.itemPID), d.path, d.display,
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

	// Every item linked to this file, and the one the journal records, captured
	// before the cascade removes the edges. The primary edge is preferred, and any
	// linked item stands in when there is none: a book part is a 'part' edge, so
	// insisting on 'primary' recorded a blank item for it. A part file has exactly
	// one linked item; a single-file rip backing several virtual tracks has one per
	// track, each of them primary, so that case already picks arbitrarily.
	itemIDs, err := itemIDsForFile(ctx, tx, d.id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT pi.pid FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		 WHERE itf.file_id = ? AND itf.role = 'primary'
		 ORDER BY itf.item_id LIMIT 1`, d.id).Scan(&d.itemPID); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if d.itemPID == "" && len(itemIDs) > 0 {
		if err := tx.QueryRowContext(ctx,
			"SELECT pid FROM playable_item WHERE id = ?", itemIDs[0]).Scan(&d.itemPID); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
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
		// A book's denormalized total is derived from the parts it currently has, so it
		// is refreshed whether any survived or not. An archived book has no parts and
		// its total is 0, the same shedding the entity rollups above do: left stale it
		// keeps feeding the item view's duration, the duration_ms filter, and its
		// series' running time with time the book no longer has.
		if err := refreshBookDuration(ctx, tx, iid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if has {
			// A surviving multi-file book that lost a part must promote a primary (or
			// it reads back headless) and emit an item update because its part
			// count/duration/chapters changed, so a change_log consumer must refresh it
			// (symmetric with the attach side). Its rollups were already recomputed above.
			if err := ensurePrimary(ctx, tx, iid); err != nil {
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

// itemIDsForFile returns the distinct item ids linked to a file by any role, in id
// order so a caller that picks one of them picks deterministically.
func itemIDsForFile(ctx context.Context, tx *sql.Tx, fileID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? ORDER BY item_id", fileID)
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
// whether already-restored rows are returned; trashedBefore, when nonzero, keeps
// only rows trashed strictly before that unix-ns instant (the age filter behind
// EmptyTrash's OlderThan window); limit 0 = no cap. These filters scan without an
// index on purpose: the journal is small and shrinks on purge. A lookup by item
// pid is keyed rather than scanned; see ActiveTrashForItems.
func (s *Store) TrashEntries(ctx context.Context, includeRestored bool, trashedBefore int64, limit int) ([]model.TrashEntry, error) {
	const op = "store.TrashEntries"
	q := "SELECT " + trashCols + " FROM trash WHERE 1=1"
	var args []any
	if !includeRestored {
		q += " AND restored_at IS NULL"
	}
	if trashedBefore != 0 {
		q += " AND trashed_at < ?"
		args = append(args, trashedBefore)
	}
	q += " ORDER BY trashed_at DESC"
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

// ActiveTrashForItems returns the restorable trash entries for each of the given
// items, keyed by item pid, each item's entries newest first: the batched,
// item-keyed twin of ActiveTrashByPID, served by the trash_item index.
//
// Items with nothing restorable and unknown pids are absent from the map rather
// than mapped to an empty slice, so a present key is itself the answer and an item
// purged between a delta page and this lookup is not an error. Entries rather than
// a bool because a partly-trashed book needs each trash pid to restore, and Size is
// the byte count the tombstone decision is about.
func (s *Store) ActiveTrashForItems(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.TrashEntry, error) {
	const op = "store.ActiveTrashForItems"
	if len(itemPIDs) == 0 {
		return nil, nil
	}
	// chunkSlice splits the input as given, so without the dedup a repeated pid lands
	// in two chunks and its entries are appended twice.
	unique := uniquePIDs(itemPIDs)
	out := make(map[model.PID][]model.TrashEntry)
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		args := make([]any, 0, len(chunk))
		for _, pid := range chunk {
			// trash.item_pid is NOT NULL DEFAULT '', so an empty input would match
			// every edge-less journal row.
			if pid == "" {
				continue
			}
			args = append(args, string(pid))
		}
		if len(args) == 0 {
			return nil
		}
		rows, err := s.read.QueryContext(ctx,
			"SELECT "+trashCols+" FROM trash WHERE restored_at IS NULL AND item_pid IN "+
				placeholders(len(args))+" ORDER BY item_pid, trashed_at DESC", args...)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanTrashEntry(rows)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			out[e.ItemPID] = append(out[e.ItemPID], *e)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
// removed from disk by an empty-trash pass) and, in the same transaction, appends an
// item update for the item the row recorded. A missing row is a silent no-op.
//
// The delta carries no payload and no playable_item column changes with it: a
// change_log row is an invalidation signal, so a tailer re-reads the item and finds
// no restorable trash entry behind it. That is the whole point here. The usual case
// is an already-archived item whose state does not move, and without the delta a
// client holding a download of it would never learn those bytes became
// unrecoverable. A multi-file book that is still present after one part was trashed
// and purged earns one too, because that part's bytes are gone as well.
//
// A row with no item (an edge-less file, or one written before the journal recorded
// book parts correctly) emits nothing, and neither does one whose item has since
// been tombstoned, such as by a virtual-track re-carve.
func (s *Store) DeleteTrashRow(ctx context.Context, trashPID model.PID) error {
	const op = "store.DeleteTrashRow"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var itemPID model.PID
		err := tx.QueryRowContext(ctx,
			"SELECT item_pid FROM trash WHERE pid = ?", string(trashPID)).Scan(&itemPID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE pid = ?", string(trashPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if itemPID == "" {
			return nil
		}
		var exists int
		switch err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM playable_item WHERE pid = ?", string(itemPID)).Scan(&exists); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, appendChange(ctx, tx, "item", itemPID, model.OpUpdate))
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
