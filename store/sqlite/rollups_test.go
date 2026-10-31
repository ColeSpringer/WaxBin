package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// artistTrackCount reads a stored rollup directly (no recompute) so the test
// proves the maintained value, not a fresh aggregation.
func rollupTrackCount(t *testing.T, st *Store, table, joinTable, nameCol, name string) int {
	t.Helper()
	q := "SELECT r.track_count FROM " + table + " r JOIN " + joinTable +
		" e ON e.id = r." + idColFor(table) + " WHERE e." + nameCol + " = ?"
	var n int
	if err := st.rdb().QueryRowContext(context.Background(), q, name).Scan(&n); err != nil {
		t.Fatalf("read %s for %q: %v", table, name, err)
	}
	return n
}

func idColFor(table string) string {
	switch table {
	case "artist_rollup":
		return "artist_id"
	case "genre_rollup":
		return "genre_id"
	default:
		return "id"
	}
}

func TestRollupsMaintainedOnWrite(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two tracks of one artist; rollups must be correct WITHOUT a manual refresh.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/R/A/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Radiohead", album: "A", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/R/A/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Radiohead", album: "A", genre: "Rock", durationMS: 200,
	})

	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Radiohead"); got != 2 {
		t.Errorf("artist rollup track_count = %d, want 2 (maintained on write)", got)
	}
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 2 {
		t.Errorf("genre rollup track_count = %d, want 2", got)
	}
	// No drift, and no manual RefreshRollups was called.
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Fatalf("derived data inconsistent after maintained writes: %+v (err %v)", rep, err)
	}
}

func TestRetagDecrementsOldGenreRollup(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "stable", content: "c1", title: "Song",
		artist: "X", album: "Al", genre: "Rock", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 1 {
		t.Fatalf("Rock rollup = %d, want 1", got)
	}
	// Retag (same essence, content changed) to a different genre.
	spec.content, spec.genre = "c2", "Jazz"
	putTrack(t, st, lib.ID, spec)

	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Rock"); got != 0 {
		t.Errorf("old genre Rock rollup = %d, want 0 after retag", got)
	}
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Jazz"); got != 1 {
		t.Errorf("new genre Jazz rollup = %d, want 1", got)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("derived data inconsistent after retag: %+v (err %v)", rep, err)
	}
}

func TestOrphanDeleteDecrementsRollup(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Re-key the single file's essence so the prior item is orphaned and deleted.
	spec := trackSpec{
		path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First",
		artist: "Solo", album: "Al", genre: "Rock", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	spec.essence, spec.content, spec.title = "e2", "c2", "Second"
	putTrack(t, st, lib.ID, spec)

	// The artist still has exactly one track (the re-keyed item), not two.
	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Solo"); got != 1 {
		t.Errorf("artist rollup after orphan delete = %d, want 1", got)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("derived data inconsistent after orphan delete: %+v (err %v)", rep, err)
	}
}

// rollupSnapshot reads every stored rollup row and album year, for comparing what one
// way of maintaining them left against another.
func rollupSnapshot(t *testing.T, st *Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT 'a', artist_id, release_group_count, track_count, total_duration_ms FROM artist_rollup ORDER BY 2",
		"SELECT 'r', release_group_id, 0, track_count, total_duration_ms FROM release_group_rollup ORDER BY 2",
		"SELECT 'g', genre_id, 0, track_count, total_duration_ms FROM genre_rollup ORDER BY 2",
		"SELECT 'y', id, 0, COALESCE(year, -1), 0 FROM album ORDER BY 2",
	} {
		rows, err := st.rdb().QueryContext(context.Background(), q)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for rows.Next() {
			var kind string
			var id, a, n, d int64
			if err := rows.Scan(&kind, &id, &a, &n, &d); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			fmt.Fprintf(&b, "%s %d %d %d %d\n", kind, id, a, n, d)
		}
		rows.Close()
	}
	return b.String()
}

