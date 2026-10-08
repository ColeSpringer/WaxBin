package port_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/port"
)

// changeCounter reads the file change counter from a database file's header, which every
// committed write and every VACUUM moves.
func changeCounter(t *testing.T, path string) uint32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint32(raw[24:28])
}

// TestTrimBackupFileSkipsWhatTheCopyLacks: a copy without a table the trim names is not
// rewritten for it, and a copy lacking one of two named tables loses the one it has.
func TestTrimBackupFileSkipsWhatTheCopyLacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "copy.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE secret(key TEXT, value TEXT); INSERT INTO secret VALUES ('k', 'v')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	before := changeCounter(t, path)
	if err := port.TrimBackupFile(ctx, path, port.BackupOptions{OmitThumbnails: true}); err != nil {
		t.Fatalf("trim a copy with no thumbnail table: %v", err)
	}
	if changeCounter(t, path) != before {
		t.Error("a trim with nothing the copy holds rewrote it")
	}
	if err := port.TrimBackupFile(ctx, path, port.BackupOptions{RedactSecrets: true, OmitThumbnails: true}); err != nil {
		t.Fatalf("trim the secrets: %v", err)
	}
	db, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM secret").Scan(&n); err != nil || n != 0 {
		t.Fatalf("secrets after the trim = %d (err %v), want 0", n, err)
	}
}

// TestTrimBackupFileRemovesACopyItCannotFinish: a copy the trim fails on may still hold what
// it was to strip, so it is removed rather than left for someone to ship.
func TestTrimBackupFileRemovesACopyItCannotFinish(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, []byte("not a catalog, but secret-bearing all the same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := port.TrimBackupFile(context.Background(), path, port.BackupOptions{RedactSecrets: true}); err == nil {
		t.Fatal("trimming a file that is no catalog succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the unfinished copy is still there (stat err %v)", err)
	}
}
