package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Maintained rollups are catalog-structural counts and durations per artist,
// release_group, and genre. A write recomputes the rollups for the entities it touched
// inside its own transaction, except a scan's put, which records them in rollup_dirty for
// the scan's next SettleRollups: each recompute is linear in the entity's members, so a
// scan paying one per file would be quadratic in its largest artist, genre or album.
// Recomputing from the base tables, instead of applying deltas, handles entities dropping
// to zero tracks. A full-catalog rebuild remains available for repair and verification.

// affectedRollups accumulates the entity ids whose rollups a write must refresh, and
// the albums whose maintained year it must (refreshAlbumYearsTx). deferred marks them
// dirty instead (PutScannedTrackInput.DeferRollups). fresh holds, by table, the albums,
// release groups and artists the write created, which are keyed from their members
// whether or not it defers.
type affectedRollups struct {
	artists  map[int64]bool
	rgs      map[int64]bool
	genres   map[int64]bool
	albums   map[int64]bool
	fresh    map[string][]int64
	deferred bool
}

func newAffectedRollups() *affectedRollups {
	return &affectedRollups{artists: map[int64]bool{}, rgs: map[int64]bool{}, genres: map[int64]bool{},
		albums: map[int64]bool{}, fresh: map[string][]int64{}}
}

// created records an album, release group or artist the write inserted, which is as
// affected as one it touched.
func (a *affectedRollups) created(table string, id int64) {
	a.fresh[table] = append(a.fresh[table], id)
	switch table {
	case "artist":
		a.artists[id] = true
	case "release_group":
		a.rgs[id] = true
	case "album":
		a.albums[id] = true
	}
}

func (a *affectedRollups) empty() bool {
	return len(a.artists) == 0 && len(a.rgs) == 0 && len(a.genres) == 0 && len(a.albums) == 0
}

// collect records the item's current artist, album artist, album, release group, a
// book's author, and genres as affected. Call it before and after relinks, and before
// orphan deletes, so every entity that gains or loses the item is refreshed.
func (a *affectedRollups) collect(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var artistID, albumArtistID, albumID, rgID, authorID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT t.artist_id, t.album_artist_id, t.album_id, al.release_group_id, bk.author_id
		FROM playable_item pi LEFT JOIN track t ON t.item_id = pi.id LEFT JOIN album al ON al.id = t.album_id
		LEFT JOIN book bk ON bk.item_id = pi.id WHERE pi.id = ?`, itemID).
		Scan(&artistID, &albumArtistID, &albumID, &rgID, &authorID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if artistID.Valid {
		a.artists[artistID.Int64] = true
	}
	// An author's spelling derives from its books (entityspelling.go).
	if authorID.Valid {
		a.artists[authorID.Int64] = true
	}
	if albumArtistID.Valid {
		a.artists[albumArtistID.Int64] = true
	}
	if albumID.Valid {
		a.albums[albumID.Int64] = true
	}
	if rgID.Valid {
		a.rgs[rgID.Int64] = true
	}
	return a.collectGenres(ctx, tx, itemID)
}

// collectGenres records the item's genres as affected, for a write that moves its genres
// and nothing the other rollups count.
func (a *affectedRollups) collectGenres(ctx context.Context, tx *sql.Tx, itemID int64) error {
	rows, err := tx.QueryContext(ctx, "SELECT genre_id FROM item_genre WHERE item_id = ?", itemID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			return err
		}
		a.genres[gid] = true
	}
	return rows.Err()
}

// collectAlbumOf records the album the item's track sits on as affected, for a write
// that moves the track's year and nothing the other rollups count; with group, its
// release group too, for a write that moves the track's album spelling.
func (a *affectedRollups) collectAlbumOf(ctx context.Context, tx *sql.Tx, itemID int64, group bool) error {
	var albumID, rgID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT t.album_id, al.release_group_id FROM track t
		LEFT JOIN album al ON al.id = t.album_id WHERE t.item_id = ?`, itemID).Scan(&albumID, &rgID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if albumID.Valid {
		a.albums[albumID.Int64] = true
	}
	if group && rgID.Valid {
		a.rgs[rgID.Int64] = true
	}
	return nil
}

// rollupKind is one kind rollup_dirty records: the set of its ids an affectedRollups holds,
// its recompute, and the count it adds to a settle's report.
type rollupKind struct {
	kind      string
	of        func(*affectedRollups) map[int64]bool
	recompute func(ctx context.Context, tx *sql.Tx, ids []int64, now int64) error
	count     func(*model.RollupSettle) *int
}

