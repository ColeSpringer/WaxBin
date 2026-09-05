package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// SetSecret stores (or replaces) a named secret. When a cipher is configured the
// value is sealed at rest (bound to its key as associated data); otherwise it is
// stored in plaintext. Values are never logged or written to a logical export, but
// a full DB backup contains them. A plaintext-mode value literally beginning with
// the reserved sealed-value marker is refused so it can never be misread as sealed.
func (s *Store) SetSecret(ctx context.Context, key, value string) error {
	const op = "store.SetSecret"
	if strings.TrimSpace(key) == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "empty secret key")
	}
	stored := value
	if s.cipher != nil {
		sealed, err := sealValue(s.cipher, s.cipherKeyID, key, value)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInternal, op, err)
		}
		stored = sealed
	} else if looksSealed(value) {
		return waxerr.New(waxerr.CodeInvalid, op, "secret value cannot begin with the reserved "+sealPrefix+" marker")
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO secret(key, value, updated_at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, stored, nowNS())
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
}

// GetSecret returns a secret value, or CodeNotFound. A sealed value is opened with
// the configured cipher; a sealed value with no cipher configured is CodeInvalid, and
// a plaintext value (either plaintext mode, or one not yet adopted by ReSealSecrets)
// is returned as-is.
func (s *Store) GetSecret(ctx context.Context, key string) (string, error) {
	const op = "store.GetSecret"
	var v string
	err := s.read.QueryRowContext(ctx, "SELECT value FROM secret WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", waxerr.New(waxerr.CodeNotFound, op, "no such secret: "+key)
	}
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if looksSealed(v) {
		if s.cipher == nil {
			return "", waxerr.New(waxerr.CodeInvalid, op, "secret is sealed but no cipher is configured: "+key)
		}
		return openValue(s.cipher, key, v)
	}
	return v, nil
}

// DeleteSecret removes a secret (no error if absent).
func (s *Store) DeleteSecret(ctx context.Context, key string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM secret WHERE key = ?", key)
		return waxerr.Wrap(waxerr.CodeIO, "store.DeleteSecret", err)
	})
}

// BackupTo writes a self-contained byte copy of the catalog to dest via
// VACUUM INTO (which captures committed state and works on a read-only source, so
// a backup can run concurrently with a writer). The copy contains every table,
// the secret table included; use port.RedactBackupFile to strip secrets from a
// copy meant to leave the host.
func (s *Store) BackupTo(ctx context.Context, dest string) error {
	const op = "store.BackupTo"
	if strings.TrimSpace(dest) == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "empty backup destination")
	}
	if _, err := s.read.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return waxerr.Wrapf(waxerr.CodeIO, op, err, "backing up to %s", dest)
	}
	// The backup carries the secret table, so restrict it like the live catalog.
	s.restrictSecretFiles(dest)
	return nil
}

