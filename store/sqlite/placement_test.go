package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// placementsOf returns the library each change row after seq names for one entity, oldest
// first.
func placementsOf(t *testing.T, st *Store, seq int64, entityType string, pid model.PID) []model.PID {
	t.Helper()
	cs, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	var out []model.PID
	for _, c := range cs {
		if c.EntityType == entityType && c.EntityPID == pid {
			out = append(out, c.LibraryPID)
		}
	}
	return out
}

// wantPlacements checks that the entity has rows and that each names want.
func wantPlacements(t *testing.T, what string, got []model.PID, want model.PID) {
	t.Helper()
	if len(got) == 0 || slices.ContainsFunc(got, func(p model.PID) bool { return p != want }) {
		t.Errorf("%s rows placed in %q, want each in %q", what, got, want)
	}
}

// twoRoots is a catalog with two in-place libraries on real folders, and a spec for a
// track whose file exists under one of them.
func twoRoots(t *testing.T) (*Store, *model.Library, *model.Library, func(lib *model.Library, name, essence, content string) trackSpec) {
	t.Helper()
	st, _ := entityFixture(t)
	add := func() *model.Library {
		root := t.TempDir()
		lib, err := st.EnsureLibrary(context.Background(), &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
		if err != nil {
			t.Fatal(err)
		}
		return lib
	}
	a, b := add(), add()
	spec := func(lib *model.Library, name, essence, content string) trackSpec {
		return trackSpec{path: realFile(t, filepath.Join(lib.DisplayRoot, name)), essence: essence, content: content,
			title: name, artist: "Artist", album: "Album"}
	}
	return st, a, b, spec
}

// TestAPutPlacesItsRows: a put's item and file rows name the file's library, on a create
// and on a retag.
func TestAPutPlacesItsRows(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	seq := latestSeq(t, st)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "pl1", content: "pl1a", title: "A", artist: "X"})
	wantPlacements(t, "the create's item", placementsOf(t, st, seq, "item", r.ItemPID), lib.PID)
	wantPlacements(t, "the create's file", placementsOf(t, st, seq, "file", r.FilePID), lib.PID)
	seq = latestSeq(t, st)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "pl1", content: "pl1b", title: "B", artist: "X"})
	wantPlacements(t, "the retag's item", placementsOf(t, st, seq, "item", r.ItemPID), lib.PID)
	wantPlacements(t, "the retag's file", placementsOf(t, st, seq, "file", r.FilePID), lib.PID)
}

// TestAMoveAcrossLibrariesPlacesTheItemWhereItLands: an item whose file moves to another
// library names each library it sat in, whichever the scans read first.
func TestAMoveAcrossLibrariesPlacesTheItemWhereItLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("the new library first", func(t *testing.T) {
		t.Parallel()
		st, a, b, spec := twoRoots(t)
		orig := putTrack(t, st, a.ID, spec(a, "1.mp3", "mv1", "mv1a"))
		seq := latestSeq(t, st)
		cp := putTrack(t, st, b.ID, spec(b, "1.mp3", "mv1", "mv1b"))
		if !cp.AttachedAsCopy || cp.ItemPID != orig.ItemPID {
			t.Fatalf("copy = %+v, want an alternate of %s", cp, orig.ItemPID)
		}
		wantPlacements(t, "the attach's item", placementsOf(t, st, seq, "item", orig.ItemPID), a.PID)
		wantPlacements(t, "the copy's file", placementsOf(t, st, seq, "file", cp.FilePID), b.PID)
		if err := os.Remove(filepath.Join(a.DisplayRoot, "1.mp3")); err != nil {
			t.Fatal(err)
		}
		seq = latestSeq(t, st)
		if _, err := st.MarkFilesMissing(ctx, []model.PID{orig.FilePID}); err != nil {
			t.Fatal(err)
		}
		wantPlacements(t, "the promotion's item", placementsOf(t, st, seq, "item", orig.ItemPID), b.PID)
	})
	t.Run("the old library first", func(t *testing.T) {
		t.Parallel()
		st, a, b, spec := twoRoots(t)
		orig := putTrack(t, st, a.ID, spec(a, "1.mp3", "mv2", "mv2a"))
		if err := os.Remove(filepath.Join(a.DisplayRoot, "1.mp3")); err != nil {
			t.Fatal(err)
		}
		seq := latestSeq(t, st)
		if _, err := st.MarkFilesMissing(ctx, []model.PID{orig.FilePID}); err != nil {
			t.Fatal(err)
		}
		wantPlacements(t, "the missing item", placementsOf(t, st, seq, "item", orig.ItemPID), a.PID)
		seq = latestSeq(t, st)
		moved := putTrack(t, st, b.ID, spec(b, "1.mp3", "mv2", "mv2a"))
		if moved.ItemPID != orig.ItemPID {
			t.Fatalf("the moved file landed on %s, want %s", moved.ItemPID, orig.ItemPID)
		}
		wantPlacements(t, "the moved item", placementsOf(t, st, seq, "item", orig.ItemPID), b.PID)
	})
}

