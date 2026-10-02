package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// departure is a file a put's link takes from the items it backed: the edge it held on
// each and which of them are books, read before the link, the place it held on each
// book's timeline, the item it joins, which an item it leaves with no file folds into (0
// for none), and whether the put honours locks.
type departure struct {
	file          int64
	into          int64
	lost          map[int64]lostEdge
	books         map[int64]bool
	spans         map[int64]partSpan
	preserveLocks bool
}

// departingTx reads the edges a file holds before a link moves it to into, and its place
// as a part of any book but into.
func departingTx(ctx context.Context, tx *sql.Tx, fileID int64, essence string, into int64, preserveLocks bool) (departure, error) {
	lost, books, err := itemLostEdgesTx(ctx, tx, fileID, essence)
	if err != nil {
		return departure{}, err
	}
	dep := departure{file: fileID, into: into, lost: lost, books: books, preserveLocks: preserveLocks}
	for id, e := range lost {
		if !books[id] || id == into || e.role == alternateRole || e.start.Valid {
			continue
		}
		span, ok, err := partSpanTx(ctx, tx, id, fileID)
		if err != nil {
			return dep, err
		}
		if ok {
			if dep.spans == nil {
				dep.spans = map[int64]partSpan{}
			}
			dep.spans[id] = span
		}
	}
	return dep, nil
}

// arrived reports whether the file came to item from another item rather than holding a
// part of it already: the merge a book's timeline grows by.
func (d departure) arrived(item int64) bool {
	if e, ok := d.lost[item]; ok && e.role != alternateRole {
		return false
	}
	return len(d.sources(item)) > 0
}

// sources are the items the file leaves for item.
func (d departure) sources(item int64) []int64 {
	var out []int64
	for id := range d.lost {
		if id != item {
			out = append(out, id)
		}
	}
	return out
}

// unfinishForArrivalTx keeps a book finished, as a part joins it from the items in from,
// only for a listener who finished one of them: the book anyone else finished did not hold
// the part.
func unfinishForArrivalTx(ctx context.Context, tx *sql.Tx, bookID int64, from []int64, now int64) error {
	if len(from) == 0 {
		return nil
	}
	args := []any{now, bookID}
	for _, id := range from {
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx, `UPDATE play_state SET finished = 0, played_changed_at = MAX(COALESCE(played_changed_at, 0), ?)
		WHERE item_id = ? AND finished = 1 AND NOT EXISTS (SELECT 1 FROM play_state s
			WHERE s.user_id = play_state.user_id AND s.finished = 1 AND s.item_id IN `+placeholders(len(from))+`)`, args...)
	return err
}

// partSpan is a part's place on its book's timeline: where it starts, how long it runs (a
// part counting for the larger of its file's duration and its furthest chapter, as the
// book's own timeline does, bookEffectiveDurationSum), and whether it is the last part. A
// span holds its start and not its end, as playback does: a place where a part starts is
// that part's.
type partSpan struct {
	offset, length int64
	last           bool
}

// inside places a position within the part into the book's timeline, a place on or past
// the part's end at its last moment, so it stays with the part it came from. A part of
// unknown length takes the place as it is.
func (s partSpan) inside(pos int64) int64 {
	if s.length <= 0 {
		return s.offset + pos
	}
	return s.offset + min(pos, s.length-1)
}

// partSpanTx returns the span of the part a file holds in a book, or for an alternate the
// span of the part at its position; ok is false when the file holds neither.
func partSpanTx(ctx context.Context, tx *sql.Tx, bookID, fileID int64) (partSpan, bool, error) {
	var role string
	var pos int
	err := tx.QueryRowContext(ctx, `SELECT role, position FROM item_file WHERE item_id = ? AND file_id = ?
		ORDER BY role = 'alternate' LIMIT 1`, bookID, fileID).Scan(&role, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		return partSpan{}, false, nil
	}
	if err != nil {
		return partSpan{}, false, err
	}
	parts, err := bookPartsQ(ctx, tx, bookID)
	if err != nil {
		return partSpan{}, false, err
	}
	extents := map[int64]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT file_id, MAX(MAX(start_ms, end_ms)) FROM chapter
		WHERE book_item_id = ? GROUP BY file_id`, bookID)
	if err != nil {
		return partSpan{}, false, err
	}
	for rows.Next() {
		var id, ext int64
		if err := rows.Scan(&id, &ext); err != nil {
			rows.Close()
			return partSpan{}, false, err
		}
		extents[id] = ext
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return partSpan{}, false, err
	}
	var off int64
	for i, p := range parts {
		length := max(p.DurationMS, extents[p.fileID])
		if p.fileID == fileID || (role == alternateRole && p.Position == pos) {
			return partSpan{offset: off, length: length, last: i == len(parts)-1}, true, nil
		}
		off += length
	}
	return partSpan{}, false, nil
}

// shiftPositionsTx moves the resume positions and bookmarks on an item at or past from by
// delta, so they stay with their audio when a part ahead of them joins or leaves.
func shiftPositionsTx(ctx context.Context, tx *sql.Tx, itemID, from, delta int64) error {
	if delta == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE play_state SET position_ms = position_ms + ?
		WHERE item_id = ? AND last_progress_at IS NOT NULL AND position_ms >= ?`, delta, itemID, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE bookmark SET position_ms = position_ms + ? WHERE item_id = ? AND position_ms >= ?",
		delta, itemID, from)
	return err
}

