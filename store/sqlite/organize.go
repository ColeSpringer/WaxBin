package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

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
//     effect, so mark rolled_back and leave the catalog's path authoritative. A copy a
//     move across filesystems left at the destination beside the source is removed
//     first (dropMoveCopyTx).
//
// A move under a library root that does not answer (a drive not mounted, a check that
// timed out) says nothing either way, so it stays planned for an open that finds the root.
// It returns the number of rows recovered, for logging.
func (s *Store) recoverOrganize(ctx context.Context) (int, error) {
	const op = "store.recoverOrganize"
	// The common case is nothing to recover; check on the read pool first so a clean
	// open does not run a write transaction (which would bump data_version for no
	// reason on every startup).
	var pendingCount int
	if err := s.rdb().QueryRowContext(ctx,
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
		roots := recoveryGate(s.log, "organize recovery: a library root does not answer; its interrupted moves wait for an open that finds it")
		for _, p := range pending {
			if p.root != nil && roots.Root(ctx, string(p.root)) != nil {
				continue
			}
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
			switch {
			case committed:
				if err := commitMoveTx(ctx, tx, p.fileID.Int64, p.journalPID, model.RelocateInput{FilePID: model.PID(p.filePID.String),
					NewPath: p.dst, NewDisplayPath: string(p.dst), NewRelPath: []byte(pathx.RelUnder(string(p.root), string(p.dst)))}); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			case p.fileID.Valid && present(p.root, p.src) && present(p.root, p.dst):
				dropped, err := s.dropMoveCopyTx(ctx, tx, p)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if !dropped {
					continue
				}
				fallthrough
			default:
				if _, err := tx.ExecContext(ctx,
					"UPDATE organize_journal SET state='rolled_back' WHERE pid=?", p.journalPID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// dropMoveCopyTx settles a planned move whose source and destination both stand. A move
// across filesystems copies and then removes, so one cut off before its source went, or
// unable to remove it, leaves a copy at the destination: a file there holding the
// source's bytes, or the start of them, that no other file row holds and that is not the
// source under a second name. That copy is removed. It reports whether the move can be
// rolled back: false keeps the row planned for the next open, with a warning, when the
// copy cannot be read or removed now.
func (s *Store) dropMoveCopyTx(ctx context.Context, tx *sql.Tx, p pendingMove) (bool, error) {
	held, err := pathHeldByAnotherTx(ctx, tx, p.dst, p.fileID.Int64)
	if err != nil || held {
		return err == nil, err
	}
	src, dst := string(p.src), string(p.dst)
	a, errA := os.Stat(pathx.Long(src))
	b, errB := os.Stat(pathx.Long(dst))
	if errA != nil || errB != nil || os.SameFile(a, b) {
		return true, nil
	}
	copied, err := copyOfStart(src, dst)
	if err == nil && copied {
		err = os.Remove(pathx.Long(dst))
	}
	if err != nil {
		s.log.Warn("organize recovery: could not settle a copy an interrupted move left; it waits for the next open",
			"src", src, "dst", dst, "err", err)
		return false, nil
	}
	return true, nil
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
	rows, err := s.rdb().QueryContext(ctx, `
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
	if err := s.rdb().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM organize_journal
		WHERE job_pid = ? AND kind = 'file' AND state = 'committed')`, string(jobPID)).Scan(&moved); err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return moved, nil
}

// organizeRuns selects the organize and undo jobs whose journal holds a file move (one
// that only carried companions moved nothing to list or undo), each with its file moves
// counted by state. %s narrows the jobs and orders and limits them; organize_journal_job
// serves both the narrowing and the counts.
const organizeRuns = `SELECT j.pid, j.kind, j.state, j.started_at,
		SUM(jo.kind = 'file' AND jo.state = 'committed'), SUM(jo.kind = 'file' AND jo.state = 'rolled_back'),
		SUM(jo.kind = 'file' AND jo.state = 'planned')
	FROM (SELECT pid, kind, state, started_at FROM job
		WHERE kind IN ('organize', 'organize-undo')
		  AND EXISTS (SELECT 1 FROM organize_journal f WHERE f.job_pid = job.pid AND f.kind = 'file') %s) j
	JOIN organize_journal jo ON jo.job_pid = j.pid
	GROUP BY j.pid ORDER BY j.pid DESC`

// OrganizeHistory pages the organize and undo runs whose journal holds a file move,
// newest started first (a job pid is a time-ordered ULID, so pid order is start order),
// each with its file moves counted by state. after is the last job of the previous page,
// empty for the first; a non-positive limit takes the default page size.
func (s *Store) OrganizeHistory(ctx context.Context, after model.PID, limit int) (model.OrganizeBatchPage, error) {
	const op = "store.OrganizeHistory"
	if limit <= 0 {
		limit = defaultPageSize
	}
	narrow, args := "ORDER BY pid DESC LIMIT ?", []any{limit + 1}
	if after != "" {
		narrow, args = "AND pid < ? "+narrow, []any{string(after), limit + 1}
	}
	batches, err := s.organizeBatches(ctx, op, fmt.Sprintf(organizeRuns, narrow), args...)
	if err != nil {
		return model.OrganizeBatchPage{}, err
	}
	page := model.OrganizeBatchPage{Batches: batches}
	if len(batches) > limit {
		page.Batches, page.HasMore, page.Next = batches[:limit], true, batches[limit-1].JobPID
	}
	return page, nil
}

// OrganizeBatchByJob reads one organize or undo run by its job, CodeNotFound when the job
// is no such run or its journal holds no file move.
func (s *Store) OrganizeBatchByJob(ctx context.Context, jobPID model.PID) (*model.OrganizeBatch, error) {
	const op = "store.OrganizeBatchByJob"
	batches, err := s.organizeBatches(ctx, op, fmt.Sprintf(organizeRuns, "AND pid = ?"), string(jobPID))
	if err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no organize run with file moves: "+string(jobPID))
	}
	return &batches[0], nil
}

func (s *Store) organizeBatches(ctx context.Context, op, q string, args ...any) ([]model.OrganizeBatch, error) {
	rows, err := s.rdb().QueryContext(ctx, q, args...)
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

// PruneOrganizeJournal deletes the journal rows of every organize or undo job whose moves
// have all settled (committed or rolled back) and whose newest was made no later than
// olderThanNS ago, returning how many went. A move's age is when the job made it, which a
// crash recovery settling it later does not change. A job goes whole or not at all, so an
// undo never finds half of one: a running job stays whatever its age, and so does one with
// a planned row, which the next read-write open settles. A pruned job can no longer be
// undone, and its moves no longer vouch for a scan's album re-key. It walks the jobs, not
// the journal, which is the larger of the two.
func (s *Store) PruneOrganizeJournal(ctx context.Context, olderThanNS int64) (int, error) {
	const op = "store.PruneOrganizeJournal"
	var n int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM organize_journal WHERE job_pid IN (
				SELECT j.pid FROM job j
				WHERE j.kind IN ('organize', 'organize-undo') AND j.state <> 'running'
				  AND EXISTS (SELECT 1 FROM organize_journal jo WHERE jo.job_pid = j.pid)
				  AND NOT EXISTS (SELECT 1 FROM organize_journal jo
					WHERE jo.job_pid = j.pid AND (jo.state = 'planned' OR jo.created_at > ?)))`,
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
