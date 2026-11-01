package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// recordLineageTx records under job, in the write's own transaction, each item the write
// folded away with the item that took it in and each item it made with the item whose file
// it came from (item_lineage), so a kind change a crash or a cancel cuts off can still say
// both. A write of no job records nothing.
func recordLineageTx(ctx context.Context, tx *sql.Tx, job model.PID, absorbed, minted map[model.PID]model.PID) error {
	if job == "" || len(absorbed) == 0 && len(minted) == 0 {
		return nil
	}
	now := nowNS()
	for _, rel := range []struct {
		m        map[model.PID]model.PID
		absorbed bool
	}{{absorbed, true}, {minted, false}} {
		for p, other := range rel.m {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO item_lineage(pid, absorbed, job_pid, other_pid, at) VALUES (?, ?, ?, ?, ?)`,
				string(p), rel.absorbed, string(job), string(other), now); err != nil {
				return err
			}
		}
	}
	return nil
}

// AbsorbedInto maps each of pids that job folded away to the item it ended in: the job's
// own fold of the pid, then any later fold of where it went (model.SettleIdentities), when
// the item at the end stands. A pid the job did not fold is absent. With no pids, it maps
// every pid the job folded.
func (s *Store) AbsorbedInto(ctx context.Context, job model.PID, pids []model.PID) (map[model.PID]model.PID, error) {
	const op = "store.AbsorbedInto"
	first := map[model.PID]model.PID{}
	if pids == nil {
		m, err := s.jobLineage(ctx, op, job, true)
		if err != nil {
			return nil, err
		}
		first = m
	}
	for _, p := range pids {
		var into model.PID
		err := s.rdb().QueryRowContext(ctx, "SELECT other_pid FROM item_lineage WHERE pid = ? AND absorbed = 1 AND job_pid = ?",
			string(p), string(job)).Scan(&into)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		first[p] = into
	}
	// Where each first fold went, through any job's later folds.
	later := map[model.PID]model.PID{}
	seen := map[model.PID]bool{}
	var frontier []model.PID
	for _, into := range first {
		frontier = append(frontier, into)
	}
	for len(frontier) > 0 {
		at := frontier[0]
		frontier = frontier[1:]
		if seen[at] {
			continue
		}
		seen[at] = true
		var next model.PID
		err := s.rdb().QueryRowContext(ctx, "SELECT other_pid FROM item_lineage WHERE pid = ? AND absorbed = 1 ORDER BY at DESC LIMIT 1",
			string(at)).Scan(&next)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		later[at] = next
		frontier = append(frontier, next)
	}
	model.SettleIdentities(nil, later, nil)
	out := map[model.PID]model.PID{}
	for p, into := range first {
		if end, ok := later[into]; ok {
			into = end
		}
		stands, err := s.itemStands(ctx, op, into)
		if err != nil {
			return nil, err
		}
		if stands {
			out[p] = into
		}
	}
	return out, nil
}

// MintedFrom maps each item job made for a file another item held to that item, when the
// item made still stands.
func (s *Store) MintedFrom(ctx context.Context, job model.PID) (map[model.PID]model.PID, error) {
	const op = "store.MintedFrom"
	minted, err := s.jobLineage(ctx, op, job, false)
	if err != nil {
		return nil, err
	}
	for p := range minted {
		stands, err := s.itemStands(ctx, op, p)
		if err != nil {
			return nil, err
		}
		if !stands {
			delete(minted, p)
		}
	}
	return minted, nil
}

// DropLineage forgets what job's writes recorded, once nothing will ask for it.
func (s *Store) DropLineage(ctx context.Context, job model.PID) error {
	const op = "store.DropLineage"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM item_lineage WHERE job_pid = ?", string(job))
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// jobLineage reads job's folds (absorbed) or mints, each pid with the other item.
func (s *Store) jobLineage(ctx context.Context, op string, job model.PID, absorbed bool) (map[model.PID]model.PID, error) {
	rows, err := s.rdb().QueryContext(ctx, "SELECT pid, other_pid FROM item_lineage WHERE job_pid = ? AND absorbed = ?", string(job), absorbed)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := map[model.PID]model.PID{}
	for rows.Next() {
		var p, other model.PID
		if err := rows.Scan(&p, &other); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[p] = other
	}
	return out, waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
}

// itemStands reports whether an item with pid is in the catalog.
func (s *Store) itemStands(ctx context.Context, op string, pid model.PID) (bool, error) {
	var stands bool
	if err := s.rdb().QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM playable_item WHERE pid = ?)", string(pid)).Scan(&stands); err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return stands, nil
}
