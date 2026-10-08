package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
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
					NewPath: p.dst, NewDisplayPath: string(p.dst), NewRelPath: []byte(pathx.RelUnder(string(p.root), string(p.dst)))}); err != nil {
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

// OrganizeJournalByJob returns a job's committed moves and companion steps in the order it
// took them, each move with its file as the catalog holds it now, or with none when the
// file row is gone. The item is the one the file backs, its primary edge first.
func (s *Store) OrganizeJournalByJob(ctx context.Context, jobPID model.PID) ([]model.OrganizeMove, error) {
	const op = "store.OrganizeJournalByJob"
	rows, err := s.read.QueryContext(ctx, `
		SELECT jo.kind, jo.src, jo.dst, COALESCE(f.pid, ''), f.path, l.root,
			COALESCE((SELECT pi.pid FROM item_file e JOIN playable_item pi ON pi.id = e.item_id
				WHERE e.file_id = f.id ORDER BY e.role = 'primary' DESC, pi.id LIMIT 1), '')
		FROM organize_journal jo
		LEFT JOIN file f ON f.id = jo.file_id
		LEFT JOIN library l ON l.id = f.library_id
		WHERE jo.job_pid = ? AND jo.state = 'committed'
		ORDER BY jo.id`, string(jobPID))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.OrganizeMove
	for rows.Next() {
		var m model.OrganizeMove
		var kind, filePID, itemPID string
		if err := rows.Scan(&kind, &m.Src, &m.Dst, &filePID, &m.Path, &m.Root, &itemPID); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		m.Kind, m.FilePID, m.ItemPID = model.JournalKind(kind), model.PID(filePID), model.PID(itemPID)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// OrganizeJobMoved reports whether a job's journal holds a committed file move.
func (s *Store) OrganizeJobMoved(ctx context.Context, jobPID model.PID) (bool, error) {
	const op = "store.OrganizeJobMoved"
	var moved bool
	if err := s.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM organize_journal
		WHERE job_pid = ? AND kind = 'file' AND state = 'committed')`, string(jobPID)).Scan(&moved); err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return moved, nil
}

// OrganizeHistory lists the jobs the organize journal holds moves for, newest first, each
// with its file moves counted by state; a non-positive limit lists them all.
func (s *Store) OrganizeHistory(ctx context.Context, limit int) ([]model.OrganizeBatch, error) {
	const op = "store.OrganizeHistory"
	q := `SELECT jo.job_pid, COALESCE(j.kind, ''), COALESCE(j.state, ''), COALESCE(j.started_at, MIN(jo.created_at)),
			SUM(jo.kind = 'file' AND jo.state = 'committed'), SUM(jo.kind = 'file' AND jo.state = 'rolled_back'),
			SUM(jo.kind = 'file' AND jo.state = 'planned')
		FROM organize_journal jo LEFT JOIN job j ON j.pid = jo.job_pid
		GROUP BY jo.job_pid HAVING SUM(jo.kind = 'file') > 0 ORDER BY MAX(jo.id) DESC`
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
	var out []model.OrganizeBatch
	for rows.Next() {
		var b model.OrganizeBatch
		var pid, state string
		if err := rows.Scan(&pid, &b.Kind, &state, &b.StartedAt, &b.Committed, &b.RolledBack, &b.Planned); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		b.JobPID, b.State = model.PID(pid), model.JobState(state)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// PruneOrganizeJournal deletes the journal rows of every job whose moves have all settled
// (committed or rolled back) and whose newest was made no later than olderThanNS ago,
// returning how many went. A move's age is when the job made it, which a crash recovery
// settling it later does not change. A job goes whole or not at all, so an undo never
// finds half of one: a running job stays whatever its age, and so does one with a planned
// row, which the next read-write open settles. A pruned job can no longer be undone, and
// its moves no longer vouch for a scan's album re-key.
func (s *Store) PruneOrganizeJournal(ctx context.Context, olderThanNS int64) (int, error) {
	const op = "store.PruneOrganizeJournal"
	var n int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM organize_journal WHERE job_pid IN (
				SELECT jo.job_pid FROM organize_journal jo GROUP BY jo.job_pid
				HAVING MAX(jo.created_at) <= ? AND SUM(jo.state = 'planned') = 0
				   AND NOT EXISTS (SELECT 1 FROM job j WHERE j.pid = jo.job_pid AND j.state = 'running'))`,
			nowNS()-olderThanNS)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		n, err = r.RowsAffected()
		return err
	})
	return int(n), err
}

// JournalCompanions records the companion steps an organize job took after its moves,
// committed and naming no file, so an undo can take them back.
func (s *Store) JournalCompanions(ctx context.Context, jobPID model.PID, steps []model.CompanionMove) error {
	const op = "store.JournalCompanions"
	if len(steps) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		for _, st := range steps {
			if _, err := tx.ExecContext(ctx, `INSERT INTO organize_journal(pid, job_pid, src, dst, state, kind, created_at)
				VALUES (?, ?, ?, ?, 'committed', ?, ?)`, string(model.NewPID()), string(jobPID), st.Src, st.Dst, string(st.Kind), now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return nil
	})
}
