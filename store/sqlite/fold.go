package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
)

// departure is a file a put's link takes from the items it backed: the edge it held on
// each and which of them are books, read before the link, and the item it joins, which
// an item it leaves with no file folds into (0 for none).
type departure struct {
	file  int64
	into  int64
	lost  map[int64]lostEdge
	books map[int64]bool
}

// departingTx reads the edges a file holds before a link moves it to into.
func departingTx(ctx context.Context, tx *sql.Tx, fileID int64, essence string, into int64) (departure, error) {
	lost, books, err := itemLostEdgesTx(ctx, tx, fileID, essence)
	return departure{file: fileID, into: into, lost: lost, books: books}, err
}

// foldItemIntoTx folds an item a put left with no file into the item the file joined (a
// track whose file became a part of a book, a book whose one part joined another, a
// track whose file became a copy of another recording), so what was done with it outlives
// its pid. Each user's play state merges into the survivor's (foldPlayStateTx), and the
// bookmarks, sessions, queue and playlist entries and acquisition move across: a queue or
// playlist that already holds the survivor drops the loser's entries instead, so a book
// made of three tracks is listed once. A bookmark moves by the offset of the file's part
// when the survivor is a book. Only tracks and books fold, and the caller deletes the
// loser afterwards.
func foldItemIntoTx(ctx context.Context, tx *sql.Tx, loser, survivor, fileID int64) error {
	var lkind, skind string
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT kind FROM playable_item WHERE id = ?),
		(SELECT kind FROM playable_item WHERE id = ?)`, loser, survivor).Scan(&lkind, &skind); err != nil {
		return err
	}
	if !folds(lkind) || !folds(skind) {
		return nil
	}
	book := skind == string(model.KindBook)
	var offset int64
	if book {
		parts, err := bookPartsQ(ctx, tx, survivor)
		if err != nil {
			return err
		}
		for _, p := range parts {
			if p.fileID == fileID {
				break
			}
			offset += p.DurationMS
		}
	}
	if err := foldPlayStateTx(ctx, tx, loser, survivor, offset, book); err != nil {
		return err
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{"UPDATE bookmark SET item_id = ?, position_ms = position_ms + ? WHERE item_id = ?", []any{survivor, offset, loser}},
		{"UPDATE play_session SET item_id = ? WHERE item_id = ?", []any{survivor, loser}},
		{`DELETE FROM play_queue WHERE item_id = ? AND EXISTS (SELECT 1 FROM play_queue s
			WHERE s.user_id = play_queue.user_id AND s.item_id = ?)`, []any{loser, survivor}},
		{"UPDATE play_queue SET item_id = ? WHERE item_id = ?", []any{survivor, loser}},
		{`DELETE FROM playlist_item WHERE item_id = ? AND EXISTS (SELECT 1 FROM playlist_item s
			WHERE s.playlist_id = playlist_item.playlist_id AND s.item_id = ?)`, []any{loser, survivor}},
		{"UPDATE playlist_item SET item_id = ? WHERE item_id = ?", []any{survivor, loser}},
		{"UPDATE OR IGNORE acquisition SET item_id = ? WHERE item_id = ?", []any{survivor, loser}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}

func folds(kind string) bool {
	return kind == string(model.KindTrack) || kind == string(model.KindBook)
}

// playRow is a play_state row less its keys.
type playRow struct {
	position, played, finished, count int64
	rating, starredAt, lastPlayed     sql.NullInt64
	lastProgress, ratingChanged       sql.NullInt64
	starredChanged, playedChanged     sql.NullInt64
	updated                           int64
}

const playRowCols = `position_ms, played, finished, play_count, rating, starred_at, last_played_at,
	last_progress_at, rating_changed_at, starred_changed_at, played_changed_at, updated_at`

func (r *playRow) fields() []any {
	return []any{&r.position, &r.played, &r.finished, &r.count, &r.rating, &r.starredAt, &r.lastPlayed,
		&r.lastProgress, &r.ratingChanged, &r.starredChanged, &r.playedChanged, &r.updated}
}

// foldPlayStateTx merges each user's play state on loser into theirs on survivor. Plays
// add up and played holds if either was played; the star and the rating come across with
// their change stamps only where the survivor has none, the way an entity merge folds
// them (repointEntityPlayState); the resume position does too, moved by offset, the
// file's place in a survivor that is a book. A part finished is not its book finished, so
// finished comes across only into a track.
func foldPlayStateTx(ctx context.Context, tx *sql.Tx, loser, survivor, offset int64, book bool) error {
	rows, err := tx.QueryContext(ctx, "SELECT user_id, "+playRowCols+" FROM play_state WHERE item_id = ?", loser)
	if err != nil {
		return err
	}
	type userRow struct {
		user int64
		row  playRow
	}
	var los []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(append([]any{&u.user}, u.row.fields()...)...); err != nil {
			rows.Close()
			return err
		}
		los = append(los, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range los {
		lo := u.row
		if lo.lastProgress.Valid {
			lo.position += offset
		}
		if book {
			lo.finished = 0
		}
		var m playRow
		err := tx.QueryRowContext(ctx, "SELECT "+playRowCols+" FROM play_state WHERE user_id = ? AND item_id = ?",
			u.user, survivor).Scan(m.fields()...)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			m = lo
		case err != nil:
			return err
		default:
			m.count += lo.count
			m.played = max(m.played, lo.played)
			m.finished = max(m.finished, lo.finished)
			m.playedChanged = laterStamp(m.playedChanged, lo.playedChanged)
			m.lastPlayed = laterStamp(m.lastPlayed, lo.lastPlayed)
			m.updated = max(m.updated, lo.updated)
			if !m.rating.Valid && !m.ratingChanged.Valid {
				m.rating, m.ratingChanged = lo.rating, lo.ratingChanged
			}
			if !m.starredAt.Valid && !m.starredChanged.Valid {
				m.starredAt, m.starredChanged = lo.starredAt, lo.starredChanged
			}
			if !m.lastProgress.Valid {
				m.position, m.lastProgress = lo.position, lo.lastProgress
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO play_state(user_id, item_id, `+playRowCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, item_id) DO UPDATE SET position_ms = excluded.position_ms, played = excluded.played,
				finished = excluded.finished, play_count = excluded.play_count, rating = excluded.rating,
				starred_at = excluded.starred_at, last_played_at = excluded.last_played_at,
				last_progress_at = excluded.last_progress_at, rating_changed_at = excluded.rating_changed_at,
				starred_changed_at = excluded.starred_changed_at, played_changed_at = excluded.played_changed_at,
				updated_at = excluded.updated_at`,
			u.user, survivor, m.position, m.played, m.finished, m.count, m.rating, m.starredAt, m.lastPlayed,
			m.lastProgress, m.ratingChanged, m.starredChanged, m.playedChanged, m.updated); err != nil {
			return err
		}
	}
	return nil
}

// laterStamp is the later of two optional stamps.
func laterStamp(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid || (b.Valid && b.Int64 > a.Int64) {
		return b
	}
	return a
}
