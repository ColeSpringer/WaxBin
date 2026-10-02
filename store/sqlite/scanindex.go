package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// LoadScopedFileIndex bulk-loads the present audio files under a library scope into
// path->ScopedFile, so the scanner can fast-path an unchanged file (size+mtime
// match) in memory and reconcile a vanished one at end-of-walk without a per-file
// SELECT. scopePrefix is a raw path prefix (typically the walk root plus a
// separator); nil/empty spans the whole library. Each entry carries its known sidecar
// observations, which the fast path stat-compares.
func (s *Store) LoadScopedFileIndex(ctx context.Context, libraryID int64, scopePrefix []byte) (map[string]model.ScopedFile, error) {
	const op = "store.LoadScopedFileIndex"
	lo, hi := scopePrefix, prefixUpperBound(scopePrefix)

	// Files (with the item each backs). A file normally has exactly one item_file
	// edge; the LEFT JOIN keeps a rare edge-less file in the index so reconciliation
	// still sees it. A file whose item is missing/archived is EXCLUDED so it is not
	// fast-pathed: a restored file with the same size+mtime must go through the full
	// path to flip its item back to present (a fast-path skip would leave it missing).
	// Such a file, if still gone, simply is not re-reconciled (it is already missing).
	fq := `SELECT f.id, f.pid, f.path, f.size, f.mtime_ns, COALESCE(pi.pid, ''), COALESCE(pi.kind, ''),
			EXISTS(SELECT 1 FROM field_provenance fp WHERE fp.item_id = pi.id AND fp.field = 'kind' AND fp.locked = 1)
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
	// One row per file. A single-file rip backs N virtual tracks through N item_file
	// edges, so without the GROUP BY that file would return N identical rows. A normal
	// track or book part has one edge, so the grouping is a no-op for them.
	fq += " GROUP BY f.id"

	rows, err := s.read.QueryContext(ctx, fq, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	byID := make(map[int64]*model.ScopedFile)
	pathByID := make(map[int64]string)
	for rows.Next() {
		var id int64
		var fpid, ipid, kind string
		var path []byte
		var size, mtime int64
		var locked bool
		if err := rows.Scan(&id, &fpid, &path, &size, &mtime, &ipid, &kind, &locked); err != nil {
			rows.Close()
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		byID[id] = &model.ScopedFile{FilePID: model.PID(fpid), Size: size, MTimeNS: mtime,
			ItemPID: model.PID(ipid), Kind: model.Kind(kind), KindLocked: locked}
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

// MarkFilesMissing reconciles files gone from disk. An item whose every file is in the
// set is marked missing, so a multi-file book that lost a single part stays present (and
// its still-present parts, unvisited by the fast-path, would never flip it back), and its
// rows are preserved, so a later rescan that re-walks the files restores it to present.
// An item that keeps a file on disk instead settles what it lost (settleMissingTx): an
// alternate takes the place of a gone primary or part, and a gone alternate's edge goes.
// A gone file left with no edge loses its row, its analysis rows going with it. An
// already-missing item emits no delta.
func (s *Store) MarkFilesMissing(ctx context.Context, filePIDs []model.PID) (*model.MissingResult, error) {
	const op = "store.MarkFilesMissing"
	res := &model.MissingResult{}
	if len(filePIDs) == 0 {
		return res, nil
	}
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
		var keeping []int64
		for _, itemID := range items {
			all, err := allFilesInSet(ctx, tx, itemID, missing)
			if err != nil {
				return err
			}
			if !all {
				keeping = append(keeping, itemID)
				continue
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
			res.Marked++
		}
		if err := settleMissingTx(ctx, tx, missing, keeping, res); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// MarkItemFilesMissing settles one item's gone files the way MarkFilesMissing settles an
// item that keeps a file on disk, touching no other item: a file this item shares with
// another (a rip's file backing its siblings) keeps its row while another edge holds it.
// The caller has checked that the item keeps a file on disk.
func (s *Store) MarkItemFilesMissing(ctx context.Context, itemPID model.PID, filePIDs []model.PID) (*model.MissingResult, error) {
	const op = "store.MarkItemFilesMissing"
	res := &model.MissingResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, err := idByPIDTx(ctx, tx, "playable_item", itemPID, op)
		if err != nil {
			return err
		}
		missing, err := fileIDSet(ctx, tx, filePIDs)
		if err != nil {
			return err
		}
		if err := settleMissingTx(ctx, tx, missing, []int64{itemID}, res); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// settleMissingTx settles what items that keep a file outside the missing set lost to it.
// A gone alternate's edge goes. A gone primary or part edge goes once an alternate on
// disk took its place (promoteLostTx); without one it stays, as a book keeps a gone part
// for a rescan to restore, and an item left with no file on disk (its other files under
// an absent root, say) is marked missing. Then every missing file left with no edge is
// dropped; Dropped counts those no promotion replaced, a gone alternate or a row no item
// claimed.
func settleMissingTx(ctx context.Context, tx *sql.Tx, missing map[int64]bool, items []int64, res *model.MissingResult) error {
	affected := newAffectedRollups()
	replaced := map[int64]bool{}
	for _, itemID := range items {
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM playable_item WHERE id = ?", itemID).Scan(&kind); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT itf.file_id, itf.role, itf.position, itf.start_frames, itf.end_frames,
				COALESCE(f.essence_hash, '')
			FROM item_file itf JOIN file f ON f.id = itf.file_id WHERE itf.item_id = ?`, itemID)
		if err != nil {
			return err
		}
		type goneEdge struct {
			fileID int64
			lost   lostEdge
		}
		var gone []goneEdge
		for rows.Next() {
			var g goneEdge
			if err := rows.Scan(&g.fileID, &g.lost.role, &g.lost.position, &g.lost.start, &g.lost.end, &g.lost.essence); err != nil {
				rows.Close()
				return err
			}
			if missing[g.fileID] {
				gone = append(gone, g)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		changed := false
		for _, g := range gone {
			if g.lost.role != alternateRole {
				p, err := promoteLostTx(ctx, tx, itemID, kind == string(model.KindBook), g.lost)
				if err != nil {
					return err
				}
				if p == nil {
					continue
				}
				res.Promoted = append(res.Promoted, *p)
				replaced[g.fileID] = true
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE item_id = ? AND file_id = ? AND role = ?",
				itemID, g.fileID, g.lost.role); err != nil {
				return err
			}
			changed = true
		}
		marked, err := markUnreachableMissingTx(ctx, tx, itemID, missing)
		if err != nil {
			return err
		}
		if marked {
			res.Marked++
		}
		if !changed {
			continue
		}
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return err
		}
		if err := refreshBookDuration(ctx, tx, itemID); err != nil {
			return err
		}
		if !marked {
			if err := appendItemUpdateTx(ctx, tx, itemID); err != nil {
				return err
			}
		}
	}
	for id := range missing {
		var held int
		var pid model.PID
		if err := tx.QueryRowContext(ctx, `SELECT pid, EXISTS(SELECT 1 FROM item_file WHERE file_id = file.id)
			FROM file WHERE id = ?`, id).Scan(&pid, &held); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if held == 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM file WHERE id = ?", id); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, "file", pid, model.OpDelete); err != nil {
			return err
		}
		if !replaced[id] {
			res.Dropped++
		}
	}
	if affected.empty() {
		return nil
	}
	return maintainRollupsTx(ctx, tx, affected, nowNS())
}