// TestATrashDeletePlacesNothing: an archived item has no library, and a file delete names
// none.
func TestATrashDeletePlacesNothing(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "pt1", content: "pt1c", title: "A", artist: "X"})
	seq := latestSeq(t, st)
	trashFile(t, st, r.FilePID)
	wantPlacements(t, "the archived item", placementsOf(t, st, seq, "item", r.ItemPID), "")
	wantPlacements(t, "the deleted file", placementsOf(t, st, seq, "file", r.FilePID), "")
}

// TestARootRemovalPlacesWhatStays: the items a removal archives and the library row carry
// no library, and an item a copy elsewhere keeps is placed in that copy's library, which is
// how a consumer journaling items per library learns it moved.
func TestARootRemovalPlacesWhatStays(t *testing.T) {
	t.Parallel()
	st, gone, kept, spec := twoRoots(t)
	plain := putTrack(t, st, gone.ID, spec(gone, "1.mp3", "rp1", "rp1a"))
	moved := putTrack(t, st, gone.ID, spec(gone, "2.mp3", "rp2", "rp2a"))
	putTrack(t, st, kept.ID, spec(kept, "2.mp3", "rp2", "rp2b"))
	seq := latestSeq(t, st)
	if _, _, err := st.removeLibrary(context.Background(), gone.PID, removeBatch, nil); err != nil {
		t.Fatal(err)
	}
	wantPlacements(t, "the archived item", placementsOf(t, st, seq, "item", plain.ItemPID), "")
	wantPlacements(t, "the library", placementsOf(t, st, seq, "library", gone.PID), "")
	if s := itemState(t, st, moved.ItemPID); s != "present" {
		t.Fatalf("the item with a copy = %s, want present", s)
	}
	wantPlacements(t, "the item a copy kept", placementsOf(t, st, seq, "item", moved.ItemPID), kept.PID)
}

// TestADeleteRowIsPlacedNowhere: a delete names no library even when the entity it names
// still sits in one when the row is written.
func TestADeleteRowIsPlacedNowhere(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "dr1", content: "dr1c", title: "A", artist: "X"})
	seq := latestSeq(t, st)
	if err := st.writeTx(context.Background(), func(tx *sql.Tx) error {
		if err := appendChange(context.Background(), tx, "item", r.ItemPID, model.OpDelete); err != nil {
			return err
		}
		return appendChange(context.Background(), tx, "file", r.FilePID, model.OpDelete)
	}); err != nil {
		t.Fatal(err)
	}
	wantPlacements(t, "the item delete", placementsOf(t, st, seq, "item", r.ItemPID), "")
	wantPlacements(t, "the file delete", placementsOf(t, st, seq, "file", r.FilePID), "")
}

