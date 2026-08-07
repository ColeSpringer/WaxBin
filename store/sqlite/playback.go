package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// SetProgress records a user's resume position for an item. High-frequency
// progress is coalesced by the playback service before it reaches here, so this
// is called on checkpoints, not every tick. It stamps last_progress_at, which is
// what puts a checkpointed-but-never-played item on the in-progress list, and it
// never touches the star/rating change stamps.
func (s *Store) SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	_, err := s.playStateWrite(ctx, "store.SetProgress", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, position_ms, last_progress_at, updated_at) VALUES (?,?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET position_ms=excluded.position_ms,
			   last_progress_at=excluded.last_progress_at, updated_at=excluded.updated_at`,
			userID, itemID, positionMS, now, now)
		return true, err
	})
	return err
}

// MarkPlayed increments a user's play count for an item, sets it played (and
// finished when finished is true), and stamps last_played_at, last_progress_at
// and played_changed_at. It never touches the star/rating change stamps. It means
// "a play happened, now", so it takes no asOf; replaying a recorded play is
// SetPlayed's job.
//
// It stamps played_changed_at because a stamp that orders only one of the two
// writers orders nothing. Monotonically, so a SetPlayed carrying a future-skewed
// asOf cannot be regressed by the next play; the COALESCE is required because
// SQLite's MAX() returns NULL if any argument is NULL, which would wipe the stamp
// on an item's first play.
func (s *Store) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	_, err := s.playStateWrite(ctx, "store.MarkPlayed", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, played, finished, play_count, last_played_at, last_progress_at, played_changed_at, updated_at)
			 VALUES (?,?,1,?,1,?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET
			   played=1, finished=MAX(finished, excluded.finished), play_count=play_count+1,
			   last_played_at=excluded.last_played_at, last_progress_at=excluded.last_progress_at,
			   played_changed_at=MAX(COALESCE(played_changed_at, 0), excluded.played_changed_at),
			   updated_at=excluded.updated_at`,
			userID, itemID, boolInt(finished), now, now, now, now)
		return true, err
	})
	return err
}

// asOfRecorded reports the recorded time an as-of carries and whether it carries a
// usable one. A nil pointer and a zero value both mean "no recorded time": unix-ns 0
// is the epoch, never a real star/rating change time, and 0 is the not-provided
// sentinel the proxy wire (proxy.AsOf / asOfToWire) and the import seam (0 = unknown)
// already use. Collapsing 0 here keeps the direct, proxied, and imported paths
// identical, rather than letting a lone &0 stamp at the epoch on the direct path
// while the wire treats the same value as absent and stamps at server-now.
func asOfRecorded(asOf *int64) (int64, bool) {
	if asOf == nil || *asOf == 0 {
		return 0, false
	}
	return *asOf, true
}

// stampFor is the change-time stamp a star/rating mutation records: the caller's
// recorded time when one is supplied (so a replayed offline toggle or a migration
// import lands in recorded time, letting the engine order it against an out-of-band
// change), otherwise the server's current time. It is shared by the item and entity
// play-state writes so the two cannot drift.
func stampFor(asOf *int64, now int64) int64 {
	if ns, ok := asOfRecorded(asOf); ok {
		return ns
	}
	return now
}

// staleReplay reports whether a value change carrying a recorded time asOf loses to
// the change already recorded at stored: a replay whose recorded time is not newer
// than the stored change is stale and must be skipped, so a replayed offline toggle
// can never undo a later out-of-band change. A NULL stored stamp (validStored false)
// carries no ordering information, so the change applies. With no recorded time the
// caller is stamping at server-now and there is nothing to order against.
func staleReplay(asOf *int64, storedChanged int64, validStored bool) bool {
	ns, ok := asOfRecorded(asOf)
	return ok && validStored && ns <= storedChanged
}

