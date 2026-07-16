package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) (*sqlite.Store, *model.Library) {
	t.Helper()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return st, lib
}

func input(libID int64, path, essence, content, title string) model.PutScannedTrackInput {
	return model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: "Artist", Album: "Album", TrackNo: 1},
	}
}

func TestLockfileClearedOnClose(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "c.db")
	lock := db + ".waxlock"

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "owner-x"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := st.OwnerInfo()
	if err != nil || info.Owner != "owner-x" {
		t.Fatalf("owner while open = %+v (err %v), want owner-x", info, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("lockfile not cleared on close: %q", data)
	}
}

func TestPutScannedTrackPreservesPIDsOnRetag(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	path := "/lib/song.mp3"

	r1, err := st.PutScannedTrack(ctx, input(lib.ID, path, "sha256:ESSENCE", "sha256:CONTENT1", "Old Title"))
	if err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if !r1.ItemCreated || !r1.FileCreated {
		t.Fatalf("first put should create item+file: %+v", r1)
	}

	// Retag: same path + same essence, different content + title.
	r2, err := st.PutScannedTrack(ctx, input(lib.ID, path, "sha256:ESSENCE", "sha256:CONTENT2", "New Title"))
	if err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if r2.FilePID != r1.FilePID {
		t.Fatalf("file pid changed on retag: %s -> %s", r1.FilePID, r2.FilePID)
	}
	if r2.ItemPID != r1.ItemPID {
		t.Fatalf("item pid changed on retag: %s -> %s", r1.ItemPID, r2.ItemPID)
	}
	if r2.ItemCreated || r2.FileCreated {
		t.Fatalf("retag should not create new rows: %+v", r2)
	}
	if !r2.ContentChanged {
		t.Fatal("retag should report content changed")
	}

	got, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.Title != "New Title" {
		t.Fatalf("title not updated: %q", got.Title)
	}
}

func TestRescanEssenceChangeReplacesItem(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	path := "/lib/song.mp3"

	r1, err := st.PutScannedTrack(ctx, input(lib.ID, path, "sha256:E1", "sha256:C1", "Title"))
	if err != nil {
		t.Fatalf("put v1: %v", err)
	}
	// Same path, different essence (a re-encode, or a tag edit on a fallback
	// format): the file is re-keyed to a new item and the old item must not ghost.
	r2, err := st.PutScannedTrack(ctx, input(lib.ID, path, "sha256:E2", "sha256:C2", "Title"))
	if err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if r2.ItemPID == r1.ItemPID {
		t.Fatal("an essence change should produce a new item")
	}
	if _, err := st.ItemByPID(ctx, r1.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("old item should be deleted (no ghost), got %v", err)
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 || items[0].PID != r2.ItemPID {
		t.Fatalf("expected exactly the new item, got %+v", items)
	}
}

func TestNoOpRescanEmitsNoChanges(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	in := input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Title")

	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("first put: %v", err)
	}
	seq1, _ := st.LatestChangeSeq(ctx)

	if _, err := st.PutScannedTrack(ctx, in); err != nil { // identical no-op rescan
		t.Fatalf("second put: %v", err)
	}
	seq2, _ := st.LatestChangeSeq(ctx)
	if seq2 != seq1 {
		t.Fatalf("no-op rescan emitted %d change rows (essence-first detection defeated)", seq2-seq1)
	}
}

