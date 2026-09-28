package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// oweLyrics records a failed lyrics lookup for id, the way the apply does.
func oweLyrics(t *testing.T, st *Store, id int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		return st.settleMarkerTx(ctx, tx, enrichEntityLyrics, id, "lrclib", false, true, false, "")
	}); err != nil {
		t.Fatalf("owe %d: %v", id, err)
	}
}

// TestTheOwedSweepInstantSeparatesPasses: the instant a pass's owed sweep measures
// against falls after every lookup deferred before it and before every one deferred
// after it, even when the wall clock has not moved in between, as it often has not on a
// clock that ticks every 15.6ms. A lookup an earlier pass deferred is asked, and one this
// pass deferred waits for the next.
func TestTheOwedSweepInstantSeparatesPasses(t *testing.T) {
	ctx := context.Background()
	st, _ := entityFixture(t)
	// A stamp floor ahead of the wall clock stands in for a clock that has not ticked
	// since the last stamp.
	st.wmu.Lock()
	st.lastStampNS = time.Now().Add(time.Hour).UnixNano()
	st.wmu.Unlock()

	oweLyrics(t, st, 1)
	asOf, err := st.ExpireDeferredLookups(ctx, 0)
	if err != nil || asOf == 0 {
		t.Fatalf("ExpireDeferredLookups = %d, %v, want an instant", asOf, err)
	}
	oweLyrics(t, st, 2)

	due := func(id int64) bool {
		t.Helper()
		var n int
		q := "SELECT COUNT(*) FROM (SELECT ? AS id) x WHERE " +
			notEnriched(enrichEntityLyrics, "x.id", model.EnrichQueueOptions{Sweep: model.SweepDeferred, DeferredBefore: asOf})
		if err := st.read.QueryRowContext(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("owed sweep: %v", err)
		}
		return n == 1
	}
	if !due(1) {
		t.Error("the owed sweep skips a lookup an earlier pass deferred")
	}
	if due(2) {
		t.Error("the owed sweep takes a lookup this pass deferred")
	}
}

// TestOwedLookupsUseTheirIndex: every ordinary pass looks for owed lookups and settles
// the old ones, so both statements seek the owed rows' own index rather than scan every
// marker the catalog holds.
func TestOwedLookupsUseTheirIndex(t *testing.T) {
	st, _ := entityFixture(t)
	for name, plan := range map[string]string{
		"probe":  queryPlan(t, st, owedLookupsExist),
		"expiry": queryPlan(t, st, expireOwedLookups, int64(1)),
	} {
		if !strings.Contains(plan, "entity_enrichment_owed") {
			t.Errorf("%s plan does not use the owed index:\n%s", name, plan)
		}
	}
}

// TestNothingOwedTakesNoWriteLock: a pass with nothing owed settles nothing, so it only
// reads, and the nightly run with nothing to do neither waits on another writer nor
// holds one up.
func TestNothingOwedTakesNoWriteLock(t *testing.T) {
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "test", BusyTimeoutMS: 50})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	raw, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold the write lock: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") })

	asOf, err := st.ExpireDeferredLookups(ctx, time.Now().UnixNano())
	if err != nil || asOf != 0 {
		t.Errorf("ExpireDeferredLookups with nothing owed = %d, %v, want 0 without a write", asOf, err)
	}
}

// TestOwedLookupsStayOutOfTheMissSweeps: an owed lookup belongs to the owed sweep alone.
// The retry sweep and its probe take settled misses only, even when an owed lookup is
// older than the retry window, as it can be under a window shorter than a week, while
// the count reports every owed lookup as due, old or new, whatever the window.
func TestOwedLookupsStayOutOfTheMissSweeps(t *testing.T) {
	ctx := context.Background()
	st, _ := entityFixture(t)
	const owed, miss, match, recent = 1, 2, 3, 4
	oweLyrics(t, st, owed)
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		if err := st.markEnrichedTx(ctx, tx, enrichEntityLyrics, miss, "lrclib", false, ""); err != nil {
			return err
		}
		return st.markEnrichedTx(ctx, tx, enrichEntityLyrics, match, "lrclib", true, "")
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, "UPDATE entity_enrichment SET enriched_at = 1"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	oweLyrics(t, st, recent)
	var cutoff int64
	if err := st.read.QueryRowContext(ctx, "SELECT enriched_at - 1 FROM entity_enrichment WHERE entity_id = ?", recent).Scan(&cutoff); err != nil {
		t.Fatalf("read the recent stamp: %v", err)
	}
	selected := func(sweep model.EnrichSweep, missCutoff int64) []int64 {
		t.Helper()
		q := "SELECT id FROM (SELECT 1 AS id UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4) x WHERE " +
			notEnriched(enrichEntityLyrics, "x.id", model.EnrichQueueOptions{Sweep: sweep, MissCutoff: missCutoff}) + " ORDER BY id"
		rows, err := st.read.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	for _, tc := range []struct {
		name  string
		sweep model.EnrichSweep
		cut   int64
		want  string
	}{
		{"retry", model.SweepRetry, cutoff, "[2]"},
		{"due with a window", model.SweepDue, cutoff, "[1 2 4]"},
		{"due without one", model.SweepDue, 0, "[1 4]"},
	} {
		if got := fmt.Sprint(selected(tc.sweep, tc.cut)); got != tc.want {
			t.Errorf("%s sweep = %s, want %s", tc.name, got, tc.want)
		}
	}

	if _, err := st.write.ExecContext(ctx, "DELETE FROM entity_enrichment WHERE entity_id = ?", miss); err != nil {
		t.Fatalf("drop the miss: %v", err)
	}
	if due, err := st.ExpiredMissesExist(ctx, cutoff); err != nil || due {
		t.Errorf("ExpiredMissesExist with only an old owed lookup = %v, %v, want false", due, err)
	}
}