// MarkItemMissing marks one item missing by pid, whatever its files say, and is the
// sole authority on the state rule the whole verb obeys. A present item flips and
// emits an item delta; a missing one is a no-op, the same idempotence MarkFilesMissing
// has. An unknown pid is CodeNotFound.
//
// Archived and remote are refused rather than downgraded, which is the non-obvious
// half of the contract. Both already say there are no local bytes, so missing would
// add nothing, and archived carries the fact that the listener deleted the item:
// rewriting it to missing would lose that and put the item back into every listing
// that excludes archived.
//
// File rows and item_file edges are untouched, so a rescan that re-walks the files
// restores present by the existing path.
//
// It marks exactly the item named, unlike MarkFilesMissing, which works from a file
// set and so reaches every item backing those files. Where several items share one
// file (a single-file rip carved into virtual tracks), marking one leaves its
// siblings claiming present, so a caller repairing such a rip passes every pid. The
// item-scoped rule is what lets the verb answer for an item with no files at all,
// which is the drift case it exists to repair.
func (s *Store) MarkItemMissing(ctx context.Context, itemPID model.PID) (model.MarkMissingOutcome, error) {
	const op = "store.MarkItemMissing"
	var outcome model.MarkMissingOutcome
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		var state string
		err := tx.QueryRowContext(ctx,
			"SELECT id, state FROM playable_item WHERE pid = ?", string(itemPID)).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(itemPID))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		switch model.ItemState(state) {
		case model.StateMissing:
			outcome = model.OutcomeAlreadyMissing
			return nil
		case model.StateArchived:
			outcome = model.OutcomeArchived
			return nil
		case model.StateRemote:
			outcome = model.OutcomeRemote
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET state = ?, updated_at = ? WHERE id = ?",
			string(model.StateMissing), nowNS(), id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "item", itemPID, model.OpUpdate); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		outcome = model.OutcomeMarked
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
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
