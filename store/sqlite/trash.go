package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	essence   sql.NullString
	// itemPID is the item the file backed, for reporting and for the delta a later
	// purge emits. It is the primary item when the file has a primary edge, and any
	// linked item otherwise: a multi-file book's non-first parts are 'part' edges, so
	// requiring 'primary' left every trashed book part with a blank item. itemKind is
	// that item's kind, which the pid may not keep (a book carved into a rip hands its
	// pid to a track), so a file put back stands only for an item of that kind.
	itemPID  model.PID
	itemKind string
	// items is every item the file backed (each track of a rip), for the journal's
	// trash_item rows.
	items []int64
	// promoted lists the alternates that took the file's place.
	promoted []model.PromotedFile
}

// PlanTrash records a trash move before the file moves: everything the journal row will
// hold, read from the file's row as it stands, under the pid the row will take. An open
// after a crash between the move and TrashFile settles the record (recoverTrash). The
// record covers the file alone: its sidecars and its folder's companions go by the run's
// own reckoning, so a recovered move leaves them where the crash did. It is as durable as
// any write here: the WAL at synchronous NORMAL keeps it through a process crash, while a
// power cut can lose the last writes, this one among them.
func (s *Store) PlanTrash(ctx context.Context, in model.TrashPending) error {
	const op = "store.PlanTrash"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(in.FilePID)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(in.FilePID))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		itemPID, itemKind, err := journalItemTx(ctx, tx, id)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO trash_pending
			(pid, file_id, item_pid, item_kind, library_id, orig_path, orig_display, essence_hash, size,
			 trash_path, trash_display, reason, job_pid, created_at)
			SELECT ?, id, ?, ?, library_id, path, display_path, COALESCE(essence_hash, ''), size, ?, ?, ?, NULLIF(?, ''), ?
			FROM file WHERE id = ?`,
			string(in.PID), string(itemPID), itemKind, in.TrashPath, in.TrashDisplay, reasonOr(in.Reason),
			string(in.JobPID), nowNS(), id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO trash_pending_item(pending_pid, item_pid)
				SELECT DISTINCT ?, pi.pid FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id WHERE itf.file_id = ?`,
				string(in.PID), id)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// AbortTrash drops the record of a trash move that did not happen. A missing record is
