package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// CreatePlaylist creates a static or smart playlist owned by ownerPID (empty =
// default user). A smart playlist requires a rule, stored as a versioned query
// document; a static one must not carry one.
func (s *Store) CreatePlaylist(ctx context.Context, name string, ownerPID model.PID, kind model.PlaylistKind, vis model.PlaylistVisibility, rule *query.Query) (model.PID, error) {
	const op = "store.CreatePlaylist"
	name = strings.TrimSpace(name)
	if name == "" {
		return "", waxerr.New(waxerr.CodeInvalid, op, "playlist name is required")
	}
	if !kind.Valid() {
		return "", waxerr.New(waxerr.CodeInvalid, op, "unknown playlist kind: "+string(kind))
	}
	if vis == "" {
		vis = model.VisibilityPrivate
	}
	if !vis.Valid() {
		return "", waxerr.New(waxerr.CodeInvalid, op, "unknown visibility: "+string(vis))
	}
	var ruleJSON any
	switch kind {
	case model.PlaylistSmart:
		if rule == nil {
			return "", waxerr.New(waxerr.CodeInvalid, op, "a smart playlist requires a rule")
		}
		if err := validatePlaylistRule(*rule, op); err != nil {
			return "", err
		}
		b, err := query.MarshalRule(*rule)
		if err != nil {
			return "", err
		}
		ruleJSON = string(b)
	default:
		if rule != nil {
			return "", waxerr.New(waxerr.CodeInvalid, op, "a static playlist must not carry a rule")
		}
	}

	pid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, ownerPID, op)
		if err != nil {
			return err
		}
		now := nowNS()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist(pid, name, owner_user_id, kind, visibility, rule, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			string(pid), name, userID, string(kind), string(vis), ruleJSON, now, now); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "playlist", pid, model.OpCreate)
	})
	if err != nil {
		return "", err
	}
	return pid, nil
}

const playlistSelect = `SELECT p.pid, p.name, u.pid, u.name, p.kind, p.visibility, p.rule,
	p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM playlist_item pli WHERE pli.playlist_id = p.id),
	EXISTS(SELECT 1 FROM art_map am
	         WHERE am.entity_type = 'playlist' AND am.entity_id = p.id AND am.role = 'front')
	FROM playlist p JOIN user u ON u.id = p.owner_user_id`

func scanPlaylist(sc rowScanner) (*model.Playlist, error) {
	var p model.Playlist
	var kind, vis string
	var rule sql.NullString
	if err := sc.Scan(&p.PID, &p.Name, &p.OwnerPID, &p.OwnerName, &kind, &vis, &rule,
		&p.CreatedAt, &p.UpdatedAt, &p.ItemCount, &p.HasArt); err != nil {
		return nil, err
	}
	p.Kind = model.PlaylistKind(kind)
	p.Visibility = model.PlaylistVisibility(vis)
	if rule.Valid && rule.String != "" {
		q, err := query.ParseRule([]byte(rule.String))
		if err != nil {
			return nil, err
		}
		p.Rule = &q
	}
	return &p, nil
}

// PlaylistByPID returns one playlist's metadata, or CodeNotFound.
func (s *Store) PlaylistByPID(ctx context.Context, pid model.PID) (*model.Playlist, error) {
	const op = "store.PlaylistByPID"
	p, err := scanPlaylist(s.read.QueryRowContext(ctx, playlistSelect+" WHERE p.pid = ?", string(pid)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return p, nil
}

// ListPlaylists lists the playlists visible to ownerPID (empty = default user):
// the user's own plus any shared by others, ordered by name.
func (s *Store) ListPlaylists(ctx context.Context, ownerPID model.PID) ([]*model.Playlist, error) {
	const op = "store.ListPlaylists"
	userID, err := userIDByPID(ctx, s.read, ownerPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		playlistSelect+" WHERE p.owner_user_id = ? OR p.visibility = 'shared' ORDER BY u.name, p.name",
		userID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.Playlist
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePlaylist removes a playlist and (by cascade) its item rows, along with any
// cover it carried. art_map is polymorphic with no FK, and a playlist is never merged
// or orphan-GC'd, so this is the only place a playlist's art rows are cleaned; see
// deleteEntityArtTx for why they cannot wait for GCArt. The source image left behind
// becomes ordinary GC-able garbage, exactly like a swapped cover.
func (s *Store) DeletePlaylist(ctx context.Context, pid model.PID) error {
	const op = "store.DeletePlaylist"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM playlist WHERE pid = ?", string(pid)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := deleteEntityArtTx(ctx, tx, "playlist", id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM playlist WHERE id = ?", id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "playlist", pid, model.OpDelete)
	})
}

// RenamePlaylist sets a playlist's display name.
func (s *Store) RenamePlaylist(ctx context.Context, pid model.PID, name string) error {
	const op = "store.RenamePlaylist"
	name = strings.TrimSpace(name)
	if name == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "playlist name is required")
	}
	return s.playlistUpdate(ctx, op, pid, "UPDATE playlist SET name = ?, updated_at = ? WHERE pid = ?", name)
}

