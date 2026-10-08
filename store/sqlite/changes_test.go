package sqlite

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// drain collects buffered changes. Mutations and Reopen publish synchronously,
// so every expected row is available when the operation returns.
func drain(ch <-chan model.Change) []model.Change {
	var out []model.Change
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		default:
			return out
		}
	}
}

func TestSubscribePublishesDeltas(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	ch, cancel := st.Subscribe()
	defer cancel()

	// A mutation publishes its change_log rows after commit.
	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "Al",
	})
	changes := drain(ch)
	if len(changes) == 0 {
		t.Fatal("subscriber received no deltas after a mutation")
	}
	var sawItemCreate bool
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == r.ItemPID && c.Op == model.OpCreate {
			sawItemCreate = true
		}
	}
	if !sawItemCreate {
		t.Errorf("expected an item-create delta for %s, got %+v", r.ItemPID, changes)
	}

	// A play_state mutation publishes a play_state delta.
	if _, err := st.SetStar(ctx, "", r.ItemPID, true, nil); err != nil {
		t.Fatal(err)
	}
	psChanges := drain(ch)
	var sawPlayState bool
	for _, c := range psChanges {
		if c.EntityType == "play_state" && c.EntityPID == r.ItemPID {
			sawPlayState = true
		}
	}
	if !sawPlayState {
		t.Errorf("expected a play_state delta, got %+v", psChanges)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ch, cancel := st.Subscribe()
	cancel()
	// The channel is closed by cancel; a closed channel reads zero values with ok=false.
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	// Further mutations must not panic (no live subscribers).
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
}

func TestDataVersionMovesOnCommit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	before, err := st.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
	after, err := st.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}
	if after == before {
		t.Errorf("data_version did not move after a commit (%d == %d)", before, after)
	}
}

// TestLatestChangeSeqEmptyFeed pins the zero every consumer bootstraps from. The
// empty feed has to be staged: Open seeds a default user, whose create delta is
// already row one, and PruneChangeLog always keeps a row, so no caller above this
// layer can ever observe it naturally.
func TestLatestChangeSeqEmptyFeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM change_log"); err != nil {
		t.Fatalf("drain the feed: %v", err)
	}

	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	if seq != 0 {
		t.Errorf("empty-feed seq = %d, want 0", seq)
	}
}

// TestDataVersionMovesAcrossAReopen: a reopen drops the pinned connection and pins a
// fresh one, whose pragma starts over, so a poller holding the old value must still see
// the value move even though nothing was written.
func TestDataVersionMovesAcrossAReopen(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	// Two hand-offs: the first can move on the fixture's own writes, and the second is
	// the quiet server whose fresh connection reports what the old one did.
	for round := 1; round <= 2; round++ {
		before, err := st.DataVersion(ctx)
		if err != nil {
			t.Fatalf("data version: %v", err)
		}
		if err := st.Suspend(); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if res, err := st.Reopen(ctx); err != nil || !res.Reopened {
			t.Fatalf("reopen = %+v (err %v), want a reopen", res, err)
		}
		after, err := st.DataVersion(ctx)
		if err != nil {
			t.Fatalf("data version: %v", err)
		}
		if after == before {
			t.Errorf("round %d: data version across a reopen = %d, want it to move from %d", round, after, before)
		}
		again, err := st.DataVersion(ctx)
		if err != nil || again != after {
			t.Errorf("round %d: data version with nothing written = %d (err %v), want it to hold at %d", round, again, err, after)
		}
	}
}