// SetRating sets (0..100) or clears (rating nil) a user's rating for an item.
// A call that would store the value already held is a silent no-op: no write,
// no play_state delta, and the change stamp keeps its time, so an idempotent
// re-rate never masquerades as a newer change to a syncing client. A real value
// change, a clear of a set rating included, bumps rating_changed_at. It leaves
// last_progress_at alone: rating is not playback, and moving it would reorder the
// in-progress list.
//
// asOf (unix nanoseconds, nil = server now) is the recorded time of the change. When
// supplied, the stamp lands in recorded time and the engine enforces recorded-time
// last-writer-wins: a change whose asOf is not newer than the stored
// rating_changed_at is skipped as a stale replay (changed=false, no delta), so a
// replayed offline rating cannot overwrite a later one. A NULL prior stamp has no
// ordering info, so it applies.
//
// The returned bool reports whether the call changed anything, and false is exactly
// "no play_state delta was appended". All three no-op cases report false: the stored
// value is already the requested one, a clear of a rating that was never set, and a
// stale replay. A caller cannot recover this from a follow-up read: a stale replay
// lands the stamp on the value it already held, and a value-identical call leaves
// the stamp untouched. The bool is the only answer to "did this write do anything".
func (s *Store) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int, asOf *int64) (bool, error) {
	var val any
	want := sql.NullInt64{}
	if rating != nil {
		v := model.RatingBounds(*rating)
		val = v
		want = sql.NullInt64{Int64: int64(v), Valid: true}
	}
	return s.playStateWrite(ctx, "store.SetRating", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		var cur, curChanged sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT rating, rating_changed_at FROM play_state WHERE user_id = ? AND item_id = ?", userID, itemID).
			Scan(&cur, &curChanged)
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
				`INSERT INTO play_state(user_id, item_id, rating, rating_changed_at, updated_at) VALUES (?,?,?,?,?)`,
				userID, itemID, val, stamp, now)
			return true, err
		}
		if cur == want {
			return false, nil
		}
		if staleReplay(asOf, curChanged.Int64, curChanged.Valid) {
			return false, nil
		}
		// updated_at is always the server's real row-touch time; only the change
		// stamp records recorded time.
		_, err = tx.ExecContext(ctx,
			`UPDATE play_state SET rating = ?, rating_changed_at = ?, updated_at = ? WHERE user_id = ? AND item_id = ?`,
			val, stampFor(asOf, now), now, userID, itemID)
		return true, err
	})
}