// SetPlaylistVisibility changes who can see a playlist.
func (s *Store) SetPlaylistVisibility(ctx context.Context, pid model.PID, vis model.PlaylistVisibility) error {
	const op = "store.SetPlaylistVisibility"
	if !vis.Valid() || vis == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown visibility: "+string(vis))
	}
	return s.playlistUpdate(ctx, op, pid, "UPDATE playlist SET visibility = ?, updated_at = ? WHERE pid = ?", string(vis))
}

// SetPlaylistRule replaces a smart playlist's rule in place, under its existing
// pid, so anything keyed on the pid (a share, a client's saved list) survives a
// rule edit. The rule is validated the way CreatePlaylist validates, so an
// unrunnable rule is rejected at write time rather than surfacing on every
// future read. Writing the byte-identical stored rule is a silent no-op (no
// update, no change_log delta). A static playlist has no rule to replace
// (CodeInvalid); an unknown pid is CodeNotFound.
func (s *Store) SetPlaylistRule(ctx context.Context, pid model.PID, rule query.Query) error {
	const op = "store.SetPlaylistRule"
	var kind string
	var stored sql.NullString
	err := s.read.QueryRowContext(ctx,
		"SELECT kind, rule FROM playlist WHERE pid = ?", string(pid)).Scan(&kind, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if model.PlaylistKind(kind) != model.PlaylistSmart {
		return waxerr.New(waxerr.CodeInvalid, op, "cannot set a rule on a static playlist")
	}
	if err := validatePlaylistRule(rule, op); err != nil {
		return err
	}
	b, err := query.MarshalRule(rule)
	if err != nil {
		return err
	}
	if stored.Valid && stored.String == string(b) {
		return nil // the byte-identical rule: no write, no change-feed churn
	}
	return s.playlistUpdate(ctx, op, pid,
		"UPDATE playlist SET rule = ?, updated_at = ? WHERE pid = ?", string(b))
}

// validatePlaylistRule compiles a smart-playlist rule against its entity's field
// whitelist so a rule with an unknown entity, an unknown field, or an invalid
// limit-mode combination is rejected at write time. Shared by CreatePlaylist
// (a deliberate tightening: create used to store rules unvalidated) and
// SetPlaylistRule, so the two write paths can never disagree on what is
// storable.
func validatePlaylistRule(rule query.Query, op string) error {
	fm, ok := fieldMapFor(rule.Entity)
	if !ok {
		return waxerr.New(waxerr.CodeInvalid, op, "unsupported rule entity: "+string(rule.Entity))
	}
	if _, err := query.Compile(rule, fm); err != nil {
		return err
	}
	return nil
}

// playlistUpdate runs a single-column playlist update (value, then now, then pid)
// and emits the update delta, erroring with CodeNotFound when no row matched.
func (s *Store) playlistUpdate(ctx context.Context, op string, pid model.PID, stmt string, value any) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, stmt, value, nowNS(), string(pid))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
		}
		return appendChange(ctx, tx, "playlist", pid, model.OpUpdate)
	})
}

// PlaylistItems returns a playlist's items: a static playlist's stored order, or a
// smart playlist's rule evaluated on read through the shared query engine. If a smart
// rule references a per-user field such as rating, starred, or play_count, it
// evaluates against userPID's play_state, so one rule yields different membership per
// user. The user is bound at read time and never stored in the rule. userPID goes
// unused for a static playlist or a smart rule that touches no user-state field.
// A rule's limit mode (random/minutes/megabytes) and relative-date operators apply
// here unchanged, because the evaluation goes through QueryItems: "25 random from
// the last 30 days" is one stored rule.
func (s *Store) PlaylistItems(ctx context.Context, pid model.PID, userPID model.PID) ([]*model.ItemView, error) {
	const op = "store.PlaylistItems"
	p, err := s.PlaylistByPID(ctx, pid)
	if err != nil {
		return nil, err
	}
	if p.Kind == model.PlaylistSmart {
		if p.Rule == nil {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "smart playlist has no rule")
		}
		return s.QueryItems(ctx, *p.Rule, userPID)
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+` JOIN playlist_item pli ON pli.item_id = pi.id
		 JOIN playlist pl ON pl.id = pli.playlist_id
		 WHERE pl.pid = ? ORDER BY pli.position`, string(pid))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.ItemView
	for rows.Next() {
		v, err := scanItemView(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddPlaylistItems appends items to a static playlist, after its current last
// position. A smart playlist rejects explicit membership edits.
func (s *Store) AddPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error {
	const op = "store.AddPlaylistItems"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil {
			return err
		}
		var pos int
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(position), -1) FROM playlist_item WHERE playlist_id = ?", plID).Scan(&pos); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, itemPID := range itemPIDs {
			itemID, err := itemIDByPID(ctx, tx, itemPID, op)
			if err != nil {
				return err
			}
			pos++
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO playlist_item(playlist_id, position, item_id) VALUES (?,?,?)", plID, pos, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return s.touchPlaylistTx(ctx, tx, plID, pid)
	})
}