// TestReadsRunThroughAReopen: reads and a writer keep running while the store is suspended
// and reopened under them. They may fail while it is down, but none may race the reopen
// (the race detector judges that) and all of them answer once it is back.
func TestReadsRunThroughAReopen(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", album: "Al"}).ItemPID
	var users atomic.Int64
	calls := []func() error{
		func() error { _, err := st.LatestChangeSeq(ctx); return err },
		func() error { _, err := st.DataVersion(ctx); return err },
		func() error { _, err := st.ItemByPID(ctx, pid); return err },
		func() error { _, err := st.BrowsePage(ctx, read.ListRecentlyAdded, read.BrowseOptions{}); return err },
		func() error { _, err := st.CreateUser(ctx, "u"+strconv.FormatInt(users.Add(1), 10)); return err },
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, call := range calls {
		wg.Go(func() {
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					_ = call()
				}
			}
		})
	}
	for range 5 {
		if err := st.Suspend(); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if _, err := st.Reopen(ctx); err != nil {
			t.Fatalf("reopen: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	for i, call := range calls {
		if err := call(); err != nil {
			t.Errorf("call %d after the last reopen: %v", i, err)
		}
	}
}

// TestChangesSincePastTheHeadIsNotFound: a cursor at the head reads an empty page, and
// one past it names a feed that was replaced under the consumer.
func TestChangesSincePastTheHeadIsNotFound(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	head, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	if rows, err := st.ChangesSince(ctx, head); err != nil || len(rows) != 0 {
		t.Fatalf("changes at the head = %v (err %v), want an empty page", rows, err)
	}
	_, err = st.ChangesSince(ctx, head+5)
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("changes past the head = %v, want CodeNotFound", err)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.FormatInt(head+5, 10)) || !strings.Contains(msg, strconv.FormatInt(head, 10)) {
		t.Errorf("error %q does not name the cursor and the head", msg)
	}
}

// TestNoteReopenedAppendsTheCatalogRow: the row is the head, carries no pid, and is
// published to in-process subscribers like any other.
func TestNoteReopenedAppendsTheCatalogRow(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	ch, cancel := st.Subscribe()
	defer cancel()
	seq, err := st.NoteReopened(ctx)
	if err != nil {
		t.Fatalf("note reopened: %v", err)
	}
	if head, err := st.LatestChangeSeq(ctx); err != nil || head != seq {
		t.Fatalf("head = %d (err %v), want the catalog row's %d", head, err, seq)
	}
	rows, err := st.ChangesSince(ctx, seq-1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("changes = %v (err %v), want the catalog row", rows, err)
	}
	if r := rows[0]; r.Seq != seq || r.EntityType != model.ChangeCatalog || r.EntityPID != "" || r.Op != model.OpUpdate {
		t.Errorf("row = %+v, want a pidless catalog update at %d", r, seq)
	}
	got := drain(ch)
	if len(got) != 1 || got[0].EntityType != model.ChangeCatalog || got[0].Seq != seq {
		t.Errorf("published = %+v, want the catalog row", got)
	}
}

// TestReadOnlyStoreNoticesAReplacedFile: a reader holds the file it opened, so a
// restore that renames a new file into place leaves it reading the old one. Its
// DataVersion says so rather than carrying on over the dead catalog.
func TestReadOnlyStoreNoticesAReplacedFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to rename over or remove a file another handle holds open, so the reader cannot be left behind")
	}
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	w, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(ctx, OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if _, err := ro.DataVersion(ctx); err != nil {
		t.Fatalf("data version: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.DataVersion(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("data version over a replaced file = %v, want CodeNotFound", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.DataVersion(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("data version over a removed file = %v, want CodeNotFound", err)
	}
}

// TestChangesSinceBehindThePrunedFeedIsNotFound: a cursor whose next row was pruned
// away would silently skip the rows it never saw, so it is refused like a cursor past
// the head; one at the horizon, and a fresh consumer at zero, read what is retained.
func TestChangesSinceBehindThePrunedFeedIsNotFound(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i),
			content: "c" + strconv.Itoa(i), title: "S", artist: "X", album: "A"})
	}
	if _, err := st.PruneChangeLog(ctx, 3); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var oldest int64
	if err := st.rdb().QueryRowContext(ctx, "SELECT MIN(seq) FROM change_log").Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ChangesSince(ctx, oldest-2); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("changes from behind the pruned feed = %v, want CodeNotFound", err)
	}
	if rows, err := st.ChangesSince(ctx, oldest-1); err != nil || len(rows) != 3 {
		t.Fatalf("changes from the horizon = %d rows (err %v), want the three retained", len(rows), err)
	}
	if rows, err := st.ChangesSince(ctx, 0); err != nil || len(rows) != 3 {
		t.Fatalf("changes from zero = %d rows (err %v), want the three retained", len(rows), err)
	}
}

