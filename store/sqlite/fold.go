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
	file  int64
	into  int64
	lost  map[int64]lostEdge
	books map[int64]bool
	spans map[int64]partSpan
	// intoPID is the pid the put minted for into, empty when into stood already, so a rip's
	// opening track folding into it may hand it its own (reconcileOrphansTx).
	intoPID       model.PID
	preserveLocks bool
}

// handPIDTx moves pid to onto the item holding from, unless another item holds to, and
// reports whether it did. It is how an item keeps the identity a client knows across the
// conversions between a whole file and a cue rip, and how a rebuild restores a stamp. A
// from the change log has named goes with a delete there (fromLogged); the caller logs
// to, as an update where a client knew it and as a create where none could.
func handPIDTx(ctx context.Context, tx *sql.Tx, from, to model.PID, fromLogged bool) (bool, error) {
	r, err := tx.ExecContext(ctx, `UPDATE playable_item SET pid = ?1 WHERE pid = ?2
		AND NOT EXISTS (SELECT 1 FROM playable_item WHERE pid = ?1)`, string(to), string(from))
	if err != nil {
		return false, err
	}
	if n, err := r.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if fromLogged {
		return true, appendChange(ctx, tx, "item", from, model.OpDelete)
	}
	return true, nil
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
// the part. A part new to the catalog (from empty) comes from no one, so the book is
// finished for no one.
func unfinishForArrivalTx(ctx context.Context, tx *sql.Tx, bookID int64, from []int64, now int64) error {
	if len(from) == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE play_state SET finished = 0, played_changed_at = MAX(COALESCE(played_changed_at, 0), ?)
			WHERE item_id = ? AND finished = 1`, now, bookID)
		return err
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

// inside places an offset into the span on its timeline, kept inside the span (clamp), so
// it stays with the part or window it came from.
func (s partSpan) inside(off int64) int64 {
	return s.offset + s.clamp(off)
}

// within is the offset into the span of a place on its timeline, kept inside the span.
func (s partSpan) within(pos int64) int64 {
	return s.clamp(pos - s.offset)
}

// clamp keeps an offset into the span inside it: a place before it at its start, and one
// on or past its end at its last moment. A span of unknown length takes the place as it is.
func (s partSpan) clamp(off int64) int64 {
	off = max(off, 0)
	if s.length > 0 {
		off = min(off, s.length-1)
	}
	return off
}

// spanAt returns the index of the span that holds a place on a timeline whose spans run in
// order: the last that starts at or before it, a place where a span starts being that
// span's, and the first for a place before them all.
func spanAt(spans []partSpan, pos int64) int {
	held := 0
	for i, s := range spans {
		if s.offset <= pos {
			held = i
		}
	}
	return held
}

// place is a listener's resume position on an item (key the user, with the times it was
// set at) or a bookmark on it (key its id).
type place struct{ key, pos, at, updated int64 }

// itemPlacesTx reads an item's places: each resume position a listener set and each
// bookmark. Rows are drained before the caller writes, since the single write connection
// cannot interleave a query and an exec.
func itemPlacesTx(ctx context.Context, tx *sql.Tx, itemID int64) (states, marks []place, err error) {
	read := func(q string) ([]place, error) {
		rows, err := tx.QueryContext(ctx, q, itemID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []place
		for rows.Next() {
			var p place
			if err := rows.Scan(&p.key, &p.pos, &p.at, &p.updated); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}
	if states, err = read(`SELECT user_id, position_ms, last_progress_at, updated_at FROM play_state
		WHERE item_id = ? AND last_progress_at IS NOT NULL`); err != nil {
		return nil, nil, err
	}
	marks, err = read("SELECT id, position_ms, 0, 0 FROM bookmark WHERE item_id = ?")
	return states, marks, err
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
	parts, err := bookTimelineTx(ctx, tx, bookID, nil)
	if err != nil {
		return partSpan{}, false, err
	}
	for _, p := range parts {
		if p.fileID == fileID || (role == alternateRole && p.position == pos) {
			return p.span, true, nil
		}
	}
	return partSpan{}, false, nil
}

// timelinePart is one part of a book on its timeline.
type timelinePart struct {
	fileID   int64
	position int
	span     partSpan
}

// partWas is where a part stood on its book's timeline before a read moved or resized it:
// its position and the length it ran.
type partWas struct {
	position int
	length   int64
}

// bookTimelineTx returns a book's parts in reading order, each with its span. A file in
// was takes the position and length given there in place of its own, for the timeline as
// it stood before a read changed them.
func bookTimelineTx(ctx context.Context, tx *sql.Tx, bookID int64, was map[int64]partWas) ([]timelinePart, error) {
	parts, err := bookPartsQ(ctx, tx, bookID)
	if err != nil {
		return nil, err
	}
	if len(was) > 0 {
		for i := range parts {
			if w, ok := was[parts[i].fileID]; ok {
				parts[i].Position = w.position
			}
		}
		sortBookParts(parts)
	}
	extents := map[int64]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT file_id, MAX(MAX(start_ms, end_ms)) FROM chapter
		WHERE book_item_id = ? GROUP BY file_id`, bookID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, ext int64
		if err := rows.Scan(&id, &ext); err != nil {
			rows.Close()
			return nil, err
		}
		extents[id] = ext
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]timelinePart, len(parts))
	var off int64
	for i, p := range parts {
		length := max(p.DurationMS, extents[p.fileID])
		if w, ok := was[p.fileID]; ok {
			length = w.length
		}
		out[i] = timelinePart{fileID: p.fileID, position: p.Position, span: partSpan{offset: off, length: length, last: i == len(parts)-1}}
		off += length
	}
	return out, nil
}

