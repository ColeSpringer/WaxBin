package port_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(dir, "catalog.db"), Owner: "test"})
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
	if _, err := port.ValidateBackup(context.Background(), backupOfFreshCatalog(t)); err != nil {
		t.Fatalf("validating a backup of a healthy catalog: %v", err)
	}
}

func TestValidateBackupRejectsForeignBaseline(t *testing.T) {
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
