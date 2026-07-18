package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// DerivedReport is the result of the derived-data consistency check: a count of
// drift in each kind of writer-maintained denormalized state versus a fresh
// recompute from the source rows. Any denormalized state can drift if a writer
// path is missed, so this single check covers all of it. All zeros means clean.
type DerivedReport struct {
	ItemsMissingFTS         int // present items with no search_fts row
	OrphanFTSRows           int // search_fts rows with no backing item
	ArtistRollupDrift       int // artists whose stored rollup != recompute
	GenreRollupDrift        int
	ReleaseGroupRollupDrift int
	SortKeyDrift            int // entities whose stored sort_key != regenerated
	BookDurationDrift       int // books whose stored total_duration_ms != summed parts
	OrphanArtSources        int // art_source images with no live art_map references
	OrphanThumbnails        int // thumb_cache rows whose source is unreferenced
}

// Consistent reports whether the writer-maintained derived data is correct: FTS
// coverage, rollups, and generated sort keys. Orphan-art counts are excluded
// because cover swaps and item deletion can leave reclaimable sources for GCArt;
// those leftovers are not consistency drift. Call Reclaimable to report art
// garbage separately.
func (r DerivedReport) Consistent() bool {
	return r.ItemsMissingFTS == 0 && r.OrphanFTSRows == 0 &&
		r.ArtistRollupDrift == 0 && r.GenreRollupDrift == 0 &&
		r.ReleaseGroupRollupDrift == 0 && r.SortKeyDrift == 0 &&
		r.BookDurationDrift == 0
}

// Reclaimable reports whether `db verify --fix` (GCArt) would reclaim space:
// orphaned art sources or thumbnails with no live entity references. It is
// informational and independent of Consistent.
func (r DerivedReport) Reclaimable() bool {
	return r.OrphanArtSources > 0 || r.OrphanThumbnails > 0
}

// VerifyDerived runs the derived-data consistency check read-only: FTS coverage
// (every present item has a row, no row outlives its item), the maintained
// rollups (versus a fresh recompute), and the generated sort keys (versus
// regeneration) are each checked against the source rows. FTS field *content* is
// not diffed yet, only coverage. It never writes; `db verify` surfaces the
// report and the operator reruns the relevant refresh if drift is found.
func (s *Store) VerifyDerived(ctx context.Context) (*DerivedReport, error) {
	const op = "store.VerifyDerived"
	rep := &DerivedReport{}

	checks := []struct {
		dst  *int
		stmt string
	}{
		{&rep.ItemsMissingFTS, `SELECT COUNT(*) FROM playable_item pi
			WHERE pi.state = 'present' AND NOT EXISTS (SELECT 1 FROM search_fts WHERE rowid = pi.id)`},
		{&rep.OrphanFTSRows, `SELECT COUNT(*) FROM search_fts s
			WHERE NOT EXISTS (SELECT 1 FROM playable_item pi WHERE pi.id = s.rowid)`},
		{&rep.ArtistRollupDrift, artistRollupDriftQ},
		{&rep.GenreRollupDrift, genreRollupDriftQ},
		{&rep.ReleaseGroupRollupDrift, releaseGroupRollupDriftQ},
		// A book's denormalized total duration must equal the sum of its parts'
		// effective durations (the same definition refreshBookDuration writes).
		{&rep.BookDurationDrift, "SELECT COUNT(*) FROM book b WHERE b.total_duration_ms <> " +
			fmt.Sprintf(bookEffectiveDurationSum, "b.item_id")},
		// An art source with no live entity reference, or a thumbnail whose source
		// has none, is reclaimable derived state. A map row pointing at a deleted
		// entity is ignored here; GCArt removes that stale map before deleting the
		// source.
		{&rep.OrphanArtSources, "SELECT COUNT(*) FROM art_source WHERE hash NOT IN (" + liveArtSourceQ + ")"},
		{&rep.OrphanThumbnails, "SELECT COUNT(*) FROM thumb_cache WHERE source_hash NOT IN (" + liveArtSourceQ + ")"},
	}
	for _, c := range checks {
		if err := s.read.QueryRowContext(ctx, c.stmt).Scan(c.dst); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}

	// Sort keys are generated in Go, so they cannot be recomputed in SQL; load
	// each (text, stored_key) pair and compare against a fresh SortKey.
	drift, err := s.sortKeyDrift(ctx)
	if err != nil {
		return nil, err
	}
	rep.SortKeyDrift = drift
	return rep, nil
}

