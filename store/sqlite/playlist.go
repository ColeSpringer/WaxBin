package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
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
	row, err := newPlaylistRow(op, name, kind, vis, rule)
	if err != nil {
		return "", err
	}
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := row.insertTx(ctx, tx, op, ownerPID)
		return err
	}); err != nil {
		return "", err
	}
	return row.pid, nil
}

// CreatePlaylistWithItems creates a static playlist holding itemPIDs in order, the
// playlist and its entries in one write, so an entry naming no item (CodeNotFound)
// leaves no playlist behind. It emits the one create delta.
func (s *Store) CreatePlaylistWithItems(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility, itemPIDs []model.PID) (model.PID, error) {
	const op = "store.CreatePlaylistWithItems"
	row, err := newPlaylistRow(op, name, model.PlaylistStatic, vis, nil)
	if err != nil {
		return "", err
	}
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := row.insertTx(ctx, tx, op, ownerPID)
		if err != nil {
			return err
		}
		return insertPlaylistEntriesTx(ctx, tx, op, plID, 0, itemPIDs)
	}); err != nil {
		return "", err
	}
	return row.pid, nil
}

// playlistRow is a validated playlist ready to insert.
type playlistRow struct {
	pid  model.PID
	name string
	kind model.PlaylistKind
	vis  model.PlaylistVisibility
	rule any // the marshaled smart rule, nil for a static playlist
}

func newPlaylistRow(op, name string, kind model.PlaylistKind, vis model.PlaylistVisibility, rule *query.Query) (playlistRow, error) {
	row := playlistRow{pid: model.NewPID(), name: strings.TrimSpace(name), kind: kind, vis: vis}
	if row.name == "" {
		return row, waxerr.New(waxerr.CodeInvalid, op, "playlist name is required")
	}
	if !kind.Valid() {
		return row, waxerr.New(waxerr.CodeInvalid, op, "unknown playlist kind: "+string(kind))
	}
	if row.vis == "" {
		row.vis = model.VisibilityPrivate
	}
	if !row.vis.Valid() {
		return row, waxerr.New(waxerr.CodeInvalid, op, "unknown visibility: "+string(vis))
	}
	switch kind {
	case model.PlaylistSmart:
		if rule == nil {
			return row, waxerr.New(waxerr.CodeInvalid, op, "a smart playlist requires a rule")
		}
		if err := validatePlaylistRule(*rule, op); err != nil {
			return row, err
		}
		b, err := query.MarshalRule(*rule)
		if err != nil {
			return row, err
		}
		row.rule = string(b)
	default:
		if rule != nil {
			return row, waxerr.New(waxerr.CodeInvalid, op, "a static playlist must not carry a rule")
		}
	}
	return row, nil
}

