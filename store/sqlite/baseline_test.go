package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/colespringer/waxbin/waxerr"
)

// TestFingerprintIgnoresLaterMigrations pins the property that is inert today and
// fatal the day a second migration lands: the fingerprint is the v1 baseline's, not
// the highest version's.
func TestFingerprintIgnoresLaterMigrations(t *testing.T) {
	baseline := fstest.MapFS{
		"migrations/0001_init/01_a.sql": mapFile("CREATE TABLE a (x INTEGER);\n"),
		"migrations/0001_init/02_b.sql": mapFile("CREATE TABLE b (y INTEGER);\n"),
	}
	withLater := fstest.MapFS{
		"migrations/0001_init/01_a.sql": mapFile("CREATE TABLE a (x INTEGER);\n"),
		"migrations/0001_init/02_b.sql": mapFile("CREATE TABLE b (y INTEGER);\n"),
		"migrations/0002_later.sql":     mapFile("ALTER TABLE a ADD COLUMN z INTEGER;\n"),
	}

	alone, err := baselineFingerprint(baseline)
	if err != nil {
		t.Fatalf("fingerprint of baseline alone: %v", err)
	}
	both, err := baselineFingerprint(withLater)
	if err != nil {
		t.Fatalf("fingerprint with a later migration: %v", err)
	}
	if alone != both {
		t.Errorf("a later migration changed the baseline fingerprint: %08x vs %08x",
			uint32(alone), uint32(both))
	}
}

// TestFingerprintRejectsAStreamWithoutABaseline guards the other half: taking
// ms[0] is only correct because version 1 is required to be there.
func TestFingerprintRejectsAStreamWithoutABaseline(t *testing.T) {
	_, err := baselineFingerprint(fstest.MapFS{
		"migrations/0002_later.sql": mapFile("CREATE TABLE a (x INTEGER);\n"),
	})
	if !waxerr.Is(err, waxerr.CodeInternal) {
		t.Fatalf("want CodeInternal for a stream with no v1 baseline, got %v (code %s)",
			err, waxerr.CodeOf(err))
	}
}

func TestFingerprintIsStableAndNonZero(t *testing.T) {
	first, err := baselineFingerprint(migrationsFS)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := baselineFingerprint(migrationsFS)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Errorf("fingerprint is not stable: %08x then %08x", uint32(first), uint32(second))
	}
	// A broken embed read would hash the empty string rather than fail loudly.
	empty := sha256.Sum256(nil)
	if first == int32(binary.BigEndian.Uint32(empty[:4])) {
		t.Error("fingerprint is the hash of the empty string; the embedded migrations did not load")
	}
	if got, err := BaselineFingerprint(); err != nil || got != first {
		t.Errorf("BaselineFingerprint() = %08x, %v; want %08x", uint32(got), err, uint32(first))
	}
}

// stampUserVersion writes a raw baseline stamp into a closed catalog, standing in
// for a catalog built from a different baseline.
func stampUserVersion(t *testing.T, path string, v int32) {
	t.Helper()
	raw, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(),
		"PRAGMA user_version = "+strconv.FormatInt(int64(v), 10)); err != nil {
		t.Fatal(err)
	}
}

// newCatalog creates a catalog at a temp path and closes it, returning the path.
func newCatalog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	st, err := Open(context.Background(), OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func TestFreshCatalogStampsBaseline(t *testing.T) {
	ctx := context.Background()
	path := newCatalog(t)

	want, err := buildBaseline()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&got); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if int32(got) != want {
		t.Fatalf("fresh catalog stamped %08x, want %08x", uint32(int32(got)), uint32(want))
	}

	// Reopening is clean and does not re-stamp anything.
	for i := 0; i < 2; i++ {
		st, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ro, err := Open(ctx, OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open of a fresh catalog: %v", err)
	}
	_ = ro.Close()
}

func TestOpenRefusesForeignBaseline(t *testing.T) {
	ctx := context.Background()
	want, err := buildBaseline()
	if err != nil {
		t.Fatal(err)
	}

	// user_version 0 is the whole installed base: every catalog created before the
	// stamp existed reads as unstamped, which is already a mismatch.
	for _, tc := range []struct {
		name  string
		stamp int32
	}{
		{"foreign baseline", want ^ 0x5a5a5a5a},
		{"never stamped", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newCatalog(t)
			stampUserVersion(t, path, tc.stamp)

			_, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Fatalf("read-write open: want CodeUnsupported, got %v (code %s)", err, waxerr.CodeOf(err))
			}
			_, err = Open(ctx, OpenOptions{Path: path, ReadOnly: true})
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Fatalf("read-only open: want CodeUnsupported, got %v (code %s)", err, waxerr.CodeOf(err))
			}
		})
	}
}

func TestAllowStaleOpensReadOnlyOnly(t *testing.T) {
	ctx := context.Background()
	want, err := buildBaseline()
	if err != nil {
		t.Fatal(err)
	}
	path := newCatalog(t)
	stampUserVersion(t, path, want^0x5a5a5a5a)

	st, err := Open(ctx, OpenOptions{Path: path, ReadOnly: true, AllowStaleBaseline: true})
	if err != nil {
		t.Fatalf("read-only open with AllowStaleBaseline: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// The write path is where a missing column costs data, so the flag does not
	// reach it.
	_, err = Open(ctx, OpenOptions{Path: path, Owner: "test", AllowStaleBaseline: true})
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("read-write open with AllowStaleBaseline: want CodeUnsupported, got %v (code %s)",
			err, waxerr.CodeOf(err))
	}
}

// TestUninitializedCatalogStillSaysInit pins the probe order: an empty file is
// missing both, and "run init" is the useful answer.
func TestUninitializedCatalogStillSaysInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	raw, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(context.Background(), "CREATE TABLE placeholder (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	_, err = Open(context.Background(), OpenOptions{Path: path, ReadOnly: true})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid (not initialized), got %v (code %s)", err, waxerr.CodeOf(err))
	}
}
