package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// Maintained rollups are catalog-structural counts and durations per artist,
// release_group, and genre. PutScannedTrack recomputes the rollups for the
// entities touched by a change inside the same transaction. Recomputing from the
// base tables, instead of applying deltas, handles entities dropping to zero
// tracks. A full-catalog rebuild remains available for repair and verification.

// affectedRollups accumulates the entity ids whose rollups a write must refresh.
type affectedRollups struct {
	artists map[int64]bool
	rgs     map[int64]bool
	genres  map[int64]bool
}

func newAffectedRollups() *affectedRollups {
	return &affectedRollups{artists: map[int64]bool{}, rgs: map[int64]bool{}, genres: map[int64]bool{}}
}

func (a *affectedRollups) empty() bool {
	return len(a.artists) == 0 && len(a.rgs) == 0 && len(a.genres) == 0
}

// collect records the item's current artist, album artist, release group, and
// genres as affected. Call it before and after relinks, and before orphan
// deletes, so every entity that gains or loses the track is refreshed.
func (a *affectedRollups) collect(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var artistID, albumArtistID, rgID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT t.artist_id, t.album_artist_id, al.release_group_id
		FROM track t LEFT JOIN album al ON al.id = t.album_id WHERE t.item_id = ?`, itemID).
		Scan(&artistID, &albumArtistID, &rgID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if artistID.Valid {
		a.artists[artistID.Int64] = true
	}
	if albumArtistID.Valid {
		a.artists[albumArtistID.Int64] = true
	}
	if rgID.Valid {
		a.rgs[rgID.Int64] = true
	}
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

// maintainRollupsTx recomputes rollups for the affected entities inside the
// caller's transaction. Each row is deleted and reinserted from a base-table
// aggregation scoped to its id, so touched entities with zero tracks still get
// the zero row expected by the consistency check.
func maintainRollupsTx(ctx context.Context, tx *sql.Tx, aff *affectedRollups, now int64) error {
	if err := refreshRollupSubset(ctx, tx, ids(aff.artists), "artist_rollup", "artist_id", artistRollupSelect, now); err != nil {
		return err
	}
	if err := refreshRollupSubset(ctx, tx, ids(aff.rgs), "release_group_rollup", "release_group_id", releaseGroupRollupSelect, now); err != nil {
		return err
	}
	return refreshRollupSubset(ctx, tx, ids(aff.genres), "genre_rollup", "genre_id", genreRollupSelect, now)
}

// refreshRollupSubset deletes and recomputes the rollup rows for a set of entity
// ids. selectTmpl is the aggregation with a "%s" where an id filter is injected.
func refreshRollupSubset(ctx context.Context, tx *sql.Tx, idList []int64, table, idCol, selectTmpl string, now int64) error {
	if len(idList) == 0 {
		return nil
	}
	ph := placeholders(len(idList))
	args := make([]any, 0, len(idList)*2+1)
	for _, id := range idList {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM "+table+" WHERE "+idCol+" IN "+ph, args...); err != nil {
		return err
	}
	stmt := strings.Replace(selectTmpl, "/*FILTER*/", "IN "+ph, 1)
	insertArgs := append([]any{now}, args...) // the SELECT's leading "?" is updated_at
	_, err := tx.ExecContext(ctx, stmt, insertArgs...)
	return err
}

// RefreshRollups recomputes every rollup from the base tables in one transaction.
// Per-write maintenance keeps rollups current during normal operation; this
// whole-catalog rebuild repairs drift reported by `db verify`.
func (s *Store) RefreshRollups(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		return rebuildRollups(ctx, tx, nowNS())
	})
}

func rebuildRollups(ctx context.Context, tx *sql.Tx, now int64) error {
	for _, q := range []string{
		"DELETE FROM artist_rollup", "DELETE FROM release_group_rollup", "DELETE FROM genre_rollup",
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, tmpl := range []string{artistRollupSelect, releaseGroupRollupSelect, genreRollupSelect} {
		// The whole-catalog rebuild drops the id filter (1=1 covers every entity).
		if _, err := tx.ExecContext(ctx, strings.Replace(tmpl, "/*FILTER*/", "IS NOT NULL OR 1=1", 1), now); err != nil {
			return err
		}
	}
	return nil
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
// SQLite bound-parameter limit.
const idBatchSize = 500

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
// (whole catalog).
const artistRollupSelect = `
INSERT INTO artist_rollup(artist_id, release_group_count, track_count, total_duration_ms, updated_at)
SELECT a.id,
       (SELECT COUNT(*) FROM release_group rg WHERE rg.primary_artist_id = a.id),
       COUNT(DISTINCT t.item_id),
       COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0),
       ?
FROM artist a
LEFT JOIN track t      ON t.artist_id = a.id
LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
LEFT JOIN file f       ON f.id = pf.file_id
WHERE a.id /*FILTER*/
GROUP BY a.id`

const releaseGroupRollupSelect = `
INSERT INTO release_group_rollup(release_group_id, track_count, total_duration_ms, updated_at)
SELECT rg.id,
       COUNT(DISTINCT t.item_id),
       COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0),
       ?
FROM release_group rg
LEFT JOIN album al     ON al.release_group_id = rg.id
LEFT JOIN track t      ON t.album_id = al.id
LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
LEFT JOIN file f       ON f.id = pf.file_id
WHERE rg.id /*FILTER*/
GROUP BY rg.id`

// The genre duration sums ALL of an item's files, not just the primary, so a
// multi-file audiobook contributes its whole running time (the artist and
// release-group rollups join through track, which books never have, so they stay
// single-file by construction and need no such change). A virtual track has a single
// primary edge carrying its window, which the effective-duration expression scopes to
// that window rather than the whole shared file.
const genreRollupSelect = `
INSERT INTO genre_rollup(genre_id, track_count, total_duration_ms, updated_at)
SELECT g.id,
       COUNT(DISTINCT ig.item_id),
       COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0),
       ?
FROM genre g
LEFT JOIN item_genre ig ON ig.genre_id = g.id
LEFT JOIN item_file pf  ON pf.item_id = ig.item_id
LEFT JOIN file f        ON f.id = pf.file_id
WHERE g.id /*FILTER*/
GROUP BY g.id`