// suspendedStore opens a catalog read-write, subscribes to it, and suspends it the way
// a maintenance hand-off does, returning the store, its path, and the subscription.
func suspendedStore(t *testing.T) (*Store, string, <-chan model.Change) {
	t.Helper()
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "server"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ch, cancel := st.Subscribe()
	t.Cleanup(cancel)
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	return st, path, ch
}

// foreground runs fn against the catalog at path as another process holding the lock
// would during a hand-off.
func foreground(t *testing.T, path string, fn func(*Store)) {
	t.Helper()
	fg, err := Open(context.Background(), OpenOptions{Path: path, Owner: "foreground"})
	if err != nil {
		t.Fatalf("foreground open: %v", err)
	}
	fn(fg)
	if err := fg.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReopenPublishesWhatAnotherProcessWrote: the feed runs on across a hand-off, so
// the reopen hands subscribers the rows the foreground process wrote, which were
// committed elsewhere and never published here, and says the catalog is the same one.
func TestReopenPublishesWhatAnotherProcessWrote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, path, ch := suspendedStore(t)
	var lib *model.Library
	foreground(t, path, func(fg *Store) {
		var err error
		lib, err = fg.EnsureLibrary(ctx, &model.Library{Root: []byte("/fg"), DisplayRoot: "/fg", Mode: model.ModeManaged, Profile: "waxbin-native"})
		if err != nil {
			t.Fatal(err)
		}
	})
	res, err := st.Reopen(ctx)
	if err != nil || !res.Reopened || res.Replaced {
		t.Fatalf("reopen = %+v (err %v), want the same catalog reopened", res, err)
	}
	got := drain(ch)
	if len(got) != 1 || got[0].EntityType != "library" || got[0].EntityPID != lib.PID {
		t.Fatalf("published = %+v, want the foreground's library row", got)
	}
}

// TestReopenNoticesAnotherCatalog: a file renamed over the path, a feed that went back,
// and a feed whose new rows were pruned away all leave nothing to catch up from, so
// the reopen reports the catalog replaced and publishes none of it.
func TestReopenNoticesAnotherCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	putRow := func(fg *Store, root string) {
		if _, err := fg.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: "waxbin-native"}); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, path string){
		"renamed over": func(t *testing.T, path string) {
			blob, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".new", blob, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".new", path); err != nil {
				t.Fatal(err)
			}
		},
		"rewound": func(t *testing.T, path string) {
			foreground(t, path, func(fg *Store) {
				if _, err := fg.wdb().ExecContext(ctx, "DELETE FROM change_log WHERE seq = (SELECT MAX(seq) FROM change_log)"); err != nil {
					t.Fatal(err)
				}
			})
		},
		"pruned past": func(t *testing.T, path string) {
			foreground(t, path, func(fg *Store) {
				putRow(fg, "/a")
				putRow(fg, "/b")
				if _, err := fg.PruneChangeLog(ctx, 1); err != nil {
					t.Fatal(err)
				}
			})
		},
	}
	for name, replace := range cases {
		t.Run(name, func(t *testing.T) {
			st, path, ch := suspendedStore(t)
			replace(t, path)
			res, err := st.Reopen(ctx)
			if err != nil || !res.Reopened || !res.Replaced {
				t.Fatalf("reopen = %+v (err %v), want the catalog reported replaced", res, err)
			}
			if got := drain(ch); len(got) != 0 {
				t.Fatalf("published = %+v, want nothing from another catalog", got)
			}
		})
	}
}