var rollupKinds = []rollupKind{
	{"artist", func(a *affectedRollups) map[int64]bool { return a.artists },
		func(ctx context.Context, tx *sql.Tx, ids []int64, now int64) error {
			if err := refreshRollupSubset(ctx, tx, ids, artistRollupSelect, now); err != nil {
				return err
			}
			_, _, err := settleSpellingsTx(ctx, tx, artistSpelling, ids)
			return err
		}, func(r *model.RollupSettle) *int { return &r.Artists }},
	{"release_group", func(a *affectedRollups) map[int64]bool { return a.rgs },
		func(ctx context.Context, tx *sql.Tx, ids []int64, now int64) error {
			if err := refreshRollupSubset(ctx, tx, ids, releaseGroupRollupSelect, now); err != nil {
				return err
			}
			_, _, err := settleSpellingsTx(ctx, tx, releaseGroupSpelling, ids)
			return err
		}, func(r *model.RollupSettle) *int { return &r.ReleaseGroups }},
	{"genre", func(a *affectedRollups) map[int64]bool { return a.genres },
		func(ctx context.Context, tx *sql.Tx, ids []int64, now int64) error {
			return refreshRollupSubset(ctx, tx, ids, genreRollupSelect, now)
		}, func(r *model.RollupSettle) *int { return &r.Genres }},
	{"album", func(a *affectedRollups) map[int64]bool { return a.albums },
		func(ctx context.Context, tx *sql.Tx, ids []int64, _ int64) error {
			if err := refreshAlbumYearsTx(ctx, tx, ids); err != nil {
				return err
			}
			_, _, err := settleSpellingsTx(ctx, tx, albumSpelling, ids)
			return err
		},
		func(r *model.RollupSettle) *int { return &r.Albums }},
}

// maintainRollupsTx recomputes rollups for the affected entities inside the
// caller's transaction, the affected albums' years, and the derived sort spellings of the
// affected albums, release groups and artists (entityspelling.go). Each row is deleted and
// reinserted from a base-table aggregation scoped to its id, so touched entities with
// zero tracks still get the zero row expected by the consistency check. A deferred set
// is only recorded in rollup_dirty.
func maintainRollupsTx(ctx context.Context, tx *sql.Tx, aff *affectedRollups, now int64) error {
	// A new entity takes its spelling from the members it has now, by the rule the settle
	// applies, rather than keying by its name until a deferred settle. Its create delta
	// covers the key.
	for _, sp := range entitySpellings {
		if _, _, err := refreshSpellingsTx(ctx, tx, sp, aff.fresh[sp.table]); err != nil {
			return err
		}
	}
	for _, k := range rollupKinds {
		set := ids(k.of(aff))
		var err error
		if aff.deferred {
			err = markRollupsDirtyTx(ctx, tx, k.kind, set)
		} else {
			err = k.recompute(ctx, tx, set, now)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// markRollupsDirtyTx records ids of kind as owed a settle.
func markRollupsDirtyTx(ctx context.Context, tx *sql.Tx, kind string, idList []int64) error {
	return chunkSlice(idList, idBatchSize, func(chunk []int64) error {
		args := make([]any, 0, 2*len(chunk))
		for _, id := range chunk {
			args = append(args, kind, id)
		}
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO rollup_dirty(kind, entity_id) VALUES "+
			strings.TrimSuffix(strings.Repeat("(?,?),", len(chunk)), ","), args...)
		return err
	})
}

// SettleRollups recomputes what deferred puts left in rollup_dirty, idBatchSize ids at a
// time, each batch selected, recomputed and cleared in one transaction, so a failure or a
// crash between batches leaves only the rest pending. A recompute is idempotent, so an id
// a later write recomputed meanwhile, or one whose entity is gone, settles harmlessly.
func (s *Store) SettleRollups(ctx context.Context) (model.RollupSettle, error) {
	const op = "store.SettleRollups"
	var out model.RollupSettle
	var pending bool
	if err := s.rdb().QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM rollup_dirty)").Scan(&pending); err != nil {
		return out, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if !pending {
		return out, nil
	}
	for _, k := range rollupKinds {
		for {
			n, err := s.settleBatch(ctx, k, idBatchSize)
			if err != nil {
				return out, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			*k.count(&out) += n
			if n < idBatchSize {
				break
			}
		}
	}
	return out, nil
}

// settleBatch settles up to limit of kind's pending ids in one transaction, returning how
// many it settled.
func (s *Store) settleBatch(ctx context.Context, k rollupKind, limit int) (int, error) {
	var n int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		batch, err := queryInt64sTx(ctx, tx,
			"SELECT entity_id FROM rollup_dirty WHERE kind = ? ORDER BY entity_id LIMIT ?", k.kind, limit)
		if err != nil || len(batch) == 0 {
			return err
		}
		if err := k.recompute(ctx, tx, batch, nowNS()); err != nil {
			return err
		}
		args := []any{k.kind}
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM rollup_dirty WHERE kind = ? AND entity_id IN "+
			placeholders(len(batch)), args...); err != nil {
			return err
		}
		n = len(batch)
		return nil
	})
	return n, err
}

