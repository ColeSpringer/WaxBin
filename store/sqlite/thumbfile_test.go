package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	msqlite "modernc.org/sqlite"
)

// thumbFixture opens a read-write store over a seeded catalog with one covered track, and
// returns the store, its catalog path and the track.
func thumbFixture(t *testing.T) (*Store, string, model.PID) {
	t.Helper()
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st := openAt(t, path, false)
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/lib"), DisplayRoot: "/lib",
		Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	return st, path, putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 200, 200))
}

func openAt(t *testing.T, path string, readOnly bool) *Store {
	t.Helper()
	st, err := Open(context.Background(), OpenOptions{Path: path, Owner: "test", ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("open (read-only %t): %v", readOnly, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// thumbAt resolves the track's front cover at box and requires a thumbnail.
func thumbAt(t *testing.T, st *Store, pid model.PID, box int) *model.ArtBlob {
	t.Helper()
	blob, err := st.ResolveArt(context.Background(), model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, box)
	if err != nil || !blob.Thumbnail {
		t.Fatalf("resolve at %d = %+v (err %v), want a thumbnail", box, blob, err)
	}
	return blob
}

// thumbFileDB opens the thumbnail file beside a catalog on its own connection.
func thumbFileDB(t *testing.T, catalog string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, "file:"+ThumbFile(catalog)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open the thumbnail file: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func dbInt(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// TestThumbnailsLiveBesideTheCatalog: a generated thumbnail is written to the file beside
// the catalog, which holds no thumbnail table, and a store opened afresh serves it from
// there rather than generating it again.
func TestThumbnailsLiveBesideTheCatalog(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	first := thumbAt(t, st, pid, 100)
	if _, err := os.Stat(ThumbFile(path)); err != nil {
		t.Fatalf("thumbnail file: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM main.sqlite_master WHERE name LIKE 'thumb%'"); n != 0 {
		t.Errorf("the catalog holds %d thumbnail tables, want none", n)
	}
	thumbs := thumbFileDB(t, path)
	if n := dbInt(t, thumbs, "SELECT COUNT(*) FROM thumb"); n != 1 {
		t.Fatalf("thumbnail rows = %d, want 1", n)
	}
	stamp := dbInt(t, thumbs, "SELECT created_at FROM thumb")
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := thumbAt(t, openAt(t, path, false), pid, 100)
	if !bytes.Equal(again.Bytes, first.Bytes) {
		t.Error("the reopened store served other bytes")
	}
	if n, at := dbInt(t, thumbs, "SELECT COUNT(*) FROM thumb"), dbInt(t, thumbs, "SELECT created_at FROM thumb"); n != 1 || at != stamp {
		t.Errorf("after the reopened resolve: %d rows stamped %d, want the one row from before (%d)", n, at, stamp)
	}
}

// TestAMissingThumbFileIsRecreatedEmpty: a read-write open beside no thumbnail file makes
// an empty one, and generated thumbnails land in it.
func TestAMissingThumbFileIsRecreatedEmpty(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	thumbAt(t, st, pid, 100)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(ThumbFile(path) + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("remove: %v", err)
		}
	}

	st = openAt(t, path, false)
	thumbs := thumbFileDB(t, path)
	if n := dbInt(t, thumbs, "SELECT COUNT(*) FROM thumb"); n != 0 {
		t.Errorf("rows in the new file = %d, want none", n)
	}
	if v := dbInt(t, thumbs, "PRAGMA user_version"); v != 1 {
		t.Errorf("user_version = %d, want 1", v)
	}
	thumbAt(t, st, pid, 100)
	if n := dbInt(t, thumbs, "SELECT COUNT(*) FROM thumb"); n != 1 {
		t.Errorf("rows after a resolve = %d, want 1", n)
	}
}

// TestAReadOnlyOpenPersistsNoThumbnail: a read-only store with no thumbnail file creates
// none and still serves a generated thumbnail, and one beside a file serves its rows.
func TestAReadOnlyOpenPersistsNoThumbnail(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.Remove(ThumbFile(path)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ro := openAt(t, path, true)
	thumbAt(t, ro, pid, 100)
	if _, err := os.Stat(ThumbFile(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("thumbnail file after a read-only resolve: %v, want none made", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rw := openAt(t, path, false)
	thumbAt(t, rw, pid, 100)
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Marked so a regenerated thumbnail cannot pass for the stored one.
	if _, err := thumbFileDB(t, path).Exec("UPDATE thumb SET width = 7"); err != nil {
		t.Fatalf("mark the row: %v", err)
	}
	if got := thumbAt(t, openAt(t, path, true), pid, 100); got.Width != 7 {
		t.Errorf("read-only resolve width = %d, want the stored row's 7", got.Width)
	}
}

// TestGCArtSweepsThumbnailsOfAGoneSource: with no foreign key across the two files, GCArt
// sweeps the thumbnails of every source the catalog no longer holds, the ones it removes
// itself and the ones already gone.
func TestGCArtSweepsThumbnailsOfAGoneSource(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	ctx := context.Background()
	thumbAt(t, st, pid, 100)
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO thumbs.thumb(source_hash, size, format, width, height, created_at, data)
		VALUES ('gone', 64, 'png', 1, 1, 1, x'00')`); err != nil {
		t.Fatalf("stray row: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.OrphanThumbnails != 1 {
		t.Fatalf("verify = %+v (err %v), want the stray row orphaned", rep, err)
	}
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM art_map"); err != nil {
		t.Fatalf("drop the cover: %v", err)
	}
	sources, thumbs, err := st.GCArt(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if sources != 1 || thumbs != 2 {
		t.Errorf("GCArt removed %d sources / %d thumbnails, want 1/2", sources, thumbs)
	}
	if n := dbInt(t, thumbFileDB(t, path), "SELECT COUNT(*) FROM thumb"); n != 0 {
		t.Errorf("thumbnail rows after GC = %d, want none", n)
	}
}

// TestASuspendAndReopenKeepTheThumbnails: the connections a reopen makes attach the same
// file, so a stored thumbnail is served and a new one is written there.
func TestASuspendAndReopenKeepTheThumbnails(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	thumbAt(t, st, pid, 100)
	thumbs := thumbFileDB(t, path)
	stamp := dbInt(t, thumbs, "SELECT created_at FROM thumb")
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := st.Reopen(context.Background()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st.thumbMem = newThumbCache(thumbCacheMax, thumbCacheBytes)
	thumbAt(t, st, pid, 100)
	if at := dbInt(t, thumbs, "SELECT created_at FROM thumb WHERE size = 128"); at != stamp {
		t.Errorf("rung 128 stamped %d after the reopen, want the stored %d", at, stamp)
	}
	thumbAt(t, st, pid, 48)
	if n := dbInt(t, thumbs, "SELECT COUNT(*) FROM thumb"); n != 2 {
		t.Errorf("rows after a new rung = %d, want 2", n)
	}
}

// TestPruneThumbnailsShrinksTheFile: the thumbnail file gives a prune's pages back to the
// filesystem as it commits, with no vacuum of its own to run.
func TestPruneThumbnailsShrinksTheFile(t *testing.T) {
	t.Parallel()
	st, path, _ := thumbFixture(t)
	pid := putNoiseCover(t, st, "e2")
	for _, box := range []int{128, 192, 256} {
		thumbAt(t, st, pid, box)
	}
	if v := dbInt(t, thumbFileDB(t, path), "PRAGMA auto_vacuum"); v != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", v)
	}
	size := func() int64 {
		t.Helper()
		// A checkpoint carries the commits, and the truncation they end with, into the file.
		if _, err := st.wdb().Exec("PRAGMA thumbs.wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		fi, err := os.Stat(ThumbFile(path))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	before := size()
	if _, _, err := st.PruneThumbnails(context.Background(), -1, 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if after := size(); after >= before {
		t.Errorf("file size after emptying the cache = %d, want below the %d it held", after, before)
	}
}

// TestIntegrityCheckIsTheCatalogs: the integrity check reports on the catalog alone, so
// damage inside the thumbnail file, a cache any request can rebuild, never reads as a
// damaged catalog, and a resolve meeting the damage generates its thumbnail again.
func TestIntegrityCheckIsTheCatalogs(t *testing.T) {
	t.Parallel()
	st, path, _ := thumbFixture(t)
	pid := putNoiseCover(t, st, "e3")
	thumbAt(t, st, pid, 256)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Garbage over the file's later pages, past the schema page an attach reads.
	f, err := os.OpenFile(ThumbFile(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, 64<<10), 16<<10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	st = openAt(t, path, false)
	problems, err := st.IntegrityCheck(context.Background())
	if err != nil || len(problems) != 1 || problems[0] != "ok" {
		t.Errorf("integrity check = %q (err %v), want the catalog's ok", problems, err)
	}
	// A thumbnail on a damaged page is generated again rather than failing the resolve.
	thumbAt(t, st, pid, 256)
}

// putNoiseCover puts a track whose cover is noise, so its thumbnails are too large to fit
// in the pages an empty table keeps and overflow onto pages of their own.
func putNoiseCover(t *testing.T, st *Store, essence string) model.PID {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 320))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.UintN(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	libs, err := st.Libraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return putWithCover(t, st, libs[0].ID, "/lib/al/"+essence+".flac", essence, &model.ArtImage{Data: buf.Bytes(),
		Attribution: model.Attribution{Source: model.SourceTag}})
}

// TestThumbFileWritesAtNormalSync: the thumbnail file commits at synchronous NORMAL, as the
// catalog does; an attached database takes SQLite's default, FULL, which syncs the WAL on
// every thumbnail the one writer stores.
func TestThumbFileWritesAtNormalSync(t *testing.T) {
	t.Parallel()
	st, _, _ := thumbFixture(t)
	for _, db := range []*sql.DB{st.wdb(), st.rdb()} {
		if v := dbInt(t, db, "PRAGMA thumbs.synchronous"); v != 1 {
			t.Errorf("thumbs.synchronous = %d, want 1 (NORMAL)", v)
		}
	}
}

// unusableThumbs is what a test writes over the thumbnail file to make it unusable.
var unusableThumbs = []byte("not a database, only bytes where the thumbnails should be")

// TestAnUnusableThumbFileIsMovedAsideAndRemade: a read-write open moves a thumbnail file it
// cannot use aside and starts a fresh one, which thumbnails then land in, while a
// read-only open leaves such a file where it is.
func TestAnUnusableThumbFileIsMovedAsideAndRemade(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ThumbFile(path), unusableThumbs, 0o600); err != nil {
		t.Fatal(err)
	}
	ro := openAt(t, path, true)
	thumbAt(t, ro, pid, 100)
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(ThumbFile(path)); err != nil || !bytes.Equal(got, unusableThumbs) {
		t.Errorf("after a read-only open the file holds %q (err %v), want it untouched", got, err)
	}

	rw := openAt(t, path, false)
	if got, err := os.ReadFile(ThumbFile(path) + ".unusable"); err != nil || !bytes.Equal(got, unusableThumbs) {
		t.Errorf("the file moved aside holds %q (err %v), want the unusable bytes", got, err)
	}
	thumbAt(t, rw, pid, 100)
	if n := dbInt(t, thumbFileDB(t, path), "SELECT COUNT(*) FROM thumb"); n != 1 {
		t.Errorf("rows in the fresh file = %d, want 1", n)
	}
}

// TestAThumbFileThatCannotBeRemadeIsNotWrittenTo: a writer that cannot move an unusable
// file aside works without one, and stores no thumbnail where no reader would find it.
func TestAThumbFileThatCannotBeRemadeIsNotWrittenTo(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ThumbFile(path), unusableThumbs, 0o600); err != nil {
		t.Fatal(err)
	}
	// A folder that is not empty refuses the move aside.
	if err := os.MkdirAll(filepath.Join(ThumbFile(path)+".unusable", "kept"), 0o755); err != nil {
		t.Fatal(err)
	}
	rw := openAt(t, path, false)
	for _, box := range []int{48, 64, 100} {
		thumbAt(t, rw, pid, box)
	}
	if n := dbInt(t, rw.wdb(), "SELECT COUNT(*) FROM thumbs.thumb"); n != 0 {
		t.Errorf("thumbnails the writer kept in memory = %d, want none", n)
	}
}

// TestAttachUnderACanceledContextWarnsNothing: an attach that fails because its context
// ended is no verdict on the file, so it spends no warning and attaches no stand-in.
func TestAttachUnderACanceledContextWarnsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, err := msqlite.NewConnector("file:" + filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := base.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	warned := 0
	c := newThumbsConnector(base, filepath.Join(dir, "c.db.thumbs"), thumbsWriter, nil, func(string, ...any) { warned++ })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.attach(ctx, conn); err == nil {
		t.Fatal("attached under a canceled context")
	}
	if warned != 0 {
		t.Errorf("warned %d times, want none", warned)
	}
}

// TestCloseDoesNotWaitOnAThumbnailReader: a reader holding an old snapshot of the thumbnail
// file does not hold up Close, whose checkpoint of a cache need not wait for it.
func TestCloseDoesNotWaitOnAThumbnailReader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "test", BusyTimeoutMS: 4000})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/lib"), DisplayRoot: "/lib",
		Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatal(err)
	}
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 200, 200))
	thumbAt(t, st, pid, 48)
	reader, err := thumbFileDB(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM thumb").Scan(&n); err != nil {
		t.Fatal(err)
	}
	thumbAt(t, st, pid, 100)
	start := time.Now()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Close took %v beside a thumbnail reader, want it not to wait out the 4s busy timeout", took)
	}
	_, _ = reader.ExecContext(ctx, "ROLLBACK")
}

// TestAForeignThumbLayoutIsMovedAside: a thumbnail file whose table is not this build's
// layout, as one left by another build, is moved aside by a read-write open like a
// damaged one, and thumbnails land in the fresh file.
func TestAForeignThumbLayoutIsMovedAside(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ThumbFile(path), ThumbFile(path) + "-wal", ThumbFile(path) + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	foreign := thumbFileDB(t, path)
	if _, err := foreign.Exec("CREATE TABLE thumb(x); PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	_ = foreign.Close()

	rw := openAt(t, path, false)
	if _, err := os.Stat(ThumbFile(path) + ".unusable"); err != nil {
		t.Errorf("the foreign file was not moved aside: %v", err)
	}
	thumbAt(t, rw, pid, 100)
	if n := dbInt(t, thumbFileDB(t, path), "SELECT COUNT(*) FROM thumb"); n != 1 {
		t.Errorf("rows in the fresh file = %d, want 1", n)
	}
}

// damageThumbFile writes garbage over the thumbnail file's pages past the schema page and
// the pointer map, as a torn write would, leaving a file an attach still accepts.
func damageThumbFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(ThumbFile(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header [2]byte
	if _, err := f.ReadAt(header[:], 16); err != nil {
		t.Fatal(err)
	}
	page := int64(binary.BigEndian.Uint16(header[:]))
	if page == 1 {
		page = 1 << 16
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 2*page {
		t.Fatalf("the thumbnail file is %d bytes, want pages past the schema and the pointer map", info.Size())
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, int(info.Size()-2*page)), 2*page); err != nil {
		t.Fatal(err)
	}
}

// TestADamagedThumbFileFailsNothing: damage to the table's pages, which an attach never
// reads, reads as a damaged cache, not a damaged catalog: verify, its repair and the census
// answer, and the next read-write open replaces the file.
func TestADamagedThumbFileFailsNothing(t *testing.T) {
	t.Parallel()
	st, path, _ := thumbFixture(t)
	pid := putNoiseCover(t, st, "e4")
	thumbAt(t, st, pid, 256)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	damageThumbFile(t, path)

	ctx := context.Background()
	st = openAt(t, path, false)
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.ThumbnailsDamaged {
		t.Fatalf("verify = %+v (err %v), want the damage reported", rep, err)
	}
	if _, _, err := st.GCArt(ctx); err != nil {
		t.Errorf("GCArt over a damaged cache: %v", err)
	}
	if stats, err := st.ThumbCacheStats(ctx); err != nil || !stats.Damaged {
		t.Errorf("census = %+v (err %v), want it to say the file is damaged", stats, err)
	}
	thumbAt(t, st, pid, 256)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openAt(t, path, false)
	if _, err := os.Stat(ThumbFile(path) + ".unusable"); err != nil {
		t.Errorf("the damaged file was not moved aside at the next open: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.ThumbnailsDamaged {
		t.Errorf("verify after the open = %+v (err %v), want a sound cache", rep, err)
	}
}

// TestAWriterThatMissedTheFileAttachesItAgain: a write connection that could not attach the
// thumbnail file when it opened attaches it once it can, so thumbnails are stored again
// and a prune acts on the file rather than on a stand-in.
func TestAWriterThatMissedTheFileAttachesItAgain(t *testing.T) {
	t.Parallel()
	st, path, pid := thumbFixture(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ThumbFile(path), ThumbFile(path) + "-wal", ThumbFile(path) + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	// A folder where the file belongs cannot be attached, and is no damage to move aside.
	if err := os.Mkdir(ThumbFile(path), 0o755); err != nil {
		t.Fatal(err)
	}
	st = openAt(t, path, false)
	thumbAt(t, st, pid, 100)
	if err := os.Remove(ThumbFile(path)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PruneThumbnails(context.Background(), -1, 1<<30); err != nil {
		t.Fatalf("prune: %v", err)
	}
	thumbAt(t, st, pid, 48)
	if n := dbInt(t, thumbFileDB(t, path), "SELECT COUNT(*) FROM thumb"); n != 1 {
		t.Errorf("rows in the file once it could be attached = %d, want the thumbnail stored after", n)
	}
}

// TestThumbPagesAreReclaimedInBoundedSteps: the pages a prune frees go back in steps of at
// most the step's size, each its own transaction, until none is left.
func TestThumbPagesAreReclaimedInBoundedSteps(t *testing.T) {
	t.Parallel()
	st, path, _ := thumbFixture(t)
	pid := putNoiseCover(t, st, "e5")
	for _, box := range []int{128, 192, 256} {
		thumbAt(t, st, pid, box)
	}
	if v := dbInt(t, thumbFileDB(t, path), "PRAGMA auto_vacuum"); v != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", v)
	}
	ctx := context.Background()
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM thumbs.thumb"); err != nil {
		t.Fatal(err)
	}
	free := dbInt(t, st.wdb(), "PRAGMA thumbs.freelist_count")
	if free < 4 {
		t.Fatalf("free pages = %d, want several to reclaim", free)
	}
	steps, err := st.reclaimThumbPages(ctx, 2)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if want := int((free + 1) / 2); steps != want {
		t.Errorf("steps = %d, want %d for %d pages two at a time", steps, want, free)
	}
	if n := dbInt(t, st.wdb(), "PRAGMA thumbs.freelist_count"); n != 0 {
		t.Errorf("free pages after the reclaim = %d, want none", n)
	}
}
