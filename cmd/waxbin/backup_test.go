package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestBackupNoThumbnails: --no-thumbnails leaves the generated thumbnails out of the copy
// and says so, while the original images stay.
func TestBackupNoThumbnails(t *testing.T) {
	db, root, _ := creditCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`INSERT INTO art_source(hash, format, width, height, size, data, created_at)
			VALUES ('h1', 'png', 400, 400, 3, x'010203', 1);
		INSERT INTO thumb_cache(source_hash, size, format, width, height, data, created_at)
			VALUES ('h1', 128, 'jpeg', 128, 128, x'0405', 1)`); err != nil {
		t.Fatalf("seed a thumbnail: %v", err)
	}
	_ = raw.Close()

	dest := filepath.Join(t.TempDir(), "lean.db")
	out, err := runCLIJSON(t, db, root, "backup", dest, "--no-thumbnails")
	if err != nil {
		t.Fatalf("backup --no-thumbnails: %v (%s)", err, out)
	}
	var env struct {
		Data struct {
			Redacted          bool `json:"redacted"`
			ThumbnailsOmitted bool `json:"thumbnailsOmitted"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("backup printed %q: %v", out, err)
	}
	if !env.Data.ThumbnailsOmitted || env.Data.Redacted {
		t.Errorf("backup reported %+v, want thumbnails omitted and secrets kept", env.Data)
	}
	copyDB, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM thumb_cache": 0,
		"SELECT COUNT(*) FROM art_source":  1,
	} {
		var n int
		if err := copyDB.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Errorf("%s in the copy = %d (err %v), want %d", q, n, err, want)
		}
	}
}