// no error.
func (s *Store) AbortTrash(ctx context.Context, pid model.PID) error {
	const op = "store.AbortTrash"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM trash_pending WHERE pid = ?", string(pid))
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// TrashFile drops a file's catalog row (after the file has been moved into the
// trash on disk) and records the undo journal row, all in one transaction, which also
// drops the move's pending record. The logical item is preserved: an alternate takes the
// file's place when it has one (see detachFileTx), and it is archived as trash when it
// loses its last file.
func (s *Store) TrashFile(ctx context.Context, in model.TrashFileInput) (*model.DetachResult, error) {
	const op = "store.TrashFile"
	res := &model.DetachResult{TrashPID: in.PID}
	if res.TrashPID == "" {
		res.TrashPID = model.NewPID()
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		d, err := detachFileTx(ctx, tx, in.FilePID, model.ArchiveTrash, op)
		if err != nil {
			return err
		}
		res.Promoted = d.promoted
		r, err := tx.ExecContext(ctx, `INSERT INTO trash
			(pid, library_id, item_pid, item_kind, orig_path, orig_display, trash_path, trash_display, essence_hash, reason, size, trashed_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(res.TrashPID), d.libraryID, string(d.itemPID), d.itemKind, d.path, d.display,
			in.TrashPath, in.TrashDisplay, d.essence, reasonOr(in.Reason), d.size, nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		trashID, err := r.LastInsertId()
		if err == nil {
			err = recordTrashItemsTx(ctx, tx, trashID, d.items)
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM trash_pending WHERE pid = ?", string(res.TrashPID))
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DetachFile drops a file's catalog row without an undo journal, for a delete in a mode
// that bypasses the trash (prune or permanent; the caller removes the file from disk,
// and any other mode is refused). The logical item is preserved like TrashFile's,
// archived as the mode.
func (s *Store) DetachFile(ctx context.Context, filePID model.PID, mode model.DeleteMode) (*model.DetachResult, error) {
	const op = "store.DetachFile"
	if !mode.BypassesTrash() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("a detach is for a delete that bypasses the trash, not %q", mode))
	}
	res := &model.DetachResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		d, err := detachFileTx(ctx, tx, filePID, mode.ArchiveReason(), op)
		if err != nil {
			return err
		}
		res.Promoted = d.promoted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// detachFileTx deletes a file row (cascading its item_file edges and analysis rows),
// promotes an alternate into the place of a primary or part edge the file held
// (promoteLostTx, then ensurePrimary), archives every item left with no files as
// reason, and writes the change_log rows. It returns the detached file's metadata for
// the trash journal, and the promoted files for the caller to re-read.
func detachFileTx(ctx context.Context, tx *sql.Tx, filePID model.PID, reason model.ArchiveReason, op string) (*detachedFile, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(filePID)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(filePID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	b := newDetachBatch(reason)
	d, err := b.detachTx(ctx, tx, id)
	if err == nil {
		err = b.settleTx(ctx, tx)
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	d.promoted = b.promoted
	return d, nil
}

// detachBatch detaches a run of files in one transaction, the way detachFileTx detaches
// one, and settles each item they touched and each rollup once for the run. An item left
// with no file is archived as reason.
type detachBatch struct {
	reason   model.ArchiveReason
	affected *affectedRollups
	items    []int64 // the items the files backed, in the order first met
	touched  map[int64]bool
	files    []model.PID
	promoted []model.PromotedFile
	archived int
}

func newDetachBatch(reason model.ArchiveReason) *detachBatch {
	return &detachBatch{reason: reason, affected: newAffectedRollups(), touched: map[int64]bool{}}
}

// detachTx deletes one file row and gives each item that keeps files an alternate in the
// place the file held, which has to happen before the next file goes, since it reads the
// edge the file left.
func (b *detachBatch) detachTx(ctx context.Context, tx *sql.Tx, fileID int64) (*detachedFile, error) {
	d := detachedFile{id: fileID}
	var pid model.PID
	if err := tx.QueryRowContext(ctx,
		"SELECT pid, library_id, path, display_path, size, essence_hash FROM file WHERE id = ?", fileID).
		Scan(&pid, &d.libraryID, &d.path, &d.display, &d.size, &d.essence); err != nil {
		return nil, err
	}

	// Every item linked to this file, and the one the journal records, captured
	// before the cascade removes the edges. The primary edge is preferred, and any
	// linked item stands in when there is none: a book part is a 'part' edge, so
	// insisting on 'primary' recorded a blank item for it. A part file has exactly
	// one linked item; a single-file rip backing several virtual tracks has one per
	// track, each of them primary, so that case already picks arbitrarily.
	itemIDs, err := itemIDsForFile(ctx, tx, d.id)
	if err != nil {
		return nil, err
	}
	d.items = itemIDs
	lost, books, err := itemLostEdgesTx(ctx, tx, d.id, d.essence.String)
	if err != nil {
		return nil, err
	}
	if d.itemPID, d.itemKind, err = journalItemTx(ctx, tx, d.id); err != nil {
		return nil, err
	}

	// The rollups sum durations from the file row and count live members, so dropping
	// the file must refresh the touched entities or `db verify` would see drift. The
	// track rows survive for a restore; an item left with no file is archived by
	// settleTx before the recompute, which then sheds it from the counts as well.
	for _, iid := range itemIDs {
		if b.touched[iid] {
			continue
		}
		b.touched[iid] = true
		b.items = append(b.items, iid)
		if err := b.affected.collect(ctx, tx, iid); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM file WHERE id = ?", d.id); err != nil {
		return nil, err
	}
	for _, iid := range itemIDs {
		has, err := itemHasAnyFile(ctx, tx, iid)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		p, err := promoteLostTx(ctx, tx, iid, books[iid], lost[iid])
		if err != nil {
			return nil, err
		}
		if p != nil {
			b.promoted = append(b.promoted, *p)
		}
	}
	b.files = append(b.files, pid)
	return &d, nil
}

// settleTx finishes each item the run touched: one that keeps a file keeps a primary
// (ensurePrimary), is marked missing when no file of it is left on disk, and emits an
// item update because its files, duration or chapters changed (symmetric with the attach
// side); one left with none is archived. Then the touched rollups are recomputed, after
// the promotions, which change which file an item's duration reads, and each file's
// delete is logged.
func (b *detachBatch) settleTx(ctx context.Context, tx *sql.Tx) error {
	now := nowNS()
	for _, iid := range b.items {
		has, err := itemHasAnyFile(ctx, tx, iid)
		if err != nil {
			return err
		}
		marked := false
		if has {
			p, err := ensurePrimary(ctx, tx, iid)
			if err != nil {
				return err
			}
			if p != nil {
				b.promoted = append(b.promoted, *p)
			}
			if marked, err = markUnreachableMissingTx(ctx, tx, iid, nil); err != nil {
				return err
			}
		}
		// A book's denormalized total is derived from the parts it currently has, so it
		// is refreshed whether any survived or not. An archived book has no parts and
		// its total is 0, the same shedding the entity rollups do: left stale it keeps
		// feeding the item view's duration, the duration_ms filter, and its series'
		// running time with time the book no longer has.
		if err := refreshBookDuration(ctx, tx, iid); err != nil {
			return err
		}
		if has {
			if !marked {
				if err := appendItemUpdateTx(ctx, tx, iid); err != nil {
					return err
				}
			}
			continue
		}
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM playable_item WHERE id=?", iid).Scan(&state); err != nil {
			return err
		}
		if state != string(model.StateArchived) {
			b.archived++
		}
		if _, err := tx.ExecContext(ctx, "UPDATE playable_item SET state=?, archive_reason=?, updated_at=? WHERE id=?",
			string(model.StateArchived), string(b.reason), now, iid); err != nil {
			return err
		}
		if err := appendItemUpdateTx(ctx, tx, iid); err != nil {
			return err
		}
	}
	if !b.affected.empty() {
		if err := maintainRollupsTx(ctx, tx, b.affected, now); err != nil {
			return err
		}
	}
	for _, pid := range b.files {
		if err := appendChange(ctx, tx, "file", pid, model.OpDelete); err != nil {
			return err
		}
	}
	return nil
}

// journalItemTx returns the item a trash journal row records for a file: the item of its
// primary edge, else any item it backs (a book's later parts are part edges), the lowest
// id first; none for a file that backs no item.
func journalItemTx(ctx context.Context, tx *sql.Tx, fileID int64) (model.PID, string, error) {
	var pid model.PID
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT pi.pid, pi.kind FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		WHERE itf.file_id = ? ORDER BY itf.role = 'primary' DESC, itf.item_id LIMIT 1`, fileID).Scan(&pid, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return pid, kind, err
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

const trashCols = `trash.pid, trash.item_pid, trash.orig_path, trash.orig_display, trash.trash_path,
	trash.trash_display, trash.reason, trash.size, trash.trashed_at, COALESCE(trash.restored_at, 0),
	COALESCE((SELECT l.pid FROM library l WHERE l.id = trash.library_id), '')`

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
	rows, err := s.rdb().QueryContext(ctx, q, args...)
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
	row := s.rdb().QueryRowContext(ctx,
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
// item-keyed twin of ActiveTrashByPID, served by the trash_item table. An entry is
// listed under every item its file backed (each track of a rip), its ItemPID naming
// the one the journal reports.
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
			args = append(args, string(pid))
		}
		rows, err := s.rdb().QueryContext(ctx,
			"SELECT "+trashCols+", ti.item_pid FROM trash_item ti JOIN trash ON trash.id = ti.trash_id"+
				" WHERE trash.restored_at IS NULL AND ti.item_pid IN "+placeholders(len(args))+
				" ORDER BY ti.item_pid, trash.trashed_at DESC", args...)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var item model.PID
			e, err := scanTrashEntry(rows, &item)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			out[item] = append(out[item], *e)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkTrashRestored marks an entry restored. It is a no-op (CodeNotFound) if the
// entry is missing or already restored, so a double restore is rejected. Each item the
// entry backed that is still archived after the restore (its rescan skipped the file, or
// read it as another item) has one entry fewer to bring it back, reading purge once none
// is left, and earns an item update, as DeleteTrashRow's items do.
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
		return waxerr.Wrap(waxerr.CodeIO, op,
			logTrashItemsTx(ctx, tx, "t.pid = ? AND pi.state = ?", string(trashPID), string(model.StateArchived)))
	})
}

// DeleteTrashRow removes a trash journal row (after its file has been permanently
// removed from disk by an empty-trash pass) and, in the same transaction, appends an
// item update for each item the row's file backed (every track of a rip). A missing row
// is a silent no-op.
//
// An item the trash archived reads purge once no restorable entry is left for it, and the
// delta announces that. An item whose view does not change earns the delta all the same:
// a book still present after one part was trashed and purged, or an archived one with
// another part still restorable, has lost that part's bytes, and a client holding a
// download of them learns it only from the delta.
//
// A row whose file backed no item emits nothing, and neither does an item since
// tombstoned, such as by a virtual-track re-carve.
func (s *Store) DeleteTrashRow(ctx context.Context, trashPID model.PID) error {
	const op = "store.DeleteTrashRow"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if err := logTrashItemsTx(ctx, tx, "t.pid = ?", string(trashPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE pid = ?", string(trashPID))
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// recordTrashItemsTx writes a journal row's trash_item rows: the items its file backed.
func recordTrashItemsTx(ctx context.Context, tx *sql.Tx, trashID int64, items []int64) error {
	for _, id := range items {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trash_item(trash_id, item_pid)
			SELECT ?, pid FROM playable_item WHERE id = ?`, trashID, id); err != nil {
			return err
		}
	}
	return nil
}

// logTrashItemsTx logs an item update for each item behind the trash rows cond selects (t
// the trash row, pi the item), since what can restore them changed. A caller deleting
// those rows logs first: their trash_item rows go with them.
func logTrashItemsTx(ctx context.Context, tx *sql.Tx, cond string, args ...any) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO change_log(ts, entity_type, entity_pid, op)
		SELECT ?, 'item', pi.pid, 'update' FROM trash t JOIN trash_item ti ON ti.trash_id = t.id
		JOIN playable_item pi ON pi.pid = ti.item_pid WHERE `+cond+` GROUP BY pi.id ORDER BY pi.id`,
		append([]any{nowNS()}, args...)...)
	return err
}

// scanTrashEntry reads a row of trashCols, then into extra any columns the query added.
func scanTrashEntry(sc rowScanner, extra ...any) (*model.TrashEntry, error) {
	var e model.TrashEntry
	var pid, itemPID, libPID string
	if err := sc.Scan(append([]any{&pid, &itemPID, &e.OrigPath, &e.OrigDisplay, &e.TrashPath, &e.TrashDisplay,
		&e.Reason, &e.Size, &e.TrashedAt, &e.RestoredAt, &libPID}, extra...)...); err != nil {
		return nil, err
	}
	e.PID = model.PID(pid)
	e.ItemPID = model.PID(itemPID)
	e.LibraryPID = model.PID(libPID)
	return &e, nil
}

func reasonOr(reason string) string {
	if reason == "" {
		return "user"
	}
	return reason
}
