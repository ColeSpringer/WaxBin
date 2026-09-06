package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// putCacheEntries seeds the enrichment cache, emptying whatever the seeded catalog
// arrived with so the census figures below are the fixture's alone.
func putCacheEntries(t *testing.T, st *sqlite.Store, rw *sql.DB, entries map[string]int) {
	t.Helper()
	if _, err := rw.Exec("DELETE FROM enrichment_cache"); err != nil {
		t.Fatalf("empty the cache: %v", err)
	}
	for key, size := range entries {
		if err := st.EnrichmentCachePut(context.Background(), key, make([]byte, size)); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
	}
}

// backdateCache ages one entry so an age policy has something to act on without a sleep.
func backdateCache(t *testing.T, rw *sql.DB, key string, age time.Duration) int64 {
	t.Helper()
	stamp := time.Now().Add(-age).UnixNano()
	if _, err := rw.Exec("UPDATE enrichment_cache SET fetched_at = ? WHERE cache_key = ?", stamp, key); err != nil {
		t.Fatalf("backdate %q: %v", key, err)
	}
	return stamp
}

func cacheKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT cache_key FROM enrichment_cache ORDER BY cache_key")
	if err != nil {
		t.Fatalf("list cache keys: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, k)
	}
	return out
}

// TestEnrichmentCacheStatsGroupsByKind: the breakdown is what a prune decision reads, so
// the kind has to be the provider and endpoint rather than the whole key, and the
// archive's group records have to stand out as the share a prune leaves alone.
func TestEnrichmentCacheStatsGroupsByKind(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	rw := writeConn(t, dbPath)

	if _, err := rw.Exec("DELETE FROM enrichment_cache"); err != nil {
		t.Fatalf("empty the cache: %v", err)
	}
	empty, err := st.EnrichmentCacheStats(ctx)
	if err != nil {
		t.Fatalf("EnrichmentCacheStats: %v", err)
	}
	if empty.Rows != 0 || empty.OldestAt != 0 || empty.NewestAt != 0 || len(empty.Kinds) != 0 {
		t.Errorf("empty cache report = %+v, want zeroes throughout", empty)
	}

	putCacheEntries(t, st, rw, map[string]int{
		"mb:artist:a":    100,
		"mb:artist:b":    50,
		"mb:rg-search:x": 300,
		"caa:rg-front:g": 20,
		"odd":            10,
	})
	rep, err := st.EnrichmentCacheStats(ctx)
	if err != nil {
		t.Fatalf("EnrichmentCacheStats: %v", err)
	}
	if rep.Rows != 5 || rep.Bytes != 480 {
		t.Errorf("totals = %d rows / %d bytes, want 5 and 480", rep.Rows, rep.Bytes)
	}
	if rep.ExemptRows != 1 || rep.ExemptBytes != 20 {
		t.Errorf("exempt = %d rows / %d bytes, want 1 and 20", rep.ExemptRows, rep.ExemptBytes)
	}
	if rep.OldestAt == 0 || rep.NewestAt < rep.OldestAt {
		t.Errorf("stamps = %d to %d, want a real range", rep.OldestAt, rep.NewestAt)
	}
	want := []struct {
		kind   string
		rows   int
		bytes  int64
		exempt bool
	}{
		{"mb:rg-search", 1, 300, false},
		{"mb:artist", 2, 150, false},
		{"caa:rg-front", 1, 20, true},
		{"odd", 1, 10, false},
	}
	if len(rep.Kinds) != len(want) {
		t.Fatalf("kinds = %+v, want %d entries", rep.Kinds, len(want))
	}
	for i, w := range want {
		got := rep.Kinds[i]
		if got.Kind != w.kind || got.Rows != w.rows || got.Bytes != w.bytes || got.Exempt != w.exempt {
			t.Errorf("kinds[%d] = %+v, want %s %d/%d exempt=%v", i, got, w.kind, w.rows, w.bytes, w.exempt)
		}
	}
}

// TestPruneEnrichmentCacheDropsOnlyEntriesPastTheAge: the archive's group record is not
// a cached answer, so no age reaches it.
func TestPruneEnrichmentCacheDropsOnlyEntriesPastTheAge(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	rw := writeConn(t, dbPath)
	putCacheEntries(t, st, rw, map[string]int{
		"mb:artist:old":  100,
		"mb:artist:old2": 40,
		"mb:artist:new":  60,
		"mb:rg-search:x": 300,
		"caa:rg-front:g": 20,
	})
	backdateCache(t, rw, "mb:artist:old", 90*24*time.Hour)
	backdateCache(t, rw, "mb:artist:old2", 90*24*time.Hour)
	backdateCache(t, rw, "caa:rg-front:g", 90*24*time.Hour)

	removed, freed, err := st.PruneEnrichmentCache(ctx, int64(60*24*time.Hour), -1)
	if err != nil {
		t.Fatalf("PruneEnrichmentCache: %v", err)
	}
	if removed != 2 || freed != 140 {
		t.Errorf("pruned %d rows / %d bytes, want 2 and 140", removed, freed)
	}
	got := cacheKeys(t, roConn(t, dbPath))
	wantLeft := []string{"caa:rg-front:g", "mb:artist:new", "mb:rg-search:x"}
	if len(got) != len(wantLeft) {
		t.Fatalf("left %v, want %v", got, wantLeft)
	}
	for i, w := range wantLeft {
		if got[i] != w {
			t.Errorf("left[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestPruneEnrichmentCacheEvictsOldestFirstToTheBudget: the budget bounds the prunable
// rows, so the exempt record neither counts toward it nor goes.
func TestPruneEnrichmentCacheEvictsOldestFirstToTheBudget(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	rw := writeConn(t, dbPath)
	putCacheEntries(t, st, rw, map[string]int{
		"caa:rg-front:g": 20,
		"mb:artist:a":    100,
		"mb:artist:b":    100,
		"mb:artist:c":    100,
	})
	backdateCache(t, rw, "caa:rg-front:g", 40*24*time.Hour)
	backdateCache(t, rw, "mb:artist:a", 30*24*time.Hour)
	backdateCache(t, rw, "mb:artist:b", 20*24*time.Hour)
	backdateCache(t, rw, "mb:artist:c", 10*24*time.Hour)

	removed, freed, err := st.PruneEnrichmentCache(ctx, -1, 200)
	if err != nil {
		t.Fatalf("PruneEnrichmentCache: %v", err)
	}
	if removed != 1 || freed != 100 {
		t.Errorf("pruned %d rows / %d bytes, want the oldest prunable row alone", removed, freed)
	}
	got := cacheKeys(t, roConn(t, dbPath))
	wantLeft := []string{"caa:rg-front:g", "mb:artist:b", "mb:artist:c"}
	if len(got) != len(wantLeft) {
		t.Fatalf("left %v, want %v", got, wantLeft)
	}
	for i, w := range wantLeft {
		if got[i] != w {
			t.Errorf("left[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestPruneEnrichmentCacheNeedsABound: a prune with neither bound is a caller bug, and
// reporting nothing removed would read as an empty cache.
func TestPruneEnrichmentCacheNeedsABound(t *testing.T) {
	st, _, _ := openStoreAt(t)
	if _, _, err := st.PruneEnrichmentCache(context.Background(), -1, -1); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unbounded prune err = %v, want CodeInvalid", err)
	}
}
