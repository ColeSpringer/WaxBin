package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// catalogSeed holds the bytes of an empty migrated catalog, built once for the
// whole test binary. Applying the schema costs several times what copying the
// file it produces costs, and almost every test here wants a catalog of its own,
// so the suite used to spend most of its wall clock re-running the same DDL
// through a race-instrumented SQLite.
var catalogSeed = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "waxbin-seed")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "seed.db")
	st, err := Open(context.Background(), OpenOptions{Path: path, Owner: "seed"})
	if err != nil {
		return nil, err
	}
	// Close checkpoints the WAL, so the one file carries the whole catalog.
	if err := st.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// seedCatalog writes an empty migrated catalog to path and returns path. Opening
// it finds no pending migration and lands where a first open would.
func seedCatalog(tb testing.TB, path string) string {
	tb.Helper()
	blob, err := catalogSeed()
	if err != nil {
		tb.Fatalf("build catalog seed: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		tb.Fatalf("write catalog seed: %v", err)
	}
	return path
}

// SeedCatalog is seedCatalog for the sqlite_test half of this binary, which
// shares the test build but not the package.
func SeedCatalog(tb testing.TB, path string) string { return seedCatalog(tb, path) }

// catalogShape describes everything a test could tell apart between two empty
// catalogs: the schema itself, the version and baseline it was stamped with, and
// what each table already holds. Values that are stamped from the clock are left
// out, since the seed is built once and the catalogs it fills are not.
func catalogShape(t *testing.T, st *Store) string {
	t.Helper()
	ctx := context.Background()

	var b strings.Builder
	version, err := st.currentVersion(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	stamp, err := baselineStamp(ctx, st.read)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	fmt.Fprintf(&b, "version=%d baseline=%d\n", version, stamp)

	rows, err := st.read.QueryContext(ctx,
		"SELECT type, name, IFNULL(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var kind, name, sql string
		if err := rows.Scan(&kind, &name, &sql); err != nil {
			t.Fatalf("scan master: %v", err)
		}
		fmt.Fprintf(&b, "%s %s %s\n", kind, name, sql)
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("master rows: %v", err)
	}

	sort.Strings(tables)
	for _, name := range tables {
		var n int64
		if err := st.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+name+`"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		fmt.Fprintf(&b, "rows %s %d\n", name, n)
	}
	return b.String()
}

// TestSeededCatalogMatchesAFreshOne pins the trade seedCatalog makes. Every test
// that takes a seeded catalog reads as though it opened a new one, so a migration
// or an open-time seeding step that only runs on the fresh path would quietly put
// most of this package on a different catalog than it appears to use.
func TestSeededCatalogMatchesAFreshOne(t *testing.T) {
	ctx := context.Background()
	open := func(path string) *Store {
		t.Helper()
		st, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
		if err != nil {
			t.Fatalf("open %s: %v", filepath.Base(path), err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}

	fresh := catalogShape(t, open(filepath.Join(t.TempDir(), "fresh.db")))
	seeded := catalogShape(t, open(seedCatalog(t, filepath.Join(t.TempDir(), "seeded.db"))))
	if fresh != seeded {
		t.Errorf("a seeded catalog differs from a fresh one:\nfresh:\n%s\nseeded:\n%s", fresh, seeded)
	}
}
