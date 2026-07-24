package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Per-user star and rating scoped to a catalog entity (artist/release_group/album/
// genre), the entity-scoped twin of the item play_state writes in playback.go. They
// share the recorded-time helpers (stampFor/staleReplay) so the item and entity paths
// order a replayed toggle the same way, and their own envelope (entityPlayStateWrite)
// so a value-identical re-write stays change-log silent, exactly as playStateWrite does
// for items. The vocabulary is model.MergeEntity, not read.EntityKind: it is the exact
// star-able entity set, it is what entity edit already speaks, and it lives in model so
// model.EntityPlayState can carry it without a model->read import cycle. Series is
// excluded (not mergeable, no consumer); widen if one appears.

// entityPlayStateWrite resolves the user and the polymorphic entity, runs the mutation,
// and emits the delta only when it reports a real change. It is the entity twin of
// playStateWrite: that envelope keys on a playable_item and emits a "play_state" delta;
// this one keys on a catalog entity resolved through its own table. A value-identical
// re-import (getStarred2 re-sends every star) reports changed=false and stays change-log
// silent instead of writing a spurious delta per unchanged row. The entity rowid is
// resolved through the tx handle (entityIDByPID), never the read pool, because the whole
// mutation runs inside a single writer transaction.
//
// The delta is emitted under the synthetic "entity_play_state" type, not the entity's own
// kind, the exact analogue of the item path's "play_state". Per-user star/rating is
// private state, so it must stay out of the shared entity-metadata delta stream a merge or
// an entity edit writes (both under "album"/"artist"/...); a consumer of ChangesSince must
// be able to tell "someone privately starred this album" from "the album's shared metadata
// changed", or a star would trigger a shared re-export. The tradeoff is that the delta
// carries the entity pid but not its kind; entity pids are globally unique, so a consumer
// can still resolve the target (and to read the state back it holds the kind already, or
// resolves the pid, then calls EntityPlayState).
func (s *Store) entityPlayStateWrite(ctx context.Context, op string, userPID model.PID, kind model.MergeEntity, entityPID model.PID, mut func(context.Context, *sql.Tx, int64, int64, int64) (bool, error)) error {
	if !kind.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(kind))
	}
	table := string(kind)
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		entityID, err := entityIDByPID(ctx, tx, table, entityPID, op)
		if err != nil {
			return err
		}
		changed, err := mut(ctx, tx, userID, entityID, nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !changed {
			return nil
		}
		return appendChange(ctx, tx, "entity_play_state", entityPID, model.OpUpdate)
	})
}

// SetEntityStar stars or unstars a catalog entity for a user, recording the star time
// for recency ordering of the starred list. Like item SetStar, a call that matches the
// stored state is a silent no-op: re-starring a starred entity preserves the original
// starred_at (so a getStarred2 re-import cannot advance its position, an intended limit),
// unstarring an unstarred one creates no row, and neither emits a delta or bumps the
// stamp. A real flip, unstar included, bumps starred_changed_at; starred_at goes NULL on
// unstar.
//
// asOf (unix nanoseconds, nil = server now) is the recorded flip time. When supplied,
// both starred_changed_at and, on a star, starred_at land in recorded time
// (recency-correct for a migration import), and the engine enforces recorded-time
// last-writer-wins: a flip whose asOf is not newer than the stored starred_changed_at is
// skipped as a stale replay. A NULL prior stamp has no ordering info, so it applies.
func (s *Store) SetEntityStar(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, starred bool, asOf *int64) error {
	table := string(kind)
	return s.entityPlayStateWrite(ctx, "store.SetEntityStar", userPID, kind, entityPID, func(ctx context.Context, tx *sql.Tx, userID, entityID, now int64) (bool, error) {
		var cur, curChanged sql.NullInt64 // starred_at (Valid mirrors the flag), starred_changed_at
		err := tx.QueryRowContext(ctx,
			"SELECT starred_at, starred_changed_at FROM entity_play_state WHERE user_id = ? AND entity_type = ? AND entity_id = ?",
			userID, table, entityID).Scan(&cur, &curChanged)
		noRow := errors.Is(err, sql.ErrNoRows)
		if err != nil && !noRow {
			return false, err
		}
		if cur.Valid == starred { // covers no-row + unstar: cur is zero-valued
			return false, nil
		}
		if staleReplay(asOf, curChanged.Int64, curChanged.Valid) {
			return false, nil
		}
		stamp := stampFor(asOf, now)
		if noRow {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO entity_play_state(user_id, entity_type, entity_id, starred_at, starred_changed_at, updated_at) VALUES (?,?,?,?,?,?)`,
				userID, table, entityID, stamp, stamp, now)
			return true, err
		}
		var starredAt any
		if starred {
			starredAt = stamp
		}
		// updated_at is always the server's real row-touch time; only the change stamp
		// records recorded time.
		_, err = tx.ExecContext(ctx,
			`UPDATE entity_play_state SET starred_at = ?, starred_changed_at = ?, updated_at = ? WHERE user_id = ? AND entity_type = ? AND entity_id = ?`,
			starredAt, stamp, now, userID, table, entityID)
		return true, err
	})
}

// SetEntityRating sets (0..100) or clears (rating nil) a user's rating for a catalog
// entity. Like item SetRating, storing the value already held is a silent no-op that
// keeps the stamp, and a real change (a clear of a set rating included) bumps
// rating_changed_at. asOf carries the recorded change time and enforces recorded-time
// last-writer-wins; see SetEntityStar.
func (s *Store) SetEntityRating(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, rating *int, asOf *int64) error {
	table := string(kind)
	var val any
	want := sql.NullInt64{}
	if rating != nil {
		v := model.RatingBounds(*rating)
		val = v
		want = sql.NullInt64{Int64: int64(v), Valid: true}
	}
	return s.entityPlayStateWrite(ctx, "store.SetEntityRating", userPID, kind, entityPID, func(ctx context.Context, tx *sql.Tx, userID, entityID, now int64) (bool, error) {
		var cur, curChanged sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT rating, rating_changed_at FROM entity_play_state WHERE user_id = ? AND entity_type = ? AND entity_id = ?",
			userID, table, entityID).Scan(&cur, &curChanged)
		noRow := errors.Is(err, sql.ErrNoRows)
		if err != nil && !noRow {
			return false, err
		}
		if noRow {
			// Clearing a rating that was never set creates no row at all.
			if rating == nil {
				return false, nil
			}
			stamp := stampFor(asOf, now)
			_, err := tx.ExecContext(ctx,
				`INSERT INTO entity_play_state(user_id, entity_type, entity_id, rating, rating_changed_at, updated_at) VALUES (?,?,?,?,?,?)`,
				userID, table, entityID, val, stamp, now)
			return true, err
		}
		if cur == want {
			return false, nil
		}
		if staleReplay(asOf, curChanged.Int64, curChanged.Valid) {
			return false, nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE entity_play_state SET rating = ?, rating_changed_at = ?, updated_at = ? WHERE user_id = ? AND entity_type = ? AND entity_id = ?`,
			val, stampFor(asOf, now), now, userID, table, entityID)
		return true, err
	})
}