// SetStar stars or unstars an item for a user, recording the star time for
// recency ordering of the starred list. A call that matches the stored state is
// a silent no-op: re-starring a starred item preserves the original starred_at
// (so "starred since" stays truthful), unstarring an unstarred one creates no
// row, and neither emits a play_state delta or bumps the change stamp. A real
// flip, unstar included, bumps starred_changed_at; starred_at goes NULL on
// unstar as before. Like SetRating it leaves last_progress_at alone, so starring
// an old half-heard item does not push it to the head of the in-progress list.
//
// asOf (unix nanoseconds, nil = server now) is the recorded time of the flip. When
// supplied, both starred_changed_at and, on a star, starred_at land in recorded time
// (recency-correct for a migration import), and the engine enforces recorded-time
// last-writer-wins: a flip whose asOf is not newer than the stored starred_changed_at
// is skipped as a stale replay (changed=false, no delta), so a replayed offline
// toggle can never resurrect an undone state. A NULL prior stamp applies.
//
// The returned bool reports whether the call changed anything, and false is exactly
// "no play_state delta was appended": a re-star of a starred item, an unstar of an
// unstarred one, and a stale replay all report false. See SetRating for why a
// follow-up state read cannot substitute for it.
func (s *Store) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool, asOf *int64) (bool, error) {
	return s.playStateWrite(ctx, "store.SetStar", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		var cur, curChanged sql.NullInt64 // starred_at (Valid mirrors the flag), starred_changed_at
		err := tx.QueryRowContext(ctx,
			"SELECT starred_at, starred_changed_at FROM play_state WHERE user_id = ? AND item_id = ?", userID, itemID).
			Scan(&cur, &curChanged)
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
				`INSERT INTO play_state(user_id, item_id, starred_at, starred_changed_at, updated_at) VALUES (?,?,?,?,?)`,
				userID, itemID, stamp, stamp, now)
			return true, err
		}
		var starredAt any
		if starred {
			starredAt = stamp
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE play_state SET starred_at = ?, starred_changed_at = ?, updated_at = ? WHERE user_id = ? AND item_id = ?`,
			starredAt, stamp, now, userID, itemID)
		return true, err
	})
}

// SetPlayed sets a user's played and finished flags for an item directly, the
// undo MarkPlayed lacks. A value-identical call is a silent no-op, and asOf works
// exactly as it does for SetStar and SetRating, including the returned bool. It
// leaves last_progress_at and last_played_at alone: un-marking a play is a state
// edit, not playback. There is no entity twin (entity_play_state has no
// played/finished columns) and no query field (a change stamp is sync plumbing).
//
// playCount: nil keeps the stored count, &0 resets it, &n sets it exactly. Without
// it an un-mark would leave played=0 beside a play_count of 1 forever, and
// play_count is indexed and orders `browse most-played`. nil alongside played=true
// raises a zero count to 1, since the flag means the item was played at least once.
//
// One hazard the star and rating setters do not have: played_changed_at moves on
// every play, stamped with the server clock, so a client whose clock trails the
// server sends an asOf older than the play it is undoing and has its un-mark
// dropped as stale. An interactive un-mark passes nil; asOf is for replaying
// recorded history.
func (s *Store) SetPlayed(ctx context.Context, userPID, itemPID model.PID,
	played, finished bool, playCount *int, asOf *int64) (bool, error) {
	const op = "store.SetPlayed"
	// Both checks live here, not in the service, so the proxied path gets them too.
	if playCount != nil && *playCount < 0 {
		return false, waxerr.New(waxerr.CodeInvalid, op, "play count cannot be negative")
	}
	// Both flags are query fields, so a finished-but-unplayed row would make
	// `played is 0` match items every UI renders as finished.
	if !played && finished {
		return false, waxerr.New(waxerr.CodeInvalid, op, "an item cannot be finished but not played")
	}
	// played means "played at least once", so it cannot be paired with an explicit
	// zero count; that row would sort as never played in `browse most-played` and is
	// unreachable through MarkPlayed. A caller zeroing the count is un-marking.
	if played && playCount != nil && *playCount == 0 {
		return false, waxerr.New(waxerr.CodeInvalid, op, "a played item cannot have a zero play count")
	}
	return s.playStateWrite(ctx, op, userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		var curPlayed, curFinished, curCount int
		var curChanged sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT played, finished, play_count, played_changed_at FROM play_state WHERE user_id = ? AND item_id = ?",
			userID, itemID).Scan(&curPlayed, &curFinished, &curCount, &curChanged)
		noRow := errors.Is(err, sql.ErrNoRows)
		if err != nil && !noRow {
			return false, err
		}
		wantCount := curCount
		if playCount != nil {
			wantCount = *playCount
		} else if played && wantCount == 0 {
			// played means "played at least once", which MarkPlayed keeps true by
			// incrementing. A caller that sets the flag without naming a count gets the
			// smallest count consistent with it, rather than a played row that
			// `browse most-played` sorts as never played.
			wantCount = 1
		}
		if noRow {
			// Clearing flags on an item with no state creates no row at all.
			if !played && !finished && wantCount == 0 {
				return false, nil
			}
			stamp := stampFor(asOf, now)
			_, err := tx.ExecContext(ctx,
				`INSERT INTO play_state(user_id, item_id, played, finished, play_count, played_changed_at, updated_at)
				 VALUES (?,?,?,?,?,?,?)`,
				userID, itemID, boolInt(played), boolInt(finished), wantCount, stamp, now)
			return true, err
		}
		flagsChanged := curPlayed != boolInt(played) || curFinished != boolInt(finished)
		if !flagsChanged && curCount == wantCount {
			return false, nil
		}
		if staleReplay(asOf, curChanged.Int64, curChanged.Valid) {
			return false, nil
		}
		// The stamp belongs to played/finished, so a count-only change leaves it
		// alone: bumping it would let a count reset outrank a genuine flag change
		// recorded earlier. When it does move it never moves backwards, for the reason
		// MarkPlayed spells out - an interactive un-mark stamping at server-now must
		// not undercut a future-skewed stamp a replay already stored.
		stamp := nullableInt64(curChanged)
		if flagsChanged {
			if s := stampFor(asOf, now); !curChanged.Valid || s > curChanged.Int64 {
				stamp = s
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE play_state SET played = ?, finished = ?, play_count = ?, played_changed_at = ?, updated_at = ?
			 WHERE user_id = ? AND item_id = ?`,
			boolInt(played), boolInt(finished), wantCount, stamp, now, userID, itemID)
		return true, err
	})
}