// moveDepartedPositionsTx keeps a book's resume positions and bookmarks with their audio
// when a part leaves it for into: one inside the part goes with it, as an offset into
// the part where into now holds it (unless the listener has a later place on into), and
// one past the part moves back by its length. With no into to go to (the file now a cue
// rip), a place inside the part moves to where the part started. A part trashed, detached
// or found gone moves no place, so a restore finds every place where it was.
func moveDepartedPositionsTx(ctx context.Context, tx *sql.Tx, bookID, into, fileID int64, span partSpan) error {
	end := span.offset + span.length
	base, ok := int64(0), false
	if into != 0 {
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM playable_item WHERE id = ?", into).Scan(&kind); err != nil {
			return err
		}
		switch kind {
		case string(model.KindTrack):
			ok = true
		case string(model.KindBook):
			s, found, err := partSpanTx(ctx, tx, into, fileID)
			if err != nil {
				return err
			}
			base, ok = s.offset, found
		}
	}
	if ok {
		delta := base - span.offset
		if _, err := tx.ExecContext(ctx, `INSERT INTO play_state(user_id, item_id, position_ms, last_progress_at, updated_at)
			SELECT user_id, ?, position_ms + ?, last_progress_at, updated_at FROM play_state
			WHERE item_id = ? AND last_progress_at IS NOT NULL AND position_ms >= ? AND position_ms < ?
			ON CONFLICT(user_id, item_id) DO UPDATE SET position_ms = excluded.position_ms,
				last_progress_at = excluded.last_progress_at
			WHERE play_state.last_progress_at IS NULL OR play_state.last_progress_at < excluded.last_progress_at`,
			into, delta, bookID, span.offset, end); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE play_state SET position_ms = 0, last_progress_at = NULL
			WHERE item_id = ? AND last_progress_at IS NOT NULL AND position_ms >= ? AND position_ms < ?`,
			bookID, span.offset, end); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bookmark SET item_id = ?, position_ms = position_ms + ?
			WHERE item_id = ? AND position_ms >= ? AND position_ms < ?`, into, delta, bookID, span.offset, end); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE play_state SET position_ms = ?
			WHERE item_id = ? AND last_progress_at IS NOT NULL AND position_ms > ? AND position_ms < ?`,
			span.offset, bookID, span.offset, end); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bookmark SET position_ms = ?
			WHERE item_id = ? AND position_ms > ? AND position_ms < ?`, span.offset, bookID, span.offset, end); err != nil {
			return err
		}
	}
	return shiftPositionsTx(ctx, tx, bookID, end, -span.length)
}

// foldItemIntoTx folds an item a put left with no file into the item the file joined (a
// track whose file became a part of a book, a book whose one part joined another, a
// track whose file became a copy of another recording), so what was done with it outlives
// its pid. Each user's play state merges into the survivor's (foldPlayStateTx), the
// custom tags a scan would keep come across (foldItemTagsTx), and the bookmarks, sessions,
// queue and playlist entries and acquisition move across: a queue or playlist that already
// holds the survivor drops the loser's entries instead, so a book made of three tracks is
// listed once. When the survivor is a book, a position or bookmark lands inside the file's
// part (partSpan.inside). Only tracks and books fold, and the caller deletes the loser
// afterwards.
func foldItemIntoTx(ctx context.Context, tx *sql.Tx, loser, survivor, fileID int64, preserveLocks bool) error {
	var lkind, skind string
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT kind FROM playable_item WHERE id = ?),
		(SELECT kind FROM playable_item WHERE id = ?)`, loser, survivor).Scan(&lkind, &skind); err != nil {
		return err
	}
	if !folds(lkind) || !folds(skind) {
		return nil
	}
	book := skind == string(model.KindBook)
	var span partSpan
	if book {
		var err error
		if span, _, err = partSpanTx(ctx, tx, survivor, fileID); err != nil {
			return err
		}
	}
	offset := span.offset
	if err := foldPlayStateTx(ctx, tx, loser, survivor, span, book); err != nil {
		return err
	}
	if err := foldItemTagsTx(ctx, tx, loser, survivor, skind, preserveLocks); err != nil {
		return err
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE bookmark SET item_id = ?, position_ms = ? + CASE WHEN ? > 0 THEN MIN(position_ms, ? - 1) ELSE position_ms END
			WHERE item_id = ?`, []any{survivor, offset, span.length, span.length, loser}},
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