// TestDeferredPutLeavesTheEntityDirtyUntilSettled: a deferred put records the entities it
// touched instead of recomputing them, and a settle recomputes and clears them.
func TestDeferredPutLeavesTheEntityDirtyUntilSettled(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// The album takes its first member's year when it is made; the second member's later
	// year is the album's (the latest, with no year shared by half) only once settled.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/R/A/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Radiohead", album: "A", genre: "Rock", year: 1997, durationMS: 100, deferRollups: true})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/R/A/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Radiohead", album: "A", genre: "Rock", year: 1998, durationMS: 100, deferRollups: true})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist_rollup"); n != 0 {
		t.Errorf("artist rollups before the settle = %d, want none (the puts deferred them)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE year = 1997"); n != 1 {
		t.Errorf("albums dated 1997 before the settle = %d, want the album's first year kept", n)
	}
	dirty := scalarInt(t, st, "SELECT COUNT(*) FROM rollup_dirty")
	if dirty != 4 {
		t.Errorf("dirty rows = %d, want 4 (artist, release group, genre, album)", dirty)
	}

	got, err := st.SettleRollups(ctx)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if want := (model.RollupSettle{Artists: 1, ReleaseGroups: 1, Genres: 1, Albums: 1}); got != want {
		t.Errorf("settle = %+v, want %+v", got, want)
	}
	if n := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Radiohead"); n != 2 {
		t.Errorf("artist rollup after the settle = %d, want 2", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE year = 1998"); n != 1 {
		t.Errorf("albums dated 1998 after the settle = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM rollup_dirty"); n != 0 {
		t.Errorf("dirty rows after the settle = %d, want none", n)
	}
	if again, err := st.SettleRollups(ctx); err != nil || again != (model.RollupSettle{}) {
		t.Errorf("a second settle = %+v (err %v), want nothing to do", again, err)
	}
}

// TestSettleRollupsAgreesWithTheRecompute: deferred puts over shared artists and a shared
// genre, settled, leave every rollup and album year where a whole-catalog recompute puts
// them.
func TestSettleRollupsAgreesWithTheRecompute(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i := range 12 {
		putTrack(t, st, lib.ID, trackSpec{
			path: fmt.Sprintf("/lib/x/%d.flac", i), essence: fmt.Sprintf("e%d", i), content: fmt.Sprintf("c%d", i),
			title: fmt.Sprintf("T%d", i), artist: []string{"Alpha", "Beta"}[i%2], albumArt: "Alpha",
			album: fmt.Sprintf("Album %d", i%3), genre: "Rock; Jazz", year: 1990 + i%2,
			durationMS: int64(100 + i), deferRollups: true,
		})
	}
	// One retag moves a track to another artist, album and genre while the first marks
	// are still pending.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/x/0.flac", essence: "e0", content: "c0b", title: "T0",
		artist: "Gamma", albumArt: "Gamma", album: "Other", genre: "Pop", year: 2001, durationMS: 100,
		deferRollups: true})
	if _, err := st.SettleRollups(ctx); err != nil {
		t.Fatalf("settle: %v", err)
	}
	settled := rollupSnapshot(t, st)
	if _, err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if recomputed := rollupSnapshot(t, st); settled != recomputed {
		t.Errorf("settled rollups differ from the recompute\nsettled:\n%s\nrecomputed:\n%s", settled, recomputed)
	}
}