// nullableInt64 renders a NullInt64 as a query argument, so an unset column stays
// NULL rather than being written as 0.
func nullableInt64(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// playStateWrite resolves the user and item, runs the mutation, and emits the
// play_state delta - the shared envelope for every per-user playback mutation.
// A mutation reporting changed=false wrote nothing and stays silent: no delta is
// appended, aligning value-identical star/rating calls with the repo's
// silent-no-op convention. It returns that same bool so a caller whose write can
// legitimately do nothing (SetStar, SetRating) can report it; a failed write
// returns false with the error.
//
// The delta itself is not that answer, which is the obvious objection and why it is
// worth stating. A consumer already tailing ChangesSince for catalog sync cannot
// reuse it for play state: model.Change carries {Seq, TS, EntityType, EntityPID, Op}
// with no user dimension, and for a play_state row EntityPID is the item pid. So a
// per-user consumer sees "item X's play state changed" and cannot tell whose.
// Routing it would mean reading every user's state for that item on every row and
// diffing, which costs more than the redundant event, and it would wake user B
// because user A starred a shared track. The bool answers per call, for the caller
// that made it.
//
// Only the star and rating mutations expose it, and the asymmetry is deliberate.
// SetProgress and MarkPlayed always write (an unconditional upsert, and a play_count
// increment), so they hardcode changed=true and discard it here; returning a
// constant would invite a branch no call can ever take. A caller that wants
// applied-or-skipped for a checkpoint has the position stamp it just sent to compare
// against, and one gating a play already reads the state first.
func (s *Store) playStateWrite(ctx context.Context, op string, userPID, itemPID model.PID, mut func(context.Context, *sql.Tx, int64, int64, int64) (bool, error)) (bool, error) {
	var changed bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		changed, err = mut(ctx, tx, userID, itemID, nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !changed {
			return nil
		}
		return appendChange(ctx, tx, "play_state", itemPID, model.OpUpdate)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// PlayStateFor returns a user's playback state for an item. A user who has never
// touched the item gets a zero-valued state (not an error), so callers do not
// special-case "no row yet".
func (s *Store) PlayStateFor(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error) {
	const op = "store.PlayStateFor"
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	itemID, err := itemIDByPIDRead(ctx, s.read, itemPID, op)
	if err != nil {
		return nil, err
	}
	st := &model.PlayState{UserPID: userPID, ItemPID: itemPID}
	var played, finished int
	var rating, starredAt, lastPlayed, lastProgress, ratingChanged, starredChanged, playedChanged, updatedAt sql.NullInt64
	err = s.read.QueryRowContext(ctx,
		`SELECT position_ms, played, finished, play_count, rating, starred_at, last_played_at,
		        last_progress_at, rating_changed_at, starred_changed_at, played_changed_at, updated_at
		 FROM play_state WHERE user_id = ? AND item_id = ?`, userID, itemID).
		Scan(&st.PositionMS, &played, &finished, &st.PlayCount, &rating, &starredAt, &lastPlayed,
			&lastProgress, &ratingChanged, &starredChanged, &playedChanged, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	st.Played, st.Finished = played == 1, finished == 1
	st.Rating, st.HasRating = int(rating.Int64), rating.Valid
	st.Starred, st.StarredAt = starredAt.Valid, starredAt.Int64
	st.LastPlayedAt, st.UpdatedAt = lastPlayed.Int64, updatedAt.Int64
	st.LastProgressAt = lastProgress.Int64
	st.RatingChangedAt, st.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
	st.PlayedChangedAt = playedChanged.Int64
	return st, nil
}

// PlayStatesForItems returns every user's playback state for each of the given
// items, keyed by item pid, each item's states ordered by user pid. Items no
// user has touched, and unknown pids, are simply absent from the map. The
// lookup is chunked like ItemsByPIDs to stay under the bound-parameter limit,
// with the same caveat: a batch spanning chunks is not an atomic snapshot.
// Play state is the "is anyone using this" signal a multi-user consumer checks
// before dropping an item; sessions are not consulted because a crashed client
// leaves its session open forever.
func (s *Store) PlayStatesForItems(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.PlayState, error) {
	const op = "store.PlayStatesForItems"
	if len(itemPIDs) == 0 {
		return nil, nil
	}
	unique := uniquePIDs(itemPIDs)
	out := make(map[model.PID][]model.PlayState)
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		args := make([]any, len(chunk))
		for i, pid := range chunk {
			args[i] = string(pid)
		}
		// Each item's rows land wholly inside its own chunk, so the per-item user
		// order below holds across the whole batch.
		rows, err := s.read.QueryContext(ctx,
			`SELECT u.pid, pi.pid, ps.position_ms, ps.played, ps.finished, ps.play_count,
			        ps.rating, ps.starred_at, ps.last_played_at, ps.last_progress_at,
			        ps.rating_changed_at, ps.starred_changed_at, ps.played_changed_at, ps.updated_at
			 FROM play_state ps
			 JOIN user u ON u.id = ps.user_id
			 JOIN playable_item pi ON pi.id = ps.item_id
			 WHERE pi.pid IN `+placeholders(len(chunk))+`
			 ORDER BY pi.pid, u.pid`, args...)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var ps model.PlayState
			var userPID, itemPID string
			var played, finished int
			var rating, starredAt, lastPlayed, lastProgress, ratingChanged, starredChanged, playedChanged sql.NullInt64
			if err := rows.Scan(&userPID, &itemPID, &ps.PositionMS, &played, &finished, &ps.PlayCount,
				&rating, &starredAt, &lastPlayed, &lastProgress, &ratingChanged, &starredChanged,
				&playedChanged, &ps.UpdatedAt); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			ps.UserPID, ps.ItemPID = model.PID(userPID), model.PID(itemPID)
			ps.Played, ps.Finished = played == 1, finished == 1
			ps.Rating, ps.HasRating = int(rating.Int64), rating.Valid
			ps.Starred, ps.StarredAt = starredAt.Valid, starredAt.Int64
			ps.LastPlayedAt, ps.LastProgressAt = lastPlayed.Int64, lastProgress.Int64
			ps.RatingChangedAt, ps.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
			ps.PlayedChangedAt = playedChanged.Int64
			out[ps.ItemPID] = append(out[ps.ItemPID], ps)
		}
		if err := rows.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddBookmark records a labeled position within an item for a user.
func (s *Store) AddBookmark(ctx context.Context, userPID, itemPID model.PID, positionMS int64, label string) (model.PID, error) {
	const op = "store.AddBookmark"
	pid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO bookmark(pid, user_id, item_id, position_ms, label, created_at) VALUES (?,?,?,?,?,?)",
			string(pid), userID, itemID, positionMS, label, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "bookmark", pid, model.OpCreate)
	})
	if err != nil {
		return "", err
	}
	return pid, nil
}

// Bookmarks lists a user's bookmarks for an item, earliest position first.
func (s *Store) Bookmarks(ctx context.Context, userPID, itemPID model.PID) ([]model.Bookmark, error) {
	const op = "store.Bookmarks"
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	itemID, err := itemIDByPIDRead(ctx, s.read, itemPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT pid, position_ms, label, created_at FROM bookmark WHERE user_id=? AND item_id=? ORDER BY position_ms",
		userID, itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.Bookmark
	for rows.Next() {
		b := model.Bookmark{ItemPID: itemPID}
		if err := rows.Scan(&b.PID, &b.PositionMS, &b.Label, &b.CreatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBookmark removes a bookmark by its pid.
func (s *Store) DeleteBookmark(ctx context.Context, bookmarkPID model.PID) error {
	const op = "store.DeleteBookmark"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "DELETE FROM bookmark WHERE pid = ?", string(bookmarkPID))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no such bookmark: "+string(bookmarkPID))
		}
		return appendChange(ctx, tx, "bookmark", bookmarkPID, model.OpDelete)
	})
}

// SetQueue replaces a user's persistent play queue with the given item order.
// Unknown item pids are an error so a queue never silently drops entries.
func (s *Store) SetQueue(ctx context.Context, userPID model.PID, itemPIDs []model.PID) error {
	const op = "store.SetQueue"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM play_queue WHERE user_id = ?", userID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for pos, pid := range itemPIDs {
			itemID, err := itemIDByPID(ctx, tx, pid, op)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO play_queue(user_id, position, item_id) VALUES (?,?,?)", userID, pos, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "play_queue", userPID, model.OpUpdate)
	})
}

