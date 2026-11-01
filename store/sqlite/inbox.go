package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/colespringer/waxbin/internal/fsx"
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

// fileOccupantsQ reads the files a where clause picks, each with its library's root and the
// item it backs (its primary edge first), one row per edge; fileOccupants keeps a file's
// first.
const fileOccupantsQ = `SELECT f.id, f.path, f.display_path, f.pid, f.library_id, COALESCE(f.essence_hash, ''),
		COALESCE(pi.pid, ''), COALESCE(pi.kind, ''), COALESCE(pi.state, ''), l.root
	FROM file f
	JOIN library l ON l.id = f.library_id
	LEFT JOIN item_file itf ON itf.file_id = f.id
	LEFT JOIN playable_item pi ON pi.id = itf.item_id
	WHERE %s
	ORDER BY f.id, itf.role <> 'primary', itf.item_id`

var (
	fileOccupantsByDisplayQ = fmt.Sprintf(fileOccupantsQ, "f.display_path = ? COLLATE NOCASE")
	fileOccupantsByEssenceQ = fmt.Sprintf(fileOccupantsQ, "f.essence_hash = ?")
)

// FileOccupants returns the cataloged files whose display path equals displayPath folded
// for case, the paths that would collide with it on a case-insensitive filesystem, so an
// import can keep the managed tree portable. NOCASE folds ASCII, covering typical file
// names; file_display_nocase serves the lookup.
func (s *Store) FileOccupants(ctx context.Context, displayPath string) ([]model.FileOccupant, error) {
	return s.fileOccupants(ctx, "store.FileOccupants", fileOccupantsByDisplayQ, displayPath)
}

// FilesByEssence returns the cataloged files holding an audio essence, in every library
// and whatever their state, for an import's duplicate check: one on disk holds the audio,
// and one that is not is the gone file of an item the staged file would restore.
func (s *Store) FilesByEssence(ctx context.Context, essence string) ([]model.FileOccupant, error) {
	return s.fileOccupants(ctx, "store.FilesByEssence", fileOccupantsByEssenceQ, essence)
}

func (s *Store) fileOccupants(ctx context.Context, op, q string, arg any) ([]model.FileOccupant, error) {
	rows, err := s.rdb().QueryContext(ctx, q, arg)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FileOccupant
	last := int64(0)
	reachable := map[string]bool{}
	for rows.Next() {
		var id int64
		var o model.FileOccupant
		var kind, state string
		var root []byte
		if err := rows.Scan(&id, &o.Path, &o.DisplayPath, &o.FilePID, &o.LibraryID, &o.Essence, &o.ItemPID, &kind, &state, &root); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if id == last {
			continue
		}
		up, seen := reachable[string(root)]
		if !seen {
			up = fsx.RootUnreachable(string(root)) == nil
			reachable[string(root)] = up
		}
		last, o.Kind, o.State, o.OnDisk, o.Reachable = id, model.Kind(kind), model.ItemState(state), pathExists(o.Path), up
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
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
	rows, err := s.rdb().QueryContext(ctx, q, args...)
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
