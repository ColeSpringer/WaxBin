package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/model"
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
		       ps.rating, ps.starred_at, ps.last_played_at, ps.rating_changed_at, ps.starred_changed_at, ps.updated_at
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
		var starredAt, lastPlayed, ratingChanged, starredChanged sql.NullInt64
		if err := rows.Scan(&userPID, &itemPID, &ps.PositionMS, &ps.Played, &ps.Finished,
			&ps.PlayCount, &rating, &starredAt, &lastPlayed, &ratingChanged, &starredChanged, &ps.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ps.UserPID, ps.ItemPID = model.PID(userPID), model.PID(itemPID)
		ps.Rating, ps.HasRating = int(rating.Int64), rating.Valid
		ps.StarredAt, ps.Starred = starredAt.Int64, starredAt.Valid
		ps.LastPlayedAt = lastPlayed.Int64
		ps.RatingChangedAt, ps.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
		out = append(out, ps)
	}
	return out, rows.Err()
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
