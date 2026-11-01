package testcatalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin/store/sqlite"
)

// openOnce opens and closes the catalog at path, as a test's first open does.
func openOnce(t *testing.T, path string) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close %s: %v", filepath.Base(path), err)
	}
}

// query opens the closed catalog at path for a look at what its first open left.
func query(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// shape is what a test could tell apart between two catalogs fresh from their first
// open: the schema, the baseline stamp, each table's row count, and where the change
// feed stands.
func shape(t *testing.T, path string) string {
	t.Helper()
	db := query(t, path)
	var b strings.Builder
	var baseline, head int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&baseline); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := db.QueryRow("SELECT IFNULL(MAX(seq), 0) FROM change_log").Scan(&head); err != nil {
		t.Fatalf("change feed: %v", err)
	}
	fmt.Fprintf(&b, "baseline %d, change feed at %d\n", baseline, head)
	rows, err := db.Query("SELECT type, name, IFNULL(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var tables []string
	for rows.Next() {
		var kind, name, text string
		if err := rows.Scan(&kind, &name, &text); err != nil {
			t.Fatalf("schema row: %v", err)
		}
		fmt.Fprintf(&b, "%s %s %s\n", kind, name, text)
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("schema rows: %v", err)
	}
	for _, name := range tables {
		var n int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + name + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		fmt.Fprintf(&b, "rows %s %d\n", name, n)
	}
	return b.String()
}

func defaultUser(t *testing.T, path string) string {
	t.Helper()
	var pid string
	if err := query(t, path).QueryRow("SELECT pid FROM user WHERE is_default = 1").Scan(&pid); err != nil {
		t.Fatalf("default user: %v", err)
	}
	return pid
}

// TestSeededCatalogMatchesAFreshOne: a seeded catalog's first open leaves what a fresh
// catalog's does, its own default user included, so no two seeded catalogs share one.
func TestSeededCatalogMatchesAFreshOne(t *testing.T) {
	t.Parallel()
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	openOnce(t, fresh)
	seeded := Seed(t, filepath.Join(t.TempDir(), "seeded.db"))
	openOnce(t, seeded)
	if f, s := shape(t, fresh), shape(t, seeded); f != s {
		t.Errorf("a seeded catalog differs from a fresh one:\nfresh:\n%s\nseeded:\n%s", f, s)
	}
	other := Seed(t, filepath.Join(t.TempDir(), "other.db"))
	openOnce(t, other)
	if a, b := defaultUser(t, seeded), defaultUser(t, other); a == b {
		t.Errorf("two seeded catalogs share the default user %s", a)
	}
}

// TestSeedPublishesThePathOnce: seeders racing for one path leave one whole catalog and
// nothing beside it, and a file already there is left as it is.
func TestSeedPublishesThePathOnce(t *testing.T) {
	t.Parallel()
	want, err := seed()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog.db")
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = seedAt(path) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("racing seeders: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the catalog holds %d bytes (err %v), want the %d-byte seed", len(got), err, len(want))
	}
	// Windows lists a removed file a scanner still holds until the scanner lets go.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err == nil && len(entries) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the folder holds %d entries (err %v), want the catalog alone", len(entries), err)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(path, []byte("a catalog of its own"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedAt(path); err != nil {
		t.Fatalf("seed over a file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a catalog of its own" {
		t.Errorf("seeding replaced a file already there")
	}
}

// TestSeedRefusesAWALWithNoCatalog: another catalog's WAL replayed onto the seed would
// leave a test on a mix of the two, so the path is refused instead.
func TestSeedRefusesAWALWithNoCatalog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(path+"-wal", []byte("frames of another catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedAt(path); err == nil {
		t.Error("seeded beside a WAL with no catalog")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the catalog exists after the refusal (err %v)", err)
	}
}
