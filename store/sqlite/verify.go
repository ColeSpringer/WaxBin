package sqlite

import (
	"context"
	"database/sql"

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
}

// Consistent reports whether every derived-data check passed.
func (r DerivedReport) Consistent() bool {
	return r == DerivedReport{}
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
// O(1) memory because it never buffers the result set) and recomputes model.SortKey per
// row. That is acceptable for `db verify`, a deliberate maintenance operation; if
// it ever needs to run hot, model.SortKey can be registered as a deterministic
// SQLite scalar function so the comparison runs entirely in SQL.
func (s *Store) sortKeyDrift(ctx context.Context) (int, error) {
	const op = "store.VerifyDerived"
	sources := []struct{ text, table string }{
		{"title", "playable_item"},
		{"name", "artist"},
		{"name", "genre"},
		{"title", "release_group"},
		{"title", "album"},
	}
	total := 0
	for _, src := range sources {
		rows, err := s.read.QueryContext(ctx, "SELECT "+src.text+", sort_key FROM "+src.table)
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
        (SELECT COALESCE(SUM(f.duration_ms), 0) FROM track t
           LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE t.artist_id = a.id)`

const genreRollupDriftQ = `
SELECT COUNT(*) FROM genre g
LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
WHERE COALESCE(gr.track_count, -1) <>
        (SELECT COUNT(DISTINCT ig.item_id) FROM item_genre ig WHERE ig.genre_id = g.id)
   OR COALESCE(gr.total_duration_ms, -1) <>
        (SELECT COALESCE(SUM(f.duration_ms), 0) FROM item_genre ig
           LEFT JOIN item_file pf ON pf.item_id = ig.item_id AND pf.role = 'primary'
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE ig.genre_id = g.id)`

const releaseGroupRollupDriftQ = `
SELECT COUNT(*) FROM release_group rg
LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
WHERE COALESCE(rr.track_count, -1) <>
        (SELECT COUNT(DISTINCT t.item_id) FROM track t
           JOIN album al ON al.id = t.album_id WHERE al.release_group_id = rg.id)
   OR COALESCE(rr.total_duration_ms, -1) <>
        (SELECT COALESCE(SUM(f.duration_ms), 0) FROM track t
           JOIN album al ON al.id = t.album_id
           LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
           LEFT JOIN file f ON f.id = pf.file_id
         WHERE al.release_group_id = rg.id)`