// EntityPlayState returns a user's star/rating state for one catalog entity. It resolves
// the user and the entity first, so an unknown user or an unknown entity pid is
// CodeNotFound (matching PlayStateFor, which resolves the item, and EntityCuration, the
// sibling that resolves the entity); only when the entity exists but the user never
// starred or rated it does it return a zero-valued state, not an error. Keying the read on
// the resolved rowid keeps those two "no row" cases distinct, so a typo'd pid surfaces as
// a not-found rather than a confident "not starred". The entity_type filter stays in the
// read because a rowid is not unique across the entity tables.
func (s *Store) EntityPlayState(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID) (*model.EntityPlayState, error) {
	const op = "store.EntityPlayState"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(kind))
	}
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	table := string(kind) // a validated whitelist value, never user text
	entityID, err := entityIDByPID(ctx, s.read, table, entityPID, op)
	if err != nil {
		return nil, err
	}
	st := &model.EntityPlayState{Kind: kind, UserPID: userPID, EntityPID: entityPID}
	var rating, starredAt, ratingChanged, starredChanged, updatedAt sql.NullInt64
	err = s.read.QueryRowContext(ctx,
		`SELECT rating, starred_at, rating_changed_at, starred_changed_at, updated_at
		 FROM entity_play_state WHERE user_id = ? AND entity_type = ? AND entity_id = ?`,
		userID, table, entityID).
		Scan(&rating, &starredAt, &ratingChanged, &starredChanged, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	st.Rating, st.HasRating = int(rating.Int64), rating.Valid
	st.Starred, st.StarredAt = starredAt.Valid, starredAt.Int64
	st.RatingChangedAt, st.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
	st.UpdatedAt = updatedAt.Int64
	return st, nil
}

// StarredEntities lists a user's starred entities of one kind, most-recently-starred
// first (starred_at DESC). It carries the change stamps a sync consumer needs and is the
// getStarred2 read-back primitive: pair it with EntityByPIDs to hydrate display names in
// one batch. As in EntityPlayState the kind's own table is joined to project the public
// pid, and the eps.entity_type filter scopes the join to this kind (a rowid is not unique
// across the entity tables).
func (s *Store) StarredEntities(ctx context.Context, userPID model.PID, kind model.MergeEntity) ([]model.EntityPlayState, error) {
	const op = "store.StarredEntities"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(kind))
	}
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	table := string(kind) // a validated whitelist value, never user text
	rows, err := s.read.QueryContext(ctx,
		`SELECT e.pid, eps.rating, eps.starred_at, eps.rating_changed_at, eps.starred_changed_at, eps.updated_at
		 FROM entity_play_state eps JOIN `+table+` e ON e.id = eps.entity_id
		 WHERE eps.user_id = ? AND eps.entity_type = ? AND eps.starred_at IS NOT NULL
		 ORDER BY eps.starred_at DESC`, userID, table)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EntityPlayState
	for rows.Next() {
		st := model.EntityPlayState{Kind: kind, UserPID: userPID, Starred: true}
		var pid string
		var rating, starredAt, ratingChanged, starredChanged, updatedAt sql.NullInt64
		if err := rows.Scan(&pid, &rating, &starredAt, &ratingChanged, &starredChanged, &updatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		st.EntityPID = model.PID(pid)
		st.Rating, st.HasRating = int(rating.Int64), rating.Valid
		st.StarredAt = starredAt.Int64
		st.RatingChangedAt, st.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
		st.UpdatedAt = updatedAt.Int64
		out = append(out, st)
	}
	return out, rows.Err()
}