// insertTx writes the playlist row and its create delta and returns the rowid.
func (row playlistRow) insertTx(ctx context.Context, tx *sql.Tx, op string, ownerPID model.PID) (int64, error) {
	userID, err := userIDByPID(ctx, tx, ownerPID, op)
	if err != nil {
		return 0, err
	}
	now := nowNS()
	r, err := tx.ExecContext(ctx,
		`INSERT INTO playlist(pid, name, owner_user_id, kind, visibility, rule, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		string(row.pid), row.name, userID, string(row.kind), string(row.vis), row.rule, now, now)
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, appendChange(ctx, tx, "playlist", row.pid, model.OpCreate)
}

// insertPlaylistEntriesTx writes itemPIDs as entries from position start on.
func insertPlaylistEntriesTx(ctx context.Context, tx *sql.Tx, op string, plID int64, start int, itemPIDs []model.PID) error {
	for i, itemPID := range itemPIDs {
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO playlist_item(playlist_id, position, item_id) VALUES (?,?,?)", plID, start+i, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
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
	p, err := scanPlaylist(s.rdb().QueryRowContext(ctx, playlistSelect+" WHERE p.pid = ?", string(pid)))
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
	userID, err := userIDByPID(ctx, s.rdb(), ownerPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb().QueryContext(ctx,
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

// SetPlaylistOwner moves a playlist to another user (an empty ownerPID selects the
// default user). Who sees a private playlist follows its owner, as at creation, while a
// smart rule over per-user state keeps evaluating for whoever reads it. Moving a
// playlist to the owner it has writes nothing.
func (s *Store) SetPlaylistOwner(ctx context.Context, pid, ownerPID model.PID) error {
	const op = "store.SetPlaylistOwner"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var id, owner int64
		err := tx.QueryRowContext(ctx,
			"SELECT id, owner_user_id FROM playlist WHERE pid = ?", string(pid)).Scan(&id, &owner)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such playlist: "+string(pid))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		userID, err := userIDByPID(ctx, tx, ownerPID, op)
		if err != nil || userID == owner {
			return err
		}
		return movePlaylistTx(ctx, tx, op, playlistRef{id: id, pid: pid}, userID)
	})
}

// TransferPlaylists moves every playlist fromPID owns to toPID (empty pids select the
// default user), each with its own delta, and returns how many moved: the step a host
// takes before it retires a user.
func (s *Store) TransferPlaylists(ctx context.Context, fromPID, toPID model.PID) (int, error) {
	const op = "store.TransferPlaylists"
	var moved int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		from, err := userIDByPID(ctx, tx, fromPID, op)
		if err != nil {
			return err
		}
		to, err := userIDByPID(ctx, tx, toPID, op)
		if err != nil || from == to {
			return err
		}
		owned, err := queryPlaylistRefsTx(ctx, tx, "SELECT id, pid FROM playlist WHERE owner_user_id = ?", from)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, p := range owned {
			if err := movePlaylistTx(ctx, tx, op, p, to); err != nil {
				return err
			}
		}
		moved = len(owned)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return moved, nil
}

type playlistRef struct {
	id  int64
	pid model.PID
}

func movePlaylistTx(ctx context.Context, tx *sql.Tx, op string, p playlistRef, userID int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE playlist SET owner_user_id = ?, updated_at = ? WHERE id = ?",
		userID, nowNS(), p.id); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return appendChange(ctx, tx, "playlist", p.pid, model.OpUpdate)
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
	err := s.rdb().QueryRowContext(ctx,
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
	rows, err := s.rdb().QueryContext(ctx,
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

// CountPlaylistItems returns how many of a playlist's members also match narrow. The
// contract is exact for any userPID that resolves: the result equals the number of
// rows PlaylistItems(pid, userPID) returns that satisfy narrow. A nil narrow counts
// every member. See the playlist.Service.CountItems doc comment for the three
// "playlist count" semantics this sits among.
//
// The "that resolves" qualifier is the one gap, and it is the strict side of it. A
// static PlaylistItems never looks the user up, so it answers for an unknown pid;
// this goes through userStateJoin, which validates any non-empty pid so a typo is not
// silently answered as the default user. The two therefore disagree on a pid no user
// holds, where this returns CodeNotFound and PlaylistItems returns the members.
//
// A static playlist is one indexed COUNT(*) off the playlist. A smart one branches on
// whether its rule selects every matching row, because narrowing only commutes with
// the rule when it does. A limit picks rows first, so pushing narrow inside a "10 most
// recent tracks" rule would count every matching track in the catalog and clamp to 10,
// where the truth is however many of those 10 match. An offset changes which rows get
// skipped. Both cases evaluate the rule and count the matches among the rows it chose.
func (s *Store) CountPlaylistItems(ctx context.Context, pid model.PID, userPID model.PID, narrow query.Node) (int, error) {
	const op = "store.CountPlaylistItems"
	p, err := s.PlaylistByPID(ctx, pid)
	if err != nil {
		return 0, err
	}
	if p.Kind != model.PlaylistSmart {
		return s.countStaticPlaylistItems(ctx, op, pid, userPID, narrow)
	}
	if p.Rule == nil {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "smart playlist has no rule")
	}
	rule := *p.Rule

	// An unlimited, unoffset rule selects every matching row, so the narrow folds into
	// its WHERE and the whole answer is one count. A budget mode always carries a
	// positive limit, so this branch is the plain count mode by construction.
	if rule.Limit == 0 && rule.Offset == 0 {
		q := rule
		q.Where = andNodes(rule.Where, narrow)
		return s.CountItems(ctx, q, userPID)
	}
	if rule.Offset == 0 && narrow == nil {
		switch rule.LimitMode {
		case query.LimitMinutes, query.LimitMegabytes:
			// A budget fills row by row, so only the evaluation knows where it stopped.
			items, err := s.QueryItems(ctx, rule, userPID)
			if err != nil {
				return 0, err
			}
			return len(items), nil
		default:
			// CountItems ignores limit and offset, so this is the full match set.
			n, err := s.CountItems(ctx, rule, userPID)
			if err != nil {
				return 0, err
			}
			return min(n, rule.Limit), nil
		}
	}
	return s.countEvaluatedPlaylist(ctx, rule, userPID, narrow)
}

// countStaticPlaylistItems counts a static playlist's entries matching narrow, driving
// off the playlist rather than the catalog. The clause and arg order mirror Facet's:
// itemJoins, the user-state join, the dimension join, then WHERE. Verified on the real
// schema with no ANALYZE, the plan seeks playlist(pid), then playlist_item, then the
// item by rowid, with no scan anywhere.
//
// COUNT(*) rather than COUNT(DISTINCT pi.id): the result has to equal
// len(PlaylistItems(...)), which returns one row per position, so an item held twice
// counts twice. The facet counts distinct items instead; see the count triangle.
func (s *Store) countStaticPlaylistItems(ctx context.Context, op string, pid, userPID model.PID, narrow query.Node) (int, error) {
	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: items")
	}
	c, err := query.Compile(query.New(query.EntityItems).WhereNode(narrow).Build(), fm)
	if err != nil {
		return 0, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return 0, err
	}
	stmt := itemCountSelect + userJoin +
		" JOIN playlist_item pcli ON pcli.item_id = pi.id" +
		" JOIN playlist pcl ON pcl.id = pcli.playlist_id" +
		" WHERE " + andWhere("pcl.pid = ?", c.Where)
	// Args in clause order: the user join's id (its ON clause precedes WHERE), then the
	// pid, then the narrow's. The pid comes before c.Args because andWhere puts
	// pcl.pid = ? ahead of c.Where, not because the joins bind anything.
	args := make([]any, 0, len(leadArgs)+1+len(c.Args))
	args = append(args, leadArgs...)
	args = append(args, string(pid))
	args = append(args, c.Args...)
	var n int
	if err := s.rdb().QueryRowContext(ctx, stmt, args...).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
}

// countEvaluatedPlaylist evaluates a smart rule and counts how many of the rows it
// chose match narrow, which is the only correct order once a limit or an offset has
// already picked the rows. It needs no new SQL: the pids go back through CountItems on
// the existing pid field, chunked because one IN condition is capped at idBatchSize.
// The rows are distinct items, so the chunk sums cannot double-count.
func (s *Store) countEvaluatedPlaylist(ctx context.Context, rule query.Query, userPID model.PID, narrow query.Node) (int, error) {
	items, err := s.QueryItems(ctx, rule, userPID)
	if err != nil {
		return 0, err
	}
	if narrow == nil {
		return len(items), nil
	}
	pids := make([]model.PID, len(items))
	for i, it := range items {
		pids[i] = it.PID
	}
	total := 0
	err = chunkSlice(pids, idBatchSize, func(chunk []model.PID) error {
		n, err := s.CountItems(ctx, query.New(rule.Entity).
			WhereValues("pid", query.OpIn, query.Values(chunk)...).
			WhereNode(narrow).Build(), userPID)
		if err != nil {
			return err
		}
		total += n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// andNodes combines two optional query nodes with AND, tolerating a nil either side.
func andNodes(a, b query.Node) query.Node {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return query.And{Nodes: []query.Node{a, b}}
	}
}

// AddPlaylistItems appends items to a static playlist, after its last entry. A smart
// playlist rejects explicit membership edits.
func (s *Store) AddPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error {
	const op = "store.AddPlaylistItems"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil {
			return err
		}
		var n, first, last int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(position), 0), COALESCE(MAX(position), -1)
			FROM playlist_item WHERE playlist_id = ?`, plID).Scan(&n, &first, &last); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A playlist an older catalog left with gaps is renumbered before it grows.
		if first != 0 || last != n-1 {
			if _, err := compactPlaylistTx(ctx, tx, plID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := insertPlaylistEntriesTx(ctx, tx, op, plID, n, itemPIDs); err != nil {
			return err
		}
		return touchPlaylistTx(ctx, tx, plID, pid)
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
		if err := insertPlaylistEntriesTx(ctx, tx, op, plID, 0, itemPIDs); err != nil {
			return err
		}
		return touchPlaylistTx(ctx, tx, plID, pid)
	})
}

