package port_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
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

// TestBackupLeavesNoPartialFileOnFailure: a backup whose copy or trim fails leaves neither a
// backup nor its part file behind, since either may hold secrets half written.
func TestBackupLeavesNoPartialFileOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")
	boom := errors.New("copy failed")
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{}, func(_ context.Context, part string) error {
		if err := os.WriteFile(part, []byte("half a catalog"), 0o600); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("a failed copy = %v, want its error", err)
	}
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{RedactSecrets: true}, func(_ context.Context, part string) error {
		return os.WriteFile(part, []byte("not a catalog, so the trim fails"), 0o600)
	}); err == nil {
		t.Fatal("a backup whose trim failed succeeded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("left behind %v (err %v), want nothing", entries, err)
	}
}

// TestRedactedBackupHoldsNoSecretBytes: a redacted backup holds no trace of a secret's
// bytes, the space it took zeroed rather than only freed, while a full backup holds them.
func TestRedactedBackupHoldsNoSecretBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secret := "hunter2-" + strings.Repeat("plaintext-", 8)
	if err := st.SetSecret(ctx, "podcast.auth.x", secret); err != nil {
		t.Fatal(err)
	}
	for _, redact := range []bool{false, true} {
		dest := filepath.Join(dir, "backup-"+strconv.FormatBool(redact)+".db")
		if err := port.WriteBackup(ctx, dest, port.BackupOptions{RedactSecrets: redact}, st.BackupTo); err != nil {
			t.Fatalf("backup (redact %v): %v", redact, err)
		}
		raw, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) == redact {
			t.Errorf("backup (redact %v) holds the secret's bytes: %v", redact, !redact)
		}
	}
}

// TestBackupDestinationIsModeOwnerOnlyFromTheStart: the copy lands in a file made owner-only
// before any byte of it is written, and the backup keeps that mode.
func TestBackupDestinationIsModeOwnerOnlyFromTheStart(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no owner-only mode bits")
	}
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dest := filepath.Join(dir, "backup.db")
	var before os.FileMode
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{}, func(ctx context.Context, part string) error {
		fi, err := os.Stat(part)
		if err != nil {
			return err
		}
		before = fi.Mode().Perm()
		return st.BackupTo(ctx, part)
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil || before != 0o600 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("part mode before the copy %v, backup mode %v (err %v); want 0600 both", before, fi.Mode().Perm(), err)
	}
}

// TestBackupSyncsThePartFileAndItsDirectoryBeforeTheRename: VACUUM INTO never syncs what it
// writes, so the part file and then its folder reach the disk before the rename publishes
// the backup, and the folder again after it. Not parallel: it swaps the package's seam.
func TestBackupSyncsThePartFileAndItsDirectoryBeforeTheRename(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dest := filepath.Join(dir, "backup.db")
	type sync struct {
		path      string
		published bool
	}
	var synced []sync
	defer port.SwapSyncPath(func(p string) error {
		_, err := os.Stat(dest)
		synced = append(synced, sync{p, err == nil})
		return nil
	})()
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{}, st.BackupTo); err != nil {
		t.Fatal(err)
	}
	want := []sync{{dest + ".part", false}, {dir, false}, {dir, true}}
	if !slices.Equal(synced, want) {
		t.Fatalf("synced %+v, want %+v", synced, want)
	}
}

// TestABackupAtAPathWithURICharacters: a catalog and a backup whose paths hold characters
// an SQLite URI reads as syntax are opened, copied, trimmed and checked like any other.
func TestABackupAtAPathWithURICharacters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := "we#ird%41"
	if runtime.GOOS != "windows" {
		name += "?"
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dest := filepath.Join(dir, "backup.db")
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{RedactSecrets: true}, st.BackupTo); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := port.ValidateBackup(ctx, dest); err != nil {
		t.Errorf("validate: %v", err)
	}
	if c, err := port.ReadCensus(ctx, dest); err != nil || c.Partial {
		t.Errorf("census = %+v (err %v), want it read whole", c, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Errorf("files beside the folder: %v, want the folder alone", entries)
	}
}

// TestBackupRefusesAnOccupiedDestination: a backup never writes over a file, though an empty
// one (a temp name made for it) takes the backup, as VACUUM INTO allowed.
func TestBackupRefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	taken := filepath.Join(dir, "taken.db")
	if err := os.WriteFile(taken, []byte("someone's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := port.WriteBackup(ctx, taken, port.BackupOptions{}, st.BackupTo); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("backup over a file = %v, want CodeConflict", err)
	}
	if b, _ := os.ReadFile(taken); string(b) != "someone's file" {
		t.Errorf("the file at the destination = %q, want it untouched", b)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := port.WriteBackup(ctx, empty, port.BackupOptions{}, st.BackupTo); err != nil {
		t.Fatalf("backup into an empty file: %v", err)
	}
	if _, err := port.ValidateBackup(ctx, empty); err != nil {
		t.Errorf("the backup in the empty file: %v", err)
	}
}

// TestAPublishedBackupStandsWhenTheLastSyncFails: once the rename has published the backup
// it is whole, so a failed sync of its folder after that does not report it as failed (a
// retry would then be refused, the destination taken). Not parallel: it swaps the seam.
func TestAPublishedBackupStandsWhenTheLastSyncFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: testcatalog.Seed(t, filepath.Join(dir, "catalog.db")), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dest := filepath.Join(dir, "backup.db")
	defer port.SwapSyncPath(func(string) error {
		if _, err := os.Stat(dest); err == nil {
			return errors.New("folder sync failed")
		}
		return nil
	})()
	if err := port.WriteBackup(ctx, dest, port.BackupOptions{}, st.BackupTo); err != nil {
		t.Fatalf("a published backup whose last sync failed = %v, want it to stand", err)
	}
	if _, err := port.ValidateBackup(ctx, dest); err != nil {
		t.Errorf("the backup: %v", err)
	}
}