// TestFailedReopenKeepsSubscribersAndTheVerdict: a reopen that fails partway leaves the
// store closed but its subscriptions open, like a suspend, and the next attempt still
// reports what the first one found.
func TestFailedReopenKeepsSubscribersAndTheVerdict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, path, ch := suspendedStore(t)
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	foreground(t, path, func(fg *Store) {
		if _, err := fg.wdb().ExecContext(ctx, "INSERT INTO schema_migrations(version, name, applied_at) VALUES (999, 'future', 0)"); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := st.Reopen(ctx); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("reopen over a newer schema = %v, want CodeUnsupported", err)
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("the failed reopen closed the subscription")
		}
	default:
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	res, err := st.Reopen(ctx)
	if err != nil || !res.Reopened || !res.Replaced {
		t.Fatalf("second reopen = %+v (err %v), want it to still report the file replaced", res, err)
	}
	if _, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/after"), DisplayRoot: "/after", Mode: model.ModeManaged, Profile: "waxbin-native"}); err != nil {
		t.Fatal(err)
	}
	if got := drain(ch); len(got) != 1 || got[0].EntityType != "library" {
		t.Fatalf("published after the reopen = %+v, want the new library row", got)
	}
}

// TestCloseWhileSuspendedEndsSubscriptions: a server shut down mid-hand-off closes a
// store that is already suspended, and its subscribers' range loops still end.
func TestCloseWhileSuspendedEndsSubscriptions(t *testing.T) {
	t.Parallel()
	st, _, ch := suspendedStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a row arrived instead of the close")
		}
	default:
		t.Fatal("closing a suspended store left its subscription open")
	}
}

// TestRepeatedSuspendKeepsItsMark: a second Suspend of a suspended store changes
// nothing, so the reopen still recognizes the catalog and catches up on it.
func TestRepeatedSuspendKeepsItsMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, path, ch := suspendedStore(t)
	if err := st.Suspend(); err != nil {
		t.Fatalf("second suspend: %v", err)
	}
	foreground(t, path, func(fg *Store) {
		if _, err := fg.EnsureLibrary(ctx, &model.Library{Root: []byte("/fg"), DisplayRoot: "/fg", Mode: model.ModeManaged, Profile: "waxbin-native"}); err != nil {
			t.Fatal(err)
		}
	})
	if res, err := st.Reopen(ctx); err != nil || res.Replaced {
		t.Fatalf("reopen = %+v (err %v), want the same catalog", res, err)
	}
	if got := drain(ch); len(got) != 1 || got[0].EntityType != "library" {
		t.Fatalf("published = %+v, want the foreground's row", got)
	}
}

// TestDataVersionRefusesWhileSuspended: during a hand-off DataVersion answers the way a
// write does, and after the reopen it answers again, moved from where it stood.
func TestDataVersionRefusesWhileSuspended(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	before, err := st.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := st.DataVersion(ctx); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("data version while suspended: err %v, want CodeUnsupported", err)
	}
	if _, err := st.Reopen(ctx); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if after, err := st.DataVersion(ctx); err != nil || after == before {
		t.Errorf("data version after the reopen = %d (err %v), want it moved from %d", after, err, before)
	}
}