// TestAnEpisodeIsPlacedWhileItHasAFile: a download places the episode in the podcast
// library, and dropping the file leaves it fileless.
func TestAnEpisodeIsPlacedWhileItHasAFile(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	putFeed(t, st, "http://feed.example/p", "One")
	var ep model.PID
	if err := st.rdb().QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind = 'episode'").Scan(&ep); err != nil {
		t.Fatal(err)
	}
	pods, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatal(err)
	}
	seq := latestSeq(t, st)
	file, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{EpisodePID: ep, LibraryID: pods.ID,
		File: model.File{Path: []byte("/podcasts/one.mp3"), DisplayPath: "/podcasts/one.mp3", RelPath: []byte("one.mp3"),
			Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed}})
	if err != nil {
		t.Fatal(err)
	}
	wantPlacements(t, "the download's episode", placementsOf(t, st, seq, "item", ep), pods.PID)
	wantPlacements(t, "the download's file", placementsOf(t, st, seq, "file", file), pods.PID)
	seq = latestSeq(t, st)
	if err := st.DropEpisodeFile(ctx, ep); err != nil {
		t.Fatal(err)
	}
	wantPlacements(t, "the dropped episode", placementsOf(t, st, seq, "item", ep), "")
}

// TestBulkItemRowsArePlaced: the writers that log item rows from a prepared statement or
// a set-based insert place them as one item's put does.
func TestBulkItemRowsArePlaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("a sort-key refresh", func(t *testing.T) {
		t.Parallel()
		st, lib := entityFixture(t)
		r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "bk1", content: "bk1c", title: "A", artist: "X"})
		if _, err := st.wdb().ExecContext(ctx, "UPDATE playable_item SET sort_key = 'WRONG' WHERE pid = ?", string(r.ItemPID)); err != nil {
			t.Fatal(err)
		}
		seq := latestSeq(t, st)
		if _, err := st.RefreshSortKeys(ctx); err != nil {
			t.Fatal(err)
		}
		wantPlacements(t, "the refreshed item", placementsOf(t, st, seq, "item", r.ItemPID), lib.PID)
	})
	t.Run("an artist sort write-back", func(t *testing.T) {
		t.Parallel()
		st, lib := entityFixture(t)
		r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "bk2", content: "bk2c", title: "A", artist: "Elvis Costello"})
		seq := latestSeq(t, st)
		if err := st.FollowArtistSortWrite(ctx, entityPIDByName(t, st, "artist", "name", "Elvis Costello"),
			[]model.PID{r.FilePID}, "Costello, Elvis"); err != nil {
			t.Fatal(err)
		}
		wantPlacements(t, "the written item", placementsOf(t, st, seq, "item", r.ItemPID), lib.PID)
	})
	t.Run("an album sort write-back", func(t *testing.T) {
		t.Parallel()
		st, lib := entityFixture(t)
		r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "bk3", content: "bk3c", title: "A", artist: "X", album: "The Wall"})
		seq := latestSeq(t, st)
		if err := st.FollowAlbumSortWrite(ctx, entityPIDByName(t, st, "album", "title", "The Wall"),
			[]model.PID{r.FilePID}, "Wall, The"); err != nil {
			t.Fatal(err)
		}
		wantPlacements(t, "the written item", placementsOf(t, st, seq, "item", r.ItemPID), lib.PID)
	})
}

// TestSubscribeDeliversThePlacement: a subscriber reads the same placement the feed does.
func TestSubscribeDeliversThePlacement(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ch, cancel := st.Subscribe()
	defer cancel()
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "sb1", content: "sb1c", title: "A", artist: "X"})
	var got []model.PID
	for _, c := range drain(ch) {
		if c.EntityType == "item" && c.EntityPID == r.ItemPID {
			got = append(got, c.LibraryPID)
		}
	}
	wantPlacements(t, "the delivered item", got, lib.PID)
}
