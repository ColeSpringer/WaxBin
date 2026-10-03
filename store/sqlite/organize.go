package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// recoverOrganize reconciles organize_journal rows left in the 'planned' state by
// a crash between the on-disk move and the catalog commit. It runs on read-write
// Open while this process holds the exclusive write flock, so any pending row
// belongs to a dead prior owner. For each:
//
//   - the move completed but the commit did not (destination present, source gone):
//     finish it by pointing the file at the destination and marking it committed.
//     Present means listed under its own spelling (fsx.Lister), since a case-insensitive
//     filesystem resolves the source of a rename between two spellings of a name as
//     readily as its destination. A destination another file row holds is rolled back
//     with a warning instead, so a stale row never keeps the catalog from opening; the
//     next scan reconciles it.
//   - otherwise (source still present, or both gone): the move did not take
//     effect, so mark rolled_back and leave the catalog's path authoritative.
//
// It returns the number of rows recovered, for logging.
func (s *Store) recoverOrganize(ctx context.Context) (int, error) {
	const op = "store.recoverOrganize"
	// The common case is nothing to recover; check on the read pool first so a clean
	// open does not run a write transaction (which would bump data_version for no
	// reason on every startup).
	var pendingCount int
	if err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM organize_journal WHERE state = 'planned'").Scan(&pendingCount); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if pendingCount == 0 {
		return 0, nil
	}

	n := 0
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		pending, err := pendingMoves(ctx, tx)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		lister := fsx.NewLister()
		present := func(root, path []byte) bool { return pathExists(path) && lister.Spelled(string(root), string(path)) }
		for _, p := range pending {
			committed := p.fileID.Valid && present(p.root, p.dst) && !present(p.root, p.src)
			if committed {
				held, err := pathHeldByAnotherTx(ctx, tx, p.dst, p.fileID.Int64)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if held {
					s.log.Warn("organize recovery: another file holds the destination, keeping the catalog's path", "src", string(p.src), "dst", string(p.dst))
					committed = false
				}
			}
			if committed {
				if err := commitMoveTx(ctx, tx, p.fileID.Int64, p.journalPID, model.RelocateInput{FilePID: model.PID(p.filePID.String),
					NewPath: p.dst, NewDisplayPath: string(p.dst), NewRelPath: relUnder(p.root, p.dst)}); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			} else if _, err := tx.ExecContext(ctx,
				"UPDATE organize_journal SET state='rolled_back' WHERE pid=?", p.journalPID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			n++
		}
		return nil
	})
	return n, err
}

type pendingMove struct {
	journalPID string
	fileID     sql.NullInt64
	filePID    sql.NullString
	root       []byte
	src, dst   []byte
}

// pendingMoves reads every 'planned' journal row plus its file's pid and library
// root. Rows are fully drained before the caller writes, since the single write
// connection cannot interleave a query and an exec.
func pendingMoves(ctx context.Context, tx *sql.Tx) ([]pendingMove, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT jo.pid, jo.file_id, f.pid, l.root, jo.src, jo.dst
		FROM organize_journal jo
		LEFT JOIN file f ON f.id = jo.file_id
		LEFT JOIN library l ON l.id = f.library_id
		WHERE jo.state = 'planned'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingMove
	for rows.Next() {
		var p pendingMove
		if err := rows.Scan(&p.journalPID, &p.fileID, &p.filePID, &p.root, &p.src, &p.dst); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pathHeldByAnotherTx reports whether a file row other than fileID holds path.
func pathHeldByAnotherTx(ctx context.Context, tx *sql.Tx, path []byte, fileID int64) (bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM file WHERE path = ? AND id != ?", path, fileID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// relUnder returns dst relative to root, falling back to the base name when the
// two share no common prefix (a recovered move to a path outside the root).
func relUnder(root, dst []byte) []byte {
	rel, err := filepath.Rel(string(root), string(dst))
	if err != nil {
		return []byte(filepath.Base(string(dst)))
	}
	return []byte(rel)
}