// foldItemTagsTx brings across the loser's custom tags a scan of the survivor would keep,
// a locked key (unless the put ignores locks) or one a fill wrote with the value still
// held, with their provenance, where the survivor holds nothing under the key. A key a book
// holds as a field stays behind on a book, and a value the loser's own file stated is the
// survivor's files' to state.
func foldItemTagsTx(ctx context.Context, tx *sql.Tx, loser, survivor int64, kind string, preserveLocks bool) error {
	prov, err := tagProvenanceTx(ctx, tx, loser)
	if err != nil || len(prov) == 0 {
		return err
	}
	tags, err := loadItemTagsTx(ctx, tx, loser)
	if err != nil {
		return err
	}
	held, err := loadItemTagsTx(ctx, tx, survivor)
	if err != nil {
		return err
	}
	heldProv, err := tagProvenanceTx(ctx, tx, survivor)
	if err != nil {
		return err
	}
	moved := false
	for key, row := range prov {
		vs := tags[key]
		keep := len(vs) > 0 && ((row.locked && preserveLocks) || (fillSource(row.source) && row.value == strings.Join(vs, "; ")))
		_, has := held[key]
		_, hasRow := heldProv[key]
		if !keep || has || hasRow || model.IsReservedTagKey(key) ||
			(kind == string(model.KindBook) && slices.Contains(model.BookOwnedTagKeys(), key)) {
			continue
		}
		if err := writeItemTagTx(ctx, tx, survivor, key, vs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, value, updated_at)
			SELECT ?, field, source, provider, locked, value, updated_at FROM field_provenance WHERE item_id = ? AND field = ?`,
			survivor, loser, model.TagLockField(key)); err != nil {
			return err
		}
		moved = true
	}
	if !moved {
		return nil
	}
	return rebuildItemSearchFTSTx(ctx, tx, survivor, kind)
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
// add up and played holds if either was played. Into a book, a star comes across whenever
// the book is not starred, since a book is starred when any of its parts was, with the
// later of the two change stamps so a replayed unstar older than the book's own cannot
// undo it, and an unstar's stamp only where the book has none; the rating comes across
// with its change stamp only where the book has none, the way an entity merge folds it
// (repointEntityPlayState). Into a track, which held the same recording as the loser, the
// later change of each wins. The resume position follows the latest listening: the
// loser's comes across when the survivor has none or an older one, placed by span, the
// file's part in a survivor that is a book, always inside that part: a part finished
// leaves the listener at its last moment, so a part that joins after it is heard next. A
// part finished is not its book finished, so finished comes across only into a track.
func foldPlayStateTx(ctx context.Context, tx *sql.Tx, loser, survivor int64, span partSpan, book bool) error {
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
		switch {
		case !lo.lastProgress.Valid:
		case book && lo.finished == 1:
			lo.position = span.inside(span.length)
		default:
			lo.position = span.inside(lo.position)
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
			if book {
				if !m.rating.Valid && !m.ratingChanged.Valid {
					m.rating, m.ratingChanged = lo.rating, lo.ratingChanged
				}
				switch {
				case !m.starredAt.Valid && lo.starredAt.Valid:
					m.starredAt, m.starredChanged = lo.starredAt, laterStamp(m.starredChanged, lo.starredChanged)
				case !m.starredAt.Valid && !m.starredChanged.Valid:
					m.starredChanged = lo.starredChanged
				}
			} else {
				if newerChange(lo.ratingChanged, m.ratingChanged) || (!m.rating.Valid && !m.ratingChanged.Valid) {
					m.rating, m.ratingChanged = lo.rating, lo.ratingChanged
				}
				if newerChange(lo.starredChanged, m.starredChanged) || (!m.starredAt.Valid && !m.starredChanged.Valid) {
					m.starredAt, m.starredChanged = lo.starredAt, lo.starredChanged
				}
			}
			if lo.lastProgress.Valid && (!m.lastProgress.Valid || lo.lastProgress.Int64 > m.lastProgress.Int64) {
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

// newerChange reports whether change stamp a is set and later than b.
func newerChange(a, b sql.NullInt64) bool {
	return a.Valid && (!b.Valid || a.Int64 > b.Int64)
}

// laterStamp is the later of two optional stamps.
func laterStamp(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid || (b.Valid && b.Int64 > a.Int64) {
		return b
	}
	return a
}