// RemovePlaylistItem removes every entry of an item from a static playlist.
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
		// the playlist (matches the by-index variant's contract).
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "item is not in the playlist")
		}
		return settlePlaylistTx(ctx, tx, plID, pid)
	})
}

// RemovePlaylistItemAt removes the entry at one index of a static playlist's listing,
// 0 being the first entry PlaylistItems returns, so a single occurrence of a duplicated
// item can be dropped without purging the rest. Every removal keeps the stored
// positions equal to the listing indexes, so each later entry's index drops by one. A
// non-empty expect makes the removal conditional: an entry holding another item, or no
// entry at all, is refused with CodeConflict, so a caller working from a listing it read
// earlier cannot remove an entry that has moved since. A negative index is CodeInvalid
// and an unguarded one past the end CodeNotFound.
func (s *Store) RemovePlaylistItemAt(ctx context.Context, pid model.PID, index int, expect model.PID) error {
	var guard []model.PID
	if expect != "" {
		guard = []model.PID{expect}
	}
	return s.removePlaylistEntries(ctx, "store.RemovePlaylistItemAt", pid, []int{index}, guard)
}

// RemovePlaylistItemsAt removes the entries at several listing indexes in one
// transaction, every index naming the listing as it stood before any of them went, so
// [3, 1] removes the fourth and the second entry. An index named twice is one entry.
// expect is nil, or holds the item each index must still list (an empty pid skips that
// check). Any refusal (RemovePlaylistItemAt's, or an expect of another length, which is
// CodeInvalid) removes nothing.
func (s *Store) RemovePlaylistItemsAt(ctx context.Context, pid model.PID, indexes []int, expect []model.PID) error {
	return s.removePlaylistEntries(ctx, "store.RemovePlaylistItemsAt", pid, indexes, expect)
}

