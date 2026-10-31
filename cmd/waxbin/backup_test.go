package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/store/sqlite"
)

// TestBackupLeavesTheThumbnailsOut: a backup holds the original images and none of the
// generated thumbnails, which stay in their own file beside the catalog.
func TestBackupLeavesTheThumbnailsOut(t *testing.T) {
	db, root, _ := creditCLIFixture(t)
	for file, q := range map[string]string{
		db: `INSERT INTO art_source(hash, format, width, height, size, data, created_at)
			VALUES ('h1', 'png', 400, 400, 3, x'010203', 1)`,
		sqlite.ThumbFile(db): `INSERT INTO thumb(source_hash, size, format, width, height, data, created_at)
			VALUES ('h1', 128, 'jpeg', 128, 128, x'0405', 1)`,
	} {
		raw, err := sql.Open("sqlite", "file:"+file)
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.Exec(q)
		_ = raw.Close()
		if err != nil {
			t.Fatalf("seed %s: %v", filepath.Base(file), err)
		}
	}

	dest := filepath.Join(t.TempDir(), "copy.db")
	out, err := runCLIJSON(t, db, root, "backup", dest)
	if err != nil {
		t.Fatalf("backup: %v (%s)", err, out)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("backup printed %q: %v", out, err)
	}
	if env.Data["dest"] != dest || env.Data["redacted"] != false || len(env.Data) != 2 {
		t.Errorf("backup reported %v, want the destination and an unredacted copy", env.Data)
	}
	copyDB, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'thumb%'": 0,
		"SELECT COUNT(*) FROM art_source":                             1,
	} {
		var n int
		if err := copyDB.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Errorf("%s in the copy = %d (err %v), want %d", q, n, err, want)
		}
	}
	if _, err := os.Stat(sqlite.ThumbFile(dest)); !os.IsNotExist(err) {
		t.Errorf("a thumbnail file beside the copy: %v, want none", err)
	}
}