// refreshRollupSubset recomputes the rollup rows for a set of entity ids, idBatchSize at a
// time. selectTmpl is the aggregation with a "/*FILTER*/" where the id filter is injected.
func refreshRollupSubset(ctx context.Context, tx *sql.Tx, idList []int64, selectTmpl string, now int64) error {
	return chunkSlice(idList, idBatchSize, func(chunk []int64) error {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, now) // the SELECT's leading "?" is updated_at
		for _, id := range chunk {
			args = append(args, id)
		}
		_, err := tx.ExecContext(ctx, strings.Replace(selectTmpl, "/*FILTER*/", "IN "+placeholders(len(chunk)), 1), args...)
		return err
	})
}

// RefreshRollups recomputes every rollup from the base tables in one transaction,
// plus each book's denormalized total duration and ISBN key, each album's year and each
// item's release year and each derived sort spelling, and renumbers each static playlist
// whose positions are not its listing indexes, reporting what it rewrote. Per-write maintenance keeps them current
// during normal operation; this whole-catalog rebuild repairs drift reported by `db
// verify`. The book columns, the two years and the playlist positions are not rollup
// tables, but they are the same kind of maintained value, they are on the same drift
// report, and this is the verb `db verify --fix` runs, so repairing them anywhere else
// would leave the fix unreachable. It recomputes everything a settle would, so it clears
// what deferred puts left pending too, and counts a pending entity when its stored value
// was stale.
func (s *Store) RefreshRollups(ctx context.Context) (*model.RefreshReport, error) {
	rep := &model.RefreshReport{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// Book totals first: a genre's running time counts them.
		var err error
		if rep.BookDurations, err = refreshAllBookDurations(ctx, tx); err != nil {
			return err
		}
		if rep.Artists, rep.ReleaseGroups, rep.Genres, err = rebuildRollups(ctx, tx, nowNS()); err != nil {
			return err
		}
		if rep.ISBNKeys, err = refreshAllISBNKeysTx(ctx, tx); err != nil {
			return err
		}
		if rep.AlbumYears, err = refreshAllAlbumYearsTx(ctx, tx); err != nil {
			return err
		}
		if rep.ReleaseYears, err = refreshAllReleaseYearsTx(ctx, tx); err != nil {
			return err
		}
		if rep.PlaylistsCompacted, err = compactAllPlaylistsTx(ctx, tx); err != nil {
			return err
		}
		if rep.SortSpellings, rep.SortSpellingKeys, err = refreshAllSpellingsTx(ctx, tx); err != nil {
			return err
		}
		// A WHERE keeps SQLite off its truncate path, which rewrites the root page of an empty
		// table too.
		_, err = tx.ExecContext(ctx, "DELETE FROM rollup_dirty WHERE kind IS NOT NULL")
		return err
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// rebuildRollups recomputes every rollup row, returning how many it wrote per table, which
// are the rows that were missing or wrong: every rollup row's entity exists, since an
// entity's delete takes its row with it.
func rebuildRollups(ctx context.Context, tx *sql.Tx, now int64) (artists, groups, genres int, err error) {
	counts := [3]*int{&artists, &groups, &genres}
	for i, tmpl := range []string{artistRollupSelect, releaseGroupRollupSelect, genreRollupSelect} {
		// The whole-catalog rebuild drops the id filter (1=1 covers every entity).
		r, err := tx.ExecContext(ctx, strings.Replace(tmpl, "/*FILTER*/", "IS NOT NULL OR 1=1", 1), now)
		if err != nil {
			return 0, 0, 0, err
		}
		n, _ := r.RowsAffected()
		*counts[i] = int(n)
	}
	return artists, groups, genres, nil
}

func ids(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return "()"
	}
	return "(" + strings.Repeat("?,", n-1) + "?)"
}

// idBatchSize bounds an IN(...) / multi-row VALUES batch to stay well under the
// SQLite bound-parameter limit, matching query's maxInValues cap.
const idBatchSize = 500

// inArray is the IN list of a cached batch read: the whole batch bound as one JSON array
// (idArray, pidArray), so a list of any length runs the same statement.
const inArray = "(SELECT value FROM json_each(?))"

// idArray renders ids as a JSON array, for json_each.
func idArray(ids []int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	b.WriteByte(']')
	return b.String()
}

// pidArray renders pids as a JSON array, for json_each.
func pidArray(pids []model.PID) string {
	b, _ := json.Marshal(pids)
	return string(b)
}

// uniquePIDs deduplicates pids preserving first-seen order, the shared front
// half of the chunked IN(...) lookups (ItemsByPIDs and friends): a repeated pid
// never bloats a chunk or straddles a chunk boundary, and an input-order
// rebuild needs no second pass to collapse duplicates.
func uniquePIDs(pids []model.PID) []model.PID {
	unique := make([]model.PID, 0, len(pids))
	seen := make(map[model.PID]struct{}, len(pids))
	for _, pid := range pids {
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		unique = append(unique, pid)
	}
	return unique
}

// chunkSlice invokes fn for each successive slice of s no longer than size, so a
// large id/pid set is processed in bounded batches. It is the shared batching
// skeleton for the chunked IN/VALUES queries.
func chunkSlice[T any](s []T, size int, fn func([]T) error) error {
	if size <= 0 {
		size = idBatchSize
	}
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		if err := fn(s[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// The rollup aggregations join each entity to its tracks and the tracks' primary
// files so durations sum from the real audio rows. COUNT(DISTINCT item) tolerates
// the LEFT JOINs (an entity with no tracks rolls up to zero, not one). The
// "/*FILTER*/" marker is replaced with an id filter (subset) or a tautology
// (whole catalog). Each is an upsert that writes a row only when it is missing or its
// values differ, so a recompute of an unchanged entity writes nothing and the rows it
// affected are the rows that drifted.
//
// Counts and durations are the live members' (liveMembers): the item and its files are
// reached through pi, which a trashed item does not join. The duration left a trashed
// member out before the count did, because trash deletes its file row, so it was only an
// accident that kept it out; a missing member keeps its file row and counts in both.
const artistRollupSelect = `
INSERT INTO artist_rollup(artist_id, release_group_count, track_count, total_duration_ms, updated_at)
SELECT a.id,
       (SELECT COUNT(*) FROM release_group rg WHERE rg.primary_artist_id = a.id),
       COUNT(DISTINCT pi.id),
       COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0),
       ?
FROM artist a
LEFT JOIN track t          ON t.artist_id = a.id
LEFT JOIN playable_item pi ON pi.id = t.item_id AND pi.state <> 'archived'
LEFT JOIN item_file pf     ON pf.item_id = pi.id AND pf.role = 'primary'
LEFT JOIN file f           ON f.id = pf.file_id
WHERE a.id /*FILTER*/
GROUP BY a.id
ON CONFLICT(artist_id) DO UPDATE SET release_group_count = excluded.release_group_count,
	track_count = excluded.track_count, total_duration_ms = excluded.total_duration_ms,
	updated_at = excluded.updated_at
WHERE (release_group_count, track_count, total_duration_ms) <>
	(excluded.release_group_count, excluded.track_count, excluded.total_duration_ms)`

const releaseGroupRollupSelect = `
INSERT INTO release_group_rollup(release_group_id, track_count, total_duration_ms, updated_at)
SELECT rg.id,
       COUNT(DISTINCT pi.id),
       COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0),
       ?
FROM release_group rg
LEFT JOIN album al         ON al.release_group_id = rg.id
LEFT JOIN track t          ON t.album_id = al.id
LEFT JOIN playable_item pi ON pi.id = t.item_id AND pi.state <> 'archived'
LEFT JOIN item_file pf     ON pf.item_id = pi.id AND pf.role = 'primary'
LEFT JOIN file f           ON f.id = pf.file_id
WHERE rg.id /*FILTER*/
GROUP BY rg.id
ON CONFLICT(release_group_id) DO UPDATE SET track_count = excluded.track_count,
	total_duration_ms = excluded.total_duration_ms, updated_at = excluded.updated_at
WHERE (track_count, total_duration_ms) <> (excluded.track_count, excluded.total_duration_ms)`

// The genre duration counts each live member as its item view does (itemDurationExpr):
// a multi-file audiobook for its timeline's total, every part, never an alternate, and
// a virtual track for its primary edge's window rather than the whole shared file. A
// book's total moves with its chapters, so a write that moves it collects its genres.
const genreRollupSelect = `
INSERT INTO genre_rollup(genre_id, track_count, total_duration_ms, updated_at)
SELECT g.id,
       COUNT(DISTINCT pi.id),
       COALESCE(SUM(` + itemDurationExpr + `), 0),
       ?
FROM genre g
LEFT JOIN item_genre ig    ON ig.genre_id = g.id
LEFT JOIN playable_item pi ON pi.id = ig.item_id AND pi.state <> 'archived'` + itemDurationJoins + `
WHERE g.id /*FILTER*/
GROUP BY g.id
ON CONFLICT(genre_id) DO UPDATE SET track_count = excluded.track_count,
	total_duration_ms = excluded.total_duration_ms, updated_at = excluded.updated_at
WHERE (track_count, total_duration_ms) <> (excluded.track_count, excluded.total_duration_ms)`