func (s *Store) removePlaylistEntries(ctx context.Context, op string, pid model.PID, indexes []int, expect []model.PID) error {
	if expect != nil && len(expect) != len(indexes) {
		return waxerr.New(waxerr.CodeInvalid, op, "expect names one item per index")
	}
	for _, i := range indexes {
		if i < 0 {
			return waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("playlist index %d: an index is 0 or more", i))
		}
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		plID, err := s.staticPlaylistIDTx(ctx, tx, pid, op)
		if err != nil || len(indexes) == 0 {
			return err
		}
		positions, held, err := playlistEntriesTx(ctx, tx, plID, expect != nil)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		gone := make(map[int64]bool, len(indexes))
		for k, i := range indexes {
			guarded := expect != nil && expect[k] != ""
			if i >= len(positions) {
				// An entry the caller saw that is no longer there is a stale listing.
				code := waxerr.CodeNotFound
				if guarded {
					code = waxerr.CodeConflict
				}
				return waxerr.New(code, op, fmt.Sprintf("no playlist entry at index %d", i))
			}
			if guarded && held[i] != expect[k] {
				return waxerr.New(waxerr.CodeConflict, op,
					fmt.Sprintf("playlist entry %d holds %s, not %s", i, held[i], expect[k]))
			}
			gone[positions[i]] = true
		}
		for p := range gone {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM playlist_item WHERE playlist_id = ? AND position = ?", plID, p); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return settlePlaylistTx(ctx, tx, plID, pid)
	})
}

