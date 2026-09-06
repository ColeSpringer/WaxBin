package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// enrichCacheExemptPrefix names the keys a prune leaves alone. They are not cached
// responses but the only carrier of which release's bytes a group holds and of the
// validator a forced re-fetch spends, and nothing re-creates one short of a full
// download per group. The census marks the kind exempt and the prune skips it.
const enrichCacheExemptPrefix = "caa:"

// EnrichmentCacheStats censuses the enrichment response cache: what it holds, the range
// of fetch stamps, and a breakdown by request kind. It is read-only.
//
// One scan folded in Go, rather than SQL string surgery over the key: a kind is
// everything up to the second colon, and the whole key when it has fewer, which no
// substring expression states as plainly. LENGTH() on the blob reads the record header
// rather than the payload, the same reason ThumbCacheStats gives.
func (s *Store) EnrichmentCacheStats(ctx context.Context) (*model.EnrichmentCacheReport, error) {
	const op = "store.EnrichmentCacheStats"
	rows, err := s.read.QueryContext(ctx, "SELECT cache_key, LENGTH(payload), fetched_at FROM enrichment_cache")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	var rep model.EnrichmentCacheReport
	byKind := map[string]*model.EnrichmentCacheKind{}
	for rows.Next() {
		var key string
		var size, fetchedAt int64
		if err := rows.Scan(&key, &size, &fetchedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		rep.Rows++
		rep.Bytes += size
		// Against the row count rather than against zero: zero is a stamp like any other,
		// and comparing to it would let the second row overwrite a genuine epoch oldest.
		if rep.Rows == 1 || fetchedAt < rep.OldestAt {
			rep.OldestAt = fetchedAt
		}
		if fetchedAt > rep.NewestAt {
			rep.NewestAt = fetchedAt
		}
		kind := enrichCacheKind(key)
		exempt := strings.HasPrefix(key, enrichCacheExemptPrefix)
		if exempt {
			rep.ExemptRows++
			rep.ExemptBytes += size
		}
		k := byKind[kind]
		if k == nil {
			k = &model.EnrichmentCacheKind{Kind: kind, Exempt: exempt}
			byKind[kind] = k
		}
		k.Rows++
		k.Bytes += size
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	for _, k := range byKind {
		rep.Kinds = append(rep.Kinds, *k)
	}
	sort.Slice(rep.Kinds, func(i, j int) bool {
		if rep.Kinds[i].Bytes != rep.Kinds[j].Bytes {
			return rep.Kinds[i].Bytes > rep.Kinds[j].Bytes
		}
		return rep.Kinds[i].Kind < rep.Kinds[j].Kind
	})
	return &rep, nil
}

// enrichCacheKind is a cache key's provider and endpoint prefix, everything up to the
// second colon, or the whole key when it has fewer than two so a one-colon key does not
// collapse to its provider.
func enrichCacheKind(key string) string {
	first := strings.Index(key, ":")
	if first < 0 {
		return key
	}
	second := strings.Index(key[first+1:], ":")
	if second < 0 {
		return key
	}
	return key[:first+1+second]
}

// PruneEnrichmentCache drops cached provider responses to fit a retention policy,
// returning how many rows went and how many bytes they were holding. It is
// PruneThumbnails' twin down to the bounds: olderThanNS drops entries fetched at least
// that long ago, maxBytes then evicts oldest first until what is left fits, a negative
// leaves either off, and either zero empties the cache. At least one is required.
//
// A pruned answer costs one request the next time its target is re-asked (a forced run,
// a retry of an expired miss, a scoped run), never a catalog value: the markers, not the
// cache, decide what is asked at all.
//
// The Cover Art Archive's group records are exempt on both passes, and do not count
// toward the budget. Each says which release's bytes a group holds and carries the
// validator a forced re-fetch spends, so losing one costs a full cover download rather
// than a lookup.
func (s *Store) PruneEnrichmentCache(ctx context.Context, olderThanNS, maxBytes int64) (removed int, freed int64, err error) {
	const op = "store.PruneEnrichmentCache"
	if olderThanNS < 0 && maxBytes < 0 {
		return 0, 0, waxerr.New(waxerr.CodeInvalid, op, "prune needs an age or a byte budget")
	}
	const notExempt = "cache_key NOT LIKE '" + enrichCacheExemptPrefix + "%'"
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		if olderThanNS >= 0 {
			// Inclusive, for PruneThumbnails' reason: a coarse wall clock hands a
			// just-written row the same nanosecond the prune reads.
			n, b, e := deleteEnrichmentCacheTx(ctx, tx, notExempt+" AND fetched_at <= ?", nowNS()-olderThanNS)
			if e != nil {
				return e
			}
			removed, freed = removed+n, freed+b
		}
		if maxBytes >= 0 {
			// The window sums the prunable rows alone, so the budget bounds what can
			// actually go. Its ordering ends in the key, so it is total.
			n, b, e := deleteEnrichmentCacheTx(ctx, tx,
				notExempt+` AND cache_key IN (
					SELECT cache_key FROM (
						SELECT cache_key, SUM(LENGTH(payload)) OVER (
							ORDER BY fetched_at DESC, cache_key
							ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS running
						FROM enrichment_cache WHERE `+notExempt+`)
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

// deleteEnrichmentCacheTx deletes the entries matching where and reports what they held,
// through RETURNING for the reason deleteThumbsTx gives.
func deleteEnrichmentCacheTx(ctx context.Context, tx *sql.Tx, where string, arg any) (int, int64, error) {
	const op = "store.PruneEnrichmentCache"
	rows, err := tx.QueryContext(ctx,
		"DELETE FROM enrichment_cache WHERE "+where+" RETURNING LENGTH(payload)", arg)
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
