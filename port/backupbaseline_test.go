package port_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// backupOfFreshCatalog creates a catalog, backs it up the way `waxbin backup`
// does, and returns the backup path.
func backupOfFreshCatalog(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	backup := filepath.Join(dir, "backup.db")
	if err := st.BackupTo(ctx, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return backup
}

// TestValidateBackupAcceptsAFreshBackup pins the property the stamp placement
// rests on: VACUUM INTO carries user_version.
func TestValidateBackupAcceptsAFreshBackup(t *testing.T) {
	t.Parallel()
	if _, err := port.ValidateBackup(context.Background(), backupOfFreshCatalog(t)); err != nil {
		t.Fatalf("validating a backup of a healthy catalog: %v", err)
	}
}

func TestValidateBackupRejectsForeignBaseline(t *testing.T) {
	t.Parallel()
	backup := backupOfFreshCatalog(t)
	stampUserVersion(t, backup, 0x5a5a5a5a)

	_, err := port.ValidateBackup(context.Background(), backup)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("want CodeUnsupported for a foreign-baseline backup, got %v (code %s)",
			err, waxerr.CodeOf(err))
	}
}

// TestRestoreLeavesTargetIntactOnForeignBaseline pins what the check buys over
// temp-and-rename: a restore never succeeds into an unopenable catalog.
func TestRestoreLeavesTargetIntactOnForeignBaseline(t *testing.T) {
	t.Parallel()
	backup := backupOfFreshCatalog(t)
	stampUserVersion(t, backup, 0x5a5a5a5a)

	target := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(target, []byte("existing catalog bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if err := port.Restore(context.Background(), backup, target, true); err == nil {
		t.Fatal("restoring a foreign-baseline backup should fail")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("target changed on a refused restore: %q -> %q", before, after)
	}
	if _, err := os.Stat(target + ".restore-tmp"); err == nil {
		t.Error("a refused restore left its temp file behind")
	}
}

func stampUserVersion(t *testing.T, path string, v int32) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(),
		"PRAGMA user_version = "+strconv.FormatInt(int64(v), 10)); err != nil {
		t.Fatal(err)
	}
}

// TestReadStampMatchesValidateBackup: the stamp read off a backup's header is the one this
// build writes, which ValidateBackup and CheckStamp accept.
func TestReadStampMatchesValidateBackup(t *testing.T) {
	t.Parallel()
	backup := backupOfFreshCatalog(t)
	f, err := os.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := port.ReadStamp(f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := port.BaselineStamp()
	if err != nil || got != want {
		t.Fatalf("stamp = %08x, build's = %08x (err %v); want them equal", uint32(got), uint32(want), err)
	}
	if _, err := port.ValidateBackup(context.Background(), backup); err != nil {
		t.Errorf("ValidateBackup: %v", err)
	}
	if err := port.CheckStamp(f); err != nil {
		t.Errorf("CheckStamp: %v", err)
	}
}

// TestCheckStampRefusesAForeignStampWithTheSameMessage: a file another baseline stamped is
// refused by the header check in the words ValidateBackup uses.
func TestCheckStampRefusesAForeignStampWithTheSameMessage(t *testing.T) {
	t.Parallel()
	backup := backupOfFreshCatalog(t)
	stampUserVersion(t, backup, 0x5a5a5a5a)
	f, err := os.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, err := port.ReadStamp(f); err != nil || got != 0x5a5a5a5a {
		t.Fatalf("stamp = %08x (err %v), want 5a5a5a5a", uint32(got), err)
	}
	checkErr := port.CheckStamp(f)
	_, validErr := port.ValidateBackup(context.Background(), backup)
	var c, v *waxerr.Error
	if !errors.As(checkErr, &c) || !errors.As(validErr, &v) || c.Code != waxerr.CodeUnsupported || v.Code != waxerr.CodeUnsupported || c.Msg != v.Msg {
		t.Fatalf("CheckStamp = %v, ValidateBackup = %v; want one CodeUnsupported message", checkErr, validErr)
	}
}

// TestReadStampRefusesANonSQLiteFile: a file that is not an SQLite database, or too short to
// hold a header, has no stamp to read.
func TestReadStampRefusesANonSQLiteFile(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"text":  strings.Repeat("not a database at all ", 10),
		"short": "SQLite format 3\x00",
	} {
		if _, err := port.ReadStamp(strings.NewReader(content)); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: err = %v, want CodeInvalid", name, err)
		}
	}
}
