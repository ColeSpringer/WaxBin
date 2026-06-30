package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// recoverOrganize reconciles organize_journal rows left in the 'planned' state by
// a crash between the on-disk move and the catalog commit. It runs on read-write
// Open while this process holds the exclusive write flock, so any pending row
// belongs to a dead prior owner. For each:
//
//   - the move completed but the commit did not (destination present, source
//     gone): finish it by pointing the file at the destination and marking it
//     committed.
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
		for _, p := range pending {
			committed := p.fileID.Valid && pathExists(p.dst) && !pathExists(p.src)
			if committed {
				rel := relUnder(p.root, p.dst)
				if _, err := tx.ExecContext(ctx,
					"UPDATE file SET path=?, display_path=?, rel_path=?, last_seen=? WHERE id=?",
					p.dst, string(p.dst), rel, nowNS(), p.fileID.Int64); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if _, err := tx.ExecContext(ctx,
					"UPDATE organize_journal SET state='committed' WHERE pid=?", p.journalPID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if p.filePID.Valid {
					if err := appendChange(ctx, tx, "file", model.PID(p.filePID.String), model.OpUpdate); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
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

// relUnder returns dst relative to root, falling back to the base name when the
// two share no common prefix (a recovered move to a path outside the root).
func relUnder(root, dst []byte) []byte {
	rel, err := filepath.Rel(string(root), string(dst))
	if err != nil {
		return []byte(filepath.Base(string(dst)))
	}
	return []byte(rel)
}
