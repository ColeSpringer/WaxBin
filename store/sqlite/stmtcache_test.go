package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// stmtClosed reports whether st refuses to run, which is what a closed statement does.
func stmtClosed(t *testing.T, st *sql.Stmt) bool {
	t.Helper()
	var n int
	err := st.QueryRowContext(context.Background()).Scan(&n)
	if err != nil && !strings.Contains(err.Error(), "statement is closed") {
		t.Fatalf("run statement: %v", err)
	}
	return err != nil
}

// TestStmtCacheReusesAndEvicts: a text is prepared once and handed out again; past the
// cap the least recently used statement is closed, unless a reader still holds it, in
// which case it closes when that reader is done.
func TestStmtCacheReusesAndEvicts(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	c := &stmtCache{limit: 2}
	get := func(q string) *cachedStmt {
		t.Helper()
		e, err := c.acquire(ctx, st.rdb(), q)
		if err != nil {
			t.Fatalf("acquire %q: %v", q, err)
		}
		return e
	}

	one := get("SELECT 1")
	c.release(one)
	if again := get("SELECT 1"); again.stmt != one.stmt {
		t.Fatal("the same text was prepared twice")
	} else {
		c.release(again)
	}

	held := get("SELECT 1") // a reader running it while two other texts push it out
	c.release(get("SELECT 2"))
	c.release(get("SELECT 3"))
	if stmtClosed(t, held.stmt) {
		t.Fatal("an evicted statement closed under the reader holding it")
	}
	c.release(held)
	if !stmtClosed(t, held.stmt) {
		t.Error("an evicted statement stayed open after its last reader was done")
	}
	two := get("SELECT 2")
	c.release(two)
	c.release(get("SELECT 4"))
	c.release(get("SELECT 3")) // 3 is the more recent, so 2 goes
	if !stmtClosed(t, two.stmt) {
		t.Error("the least recently used statement stayed open past the cap")
	}
}

// TestStmtCacheFollowsTheReadPool: the cache moves to a new pool when told (swap), closing
// the old pool's statements, and a reader still holding a pool it does not hold is turned
// away rather than flushing what the live pool prepared.
func TestStmtCacheFollowsTheReadPool(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	other, err := sql.Open(driverName, "file:"+filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var c stmtCache
	first, err := c.acquire(ctx, st.rdb(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	c.release(first)
	if _, err := c.acquire(ctx, other, "SELECT 1"); err == nil {
		t.Fatal("a pool the cache does not hold was served")
	}
	if stmtClosed(t, first.stmt) {
		t.Fatal("a stale reader flushed the live pool's statement")
	}
	c.swap(other)
	if !stmtClosed(t, first.stmt) {
		t.Error("the old pool's statement stayed open after the swap")
	}
	second, err := c.acquire(ctx, other, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	c.release(second)
	if _, err := c.acquire(ctx, st.rdb(), "SELECT 1"); err == nil {
		t.Error("the pool the cache moved away from was served")
	}
}

// TestHotReadsSurviveAReopen: the cached reads prepare again on the pool a maintenance
// reopen opens, rather than reaching for the closed one.
func TestHotReadsSurviveAReopen(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", album: "Al"}).ItemPID
	reads := func() {
		t.Helper()
		if _, err := st.ItemByPID(ctx, pid); err != nil {
			t.Fatalf("item: %v", err)
		}
		if p, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), "", 10, false, ""); err != nil || len(p.Items) != 1 {
			t.Fatalf("query page: %+v (err %v)", p, err)
		}
		if p, err := st.BrowsePage(ctx, read.ListRecentlyAdded, read.BrowseOptions{}); err != nil || len(p.Items) != 1 {
			t.Fatalf("browse: %+v (err %v)", p, err)
		}
	}
	reads()
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := st.Reopen(ctx); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reads()
}

// TestStmtCacheUnderConcurrentReaders: readers sharing a cache smaller than the texts they
// run keep evicting each other's statements, and every read still answers its own query.
func TestStmtCacheUnderConcurrentReaders(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	c := &stmtCache{limit: 3}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				k := (g + i) % 6
				var got int
				if err := c.queryRowContext(ctx, st.rdb(), fmt.Sprintf("SELECT %d", k)).Scan(&got); err != nil || got != k {
					t.Errorf("SELECT %d = %d (err %v)", k, got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestHotReadsWriteTheirLimit: a page's limit is part of its statement's text. SQLite
// plans with a bound LIMIT's value, so binding the limit made every run of a cached
// statement prepare it again (BenchmarkReadScaling's browse cases ran three times slower).
func TestHotReadsWriteTheirLimit(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	stmts := map[string]string{}
	for _, list := range []read.DiscoveryList{read.ListAlphabetical, read.ListRandom} {
		stmt, _, err := st.browseStmt(ctx, list, read.BrowseOptions{Seed: 1}, 10, "test")
		if err != nil {
			t.Fatal(err)
		}
		stmts[string(list)] = stmt
	}
	stmt, _, err := st.queryPageStmt(ctx, query.New(query.EntityItems).Build(), "", 10, false, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	stmts["query page"] = stmt
	for name, stmt := range stmts {
		if strings.Contains(stmt, "LIMIT ?") || !strings.Contains(stmt, "LIMIT 11") {
			t.Errorf("%s does not write its limit of 10 (and one more): %s", name, stmt)
		}
	}
}
