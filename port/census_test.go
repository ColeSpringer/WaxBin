package port_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/store/sqlite"
)

// TestCensusExcludesThePodcastLibrary keeps the internal podcast download dir out
// of the roots `db reset` tells the operator to re-add: it is re-created on demand,
// and `library add` would pull it into scan and organize, which ModePodcast exists
// to prevent.
func TestCensusExcludesThePodcastLibrary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/music"), DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native",
	}); err != nil {
		t.Fatal(err)
	}
	podcastDir := filepath.Join(dir, "podcasts")
	if _, err := st.EnsurePodcastLibrary(ctx, podcastDir); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := port.ReadCensus(ctx, db)
	if err != nil {
		t.Fatalf("ReadCensus: %v", err)
	}
	if c.Partial {
		t.Error("census of a healthy catalog reported itself partial")
	}
	for _, r := range c.Roots {
		if r == podcastDir {
			t.Errorf("census reported the internal podcast library as a root: %v", c.Roots)
		}
	}
	if len(c.Roots) != 1 || c.Roots[0] != "/music" {
		t.Errorf("roots = %v, want just the registered one", c.Roots)
	}
}

// openWithTracks opens a fresh catalog at db with one managed library and a scanned
// track per name, returning the store and each track's put result.
func openWithTracks(t *testing.T, ctx context.Context, db string, names ...string) (*sqlite.Store, []*model.ScanItemResult) {
	t.Helper()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The tests close the store themselves before the census reads the file; the
	// cleanup covers a failure before they get there.
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/music"), DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []*model.ScanItemResult
	for _, name := range names {
		path := "/music/" + name + ".mp3"
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(name + ".mp3"),
				Kind: model.FileAudio, Size: 1, MTimeNS: 1,
				ContentHash: "c" + name, EssenceHash: "e" + name, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: name,
				SortKey: model.SortKey(name), IdentityKey: "essence:e" + name,
			},
			Track: model.Track{Artist: "Artist", Album: "Album", TrackNo: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
	}
	return st, out
}

// TestCensusCountsOnlyRestorableTrash matches what `trash list` shows: a restored
// entry is journal history, and nothing is at stake in discarding it.
func TestCensusCountsOnlyRestorableTrash(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, tracks := openWithTracks(t, ctx, db, "a", "b")
	var trashPIDs []model.PID
	for i, name := range []string{"a", "b"} {
		res := tracks[i]
		tpid, err := st.TrashFile(ctx, model.TrashFileInput{
			FilePID: res.FilePID, TrashPath: []byte("/t/" + name), TrashDisplay: "/t/" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		trashPIDs = append(trashPIDs, tpid)
	}
	if err := st.MarkTrashRestored(ctx, trashPIDs[0]); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := port.ReadCensus(ctx, db)
	if err != nil {
		t.Fatalf("ReadCensus: %v", err)
	}
	if c.TrashEntries != 1 {
		t.Errorf("trash entries = %d, want the 1 still restorable", c.TrashEntries)
	}
}

// TestCensusReportsAnUnreadableCatalogAsPartial is what keeps `db reset` from
// printing "Discarded: 0 items" for a catalog it could not read.
func TestCensusReportsAnUnreadableCatalogAsPartial(t *testing.T) {
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(db, []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := port.ReadCensus(context.Background(), db)
	if err != nil {
		t.Fatalf("ReadCensus should not fail on an unreadable catalog: %v", err)
	}
	if !c.Partial {
		t.Errorf("census = %+v, want Partial so the zeros are not read as contents", c)
	}
}

// TestCensusCountsPlaySessions keeps the listening log in what `db reset` says it is
// about to discard: play state is what an item is worth to a user now, and the
// sessions are the years of history behind stats --year.
func TestCensusCountsPlaySessions(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, tracks := openWithTracks(t, ctx, db, "a")
	res := tracks[0]
	if _, err := st.RecordSession(ctx, "", res.ItemPID, "test", 1_600_000_000_000_000_000, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartSession(ctx, "", res.ItemPID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := port.ReadCensus(ctx, db)
	if err != nil {
		t.Fatalf("ReadCensus: %v", err)
	}
	if c.PlaySessions != 2 {
		t.Errorf("play sessions = %d, want 2 (the recorded one and the open one)", c.PlaySessions)
	}
}
