package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// seedRungs resolves one covered track at each of the given boxes, leaving a
// thumb_cache row per rung. It returns the track's pid and the source bytes.
//
// It checks that it delivered what it promises. Its fixture is 400x400, so a box at or
// above 512 rounds to a rung the source already fits and writes no row at all, and two
// boxes inside one rung write a single row between them. Either leaves a caller counting
// rows or backdating one to fail several frames away from the argument that caused it.
func seedRungs(t *testing.T, st *sqlite.Store, libID int64, boxes ...int) (model.PID, []byte) {
	t.Helper()
	raw := sizedCoverPNG(t, 400, 400)
	pid := putCoveredTrack(t, st, libID, "/lib/a.flac", "ess-a", "A", "Album",
		stamped(t, raw, model.SourceTag, "", ""))
	seeded := map[int]bool{}
	for _, box := range boxes {
		blob, err := st.ResolveArt(context.Background(),
			model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, box)
		if err != nil {
			t.Fatalf("resolve at %d: %v", box, err)
		}
		if !blob.Thumbnail {
			t.Fatalf("box %d rounds to rung %d, which the 400x400 fixture already fits, so "+
				"nothing was cached; seedRungs promises a row per box", box, blob.Box)
		}
		if seeded[blob.Box] {
			t.Fatalf("boxes %v collapse onto rung %d; seedRungs promises a row per box", boxes, blob.Box)
		}
		seeded[blob.Box] = true
	}
	return pid, raw
}

// writeConn opens a second read-write connection, so a test can age rows the store
// deliberately has no API to backdate.
func writeConn(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	// The busy timeout matters: the store owns a write connection on the same file,
	// and a backdate that lands mid-commit would otherwise fail outright instead of
	// waiting the moment out.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// backdate moves one rung's created_at to age before now, so an age policy has
// something to act on without the test sleeping.
func backdate(t *testing.T, db *sql.DB, size int, age time.Duration) {
	t.Helper()
	if _, err := db.Exec("UPDATE thumb_cache SET created_at = ? WHERE size = ?",
		time.Now().Add(-age).UnixNano(), size); err != nil {
		t.Fatalf("backdate rung %d: %v", size, err)
	}
}

// TestThumbCacheStatsCountsEachRungSeparately is the census the size surface exists
// for: one cover browsed at three rungs is three derivatives, and the breakdown has
// to show that rather than reporting a single cached cover.
func TestThumbCacheStatsCountsEachRungSeparately(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	_, raw := seedRungs(t, st, lib.ID, 48, 96, 192)

	rep, err := st.ThumbCacheStats(context.Background())
	if err != nil {
		t.Fatalf("ThumbCacheStats: %v", err)
	}
	if rep.Rows != 3 {
		t.Errorf("Rows = %d, want 3 (one per rung)", rep.Rows)
	}
	if rep.Sources != 1 {
		t.Errorf("Sources = %d, want 1 (all three came from one cover)", rep.Sources)
	}
	if rep.ArtSources != 1 || rep.ArtSourceBytes != int64(len(raw)) {
		t.Errorf("originals = %d rows/%d bytes, want 1/%d", rep.ArtSources, rep.ArtSourceBytes, len(raw))
	}
	if len(rep.Rungs) != 3 {
		t.Fatalf("Rungs = %+v, want one entry per rung", rep.Rungs)
	}
	// Largest box first: the rung worth pruning is the one that costs the most.
	var total int64
	for i, r := range rep.Rungs {
		if want := []int{192, 96, 48}[i]; r.Size != want {
			t.Errorf("Rungs[%d].Size = %d, want %d (largest box first)", i, r.Size, want)
		}
		if r.Rows != 1 {
			t.Errorf("Rungs[%d].Rows = %d, want 1", i, r.Rows)
		}
		if r.Bytes <= 0 {
			t.Errorf("Rungs[%d].Bytes = %d, want the stored blob's length", i, r.Bytes)
		}
		total += r.Bytes
	}
	if rep.Bytes != total {
		t.Errorf("Bytes = %d, want %d (the sum of the rungs)", rep.Bytes, total)
	}

	db := roConn(t, dbPath)
	if n := scalarInt64(t, db, "SELECT COALESCE(SUM(LENGTH(data)),0) FROM thumb_cache"); n != rep.Bytes {
		t.Errorf("Bytes = %d, want %d (what the table actually holds)", rep.Bytes, n)
	}
	if rep.OldestAt == 0 || rep.OldestAt > rep.NewestAt {
		t.Errorf("age range = [%d, %d], want a populated ascending range", rep.OldestAt, rep.NewestAt)
	}
}

// TestThumbCacheStatsReportsOriginalsWithNoDerivatives pins that an empty cache still
// reports the originals it would be derived from. Zeros across the board would read as
// "no artwork at all", which is the one thing an empty cache does not mean.
func TestThumbCacheStatsReportsOriginalsWithNoDerivatives(t *testing.T) {
	st, _, lib := openStoreAt(t)
	raw := sizedCoverPNG(t, 400, 400)
	putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "A", "Album",
		stamped(t, raw, model.SourceTag, "", ""))

	rep, err := st.ThumbCacheStats(context.Background())
	if err != nil {
		t.Fatalf("ThumbCacheStats: %v", err)
	}
	if rep.Rows != 0 || rep.Bytes != 0 || len(rep.Rungs) != 0 {
		t.Errorf("cache = %d rows/%d bytes/%d rungs, want empty", rep.Rows, rep.Bytes, len(rep.Rungs))
	}
	if rep.OldestAt != 0 || rep.NewestAt != 0 {
		t.Errorf("age range = [%d, %d], want zeroes for an empty cache", rep.OldestAt, rep.NewestAt)
	}
	if rep.ArtSources != 1 || rep.ArtSourceBytes != int64(len(raw)) {
		t.Errorf("originals = %d rows/%d bytes, want 1/%d", rep.ArtSources, rep.ArtSourceBytes, len(raw))
	}
}