// SetPlaylistItems replaces a static playlist's contents with the given order.
func (s *Store) SetPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error {
	const op = "store.SetPlaylistItems"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM playlist_item WHERE playlist_id = ?", plID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for pos, itemPID := range itemPIDs {
			itemID, err := itemIDByPID(ctx, tx, itemPID, op)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO playlist_item(playlist_id, position, item_id) VALUES (?,?,?)", plID, pos, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return s.touchPlaylistTx(ctx, tx, plID, pid)
	})
}

// RemovePlaylistItem removes every position of an item from a static playlist.
func (s *Store) RemovePlaylistItem(ctx context.Context, pid model.PID, itemPID model.PID) error {
	const op = "store.RemovePlaylistItem"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx,
			"DELETE FROM playlist_item WHERE playlist_id = ? AND item_id = ?", plID, itemID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Don't report success or churn the change feed for an item that was not in
		// the playlist (matches the by-position variant's contract).
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "item is not in the playlist")
		}
		return s.touchPlaylistTx(ctx, tx, plID, pid)
	})
}

// RemovePlaylistItemAt removes the entry at one position of a static playlist, so a
// single occurrence of a duplicated item can be dropped without purging the rest.
// Positions stay non-contiguous; the surviving order is preserved.
func (s *Store) RemovePlaylistItemAt(ctx context.Context, pid model.PID, position int) error {
	const op = "store.RemovePlaylistItemAt"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx,
			"DELETE FROM playlist_item WHERE playlist_id = ? AND position = ?", plID, position)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no playlist entry at that position")
		}
		return s.touchPlaylistTx(ctx, tx, plID, pid)
	})
}

// staticPlaylistIDTx resolves a playlist pid to its rowid, requiring it be static
// (membership edits do not apply to a smart playlist's computed contents).
func (s *Store) staticPlaylistIDTx(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, error) {
	var id int64
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT id, kind FROM playlist WHERE pid = ?", string(pid)).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if model.PlaylistKind(kind) != model.PlaylistStatic {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "cannot edit the membership of a smart playlist")
	}
	return id, nil
}

// touchPlaylistTx bumps updated_at and emits the playlist update delta.
func (s *Store) touchPlaylistTx(ctx context.Context, tx *sql.Tx, plID int64, pid model.PID) error {
	if _, err := tx.ExecContext(ctx, "UPDATE playlist SET updated_at = ? WHERE id = ?", nowNS(), plID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.playlist", err)
	}
	return appendChange(ctx, tx, "playlist", pid, model.OpUpdate)
}

// ItemByPlaylistPath resolves an M3U8 playlist entry to a cataloged item. An
// absolute entry is matched against the raw path using the UNIQUE path index. That
// is the round-trip case for WaxBin exports, where display-path bytes are the
// stored path. A relative entry, common in playlists authored by other tools,
// falls back to a unique path-suffix match on the display path, anchored at a
// separator so "b/x.mp3" does not match "prefix/ab/x.mp3". An ambiguous suffix
// or no match is CodeNotFound, so import never guesses.
func (s *Store) ItemByPlaylistPath(ctx context.Context, p string) (*model.ItemView, error) {
	const op = "store.ItemByPlaylistPath"
	// Normalize separators and fold away "." / redundant separators so a dotted entry
	// like "./Artist/Track.mp3" or "a/./b.mp3" matches the stored path.
	clean := filepath.Clean(filepath.FromSlash(p))
	v, err := scanItemView(s.read.QueryRowContext(ctx, itemSelect+" WHERE f.path = ? LIMIT 1", []byte(clean)))
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if filepath.IsAbs(clean) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no item at path: "+p)
	}

	// Relative entry: a unique path-suffix match on the display path.
	pattern := "%" + escapeLike(string(filepath.Separator)+clean)
	rows, err := s.read.QueryContext(ctx,
		itemSelect+` WHERE f.display_path LIKE ? ESCAPE '\' LIMIT 2`, pattern)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var matches []*model.ItemView
	for rows.Next() {
		m, serr := scanItemView(rows)
		if serr != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, serr)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return nil, waxerr.New(waxerr.CodeNotFound, op, "no unique item for relative path: "+p)
}

// escapeLike escapes LIKE metacharacters so a literal matches verbatim under
// "LIKE ? ESCAPE '\\'".
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
