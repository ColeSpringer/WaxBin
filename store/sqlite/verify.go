package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// DerivedReport is the result of the derived-data consistency check: a count of
// drift in each kind of writer-maintained denormalized state versus a fresh
// recompute from the source rows. All zeros means clean.
type DerivedReport struct {
	ItemsMissingFTS         int // present items with no search_fts row
	OrphanFTSRows           int // search_fts rows with no backing item
	ArtistRollupDrift       int // artists whose stored rollup != recompute
	GenreRollupDrift        int
	ReleaseGroupRollupDrift int
	SortKeyDrift            int // entities whose stored sort_key != regenerated
	BookDurationDrift       int // books whose stored total_duration_ms != summed parts
	BookISBNKeyDrift        int // books whose stored isbn_key != identity.ISBNKey(isbn)
	OrphanArtSources        int // art_source images with no live art_map references
	OrphanThumbnails        int // thumb_cache rows whose source is unreferenced
	// field_provenance rows under a "tag.<KEY>" whose key WaxBin has since reserved.
	// Nothing can read, edit, or unlock one, and the scan's own sweep cannot reach the
	// row on an item that holds no custom tags at all.
	OrphanReservedTagProvenance int
}

// Consistent reports whether the writer-maintained derived data is correct: FTS
// coverage, rollups, and generated sort keys. Orphan-art counts are excluded
// because a cover swap or an item deletion leaves reclaimable sources behind as a
// matter of course; Reclaimable reports those.
func (r DerivedReport) Consistent() bool {
	return r.SortKeyDrift == 0 && r.consistentApartFromSortKeys()
}

// SortKeyDriftOnly reports whether stale sort keys are the only inconsistency, so
// `db verify` can recommend --fix and nothing else: re-scanning repairs the other
// kinds of drift but never rewrites a sort key.
func (r DerivedReport) SortKeyDriftOnly() bool {
	return r.SortKeyDrift > 0 && r.consistentApartFromSortKeys()
}

func (r DerivedReport) consistentApartFromSortKeys() bool {
	return r.ItemsMissingFTS == 0 && r.OrphanFTSRows == 0 &&
		r.ArtistRollupDrift == 0 && r.GenreRollupDrift == 0 &&
		r.ReleaseGroupRollupDrift == 0 && r.BookDurationDrift == 0 &&
		r.BookISBNKeyDrift == 0
}

// Reclaimable reports whether `db verify --fix` would reclaim space: orphaned art
// sources or thumbnails with no live entity references, or provenance rows under a
// reserved tag key. It is informational, and independent of Consistent.
func (r DerivedReport) Reclaimable() bool {
	return r.OrphanArtSources > 0 || r.OrphanThumbnails > 0 || r.OrphanReservedTagProvenance > 0
}

// VerifyDerived checks FTS coverage, the maintained rollups, and the generated
// sort keys against the source rows. FTS field content is not diffed yet, only
// coverage. It never writes: `db verify` surfaces the report and the operator
// runs the matching repair.
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
		// A map row pointing at a deleted entity does not count as a reference here,
		// matching GCArt, which removes the stale map before deleting the source.
		{&rep.OrphanArtSources, "SELECT COUNT(*) FROM art_source WHERE hash NOT IN (" + liveArtSourceQ + ")"},
		{&rep.OrphanThumbnails, "SELECT COUNT(*) FROM thumb_cache WHERE source_hash NOT IN (" + liveArtSourceQ + ")"},
	}
	for _, c := range checks {
		if err := s.read.QueryRowContext(ctx, c.stmt).Scan(c.dst); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	// Stands apart from the table above because it is the one check with bound
	// arguments: the reserved key set lives in Go, not in the schema.
	q, args := reservedTagProvenanceQuery("SELECT COUNT(*)")
	if err := s.read.QueryRowContext(ctx, q, args...).Scan(&rep.OrphanReservedTagProvenance); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	isbnDrift, err := s.bookISBNKeyDrift(ctx)
	if err != nil {
		return nil, err
	}
	rep.BookISBNKeyDrift = isbnDrift

	drift, err := s.sortKeyDrift(ctx)
	if err != nil {
		return nil, err
	}
	rep.SortKeyDrift = drift
	return rep, nil
}

// bookISBNKeyDrift counts books whose stored isbn_key does not match recomputing it from
// the raw column. Like the sort keys it is generated in Go, so the comparison streams
// rather than running in SQL, and only rows with something in either column are read. A
// rescan rewrites the pair through upsertBook, which is the repair.
func (s *Store) bookISBNKeyDrift(ctx context.Context) (int, error) {
	const op = "store.VerifyDerived"
	rows, err := s.read.QueryContext(ctx,
		"SELECT isbn, isbn_key FROM book WHERE isbn <> '' OR isbn_key <> ''")
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	drift := 0
	for rows.Next() {
		var isbn, key string
		if err := rows.Scan(&isbn, &key); err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if identity.ISBNKey(isbn) != key {
			drift++
		}
	}
	return drift, waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
}

// sortKeyDrift counts rows whose stored sort key differs from regenerating it. It
// walks sortKeySources (sortkeys.go) and uses that source's own recompute
// expression, so the check and the repair cannot disagree; a divergence would
// present as permanent unfixable drift. A curated entity is checked against its
// sort override, which is what applyEntityFieldTx stored the key from.
//
// The tag-derived columns (track.artist_sort and friends) are deliberately absent:
// their input is a tag the catalog does not keep, and a locked one holds literal
// user text that no fix should repair.
//
// Sort keys are generated in Go, so this streams every row (O(n) time, O(1)
// memory) and recomputes model.SortKey per row. That is acceptable for `db
// verify`; if it ever needs to run hot, model.SortKey can be registered as a
// deterministic SQLite scalar function so the comparison runs in SQL.
func (s *Store) sortKeyDrift(ctx context.Context) (int, error) {
	const op = "store.VerifyDerived"
	total := 0
	for _, src := range sortKeySources {
		q, args := src.query("")
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
// thumbnails are reclaimable. The arms must mirror GCArt's slot list exactly, or
// verify reports as orphaned what GC correctly keeps.
const liveArtSourceQ = `SELECT source_hash FROM art_map m WHERE
    (m.entity_type='track'         AND EXISTS (SELECT 1 FROM playable_item e WHERE e.id = m.entity_id))
 OR (m.entity_type='episode'       AND EXISTS (SELECT 1 FROM playable_item e WHERE e.id = m.entity_id))
 OR (m.entity_type='album'         AND EXISTS (SELECT 1 FROM album e         WHERE e.id = m.entity_id))
 OR (m.entity_type='release_group' AND EXISTS (SELECT 1 FROM release_group e WHERE e.id = m.entity_id))
 OR (m.entity_type='artist'        AND EXISTS (SELECT 1 FROM artist e        WHERE e.id = m.entity_id))
 OR (m.entity_type='genre'         AND EXISTS (SELECT 1 FROM genre e         WHERE e.id = m.entity_id))
 OR (m.entity_type='podcast'       AND EXISTS (SELECT 1 FROM podcast e       WHERE e.id = m.entity_id))
 OR (m.entity_type='playlist'      AND EXISTS (SELECT 1 FROM playlist e      WHERE e.id = m.entity_id))`

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