// TestReadsRefuseWhileClosed: a suspended or closed store answers a read the way it
// answers a write, whichever way the read reaches the pool.
func TestReadsRefuseWhileClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, how := range []string{"suspended", "closed"} {
		st, lib := entityFixture(t)
		pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
			title: "A", artist: "X", album: "Al"}).ItemPID
		if _, err := st.ItemByPID(ctx, pid); err != nil {
			t.Fatalf("item: %v", err)
		}
		shut := st.Suspend
		if how == "closed" {
			shut = st.Close
		}
		if err := shut(); err != nil {
			t.Fatalf("%s: %v", how, err)
		}
		_, seqErr := st.LatestChangeSeq(ctx)
		_, itemErr := st.ItemByPID(ctx, pid)
		_, browseErr := st.BrowsePage(ctx, read.ListRecentlyAdded, read.BrowseOptions{})
		_, statsErr := st.ThumbCacheStats(ctx)
		sessionsErr := st.ExportSessions(ctx, func(model.PID) bool { return true },
			func(int) error { return nil }, func(model.PlaySession) error { return nil })
		for name, err := range map[string]error{
			"LatestChangeSeq": seqErr, "ItemByPID": itemErr, "BrowsePage": browseErr,
			"ThumbCacheStats": statsErr, "ExportSessions": sessionsErr,
		} {
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Errorf("%s store: %s = %v, want CodeUnsupported", how, name, err)
			}
		}
	}
}

// TestCloseInsideASynctestBubble: a store opened and closed inside a synctest bubble
// leaves no goroutine of its own behind there, so the bubble can end. The scenario runs
// in a child process, where its close is the first one, since only the first close in a
// process would start a lazily built closedGen.
func TestCloseInsideASynctestBubble(t *testing.T) {
	t.Parallel()
	if os.Getenv("WAXBIN_BUBBLE_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCloseInsideASynctestBubble$", "-test.count=1")
		cmd.Env = append(os.Environ(), "WAXBIN_BUBBLE_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "c.db")
	synctest.Test(t, func(t *testing.T) {
		st, err := Open(context.Background(), OpenOptions{Path: path, Owner: "test"})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
}

// TestDataVersionRefusesOnAClosedReadOnlyStore: a closed read-only store answers as
// closed even once a restore has renamed another catalog over its path.
func TestDataVersionRefusesOnAClosedReadOnlyStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	w, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(ctx, OpenOptions{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.DataVersion(ctx); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("data version on a closed read-only store = %v, want CodeUnsupported", err)
	}
}

// TestConcurrentReopensAgree: two reopens of one suspended store both succeed, and only
// one of them does the reopening.
func TestConcurrentReopensAgree(t *testing.T) {
	t.Parallel()
	st, _, _ := suspendedStore(t)
	results := make(chan ReopenResult, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			res, err := st.Reopen(context.Background())
			if err != nil {
				t.Errorf("reopen: %v", err)
			}
			results <- res
		})
	}
	wg.Wait()
	close(results)
	reopened := 0
	for res := range results {
		if res.Reopened {
			reopened++
		}
	}
	if reopened != 1 {
		t.Errorf("%d of two concurrent reopens reported reopening the store, want 1", reopened)
	}
}

// reopenBehindALock starts a reopen of st while a foreground store holds the lock at
// path, and returns once that reopen holds handoffMu, waiting out the lock. It returns
// the foreground store and the reopen's result.
func reopenBehindALock(t *testing.T, st *Store, path string) (*Store, <-chan error) {
	t.Helper()
	fg, err := Open(context.Background(), OpenOptions{Path: path, Owner: "foreground"})
	if err != nil {
		t.Fatalf("foreground open: %v", err)
	}
	t.Cleanup(func() { _ = fg.Close() })
	reopened := make(chan error, 1)
	go func() { _, err := st.Reopen(context.Background()); reopened <- err }()
	for deadline := time.Now().Add(10 * time.Second); st.handoffMu.TryLock(); time.Sleep(time.Millisecond) {
		st.handoffMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the reopen never started")
		}
	}
	return fg, reopened
}

