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
// is called on checkpoints, not every tick. It never touches the star/rating
// change stamps.
func (s *Store) SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	return s.playStateWrite(ctx, "store.SetProgress", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, position_ms, updated_at) VALUES (?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET position_ms=excluded.position_ms, updated_at=excluded.updated_at`,
			userID, itemID, positionMS, now)
		return true, err
	})
}

// MarkPlayed increments a user's play count for an item, sets it played (and
// finished when finished is true), and stamps last_played_at. It never touches
// the star/rating change stamps.
func (s *Store) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	return s.playStateWrite(ctx, "store.MarkPlayed", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, played, finished, play_count, last_played_at, updated_at)
			 VALUES (?,?,1,?,1,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET
			   played=1, finished=MAX(finished, excluded.finished), play_count=play_count+1,
			   last_played_at=excluded.last_played_at, updated_at=excluded.updated_at`,
			userID, itemID, boolInt(finished), now, now)
		return true, err
	})
}

// SetRating sets (0..100) or clears (rating nil) a user's rating for an item.
// A call that would store the value already held is a silent no-op: no write,
// no play_state delta, and the change stamp keeps its time, so an idempotent
// re-rate never masquerades as a newer change to a syncing client. A real value
// change, a clear of a set rating included, bumps rating_changed_at.
func (s *Store) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error {
	var val any
	want := sql.NullInt64{}
	if rating != nil {
		v := model.RatingBounds(*rating)
		val = v
		want = sql.NullInt64{Int64: int64(v), Valid: true}
	}
	return s.playStateWrite(ctx, "store.SetRating", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		var cur sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT rating FROM play_state WHERE user_id = ? AND item_id = ?", userID, itemID).Scan(&cur)
		noRow := errors.Is(err, sql.ErrNoRows)
		if err != nil && !noRow {
			return false, err
		}
		if noRow {
			// Clearing a rating that was never set creates no row at all.
			if rating == nil {
				return false, nil
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO play_state(user_id, item_id, rating, rating_changed_at, updated_at) VALUES (?,?,?,?,?)`,
				userID, itemID, val, now, now)
			return true, err
		}
		if cur == want {
			return false, nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE play_state SET rating = ?, rating_changed_at = ?, updated_at = ? WHERE user_id = ? AND item_id = ?`,
			val, now, now, userID, itemID)
		return true, err
	})
}

// SetStar stars or unstars an item for a user, recording the star time for
// recency ordering of the starred list. A call that matches the stored state is
// a silent no-op: re-starring a starred item preserves the original starred_at
// (so "starred since" stays truthful), unstarring an unstarred one creates no
// row, and neither emits a play_state delta or bumps the change stamp. A real
// flip, unstar included, bumps starred_changed_at; starred_at goes NULL on
// unstar as before.
func (s *Store) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error {
	return s.playStateWrite(ctx, "store.SetStar", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) (bool, error) {
		var cur sql.NullInt64 // starred_at; Valid mirrors the starred flag
		err := tx.QueryRowContext(ctx,
			"SELECT starred_at FROM play_state WHERE user_id = ? AND item_id = ?", userID, itemID).Scan(&cur)
		noRow := errors.Is(err, sql.ErrNoRows)
		if err != nil && !noRow {
			return false, err
		}
		if cur.Valid == starred { // covers no-row + unstar: cur is zero-valued
			return false, nil
		}
		if noRow {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO play_state(user_id, item_id, starred_at, starred_changed_at, updated_at) VALUES (?,?,?,?,?)`,
				userID, itemID, now, now, now)
			return true, err
		}
		var starredAt any
		if starred {
			starredAt = now
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE play_state SET starred_at = ?, starred_changed_at = ?, updated_at = ? WHERE user_id = ? AND item_id = ?`,
			starredAt, now, now, userID, itemID)
		return true, err
	})
}

// playStateWrite resolves the user and item, runs the mutation, and emits the
// play_state delta - the shared envelope for every per-user playback mutation.
// A mutation reporting changed=false wrote nothing and stays silent: no delta is
// appended, aligning value-identical star/rating calls with the repo's
// silent-no-op convention.
func (s *Store) playStateWrite(ctx context.Context, op string, userPID, itemPID model.PID, mut func(context.Context, *sql.Tx, int64, int64, int64) (bool, error)) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		changed, err := mut(ctx, tx, userID, itemID, nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !changed {
			return nil
		}
		return appendChange(ctx, tx, "play_state", itemPID, model.OpUpdate)
	})
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
	var rating, starredAt, lastPlayed, ratingChanged, starredChanged, updatedAt sql.NullInt64
	err = s.read.QueryRowContext(ctx,
		`SELECT position_ms, played, finished, play_count, rating, starred_at, last_played_at,
		        rating_changed_at, starred_changed_at, updated_at
		 FROM play_state WHERE user_id = ? AND item_id = ?`, userID, itemID).
		Scan(&st.PositionMS, &played, &finished, &st.PlayCount, &rating, &starredAt, &lastPlayed,
			&ratingChanged, &starredChanged, &updatedAt)
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
	st.RatingChangedAt, st.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
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
			        ps.rating, ps.starred_at, ps.last_played_at, ps.rating_changed_at, ps.starred_changed_at, ps.updated_at
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
			var rating, starredAt, lastPlayed, ratingChanged, starredChanged sql.NullInt64
			if err := rows.Scan(&userPID, &itemPID, &ps.PositionMS, &played, &finished, &ps.PlayCount,
				&rating, &starredAt, &lastPlayed, &ratingChanged, &starredChanged, &ps.UpdatedAt); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			ps.UserPID, ps.ItemPID = model.PID(userPID), model.PID(itemPID)
			ps.Played, ps.Finished = played == 1, finished == 1
			ps.Rating, ps.HasRating = int(rating.Int64), rating.Valid
			ps.Starred, ps.StarredAt = starredAt.Valid, starredAt.Int64
			ps.LastPlayedAt = lastPlayed.Int64
			ps.RatingChangedAt, ps.StarredChangedAt = ratingChanged.Int64, starredChanged.Int64
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