// chapterExtentTx is how far a part's chapters run on a book: its furthest chapter
// start or end, which a part's length on the timeline takes when it passes the file's
// duration (bookTimelineTx).
func chapterExtentTx(ctx context.Context, tx *sql.Tx, bookID, fileID int64) (int64, error) {
	var ext int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(MAX(start_ms, end_ms)), 0) FROM chapter
		WHERE book_item_id = ? AND file_id = ?`, bookID, fileID).Scan(&ext)
	return ext, err
}

// remapPlacesTx keeps a book's resume positions and bookmarks with their audio when a put
// reorders or resizes its parts: each place keeps its offset into the part that held it,
// at that part's start on the timeline after, inside its new length. It does nothing
// unless the same parts stand either side and one of them moved or changed length, since a
// part joining or leaving moves its places itself (shiftPositionsTx,
// moveDepartedPositionsTx).
func remapPlacesTx(ctx context.Context, tx *sql.Tx, bookID int64, before, after []timelinePart) error {
	now := make(map[int64]partSpan, len(after))
	for _, p := range after {
		now[p.fileID] = p.span
	}
	moved := false
	for _, p := range before {
		a, ok := now[p.fileID]
		if !ok {
			return nil
		}
		moved = moved || a.offset != p.span.offset || a.length != p.span.length
	}
	if !moved || len(before) != len(after) {
		return nil
	}
	spans := make([]partSpan, len(before))
	for i, p := range before {
		spans[i] = p.span
	}
	remap := func(pos int64) int64 {
		p := before[spanAt(spans, pos)]
		return now[p.fileID].inside(pos - p.span.offset)
	}
	states, marks, err := itemPlacesTx(ctx, tx, bookID)
	if err != nil {
		return err
	}
	for _, p := range states {
		if _, err := tx.ExecContext(ctx, "UPDATE play_state SET position_ms = ? WHERE user_id = ? AND item_id = ?",
			remap(p.pos), p.key, bookID); err != nil {
			return err
		}
	}
	for _, m := range marks {
		if _, err := tx.ExecContext(ctx, "UPDATE bookmark SET position_ms = ? WHERE id = ?", remap(m.pos), m.key); err != nil {
			return err
		}
	}
	return nil
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
// track whose file became a copy of another recording, a rip's track whose file is one
// item again), so what was done with it outlives its pid. Each user's play state merges
// into the survivor's (foldPlayStateTx), the
// custom tags a scan would keep come across (foldItemTagsTx), and the bookmarks, sessions,
// queue and playlist entries and acquisition move across: a queue or playlist that already
// holds the survivor drops the loser's entries instead, so a book made of three tracks is
// listed once, and each playlist and queue changed is settled (entryHolders), here or
// with the caller's batch. When the survivor is a book, a position or bookmark lands inside the file's
// part (partSpan.inside), and a loser that played a window of the file (window, a rip's
// track) lands its places inside that window. Only tracks and books fold, and the caller
// deletes the loser afterwards.
func foldItemIntoTx(ctx context.Context, tx *sql.Tx, loser, survivor, fileID int64, window partSpan, preserveLocks bool, batch *entryHolders) error {
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
	if window != (partSpan{}) {
		// A window with an open end runs to the end of what holds it, the part or the file.
		length := window.length
		if length == 0 {
			total := span.length
			if !book {
				if err := tx.QueryRowContext(ctx, "SELECT COALESCE(duration_ms, 0) FROM file WHERE id = ?", fileID).Scan(&total); err != nil {
					return err
				}
			}
			if total > window.offset {
				length = total - window.offset
			}
		}
		span = partSpan{offset: span.offset + window.offset, length: length, last: span.last}
	}
	offset := span.offset
	if err := foldPlayStateTx(ctx, tx, loser, survivor, span, book || window != (partSpan{})); err != nil {
		return err
	}
	if err := foldItemTagsTx(ctx, tx, loser, survivor, skind, preserveLocks); err != nil {
		return err
	}
	var own entryHolders
	if batch == nil {
		batch = &own
	}
	if err := batch.addTx(ctx, tx, "item_id = ?", loser); err != nil {
		return err
	}
	stmts := []struct {
		q    string
		args []any
	}{
		// partSpan.inside, set-based.
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
	return own.settleTx(ctx, tx)
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
// add up and played holds if either was played. A part of the survivor (a book's part, or
// a rip's track folding into its file) brings its star whenever the survivor is not
// starred, since the whole is starred when any of its parts was, with the later of the two
// change stamps so a replayed unstar older than the survivor's own cannot undo it, and an
// unstar's stamp only where the survivor has none; its rating comes across with its change
// stamp only where the survivor has none, the way an entity merge folds it
// (repointEntityPlayState). Into a track that held the same recording as the loser, the
// later change of each wins. The resume position follows the latest listening: the
// loser's comes across when the survivor has none or an older one, placed by span, the
// part's place in the survivor, always inside that part: a part finished leaves the
// listener at its last moment, so the part after it is heard next. A part finished is not
// its whole finished, so finished comes across only from a whole recording.
func foldPlayStateTx(ctx context.Context, tx *sql.Tx, loser, survivor int64, span partSpan, part bool) error {
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
		case part && lo.finished == 1:
			lo.position = span.inside(span.length)
		default:
			lo.position = span.inside(lo.position)
		}
		if part {
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
			if part {
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

// finishedByTx returns the users who finished every one of items, each with the latest
// stamp their finishes carry.
func finishedByTx(ctx context.Context, tx *sql.Tx, items []int64) (map[int64]sql.NullInt64, error) {
	if len(items) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(items)+1)
	for _, id := range items {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id, MAX(played_changed_at) FROM play_state
		WHERE finished = 1 AND item_id IN `+placeholders(len(items))+` GROUP BY user_id HAVING COUNT(*) = ?`,
		append(args, len(items))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]sql.NullInt64{}
	for rows.Next() {
		var user int64
		var stamp sql.NullInt64
		if err := rows.Scan(&user, &stamp); err != nil {
			return nil, err
		}
		out[user] = stamp
	}
	return out, rows.Err()
}

// finishTx marks each of items played and finished for the users in by, with the stamp
// their finish carried where it is the later one.
func finishTx(ctx context.Context, tx *sql.Tx, items []int64, by map[int64]sql.NullInt64, now int64) error {
	for user, stamp := range by {
		for _, id := range items {
			if _, err := tx.ExecContext(ctx, `INSERT INTO play_state(user_id, item_id, played, finished, played_changed_at, updated_at)
				VALUES (?, ?, 1, 1, ?, ?)
				ON CONFLICT(user_id, item_id) DO UPDATE SET played = 1, finished = 1, updated_at = excluded.updated_at,
					played_changed_at = CASE WHEN excluded.played_changed_at > COALESCE(play_state.played_changed_at, 0)
						THEN excluded.played_changed_at ELSE play_state.played_changed_at END`,
				user, id, stamp, now); err != nil {
				return err
			}
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
