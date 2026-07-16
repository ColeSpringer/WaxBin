package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Compile-time assertion that Store satisfies the job-store port.
var _ model.JobStore = (*Store)(nil)

// AcquireLease inserts a lease for lease.Scope, returning false (no error) when
// the scope is already held. The scope PRIMARY KEY makes this atomic.
func (s *Store) AcquireLease(ctx context.Context, lease *model.Lease) (bool, error) {
	acquired := false
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			`INSERT INTO lease(scope, owner, job_id, acquired_at, heartbeat_at)
			 VALUES (?,?,?,?,?) ON CONFLICT(scope) DO NOTHING`,
			lease.Scope, lease.Owner, nullInt64(lease.JobID), lease.AcquiredAt, lease.HeartbeatAt)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.AcquireLease", err)
		}
		n, _ := r.RowsAffected()
		acquired = n > 0
		return nil
	})
	return acquired, err
}

// RenewLease bumps a lease heartbeat.
func (s *Store) RenewLease(ctx context.Context, scope, owner string, ts int64) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE lease SET heartbeat_at = ? WHERE scope = ? AND owner = ?", ts, scope, owner)
		return waxerr.Wrap(waxerr.CodeIO, "store.RenewLease", err)
	})
}

// ReleaseLease drops a lease held by owner.
func (s *Store) ReleaseLease(ctx context.Context, scope, owner string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM lease WHERE scope = ? AND owner = ?", scope, owner)
		return waxerr.Wrap(waxerr.CodeIO, "store.ReleaseLease", err)
	})
}

// CreateJob inserts a new job row, assigning its ID and PID.
func (s *Store) CreateJob(ctx context.Context, j *model.Job) error {
	pid := model.NewPID()
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			`INSERT INTO job(pid, kind, scope, state, owner, progress, message, error, started_at, heartbeat_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			string(pid), j.Kind, j.Scope, string(j.State), j.Owner, j.Progress, j.Message, j.Error,
			j.StartedAt, j.HeartbeatAt)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.CreateJob", err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.CreateJob", err)
		}
		j.ID, j.PID = id, pid
		return appendChange(ctx, tx, "job", pid, model.OpCreate)
	})
}

// UpdateJob writes a job's mutable state.
func (s *Store) UpdateJob(ctx context.Context, j *model.Job) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE job SET state=?, progress=?, message=?, error=?, result=?, heartbeat_at=?, finished_at=? WHERE id=?`,
			string(j.State), j.Progress, j.Message, j.Error, j.Result, j.HeartbeatAt, nullInt64(j.FinishedAt), j.ID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.UpdateJob", err)
		}
		return appendChange(ctx, tx, "job", j.PID, model.OpUpdate)
	})
}

// Heartbeat bumps a running job's liveness, progress, and message.
func (s *Store) Heartbeat(ctx context.Context, jobID, ts int64, progress float64, msg string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE job SET heartbeat_at=?, progress=?, message=? WHERE id=?", ts, progress, msg, jobID)
		return waxerr.Wrap(waxerr.CodeIO, "store.Heartbeat", err)
	})
}

// ListJobs returns recent jobs, newest first.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]*model.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.read.QueryContext(ctx, jobSelect+" ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.ListJobs", err)
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.ListJobs", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobByPID returns a single job by public id.
func (s *Store) JobByPID(ctx context.Context, pid model.PID) (*model.Job, error) {
	j, err := scanJob(s.read.QueryRowContext(ctx, jobSelect+" WHERE pid = ?", string(pid)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.JobByPID", "no such job: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.JobByPID", err)
	}
	return j, nil
}

// HasRunningJob reports whether any job is currently in the running state. It is
// the guard a maintenance hand-off checks before closing the store: closing it
// while a server-run scan/analyze/enrich/organize is mid-pass would abort that job.
// Orphaned "running" rows from a dead owner are reclaimed to crashed on Open, so a
// running row here means a live in-process job.
func (s *Store) HasRunningJob(ctx context.Context) (bool, error) {
	var exists int
	err := s.read.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM job WHERE state = ?)", string(model.JobRunning)).Scan(&exists)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.HasRunningJob", err)
	}
	return exists > 0, nil
}

// ReclaimOrphans is called on Open while holding the exclusive write flock: any
// still-running job belongs to a dead prior owner, so mark it crashed and drop
// every lease. Recovery is based on the flock, not PID checks.
func (s *Store) ReclaimOrphans(ctx context.Context, ts int64) (int, error) {
	var reclaimed int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// Collect the pids first so each transition gets a change_log delta, like
		// every other job mutation (a consumer that cached one as "running" sees
		// it go crashed).
		rows, err := tx.QueryContext(ctx, "SELECT pid FROM job WHERE state = ?", string(model.JobRunning))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
		}
		var pids []model.PID
		for rows.Next() {
			var pid model.PID
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
			}
			pids = append(pids, pid)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx,
			`UPDATE job SET state=?, finished_at=?, error='reclaimed: owner not live' WHERE state=?`,
			string(model.JobCrashed), ts, string(model.JobRunning)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
		}
		reclaimed = len(pids)
		for _, pid := range pids {
			if err := appendChange(ctx, tx, "job", pid, model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM lease"); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, "store.ReclaimOrphans", err)
		}
		return nil
	})
	return reclaimed, err
}

const jobSelect = `SELECT id, pid, kind, scope, state, owner, progress, message, error, result,
	started_at, heartbeat_at, finished_at FROM job`

func scanJob(sc rowScanner) (*model.Job, error) {
	var j model.Job
	var finished sql.NullInt64
	if err := sc.Scan(&j.ID, &j.PID, &j.Kind, &j.Scope, &j.State, &j.Owner, &j.Progress,
		&j.Message, &j.Error, &j.Result, &j.StartedAt, &j.HeartbeatAt, &finished); err != nil {
		return nil, err
	}
	j.FinishedAt = finished.Int64
	return &j, nil
}
