package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// ThumbCacheStats censuses the generated thumbnail cache: what it holds, the source
// images behind it, and a breakdown by ladder rung. It is read-only.
//
// The reads share one transaction, so they share one snapshot. The report path opens
// read-only precisely so it can run beside a live writer, and three separate reads off
// the pool would let a server generating thumbnails land between them, printing a
// breakdown that sums past the total above it.
//
// The byte sums come from LENGTH() on the blob column, which SQLite answers out of the
// record header without faulting in the overflow pages the blobs themselves live on,
// so the census does not read the cache it is measuring. The originals total comes
// from art_source's own size column for the same reason.
func (s *Store) ThumbCacheStats(ctx context.Context) (*model.ThumbCacheReport, error) {
	const op = "store.ThumbCacheStats"
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer func() { _ = tx.Rollback() }()

	var rep model.ThumbCacheReport
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(data)), 0), COUNT(DISTINCT source_hash),
		        COALESCE(MIN(created_at), 0), COALESCE(MAX(created_at), 0)
		 FROM thumb_cache`).
		Scan(&rep.Rows, &rep.Bytes, &rep.Sources, &rep.OldestAt, &rep.NewestAt); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(size), 0) FROM art_source").
		Scan(&rep.ArtSources, &rep.ArtSourceBytes); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT size, COUNT(*), COALESCE(SUM(LENGTH(data)), 0) FROM thumb_cache
		 GROUP BY size ORDER BY size DESC`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.ThumbRung
		if err := rows.Scan(&r.Size, &r.Rows, &r.Bytes); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		rep.Rungs = append(rep.Rungs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return &rep, nil
}

// PruneThumbnails drops cached thumbnails to fit a retention policy, returning how
// many rows went and how many bytes they were holding. olderThanNS drops entries
// generated at least that long ago; maxBytes then evicts until the cache fits that
// budget. A negative leaves either bound off, and the two agree on zero: every entry
// is at least zero old and none fits in zero bytes, so either zero empties the cache.
// At least one bound is required, since a prune with neither is a caller bug and
// reporting nothing removed would read as an empty cache.
//
// Both bounds order by generation time rather than by use. Nothing records when a
// thumbnail was last served and nothing should: the stamp would turn every art resolve
// into a write. Evicting a hot entry costs the decode that regenerates it on the next
// request, not the picture.
//
// The in-process thumbnail cache is deliberately left alone. Its entries are still
// valid pictures and it is bounded on its own, and a served entry is never written
// back, so holding one cannot undo the prune.
func (s *Store) PruneThumbnails(ctx context.Context, olderThanNS, maxBytes int64) (removed int, freed int64, err error) {
	const op = "store.PruneThumbnails"
	if olderThanNS < 0 && maxBytes < 0 {
		return 0, 0, waxerr.New(waxerr.CodeInvalid, op, "prune needs an age or a byte budget")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		if olderThanNS >= 0 {
			// The cutoff is inclusive, which is what makes a zero age empty the cache: a
			// coarse wall clock hands a just-written row the same nanosecond the prune
			// reads, and an exclusive bound would leave it behind.
			n, b, e := deleteThumbsTx(ctx, tx, "created_at <= ?", nowNS()-olderThanNS)
			if e != nil {
				return e
			}
			removed, freed = removed+n, freed+b
		}
		if maxBytes >= 0 {
			// Keep the newest entries whose running total still fits and drop the rest.
			// The window's ordering ends in the primary key, so it is total: no two rows
			// share a running total, and the row that crosses the budget is the one
			// dropped rather than a group of peers.
			n, b, e := deleteThumbsTx(ctx, tx,
				`(source_hash, size) IN (
					SELECT source_hash, size FROM (
						SELECT source_hash, size, SUM(LENGTH(data)) OVER (
							ORDER BY created_at DESC, source_hash, size
							ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS running
						FROM thumb_cache)
					WHERE running > ?)`, maxBytes)
			if e != nil {
				return e
			}
			removed, freed = removed+n, freed+b
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return removed, freed, nil
}

// deleteThumbsTx deletes the thumbnails matching where and reports what they held.
// RETURNING is what makes the figure available at all: a row's cost is only knowable
// while it still exists, and measuring it with a matching SELECT first would evaluate
// where twice, which for the budget pass means scanning and sorting the whole table
// again to delete one prefix of it.
func deleteThumbsTx(ctx context.Context, tx *sql.Tx, where string, arg any) (int, int64, error) {
	const op = "store.PruneThumbnails"
	rows, err := tx.QueryContext(ctx,
		"DELETE FROM thumb_cache WHERE "+where+" RETURNING LENGTH(data)", arg)
	if err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var removed int
	var freed int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		removed++
		freed += n
	}
	if err := rows.Err(); err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return removed, freed, nil
}