// TestPruneThumbnailsDropsOnlyEntriesPastTheAge pins the age bound's edge: the rung
// inside the window survives.
func TestPruneThumbnailsDropsOnlyEntriesPastTheAge(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48, 192)
	db := roConn(t, dbPath)
	backdate(t, writeConn(t, dbPath), 192, 72*time.Hour)

	removed, freed, err := st.PruneThumbnails(context.Background(), int64(24*time.Hour), -1)
	if err != nil {
		t.Fatalf("PruneThumbnails: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the backdated rung)", removed)
	}
	if freed <= 0 {
		t.Errorf("freed = %d, want the removed rung's bytes", freed)
	}
	if n := scalarInt64(t, db, thumbSizes); n != 48 {
		t.Errorf("surviving rung = %d, want 48 (the one inside the window)", n)
	}
}

// TestPruneThumbnailsKeepsTheNewestInsideTheByteBudget pins the size bound: with
// nothing but a creation stamp to order by, the budget evicts oldest first.
func TestPruneThumbnailsKeepsTheNewestInsideTheByteBudget(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48, 96, 192)
	w := writeConn(t, dbPath)
	backdate(t, w, 192, 72*time.Hour)
	backdate(t, w, 96, 48*time.Hour)
	backdate(t, w, 48, 24*time.Hour)

	db := roConn(t, dbPath)
	keep := scalarInt64(t, db, "SELECT COALESCE(SUM(LENGTH(data)),0) FROM thumb_cache WHERE size IN (48, 96)")

	removed, freed, err := st.PruneThumbnails(context.Background(), -1, keep)
	if err != nil {
		t.Fatalf("PruneThumbnails: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (the oldest rung alone overflows the budget)", removed)
	}
	if got := scalarInt64(t, db, "SELECT COALESCE(SUM(LENGTH(data)),0) FROM thumb_cache"); got != keep {
		t.Errorf("cache holds %d bytes, want %d (exactly the budget)", got, keep)
	}
	if freed <= 0 {
		t.Errorf("freed = %d, want the evicted rung's bytes", freed)
	}
	if n := scalarInt64(t, db, "SELECT COUNT(*) FROM thumb_cache WHERE size = 192"); n != 0 {
		t.Error("the oldest rung survived a budget it does not fit in")
	}
}

// TestPruneThumbnailsZeroBudgetEmptiesTheCache pins that a zero budget is a real
// budget rather than "no bound given": every derivative regenerates on request, so
// emptying the cache is a legitimate ask.
func TestPruneThumbnailsZeroBudgetEmptiesTheCache(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48, 192)

	removed, _, err := st.PruneThumbnails(context.Background(), -1, 0)
	if err != nil {
		t.Fatalf("PruneThumbnails: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (nothing fits in a zero budget)", removed)
	}
	if n := scalarInt64(t, roConn(t, dbPath), thumbRows); n != 0 {
		t.Errorf("thumb_cache rows = %d, want 0", n)
	}
}

// TestPruneThumbnailsRefusesWithNoPolicy pins that an unbounded prune is refused
// rather than silently doing nothing: a caller that passes neither bound has a bug,
// and "removed 0" would read as an empty cache.
func TestPruneThumbnailsRefusesWithNoPolicy(t *testing.T) {
	st, _, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48)

	_, _, err := st.PruneThumbnails(context.Background(), -1, -1)
	if waxerr.CodeOf(err) != waxerr.CodeInvalid {
		t.Errorf("err = %v, want a CodeInvalid refusal", err)
	}
}

// TestPruneThumbnailsZeroAgeEmptiesTheCache is the age bound's mirror of the zero
// budget: everything in the cache was generated at least zero ago, so a zero age is a
// real instruction rather than the absence of one. The two bounds have to agree on
// what zero means, or `--older-than 0d` refuses an invocation that named a bound.
func TestPruneThumbnailsZeroAgeEmptiesTheCache(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48, 192)

	removed, _, err := st.PruneThumbnails(context.Background(), 0, -1)
	if err != nil {
		t.Fatalf("PruneThumbnails: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (nothing is newer than now)", removed)
	}
	if n := scalarInt64(t, roConn(t, dbPath), thumbRows); n != 0 {
		t.Errorf("thumb_cache rows = %d, want 0", n)
	}
}

// TestThumbCacheStatsRungsSumToTheTotal pins the invariant a reader checks by eye.
// The census answers from one read snapshot, so a writer generating thumbnails
// alongside it cannot leave the breakdown adding up to more than the total above it.
func TestThumbCacheStatsRungsSumToTheTotal(t *testing.T) {
	st, _, lib := openStoreAt(t)
	seedRungs(t, st, lib.ID, 48, 96, 192)

	rep, err := st.ThumbCacheStats(context.Background())
	if err != nil {
		t.Fatalf("ThumbCacheStats: %v", err)
	}
	var total int64
	var rows int
	for _, r := range rep.Rungs {
		total += r.Bytes
		rows += r.Rows
	}
	if total != rep.Bytes || rows != rep.Rows {
		t.Errorf("rungs sum to %d rows/%d bytes, header says %d/%d", rows, total, rep.Rows, rep.Bytes)
	}
}