// Queue returns a user's play queue in order as item views.
func (s *Store) Queue(ctx context.Context, userPID model.PID) ([]*model.ItemView, error) {
	const op = "store.Queue"
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+" JOIN play_queue q ON q.item_id = pi.id WHERE q.user_id = ? ORDER BY q.position", userID)
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

// StartSession opens a play_session and returns its pid; EndSession closes it
// with the elapsed play time. Stats are built from session history.
func (s *Store) StartSession(ctx context.Context, userPID, itemPID model.PID, client string) (model.PID, error) {
	const op = "store.StartSession"
	pid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO play_session(pid, user_id, item_id, started_at, client) VALUES (?,?,?,?,?)",
			string(pid), userID, itemID, nowNS(), client); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return pid, nil
}

// EndSession closes a session with the milliseconds played.
func (s *Store) EndSession(ctx context.Context, sessionPID model.PID, msPlayed int64) error {
	const op = "store.EndSession"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			"UPDATE play_session SET ended_at = ?, ms_played = ? WHERE pid = ?", nowNS(), msPlayed, string(sessionPID))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no such session: "+string(sessionPID))
		}
		return nil
	})
}

// itemIDByPIDRead resolves an item pid to its rowid on a read connection.
func itemIDByPIDRead(ctx context.Context, q queryer, pid model.PID, op string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}
