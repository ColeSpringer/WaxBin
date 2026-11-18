package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
)

// replaceFileAuxTx makes a file's file_aux_state rows exactly obs: it deletes the
// existing rows and inserts the current set. The full scan path uses it (obs = the
// sidecars that still exist) so a sidecar deleted since the last scan has its
// observation pruned; otherwise the vanished path would be stat'd, and force a full
// re-hash of the file, on every future scan forever.
func replaceFileAuxTx(ctx context.Context, tx *sql.Tx, fileID int64, obs []model.AuxObservation) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_aux_state WHERE file_id = ?", fileID); err != nil {
		return err
	}
	return putFileAuxTx(ctx, tx, fileID, obs)
}

// putFileAuxTx upserts a file's sidecar observations (one row per (file_id, kind)),
// so the scanner can stat-compare a sidecar on the next scan and skip re-parsing an
// unchanged one.
func putFileAuxTx(ctx context.Context, tx *sql.Tx, fileID int64, obs []model.AuxObservation) error {
	for _, o := range obs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO file_aux_state(file_id, kind, path, size, mtime_ns, hash, missing)
			 VALUES (?,?,?,?,?,?,?)
			 ON CONFLICT(file_id, kind) DO UPDATE SET
			   path=excluded.path, size=excluded.size, mtime_ns=excluded.mtime_ns,
			   hash=excluded.hash, missing=excluded.missing`,
			fileID, o.Kind, o.Path, o.Size, o.MTimeNS, o.Hash, boolInt(o.Missing)); err != nil {
			return err
		}
	}
	return nil
}