// AllPlayStates returns every user's playback state with user and item pids, for
// the logical export. It is ordered for a stable export.
func (s *Store) AllPlayStates(ctx context.Context) ([]model.PlayState, error) {
	const op = "store.AllPlayStates"
	rows, err := s.read.QueryContext(ctx, `
		SELECT u.pid, pi.pid, ps.position_ms, ps.played, ps.finished, ps.play_count,
		       ps.rating, ps.starred_at, ps.last_played_at, ps.last_progress_at,
		       ps.rating_changed_at, ps.starred_changed_at, ps.played_changed_at, ps.updated_at
		FROM play_state ps
		JOIN user u ON u.id = ps.user_id
		JOIN playable_item pi ON pi.id = ps.item_id
		ORDER BY u.pid, pi.pid`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.PlayState
	for rows.Next() {
		var ps model.PlayState
		var userPID, itemPID string
		var rating sql.NullInt64
		var starredAt, lastPlayed, lastProgress, ratingChanged, starredChanged, playedChanged sql.NullInt64
		if err := rows.Scan(&userPID, &itemPID, &ps.PositionMS, &ps.Played, &ps.Finished,
			&ps.PlayCount, &rating, &starredAt, &lastPlayed, &lastProgress,
			&ratingChanged, &starredChanged, &playedChanged, &ps.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ps.UserPID, ps.ItemPID = model.PID(userPID), model.PID(itemPID)
		ps.Rating, ps.HasRating = int(rating.Int64), rating.Valid
		ps.StarredAt, ps.Starred = starredAt.Int64, starredAt.Valid
		ps.LastPlayedAt, ps.LastProgressAt = lastPlayed.Int64, lastProgress.Int64
		ps.RatingChangedAt, ps.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
		ps.PlayedChangedAt = playedChanged.Int64
		out = append(out, ps)
	}
	return out, rows.Err()
}

// ExportCounts counts what Export would write, under its filters (no podcast
// library, no episodes, and no play state or session on one), in one statement so
// the four figures come from one snapshot. It is the manifest's source when the
// body is not wanted.
func (s *Store) ExportCounts(ctx context.Context) (read.ExportCounts, error) {
	const op = "store.ExportCounts"
	var c read.ExportCounts
	err := s.read.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM library WHERE mode != ?),
		       (SELECT COUNT(*) FROM playable_item WHERE kind != ?),
		       (SELECT COUNT(*) FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id WHERE pi.kind != ?),
		       (SELECT COUNT(*) FROM play_session ps JOIN playable_item pi ON pi.id = ps.item_id WHERE pi.kind != ?)`,
		string(model.ModePodcast), string(model.KindEpisode), string(model.KindEpisode), string(model.KindEpisode)).
		Scan(&c.Libraries, &c.Items, &c.PlayStates, &c.PlaySessions)
	if err != nil {
		return read.ExportCounts{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return c, nil
}

// ExportSessions streams the listening log for the export from one read snapshot.
// counted receives, before any row, the number of sessions keep admits, so the
// manifest can be written ahead of them; each admitted session then arrives in
// export order (user, start, pid), an open one with a zero EndedAt. The count and
// the rows come from one read transaction, so they cannot disagree, and the log is
// never held whole: it is the one exported table that grows with listening rather
// than with the catalog.
func (s *Store) ExportSessions(ctx context.Context, keep func(itemPID model.PID) bool, counted func(n int) error, each func(model.PlaySession) error) error {
	const op = "store.ExportSessions"
	conn, err := s.read.Conn(ctx)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer tx.Rollback()
	const from = ` FROM play_session ps
		JOIN user u ON u.id = ps.user_id
		JOIN playable_item pi ON pi.id = ps.item_id
		ORDER BY u.pid, ps.started_at, ps.pid`

	n := 0
	rows, err := tx.QueryContext(ctx, "SELECT pi.pid"+from)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if keep(model.PID(pid)) {
			n++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := counted(n); err != nil {
		return err
	}

	rows, err = tx.QueryContext(ctx,
		"SELECT ps.pid, u.pid, pi.pid, ps.started_at, ps.ended_at, ps.ms_played, ps.client"+from)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var ps model.PlaySession
		var ended sql.NullInt64
		if err := rows.Scan(&ps.PID, &ps.UserPID, &ps.ItemPID, &ps.StartedAt, &ended, &ps.MsPlayed, &ps.Client); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !keep(ps.ItemPID) {
			continue
		}
		ps.EndedAt = ended.Int64
		if err := each(ps); err != nil {
			return err
		}
	}
	return waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
}

// RelocateLibraryRoot re-points a library (and every file under it) at a new root
// path, for a portable restore onto a different machine or mount. File rel paths
// are preserved, so path = newRoot/rel. The new root must be absolute.
func (s *Store) RelocateLibraryRoot(ctx context.Context, libPID model.PID, newRoot string) error {
	const op = "store.RelocateLibraryRoot"
	if !filepath.IsAbs(newRoot) {
		return waxerr.New(waxerr.CodeInvalid, op, "new root must be absolute: "+newRoot)
	}
	newRoot = filepath.Clean(newRoot)
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var libID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM library WHERE pid = ?", string(libPID)).Scan(&libID)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such library: "+string(libPID))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE library SET root=?, display_root=? WHERE id=?",
			[]byte(newRoot), newRoot, libID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Collect (id, rel) first; the single write connection cannot update while a
		// query is open.
		type fileRel struct {
			id  int64
			rel []byte
		}
		rows, err := tx.QueryContext(ctx, "SELECT id, rel_path FROM file WHERE library_id = ?", libID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		var files []fileRel
		for rows.Next() {
			var f fileRel
			if err := rows.Scan(&f.id, &f.rel); err != nil {
				rows.Close()
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			files = append(files, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		for _, f := range files {
			newPath := filepath.Join(newRoot, string(f.rel))
			if _, err := tx.ExecContext(ctx, "UPDATE file SET path=?, display_path=? WHERE id=?",
				[]byte(newPath), newPath, f.id); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "library", libPID, model.OpUpdate)
	})
}
