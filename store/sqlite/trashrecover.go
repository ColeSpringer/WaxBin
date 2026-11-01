package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/waxerr"
)

// pendingTrash is a trash_pending row and what recovery found for it on disk.
type pendingTrash struct {
	pid, job           string
	fileID             sql.NullInt64
	root               []byte
	origPath, trash    []byte
	origDisplay, shown string
	finish             bool
}

// recoverTrash settles the trash moves a prior owner planned and never journaled (see
// PlanTrash). It runs on a read-write open, which holds the write flock, so every pending
// row belongs to a run that is gone. For each row:
//
//   - the file in the trash and not at its path: the move happened, so it is finished,
//     the file row detached when it is still there and the journal row written from the
//     pending row either way;
//   - the file at its path and not in the trash, or in neither place: the move did not
//     happen, and the row is dropped (a scan finds a file that is gone);
//   - both: a move across filesystems copies and then removes, so a crash between, or a
//     source that would not go, leaves both, as does a stranded file copied back by hand.
//     A trash copy holding the original's bytes, or the start of them, holds nothing the
//     original does not, so it goes and the row is dropped: the move did not happen, and
//     a restore made by hand stands. Any other pair is left for the operator, named in a
//     warning, and the row stays.
//
// A row under a library root that does not answer (a drive not mounted, a check that timed
// out) says nothing of the move and waits for an open that finds the root, as does one whose
// files cannot be read or removed, named in a warning: neither fails the open. The disk is
// settled before the catalog, so a failure part way leaves what the next open settles. It
// returns the number of rows settled.
func (s *Store) recoverTrash(ctx context.Context) (int, error) {
	const op = "store.recoverTrash"
	var n int
	if err := s.rdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM trash_pending").Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if n == 0 {
		return 0, nil
	}
	rows, err := s.rdb().QueryContext(ctx, `SELECT p.pid, COALESCE(p.job_pid, ''), p.file_id, l.root, p.orig_path,
		p.orig_display, p.trash_path, p.trash_display
		FROM trash_pending p JOIN library l ON l.id = p.library_id ORDER BY p.created_at`)
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	var pending []pendingTrash
	for rows.Next() {
		var p pendingTrash
		if err := rows.Scan(&p.pid, &p.job, &p.fileID, &p.root, &p.origPath, &p.origDisplay, &p.trash, &p.shown); err != nil {
			_ = rows.Close()
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		pending = append(pending, p)
	}
	if err := rows.Close(); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	var settled []pendingTrash
	roots := recoveryGate(s.log, "trash recovery: a library root does not answer; its interrupted trash moves wait for an open that finds it")
	for _, p := range pending {
		if roots.Root(ctx, string(p.root)) != nil {
			continue
		}
		ok, err := s.settleTrashOnDisk(&p)
		if err != nil {
			s.log.Warn("trash recovery: could not settle an interrupted trash move; it waits for the next open",
				"file", p.origDisplay, "trash", p.shown, "job", p.job, "err", err)
			continue
		}
		if ok {
			settled = append(settled, p)
		}
	}
	if len(settled) == 0 {
		return 0, nil
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, p := range settled {
			if p.finish {
				if err := finishPendingTrashTx(ctx, tx, p); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM trash_pending WHERE pid = ?", p.pid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return len(settled), nil
}

// settleTrashOnDisk decides a pending row from the disk and makes the disk agree: it
// reports whether the row is settled, and marks one to finish.
func (s *Store) settleTrashOnDisk(p *pendingTrash) (bool, error) {
	orig, trashed := string(p.origPath), string(p.trash)
	origHere, trashHere := pathExists(p.origPath), pathExists(p.trash)
	switch {
	case trashHere && !origHere:
		p.finish = true
	case !trashHere:
		removeEntryFolder(trashed)
	default:
		copied, err := copyOfStart(orig, trashed)
		if err != nil {
			return false, err
		}
		if !copied {
			s.log.Warn("trash recovery: a file is both at its path and in the trash, and the two differ; remove the one not wanted and the next open settles the move",
				"file", p.origDisplay, "trash", p.shown, "job", p.job)
			return false, nil
		}
		if err := os.Remove(pathx.Long(trashed)); err != nil {
			return false, err
		}
		removeEntryFolder(trashed)
	}
	return true, nil
}

// recoveryGate checks each library root a recovery meets once, bounded in time, and warns
// once, with what, for each that does not answer.
func recoveryGate(log *slog.Logger, what string) *fsx.RootGate {
	return &fsx.RootGate{Down: func(root string, err error) { log.Warn(what, "root", root, "err", err) }}
}

// finishPendingTrashTx writes the journal row a pending move planned, detaching its file
// row when it is still there, as TrashFile would have.
func finishPendingTrashTx(ctx context.Context, tx *sql.Tx, p pendingTrash) error {
	if p.fileID.Valid {
		b := newDetachBatch()
		_, err := b.detachTx(ctx, tx, p.fileID.Int64)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := b.settleTx(ctx, tx); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO trash
		(pid, library_id, item_pid, item_kind, orig_path, orig_display, trash_path, trash_display, essence_hash, reason, size, trashed_at)
		SELECT pid, library_id, item_pid, item_kind, orig_path, orig_display, trash_path, trash_display,
			NULLIF(essence_hash, ''), reason, size, created_at
		FROM trash_pending WHERE pid = ?`, p.pid)
	return err
}

// removeEntryFolder removes the trash entry folder a move made for path, and the trash
// folder when that leaves it empty.
func removeEntryFolder(path string) {
	entry := filepath.Dir(path)
	_ = os.Remove(pathx.Long(entry))
	_ = os.Remove(pathx.Long(filepath.Dir(entry)))
}

// copyOfStart reports whether copied holds orig's bytes or the start of them, reading the
// two side by side.
func copyOfStart(orig, copied string) (bool, error) {
	a, err := os.Open(pathx.Long(orig))
	if err != nil {
		return false, err
	}
	defer a.Close()
	b, err := os.Open(pathx.Long(copied))
	if err != nil {
		return false, err
	}
	defer b.Close()
	bufA, bufB := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		nb, errB := io.ReadFull(b, bufB)
		na, errA := io.ReadFull(a, bufA[:nb])
		if errA != nil && !errors.Is(errA, io.ErrUnexpectedEOF) && !errors.Is(errA, io.EOF) {
			return false, errA
		}
		if na < nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		switch {
		case errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF):
			return true, nil
		case errB != nil:
			return false, errB
		}
	}
}