// playlistEntriesTx reads a playlist's stored positions in listing order and, when
// withItems, the item each entry holds.
func playlistEntriesTx(ctx context.Context, tx *sql.Tx, plID int64, withItems bool) ([]int64, []model.PID, error) {
	if !withItems {
		positions, err := queryInt64sTx(ctx, tx,
			"SELECT position FROM playlist_item WHERE playlist_id = ? ORDER BY position", plID)
		return positions, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT pli.position, pi.pid FROM playlist_item pli
		JOIN playable_item pi ON pi.id = pli.item_id
		WHERE pli.playlist_id = ? ORDER BY pli.position`, plID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var positions []int64
	var items []model.PID
	for rows.Next() {
		var p int64
		var pid model.PID
		if err := rows.Scan(&p, &pid); err != nil {
			return nil, nil, err
		}
		positions, items = append(positions, p), append(items, pid)
	}
	return positions, items, rows.Err()
}

// queryPlaylistRefsTx collects (id, pid) playlist pairs, draining the cursor before it
// returns so the transaction's one connection is free again.
func queryPlaylistRefsTx(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]playlistRef, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []playlistRef
	for rows.Next() {
		var p playlistRef
		if err := rows.Scan(&p.id, &p.pid); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// compactPlaylistTx renumbers a playlist's entries 0..n-1 in their current order, so a
// stored position is the entry's listing index, and reports whether any moved. The
// primary key is checked row by row, so the entries that move first go below every
// stored position and are then turned into their indexes.
func compactPlaylistTx(ctx context.Context, tx *sql.Tx, plID int64) (bool, error) {
	var base int64
	if err := tx.QueryRowContext(ctx,
		"SELECT MIN(COALESCE(MIN(position), 0), 0) - 1 FROM playlist_item WHERE playlist_id = ?", plID).Scan(&base); err != nil {
		return false, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE playlist_item SET position = ?1 - r.idx
		FROM (SELECT position AS pos, ROW_NUMBER() OVER (ORDER BY position) - 1 AS idx
		      FROM playlist_item WHERE playlist_id = ?2) AS r
		WHERE playlist_item.playlist_id = ?2 AND playlist_item.position = r.pos AND r.idx <> r.pos`, base, plID)
	if err != nil {
		return false, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return false, nil
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE playlist_item SET position = ?1 - position WHERE playlist_id = ?2 AND position <= ?1", base, plID)
	return err == nil, err
}

// playlistPositionDriftQ selects the playlists whose positions are not 0..n-1, which
// the primary key's uniqueness reduces to the smallest and the largest.
const playlistPositionDriftQ = `SELECT playlist_id FROM playlist_item GROUP BY playlist_id
	HAVING MIN(position) <> 0 OR MAX(position) <> COUNT(*) - 1`

// compactAllPlaylistsTx renumbers every playlist with drifted positions. The listings
// keep their order, so nothing is touched and no delta is emitted.
func compactAllPlaylistsTx(ctx context.Context, tx *sql.Tx) error {
	drifted, err := queryInt64sTx(ctx, tx, playlistPositionDriftQ)
	if err != nil {
		return err
	}
	for _, id := range drifted {
		if _, err := compactPlaylistTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
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
func touchPlaylistTx(ctx context.Context, tx *sql.Tx, plID int64, pid model.PID) error {
	if _, err := tx.ExecContext(ctx, "UPDATE playlist SET updated_at = ? WHERE id = ?", nowNS(), plID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.playlist", err)
	}
	return appendChange(ctx, tx, "playlist", pid, model.OpUpdate)
}

// settlePlaylistTx closes a membership change that removed entries: the positions are
// renumbered and the playlist touched.
func settlePlaylistTx(ctx context.Context, tx *sql.Tx, plID int64, pid model.PID) error {
	if _, err := compactPlaylistTx(ctx, tx, plID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.playlist", err)
	}
	return touchPlaylistTx(ctx, tx, plID, pid)
}

// entryHolders is what deleting or re-pointing items changes beyond the items
// themselves: the static playlists listing them and the users whose queue holds them.
// A cascade or a re-point changes those rows with no write of their own, so the
// holders are read before and settled after: by the delete or fold itself, or once by
// a caller that batches a loop of them (deleteItemCascade, foldItemIntoTx).
type entryHolders struct {
	playlists map[int64]model.PID // by rowid
	queues    map[model.PID]bool  // user pids
}

// addTx records the holders of the items cond selects, cond being a condition on an
// item_id column that both the playlist and the queue table carry.
func (h *entryHolders) addTx(ctx context.Context, tx *sql.Tx, cond string, args ...any) error {
	pls, err := queryPlaylistRefsTx(ctx, tx, `SELECT DISTINCT p.id, p.pid FROM playlist_item pli
		JOIN playlist p ON p.id = pli.playlist_id WHERE pli.`+cond, args...)
	if err != nil {
		return err
	}
	for _, p := range pls {
		if h.playlists == nil {
			h.playlists = map[int64]model.PID{}
		}
		h.playlists[p.id] = p.pid
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT u.pid FROM play_queue q
		JOIN user u ON u.id = q.user_id WHERE q.`+cond, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u model.PID
		if err := rows.Scan(&u); err != nil {
			return err
		}
		if h.queues == nil {
			h.queues = map[model.PID]bool{}
		}
		h.queues[u] = true
	}
	return rows.Err()
}

// settleTx renumbers and touches each playlist and emits a queue delta per user, then
// forgets them, so a second settle in the transaction writes nothing.
func (h *entryHolders) settleTx(ctx context.Context, tx *sql.Tx) error {
	for _, id := range slices.Sorted(maps.Keys(h.playlists)) {
		if err := settlePlaylistTx(ctx, tx, id, h.playlists[id]); err != nil {
			return err
		}
	}
	for _, u := range slices.Sorted(maps.Keys(h.queues)) {
		if err := appendChange(ctx, tx, "play_queue", u, model.OpUpdate); err != nil {
			return err
		}
	}
	h.playlists, h.queues = nil, nil
	return nil
}

// maxPathCandidates bounds the items a relative M3U8 entry may name: past it the entry
// names nothing, since no label could pick among so many.
const maxPathCandidates = 100

// ItemsByPlaylistPath returns the cataloged items an M3U8 entry's path names, through
// any of their files: a copy's path names its item, a later book part's path its book,
// and a cue rip's file every track carved from it. An absolute entry is matched
// against the raw path using the UNIQUE path index; that is the round-trip case for
// WaxBin exports, where display-path bytes are the stored path. A relative entry,
// common in playlists authored by other tools, matches the files whose display path
// ends with it at a separator, so "b/x.mp3" does not match "prefix/ab/x.mp3". Choosing
// among several items is the caller's; an entry that names none returns none.
func (s *Store) ItemsByPlaylistPath(ctx context.Context, p string) ([]*model.ItemView, error) {
	const op = "store.ItemsByPlaylistPath"
	// Normalize separators and fold away "." / redundant separators so a dotted entry
	// like "./Artist/Track.mp3" or "a/./b.mp3" matches the stored path.
	clean := filepath.Clean(filepath.FromSlash(p))
	items, err := s.itemsBehindFiles(ctx, op, "fx.path = ?", []byte(clean), 0)
	if err != nil || len(items) > 0 || filepath.IsAbs(clean) {
		return items, err
	}
	// A pattern past SQLite's LIKE limit names no path a file can have.
	pattern := "%" + query.LikeEscape(string(filepath.Separator)+clean)
	if len(pattern) > query.MaxLikePatternBytes {
		return nil, nil
	}
	items, err = s.itemsBehindFiles(ctx, op, `fx.display_path LIKE ? ESCAPE '\'`, pattern, maxPathCandidates+1)
	if err != nil || len(items) > maxPathCandidates {
		return nil, err
	}
	return items, nil
}

// itemsBehindFiles reads the items with an edge of any role to the files cond selects.
func (s *Store) itemsBehindFiles(ctx context.Context, op, cond string, arg any, limit int) ([]*model.ItemView, error) {
	q := itemSelect + ` WHERE pi.id IN (SELECT ie.item_id FROM item_file ie
		JOIN file fx ON fx.id = ie.file_id WHERE ` + cond + `) ORDER BY pi.id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.rdb().QueryContext(ctx, q, arg)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}
