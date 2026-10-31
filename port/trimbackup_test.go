package port_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/store/sqlite"
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

// TestTrimBackupFileSkipsWhatTheCopyLacks: a copy without the secret table is not
// rewritten for it, and a copy holding one loses it.
func TestTrimBackupFileSkipsWhatTheCopyLacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	bare, path := filepath.Join(dir, "bare.db"), filepath.Join(dir, "copy.db")
	for p, ddl := range map[string]string{
		bare: "CREATE TABLE other(x INTEGER)",
		path: "CREATE TABLE secret(key TEXT, value TEXT); INSERT INTO secret VALUES ('k', 'v')",
	} {
		db, err := sql.Open("sqlite", "file:"+p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}

	before := changeCounter(t, bare)
	if err := port.TrimBackupFile(ctx, bare, port.BackupOptions{RedactSecrets: true}); err != nil {
		t.Fatalf("trim a copy with no secret table: %v", err)
	}
	if changeCounter(t, bare) != before {
		t.Error("a trim with nothing the copy holds rewrote it")
	}
	if err := port.TrimBackupFile(ctx, path, port.BackupOptions{RedactSecrets: true}); err != nil {
		t.Fatalf("trim the secrets: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM secret").Scan(&n); err != nil || n != 0 {
		t.Fatalf("secrets after the trim = %d (err %v), want 0", n, err)
	}
}

// TestABackupCarriesNoThumbnails: the thumbnails live in a file of their own beside the
// catalog, so a backup holds the original images and none of what was generated from
// them, and leaves no thumbnail file beside itself.
func TestABackupCarriesNoThumbnails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st := coveredCatalog(t, ctx, filepath.Join(dir, "catalog.db"))
	dest := filepath.Join(dir, "backup.db")
	if err := st.BackupTo(ctx, dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := port.TrimBackupFile(ctx, dest, port.BackupOptions{}); err != nil {
		t.Fatalf("trim: %v", err)
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
		t.Errorf("a thumbnail file beside the backup: %v, want none", err)
	}
}

// coveredCatalog opens a catalog at db holding one track with a cover and a thumbnail
// generated from it.
func coveredCatalog(t *testing.T, ctx context.Context, db string) *sqlite.Store {
	t.Helper()
	st, _ := openWithTracks(t, ctx, db)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	libs, err := st.Libraries(ctx)
	if err != nil || len(libs) == 0 {
		t.Fatalf("libraries: %v", err)
	}
	res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: libs[0].ID,
		File: model.File{Path: []byte("/music/c.mp3"), DisplayPath: "/music/c.mp3", RelPath: []byte("c.mp3"),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, ContentHash: "cc", EssenceHash: "ec", ScanState: model.ScanIndexed},
		Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: "C",
			IdentityKey: "essence:ec"},
		Track:    model.Track{Artist: "Artist", Album: "Album", TrackNo: 1},
		CoverArt: &model.ArtImage{Data: buf.Bytes(), Attribution: model.Attribution{Source: model.SourceTag}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: res.ItemPID}, model.ArtRoleFront, 100)
	if err != nil || !blob.Thumbnail {
		t.Fatalf("resolve = %+v (err %v), want a thumbnail", blob, err)
	}
	return st
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
