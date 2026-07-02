package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// LoadScopedFileIndex bulk-loads the present audio files under a library scope into
// path->ScopedFile, so the scanner can fast-path an unchanged file (size+mtime
// match) in memory and reconcile a vanished one at end-of-walk without a per-file
// SELECT. scopePrefix is a raw path prefix (typically the walk root plus a
// separator); nil/empty spans the whole library. Each entry carries the item the
// file backs (for sidecar-only updates) and its known sidecar observations (for the
// stat-gated sidecar re-parse).
func (s *Store) LoadScopedFileIndex(ctx context.Context, libraryID int64, scopePrefix []byte) (map[string]model.ScopedFile, error) {
	const op = "store.LoadScopedFileIndex"
	lo, hi := scopePrefix, prefixUpperBound(scopePrefix)

	// Files (with the item each backs). A file normally has exactly one item_file
	// edge; the LEFT JOIN keeps a rare edge-less file in the index so reconciliation
	// still sees it. A file whose item is missing/archived is EXCLUDED so it is not
	// fast-pathed: a restored file with the same size+mtime must go through the full
	// path to flip its item back to present (a fast-path skip would leave it missing).
	// Such a file, if still gone, simply is not re-reconciled (it is already missing).
	fq := `SELECT f.id, f.pid, f.path, f.size, f.mtime_ns, COALESCE(pi.pid,'')
		FROM file f
		LEFT JOIN item_file itf ON itf.file_id = f.id
		LEFT JOIN playable_item pi ON pi.id = itf.item_id
		WHERE f.library_id = ? AND f.kind = ? AND (pi.state IS NULL OR pi.state = 'present')`
	args := []any{libraryID, string(model.FileAudio)}
	if len(lo) > 0 {
		fq += " AND f.path >= ?"
		args = append(args, lo)
		if hi != nil {
			fq += " AND f.path < ?"
			args = append(args, hi)
		}
	}

	rows, err := s.read.QueryContext(ctx, fq, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	byID := make(map[int64]*model.ScopedFile)
	pathByID := make(map[int64]string)
	for rows.Next() {
		var id int64
		var fpid, ipid string
		var path []byte
		var size, mtime int64
		if err := rows.Scan(&id, &fpid, &path, &size, &mtime, &ipid); err != nil {
			rows.Close()
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		byID[id] = &model.ScopedFile{FilePID: model.PID(fpid), ItemPID: model.PID(ipid), Size: size, MTimeNS: mtime}
		pathByID[id] = string(path)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	rows.Close()

	// Sidecar observations for the same scope, attached to their file entries. Build
	// this query's args independently of the files query so the two cannot drift if
	// either query's filters change later.
	aq := `SELECT fa.file_id, fa.kind, fa.path, fa.size, fa.mtime_ns, fa.hash, fa.missing
		FROM file_aux_state fa JOIN file f ON f.id = fa.file_id
		WHERE f.library_id = ? AND f.kind = ?`
	aargs := []any{libraryID, string(model.FileAudio)}
	if len(lo) > 0 {
		aq += " AND f.path >= ?"
		aargs = append(aargs, lo)
		if hi != nil {
			aq += " AND f.path < ?"
			aargs = append(aargs, hi)
		}
	}
	arows, err := s.read.QueryContext(ctx, aq, aargs...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	for arows.Next() {
		var fid int64
		var o model.AuxObservation
		var missing int
		if err := arows.Scan(&fid, &o.Kind, &o.Path, &o.Size, &o.MTimeNS, &o.Hash, &missing); err != nil {
			arows.Close()
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		o.Missing = missing != 0
		if e, ok := byID[fid]; ok {
			e.Aux = append(e.Aux, o)
		}
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	arows.Close()

	out := make(map[string]model.ScopedFile, len(byID))
	for id, e := range byID {
		out[pathByID[id]] = *e
	}
	return out, nil
}

// prefixUpperBound returns the smallest byte string strictly greater than every
// string beginning with prefix, so a path-prefix scope can be expressed as the
// half-open range [prefix, upper) against the indexed path column. It returns nil
// when prefix is all 0xFF (no finite upper bound), leaving the range open-ended.
func prefixUpperBound(prefix []byte) []byte {
	hi := append([]byte(nil), prefix...)
	for i := len(hi) - 1; i >= 0; i-- {
		if hi[i] != 0xFF {
			hi[i]++
			return hi[:i+1]
		}
	}
	return nil
}

// MarkFilesMissing marks the items backing the given files as missing, but only when
// EVERY file of an item is in the set, so a multi-file book that lost a single part
// stays present (and its still-present parts, unvisited by the fast-path, would never
// flip it back). Rows (file, item_file edges, entities) are preserved, so a later
// rescan that re-walks the file restores its item to present. Returns the number of
// items newly marked missing; an already-missing item emits no delta.
func (s *Store) MarkFilesMissing(ctx context.Context, filePIDs []model.PID) (int, error) {
	const op = "store.MarkFilesMissing"
	if len(filePIDs) == 0 {
		return 0, nil
	}
	var marked int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		missing, err := fileIDSet(ctx, tx, filePIDs)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		items, err := itemsBackingFiles(ctx, tx, missing)
		if err != nil {
			return err
		}
		now := nowNS()
		for _, itemID := range items {
			all, err := allFilesInSet(ctx, tx, itemID, missing)
			if err != nil {
				return err
			}
			if !all {
				continue // still has a present file; do not mark missing
			}
			var pid string
			var state string
			if err := tx.QueryRowContext(ctx,
				"SELECT pid, state FROM playable_item WHERE id = ?", itemID).Scan(&pid, &state); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if state == string(model.StateMissing) {
				continue // idempotent: no delta for an already-missing item
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE playable_item SET state = ?, updated_at = ? WHERE id = ?",
				string(model.StateMissing), now, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", model.PID(pid), model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			marked++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return marked, nil
}

// fileIDSet resolves file PIDs to a set of internal file ids, chunking the lookup so
// a large deletion does not overflow the SQLite parameter limit.
func fileIDSet(ctx context.Context, tx *sql.Tx, pids []model.PID) (map[int64]bool, error) {
	set := make(map[int64]bool, len(pids))
	err := chunkSlice(pids, idBatchSize, func(batch []model.PID) error {
		args := make([]any, len(batch))
		for j, p := range batch {
			args[j] = string(p)
		}
		return scanIDsInto(ctx, tx, set,
			"SELECT id FROM file WHERE pid IN "+placeholders(len(batch)), args)
	})
	return set, err
}

// itemsBackingFiles returns the distinct items that back any file in the set.
func itemsBackingFiles(ctx context.Context, tx *sql.Tx, fileIDs map[int64]bool) ([]int64, error) {
	seen := make(map[int64]bool)
	err := chunkSlice(ids(fileIDs), idBatchSize, func(batch []int64) error {
		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		return scanIDsInto(ctx, tx, seen,
			"SELECT DISTINCT item_id FROM item_file WHERE file_id IN "+placeholders(len(batch)), args)
	})
	if err != nil {
		return nil, err
	}
	return ids(seen), nil
}

// scanIDsInto runs an id-selecting query and adds each result id to set.
func scanIDsInto(ctx context.Context, tx *sql.Tx, set map[int64]bool, query string, args []any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", err)
		}
		set[id] = true
	}
	return waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", rows.Err())
}

// allFilesInSet reports whether every file backing itemID is in the missing set.
func allFilesInSet(ctx context.Context, tx *sql.Tx, itemID int64, missing map[int64]bool) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT file_id FROM item_file WHERE item_id = ?", itemID)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", err)
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		var fid int64
		if err := rows.Scan(&fid); err != nil {
			return false, waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", err)
		}
		any = true
		if !missing[fid] {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.MarkFilesMissing", err)
	}
	return any, nil
}

// UpdateFileStateIfUnchanged updates a file's size/mtime/content_hash only when its
// stored size and mtime still match the caller's expected values (optimistic
// concurrency). An on-disk tag write (organize/replaygain/PID-stamp) computes a new
// hash/size/mtime outside any transaction, then calls this to record the result: a
// match means the writer's read is still current and the row is updated (so the next
// scan's stat matches and the fast-path skips re-hashing WaxBin's own write); a
// mismatch means a concurrent scan/move already touched the file, so the update is
// skipped and left for the next scan to reconcile. essence_hash is left untouched: a
// tag edit does not alter audio essence, so item identity is preserved.
func (s *Store) UpdateFileStateIfUnchanged(ctx context.Context, in model.FileStateUpdate) (bool, error) {
	const op = "store.UpdateFileStateIfUnchanged"
	var updated bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE file SET size = ?, mtime_ns = ?, content_hash = ?, last_seen = ?
			 WHERE pid = ? AND size = ? AND mtime_ns = ?`,
			in.NewSize, in.NewMTimeNS, in.NewContentHash, nowNS(),
			string(in.FilePID), in.ExpectedSize, in.ExpectedMTimeNS)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n > 0 {
			updated = true
			return appendChange(ctx, tx, "file", in.FilePID, model.OpUpdate)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}