// sortKeyDrift counts entities whose stored sort_key differs from regenerating it
// from the display text. It covers every table that carries a generated sort key.
//
// Sort keys are generated in Go, so this streams every entity row (O(n) time,
// O(1) memory because it never buffers the result set) and recomputes
// model.SortKey per row. That is acceptable for `db verify`, a deliberate
// maintenance operation; if it ever needs to run hot, model.SortKey can be
// registered as a deterministic SQLite scalar function so the comparison runs
// entirely in SQL.
func (s *Store) sortKeyDrift(ctx context.Context) (int, error) {
	const op = "store.VerifyDerived"
	// entityType is set for the tables whose sort key a user can override through
	// entity curation (artist/release_group/album). A curated sort override deliberately
	// diverges from the name-derived key, so it is excluded from the drift count.
	sources := []struct{ text, table, entityType string }{
		{"title", "playable_item", ""},
		{"name", "artist", string(model.MergeArtist)},
		{"name", "genre", ""},
		{"title", "release_group", string(model.MergeReleaseGroup)},
		{"title", "album", string(model.MergeAlbum)},
		{"name", "series", ""},
		{"title", "podcast", ""},
	}
	total := 0
	for _, src := range sources {
		// text/table are internal constants (no user input), so they are interpolated;
		// entity_type is bound as a parameter for consistency with the rest of the store.
		q := "SELECT " + src.text + ", sort_key FROM " + src.table
		var args []any
		if src.entityType != "" {
			q += " t WHERE NOT EXISTS (SELECT 1 FROM entity_curation ec" +
				" WHERE ec.entity_type = ? AND ec.entity_id = t.id AND ec.field = 'sort')"
			args = append(args, src.entityType)
		}
		rows, err := s.read.QueryContext(ctx, q, args...)
		if err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		drift, err := countSortKeyDrift(rows)
		if err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		total += drift
	}
	return total, nil
}

func countSortKeyDrift(rows *sql.Rows) (int, error) {
	defer rows.Close()
	drift := 0
	for rows.Next() {
		var text, stored string
		if err := rows.Scan(&text, &stored); err != nil {
			return 0, err
		}
		if model.SortKey(text) != stored {
			drift++
		}
	}
	return drift, rows.Err()
}

// Each rollup-drift query counts entities whose stored rollup row disagrees with
// a recompute from the base tables (a missing rollup row counts as drift via the
// COALESCE(...,-1) sentinel, which never equals a real non-negative count).
const artistRollupDriftQ = `
SELECT COUNT(*) FROM artist a
LEFT JOIN artist_rollup ar ON ar.artist_id = a.id
WHERE COALESCE(ar.track_count, -1) <>
        (SELECT COUNT(DISTINCT t.item_id) FROM track t WHERE t.artist_id = a.id)
   OR COALESCE(ar.release_group_count, -1) <>
        (SELECT COUNT(*) FROM release_group rg WHERE rg.primary_artist_id = a.id)
   OR COALESCE(ar.total_duration_ms, -1) <>
        (SELECT COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0) FROM track t
           LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE t.artist_id = a.id)`

const genreRollupDriftQ = `
SELECT COUNT(*) FROM genre g
LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
WHERE COALESCE(gr.track_count, -1) <>
        (SELECT COUNT(DISTINCT ig.item_id) FROM item_genre ig WHERE ig.genre_id = g.id)
   OR COALESCE(gr.total_duration_ms, -1) <>
        (SELECT COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0) FROM item_genre ig
           LEFT JOIN item_file pf ON pf.item_id = ig.item_id
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE ig.genre_id = g.id)`

// liveArtSourceQ selects the source hashes still reachable from a live entity: an
// art_map row whose (entity_type, entity_id) exists in its table. A source not in
// this set is referenced only by dead-entity map rows, or none, so it and its
// thumbnails are reclaimable.
const liveArtSourceQ = `SELECT source_hash FROM art_map m WHERE
    (m.entity_type='track'         AND EXISTS (SELECT 1 FROM playable_item e WHERE e.id = m.entity_id))
 OR (m.entity_type='album'         AND EXISTS (SELECT 1 FROM album e         WHERE e.id = m.entity_id))
 OR (m.entity_type='release_group' AND EXISTS (SELECT 1 FROM release_group e WHERE e.id = m.entity_id))
 OR (m.entity_type='artist'        AND EXISTS (SELECT 1 FROM artist e        WHERE e.id = m.entity_id))
 OR (m.entity_type='genre'         AND EXISTS (SELECT 1 FROM genre e         WHERE e.id = m.entity_id))`

const releaseGroupRollupDriftQ = `
SELECT COUNT(*) FROM release_group rg
LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
WHERE COALESCE(rr.track_count, -1) <>
        (SELECT COUNT(DISTINCT t.item_id) FROM track t
           JOIN album al ON al.id = t.album_id WHERE al.release_group_id = rg.id)
   OR COALESCE(rr.total_duration_ms, -1) <>
        (SELECT COALESCE(SUM(` + itemEffectiveDurationExpr + `), 0) FROM track t
           JOIN album al ON al.id = t.album_id
           LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE al.release_group_id = rg.id)`