// TestOpenSettlesWhatACrashLeft: rows a deferred put left dirty when its process went away
// are settled by the next read-write open.
func TestOpenSettlesWhatACrashLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := seedCatalog(t, filepath.Join(t.TempDir(), "c.db"))
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/lib"), DisplayRoot: "/lib",
		Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	for i := range 3 {
		putTrack(t, st, lib.ID, trackSpec{path: fmt.Sprintf("/lib/%d.flac", i), essence: fmt.Sprintf("e%d", i),
			content: fmt.Sprintf("c%d", i), title: "T", artist: "Radiohead", album: "A", genre: "Rock",
			durationMS: 100, deferRollups: true})
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(ctx, OpenOptions{Path: path, Owner: "test"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM rollup_dirty"); n != 0 {
		t.Errorf("dirty rows after the open = %d, want none", n)
	}
	if n := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "Radiohead"); n != 3 {
		t.Errorf("artist rollup after the open = %d, want 3", n)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() || rep.RollupsPending != 0 {
		t.Errorf("verify after the open = %+v (err %v), want consistent with nothing pending", rep, err)
	}
	// The entity read the rollup backs agrees with the facet, which counts live.
	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	for _, b := range res.Buckets {
		if b.EntityPID == "" {
			continue
		}
		info, err := st.EntityByPID(ctx, read.EntityArtist, b.EntityPID)
		if err != nil || info.ItemCount != b.Count {
			t.Errorf("artist %s: facet %d, entity %+v (err %v); want them to agree", b.Display, b.Count, info, err)
		}
	}
}

// TestAPartialSettleReadsAsPending: between two batches of a settle, what is settled reads
// right and what is not reads as pending, never as drift.
func TestAPartialSettleReadsAsPending(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, artist := range []string{"Alpha", "Beta", "Gamma"} {
		putTrack(t, st, lib.ID, trackSpec{path: fmt.Sprintf("/lib/%d.flac", i), essence: fmt.Sprintf("e%d", i),
			content: fmt.Sprintf("c%d", i), title: "T", artist: artist, album: artist, genre: "Rock",
			durationMS: 100, deferRollups: true})
	}
	holdScanLease(t, st)
	n, err := st.settleBatch(ctx, rollupKinds[0], 1)
	if err != nil || n != 1 {
		t.Fatalf("one artist settled: %d (err %v)", n, err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.ArtistRollupDrift != 0 || !rep.Consistent() {
		t.Errorf("verify between batches = %+v, want no drift", rep)
	}
	// Two artists, three groups, one genre and three albums wait.
	if rep.RollupsPending != 9 {
		t.Errorf("pending between batches = %d, want 9", rep.RollupsPending)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist_rollup WHERE track_count = 1"); n != 1 {
		t.Errorf("artists settled with their track = %d, want the one", n)
	}
}

// holdScanLease takes the filesystem lease a scan runs under, so verify reads the
// catalog as it would beside a running scan.
func holdScanLease(t *testing.T, st *Store) {
	t.Helper()
	ok, err := st.AcquireLease(context.Background(), &model.Lease{Scope: model.FSMutateScope, Owner: "test",
		AcquiredAt: 1, HeartbeatAt: 1})
	if err != nil || !ok {
		t.Fatalf("take the scan lease: %t (err %v)", ok, err)
	}
}

// TestVerifyReportsPendingNotDrift: a check that runs while a scan's rollups wait for their
// settle reports them as pending, not as drift, and stays consistent.
func TestVerifyReportsPendingNotDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	holdScanLease(t, st)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Radiohead", album: "A", genre: "Rock", year: 1997, durationMS: 100})
	// A second track deferred: the artist, genre, group and album rows are now stale.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Radiohead", album: "A", genre: "Rock", year: 1998, durationMS: 100, deferRollups: true})
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.RollupsPending != 4 {
		t.Errorf("pending = %d, want 4", rep.RollupsPending)
	}
	if rep.ArtistRollupDrift+rep.GenreRollupDrift+rep.ReleaseGroupRollupDrift+rep.AlbumYearDrift != 0 || !rep.Consistent() {
		t.Errorf("verify = %+v, want the pending rows left out of the drift", rep)
	}
}

// TestDeferredNestedPutsMarkInsteadOfRecompute: a book part, a cue rip, a copy joining an
// item and a file re-read as another kind each build their own set of touched entities,
// and each honours the put's deferral.
func TestDeferredNestedPutsMarkInsteadOfRecompute(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	stored := func() int {
		return scalarInt(t, st, `SELECT (SELECT COUNT(*) FROM artist_rollup) + (SELECT COUNT(*) FROM genre_rollup)
			+ (SELECT COUNT(*) FROM release_group_rollup)`)
	}

	book := bookSpecInput(lib.ID, bookSpec{path: "/lib/B/1.m4b", essence: "be", content: "bc", title: "Tome",
		author: "Auth", genres: []string{"Fantasy"}, durationMS: 1000})
	book.DeferRollups = true
	if _, err := st.PutScannedBook(ctx, book); err != nil {
		t.Fatalf("book: %v", err)
	}
	rip := model.PutScannedVirtualTracksInput{LibraryID: lib.ID, DeferRollups: true,
		File: model.File{Path: []byte("/lib/R/rip.flac"), DisplayPath: "/lib/R/rip.flac", RelPath: []byte("rip.flac"),
			Kind: model.FileAudio, Size: 2, MTimeNS: 1, ContentHash: "rc", EssenceHash: "re", DurationMS: 60000,
			ScanState: model.ScanIndexed}}
	for i, w := range [][2]int64{{0, 1500}, {1500, 0}} {
		rip.Tracks = append(rip.Tracks, model.VirtualTrack{
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: fmt.Sprint(i),
				IdentityKey: identity.VirtualTrackKey("re", i+1, w[0])},
			Track:       model.Track{Artist: "Rip", AlbumArtist: "Rip", Album: "Rip Album", Genre: "Jazz", Genres: []string{"Jazz"}, TrackNo: i + 1},
			StartFrames: w[0], EndFrames: w[1]})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, rip); err != nil {
		t.Fatalf("rip: %v", err)
	}
	// A copy is told from a move by both files being on disk.
	dir := t.TempDir()
	disk, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(dir), DisplayRoot: dir,
		Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	song, copied := filepath.Join(dir, "1.flac"), filepath.Join(dir, "copy.flac")
	for _, p := range []string{song, copied} {
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	putTrack(t, st, disk.ID, trackSpec{path: song, essence: "te", content: "tc", title: "Song",
		artist: "Solo", album: "S", genre: "Pop", durationMS: 100, deferRollups: true})
	if res := putTrack(t, st, disk.ID, trackSpec{path: copied, essence: "te", content: "tc2", title: "Song",
		artist: "Solo", album: "S", genre: "Pop", durationMS: 100, deferRollups: true}); !res.AttachedAsCopy {
		t.Fatalf("second file = %+v, want it joined as a copy", res)
	}
	// The song's file read as a book turns its item into one in place.
	again := bookSpecInput(disk.ID, bookSpec{path: song, essence: "te", content: "tc", title: "Song Book",
		author: "Solo", genres: []string{"Pop"}, durationMS: 100})
	again.DeferRollups = true
	if _, err := st.PutScannedBook(ctx, again); err != nil {
		t.Fatalf("re-read as a book: %v", err)
	}

	if n := stored(); n != 0 {
		t.Errorf("rollup rows before the settle = %d, want none", n)
	}
	if _, err := st.SettleRollups(ctx); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() || rep.RollupsPending != 0 {
		t.Errorf("verify after the settle = %+v (err %v), want consistent with nothing pending", rep, err)
	}
}

// TestPendingWithNoScanRunningIsDrift: pending rows no running scan will settle (its settle
// failed) leave stale values behind, so with no scan holding the filesystem lease verify
// reads them as drift, which db verify --fix repairs.
func TestPendingWithNoScanRunningIsDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Radiohead", album: "A", genre: "Rock", durationMS: 100, deferRollups: true})
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.RollupsPending != 4 || rep.ArtistRollupDrift == 0 || rep.Consistent() {
		t.Errorf("verify = %+v, want the pending entities read as drift", rep)
	}
	if _, err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() || rep.RollupsPending != 0 {
		t.Errorf("verify after the repair = %+v (err %v), want consistent", rep, err)
	}
}