func TestQueryItemsOffsetWithoutLimit(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	for i := 0; i < 5; i++ {
		s := strconv.Itoa(i)
		if _, err := st.PutScannedTrack(ctx,
			input(lib.ID, "/lib/"+s+".mp3", "sha256:E"+s, "sha256:C"+s, fmt.Sprintf("T%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	all, err := st.QueryItems(ctx, query.New(query.EntityItems).OrderBy("title", false).Build(), "")
	if err != nil || len(all) != 5 {
		t.Fatalf("baseline query: %v len=%d", err, len(all))
	}
	got, err := st.QueryItems(ctx, query.New(query.EntityItems).OrderBy("title", false).Offset(2).Build(), "")
	if err != nil {
		t.Fatalf("offset query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("offset 2 without limit should return 3 rows, got %d", len(got))
	}
	if got[0].PID != all[2].PID {
		t.Fatalf("offset returned the wrong first row")
	}
}

func TestConcurrentCloseAndReads(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	if _, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "sha256:E", "sha256:C", "T")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				// May error once the store is closed; must never panic or race.
				_, _ = st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
			}
		}()
	}
	_ = st.Close() // racing the readers
	wg.Wait()
}

func TestPutScannedTrackKeepsDuplicateCopies(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.mp3")
	pathB := filepath.Join(dir, "b.mp3")
	// Real files on disk so the move-vs-copy check sees both as present.
	if err := os.WriteFile(pathA, []byte("audio-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("audio-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	rA, err := st.PutScannedTrack(ctx, input(lib.ID, pathA, "sha256:DUP", "sha256:CA", "Song"))
	if err != nil {
		t.Fatalf("put A: %v", err)
	}
	rB, err := st.PutScannedTrack(ctx, input(lib.ID, pathB, "sha256:DUP", "sha256:CB", "Song"))
	if err != nil {
		t.Fatalf("put B: %v", err)
	}

	if rB.Relinked {
		t.Fatal("a duplicate copy (old path still present) must not be treated as a re-link")
	}
	if !rB.FileCreated {
		t.Fatal("a duplicate copy should get its own file row")
	}
	if rA.FilePID == rB.FilePID {
		t.Fatalf("duplicate copies should have distinct file pids: %s", rA.FilePID)
	}
	if _, err := st.FileByPath(ctx, []byte(pathA)); err != nil {
		t.Fatalf("original copy missing from catalog: %v", err)
	}
	if _, err := st.FileByPath(ctx, []byte(pathB)); err != nil {
		t.Fatalf("new copy missing from catalog: %v", err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "c.db")

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "a"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Stamp a newer schema version directly, simulating a newer writer/downgrade.
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'newer', 0)",
		sqlite.SchemaVersion+5); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "b"})
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("want CodeUnsupported opening a newer schema read-write, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

func TestPutScannedTrackRelinksOnMove(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	r1, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/old.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put A: %v", err)
	}

	// Same essence appears at a new path, which is a move, not a duplicate.
	r2, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/new.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put B: %v", err)
	}
	if !r2.Relinked {
		t.Fatalf("expected re-link, got %+v", r2)
	}
	if r2.FilePID != r1.FilePID {
		t.Fatalf("re-link should preserve file pid: %s -> %s", r1.FilePID, r2.FilePID)
	}

	if _, err := st.FileByPath(ctx, []byte("/lib/new.mp3")); err != nil {
		t.Fatalf("new path should resolve: %v", err)
	}
	if _, err := st.FileByPath(ctx, []byte("/lib/old.mp3")); err == nil {
		t.Fatal("old path should no longer resolve after re-link")
	}
}

func TestLeaseExclusion(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)

	ok, err := st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "test", AcquiredAt: 1, HeartbeatAt: 1})
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	ok, err = st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "test2", AcquiredAt: 2, HeartbeatAt: 2})
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok {
		t.Fatal("second acquire should fail while scope held")
	}
	if err := st.ReleaseLease(ctx, "scan", "test"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err = st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "test2", AcquiredAt: 3, HeartbeatAt: 3})
	if err != nil || !ok {
		t.Fatalf("re-acquire after release: ok=%v err=%v", ok, err)
	}
}

func TestReclaimOrphans(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)

	job := &model.Job{Kind: "scan", Scope: "scan", State: model.JobRunning, Owner: "dead", StartedAt: 1, HeartbeatAt: 1}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "dead", AcquiredAt: 1, HeartbeatAt: 1}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	n, err := st.ReclaimOrphans(ctx, 99)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d jobs, want 1", n)
	}

	got, err := st.JobByPID(ctx, job.PID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != model.JobCrashed {
		t.Fatalf("orphaned job state = %s, want crashed", got.State)
	}

	// Lease was dropped, so it can be re-acquired.
	ok, err := st.AcquireLease(ctx, &model.Lease{Scope: "scan", Owner: "live", AcquiredAt: 100, HeartbeatAt: 100})
	if err != nil || !ok {
		t.Fatalf("lease should be free after reclaim: ok=%v err=%v", ok, err)
	}
}
