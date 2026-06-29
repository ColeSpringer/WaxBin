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
// is called on checkpoints, not every tick.
func (s *Store) SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	return s.playStateWrite(ctx, "store.SetProgress", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, position_ms, updated_at) VALUES (?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET position_ms=excluded.position_ms, updated_at=excluded.updated_at`,
			userID, itemID, positionMS, now)
		return err
	})
}

// MarkPlayed increments a user's play count for an item, sets it played (and
// finished when finished is true), and stamps last_played_at.
func (s *Store) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	return s.playStateWrite(ctx, "store.MarkPlayed", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, played, finished, play_count, last_played_at, updated_at)
			 VALUES (?,?,1,?,1,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET
			   played=1, finished=MAX(finished, excluded.finished), play_count=play_count+1,
			   last_played_at=excluded.last_played_at, updated_at=excluded.updated_at`,
			userID, itemID, boolInt(finished), now, now)
		return err
	})
}

// SetRating sets (0..100) or clears (rating nil) a user's rating for an item.
func (s *Store) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error {
	var val any
	if rating != nil {
		val = model.RatingBounds(*rating)
	}
	return s.playStateWrite(ctx, "store.SetRating", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, rating, updated_at) VALUES (?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET rating=excluded.rating, updated_at=excluded.updated_at`,
			userID, itemID, val, now)
		return err
	})
}

// SetStar stars or unstars an item for a user, recording the star time for
// recency ordering of the starred list.
func (s *Store) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error {
	return s.playStateWrite(ctx, "store.SetStar", userPID, itemPID, func(ctx context.Context, tx *sql.Tx, userID, itemID, now int64) error {
		var starredAt any
		if starred {
			starredAt = now
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO play_state(user_id, item_id, starred_at, updated_at) VALUES (?,?,?,?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET starred_at=excluded.starred_at, updated_at=excluded.updated_at`,
			userID, itemID, starredAt, now)
		return err
	})
}

// playStateWrite resolves the user and item, runs the mutation, and emits the
// play_state delta - the shared envelope for every per-user playback mutation.
func (s *Store) playStateWrite(ctx context.Context, op string, userPID, itemPID model.PID, mut func(context.Context, *sql.Tx, int64, int64, int64) error) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		userID, err := userIDByPID(ctx, tx, userPID, op)
		if err != nil {
			return err
		}
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if err := mut(ctx, tx, userID, itemID, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
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
	var rating, starredAt, lastPlayed, updatedAt sql.NullInt64
	err = s.read.QueryRowContext(ctx,
		`SELECT position_ms, played, finished, play_count, rating, starred_at, last_played_at, updated_at
		 FROM play_state WHERE user_id = ? AND item_id = ?`, userID, itemID).
		Scan(&st.PositionMS, &played, &finished, &st.PlayCount, &rating, &starredAt, &lastPlayed, &updatedAt)
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
	return st, nil
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