// TestSuspendWaitsForAReopenInFlight: a suspend made while a reopen waits for the lock
// takes effect after that reopen, so the store ends suspended with a mark of its own and
// the lock free, rather than reopened behind the call that suspended it.
func TestSuspendWaitsForAReopenInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, path, _ := suspendedStore(t)
	fg, reopened := reopenBehindALock(t, st, path)
	suspended := make(chan error, 1)
	go func() { suspended <- st.Suspend() }()
	var err error
	returned := false
	select {
	case err = <-suspended:
		returned = true
	case <-time.After(100 * time.Millisecond):
	}
	if err := fg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-reopened; err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !returned {
		err = <-suspended
	}
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := st.LatestChangeSeq(ctx); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("a read after the suspend = %v, want the store closed", err)
	}
	next, err := Open(ctx, OpenOptions{Path: path, Owner: "next"})
	if err != nil {
		t.Fatalf("open after the suspend: %v, want the lock free", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := st.Reopen(ctx); err != nil || !res.Reopened || res.Replaced {
		t.Errorf("reopen after the suspend = %+v (err %v), want the same catalog reopened", res, err)
	}
}

// TestCloseStopsAReopenInFlight: a close made while a reopen waits out a held lock stops
// that reopen rather than waiting behind it, even when the reopen's own context cannot
// end, and leaves the store closed and the lock free.
func TestCloseStopsAReopenInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, path, _ := suspendedStore(t)
	fg, reopened := reopenBehindALock(t, st, path)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-reopened; !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Errorf("the reopen the close overtook = %v, want CodeCanceled", err)
	}
	if err := fg.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestChangeSeq(ctx); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("a read after the close = %v, want the store closed", err)
	}
	next, err := Open(ctx, OpenOptions{Path: path, Owner: "next"})
	if err != nil {
		t.Fatalf("open after the close: %v, want the lock free", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSuspendWaitsForAReadToFinish: a suspend keeps the write lock until the reads it
// found running have let go of the catalog file, and gives up with a warning on one that
// outlives the bound, letting the lock go then.
func TestSuspendWaitsForAReadToFinish(t *testing.T) {
	// Not parallel: it sets settleBound, which every store reads.
	old := settleBound
	t.Cleanup(func() { settleBound = old })
	ctx := context.Background()
	logs := &warnRecorder{}
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "test", Logger: slog.New(logs)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The WAL is emptied first so the read holds no WAL snapshot, which the suspend's
	// checkpoint would wait out by itself.
	openRead := func() *sql.Rows {
		t.Helper()
		if _, err := st.wdb().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		rows, err := st.rdb().QueryContext(ctx, "SELECT seq FROM change_log")
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	lockFree := func() bool {
		t.Helper()
		l, err := acquireWriteLock(path+".waxlock", "other", "", nowNS())
		if waxerr.Is(err, waxerr.CodeConflict) {
			return false
		}
		if err != nil {
			t.Fatalf("probe the lock: %v", err)
		}
		_ = l.release()
		return true
	}

	settleBound = time.Minute
	pool := st.rdb()
	rows := openRead()
	done := make(chan error, 1)
	go func() { done <- st.Suspend() }()
	select {
	case err := <-done:
		t.Fatalf("suspend returned (err %v) while a read still held its connection", err)
	case <-time.After(100 * time.Millisecond):
	}
	if lockFree() {
		t.Error("the write lock was free while a read still held the catalog file")
	}
	rows.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("suspend: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("suspend did not return once the read finished")
	}
	if n := pool.Stats().OpenConnections; n != 0 {
		t.Errorf("read connections open after the suspend = %d, want 0", n)
	}
	if !lockFree() {
		t.Error("the write lock was still held after the suspend")
	}
	if n := logs.count("still open"); n != 0 {
		t.Errorf("%d warnings about connections left open, want none for a read that finished in time", n)
	}

	settleBound = 50 * time.Millisecond
	if _, err := st.Reopen(ctx); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	pool = st.rdb()
	rows = openRead()
	defer rows.Close()
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if n := pool.Stats().OpenConnections; n != 1 {
		t.Errorf("read connections open after giving up = %d, want the one still reading", n)
	}
	if !lockFree() {
		t.Error("the write lock was still held after the suspend gave up on the read")
	}
	if n := logs.count("still open"); n != 1 {
		t.Errorf("%d warnings about connections left open, want one for the read left running", n)
	}
}
