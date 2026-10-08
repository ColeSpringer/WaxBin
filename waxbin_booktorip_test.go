package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestABookReadAgainAsACueRipKeepsItsState: a single-file book whose chapters come from a
// .cue beside it, read again as a track once its root is no longer an audiobook root, is
// carved into the sheet's tracks. The track that opens the file carries on as the book
// did, its pid, star and playlist entry included, and a resume position and a bookmark
// move to the track whose window holds them, as an offset into it.
func TestABookReadAgainAsACueRipKeepsItsState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeCueRip(t, root, 7, "First", "Second") // track 2 opens at ten seconds
	open := func(media model.MediaType) *waxbin.Library {
		t.Helper()
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
			Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Media: media}}})
		if err != nil {
			t.Fatal(err)
		}
		return lib
	}
	lib := open(model.MediaAudiobook)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("items = %+v (err %v), want the one book", items, err)
	}
	book := items[0].PID
	pb := lib.Playback()
	if _, err := pb.SetStar(ctx, "", book, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.Checkpoint(ctx, "", book, 15000, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.AddBookmark(ctx, "", book, 12000, "mark"); err != nil {
		t.Fatal(err)
	}
	pl, err := lib.Playlists().CreateStatic(ctx, "Keep", "", model.VisibilityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Playlists().Add(ctx, pl, book); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	lib = open(model.MediaMixed)
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	tracks, err := lib.Query(ctx, query.New(query.EntityItems).OrderBy("title", false).Build(), "")
	if err != nil || len(tracks) != 2 || tracks[0].Title != "First" || tracks[1].Title != "Second" {
		t.Fatalf("items after the re-read = %+v (err %v), want the rip's two tracks", tracks, err)
	}
	first, second := tracks[0], tracks[1]
	if first.PID != book {
		t.Errorf("the opening track's pid = %s, want the book's %s", first.PID, book)
	}
	pb = lib.Playback()
	st, err := pb.State(ctx, "", first.PID)
	if err != nil || !st.Starred {
		t.Errorf("the opening track's state = %+v (err %v), want the book's star", st, err)
	}
	st, err = pb.State(ctx, "", second.PID)
	if err != nil || st.PositionMS != 5000 {
		t.Errorf("the second track's state = %+v (err %v), want the position five seconds in", st, err)
	}
	marks, err := pb.Bookmarks(ctx, "", second.PID)
	if err != nil || len(marks) != 1 || marks[0].PositionMS != 2000 {
		t.Errorf("the second track's bookmarks = %+v (err %v), want the mark two seconds in", marks, err)
	}
	entries, err := lib.Playlists().Items(ctx, pl, "")
	if err != nil || len(entries) != 1 || entries[0].PID != book {
		t.Errorf("playlist entries = %+v (err %v), want the opening track in the book's place", entries, err)
	}
}

// TestAFinishedBookCarvedIntoARipFinishesEveryTrack: a book heard to its end, read again as
// a cue rip, leaves every track of the rip played and finished, its plays with the track
// that opens the file; and the rip, every track finished, read back as one file is that
// file finished again.
func TestAFinishedBookCarvedIntoARipFinishesEveryTrack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeCueRip(t, root, 19, "First", "Second")
	open := func(media model.MediaType) *waxbin.Library {
		t.Helper()
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
			Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Media: media}}})
		if err != nil {
			t.Fatal(err)
		}
		return lib
	}
	lib := open(model.MediaAudiobook)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("items = %+v (err %v), want the one book", items, err)
	}
	if err := lib.Playback().MarkPlayed(ctx, "", items[0].PID, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	lib = open(model.MediaMixed)
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	pb := lib.Playback()
	for title, plays := range map[string]int{"First": 1, "Second": 0} {
		st, err := pb.State(ctx, "", itemPIDByTitle(t, ctx, lib, title))
		if err != nil || !st.Played || !st.Finished || st.PlayCount != plays {
			t.Errorf("%s after the carve = %+v (err %v), want played and finished with %d plays", title, st, err, plays)
		}
	}

	if err := os.Remove(filepath.Join(root, "album.cue")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err = lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items after the sheet went = %+v (err %v), want one whole-file track", items, err)
	}
	if st, err := pb.State(ctx, "", items[0].PID); err != nil || !st.Played || !st.Finished {
		t.Errorf("the whole file read back = %+v (err %v), want it finished again", st, err)
	}
}

// TestARipReadBackAsOneFileKeepsItsState: the cue sheet removed, a rip's file is one
// whole-file track again, and its tracks fold into it: the opening track's pid and star,
// a position inside the second track as that place in the file, and a playlist entry.
func TestARipReadBackAsOneFileKeepsItsState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeCueRip(t, root, 9, "First", "Second") // track 2 opens at ten seconds
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	first, second := itemPIDByTitle(t, ctx, lib, "First"), itemPIDByTitle(t, ctx, lib, "Second")
	pb := lib.Playback()
	if _, err := pb.SetStar(ctx, "", first, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.Checkpoint(ctx, "", second, 3000, nil); err != nil {
		t.Fatal(err)
	}
	pl, err := lib.Playlists().CreateStatic(ctx, "Keep", "", model.VisibilityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Playlists().Add(ctx, pl, second); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "album.cue")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Virtual {
		t.Fatalf("items after the sheet went = %+v (err %v), want one whole-file track", items, err)
	}
	whole := items[0].PID
	if whole != first {
		t.Errorf("the whole-file track's pid = %s, want the opening track's %s", whole, first)
	}
	st, err := pb.State(ctx, "", whole)
	if err != nil || !st.Starred || st.PositionMS != 13000 {
		t.Errorf("the whole-file track's state = %+v (err %v), want the star and the place thirteen seconds in", st, err)
	}
	entries, err := lib.Playlists().Items(ctx, pl, "")
	if err != nil || len(entries) != 1 || entries[0].PID != whole {
		t.Errorf("playlist entries = %+v (err %v), want the whole-file track", entries, err)
	}
}

// TestARipReadBackAsOneFileUpdatesTheOpeningTrack: the whole-file track a rip's file
// becomes carries on under the opening track's pid, so the change log updates that pid and
// deletes the other track's, names no pid besides, and the scan counts an update rather
// than a new item.
func TestARipReadBackAsOneFileUpdatesTheOpeningTrack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeCueRip(t, root, 15, "First", "Second")
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	first, second := itemPIDByTitle(t, ctx, lib, "First"), itemPIDByTitle(t, ctx, lib, "Second")
	seq, err := lib.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "album.cue")); err != nil {
		t.Fatal(err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total.ItemsCreated != 0 || res.Total.ItemsUpdated != 1 {
		t.Errorf("scan = created %d, updated %d, want the one update", res.Total.ItemsCreated, res.Total.ItemsUpdated)
	}
	changes, err := lib.Changes(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[model.PID][]model.ChangeOp{}
	for _, c := range changes {
		if c.EntityType == "item" {
			ops[c.EntityPID] = append(ops[c.EntityPID], c.Op)
		}
	}
	if len(ops) != 2 || !slices.Equal(ops[first], []model.ChangeOp{model.OpUpdate}) ||
		!slices.Equal(ops[second], []model.ChangeOp{model.OpDelete}) {
		t.Errorf("item changes = %v, want an update of %s and a delete of %s", ops, first, second)
	}
}

// TestARipTrackFinishedLeavesTheWholeFileUnfinished: a rip's track heard to its end, read
// back as part of one whole-file track, leaves that track played but not finished, with
// the place where the finished track ends, since the rest of the file was never heard.
func TestARipTrackFinishedLeavesTheWholeFileUnfinished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeCueRip(t, root, 13, "First", "Second") // track 2 opens at ten seconds
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	pb := lib.Playback()
	if err := pb.MarkPlayed(ctx, "", itemPIDByTitle(t, ctx, lib, "First"), true, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "album.cue")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Virtual {
		t.Fatalf("items after the sheet went = %+v (err %v), want one whole-file track", items, err)
	}
	st, err := pb.State(ctx, "", items[0].PID)
	if err != nil || !st.Played || st.Finished || st.PositionMS != 9999 {
		t.Errorf("the whole-file track's state = %+v (err %v), want played, unfinished, at the first track's end", st, err)
	}
}

// TestARipsLastTrackFinishedLeavesThePlaceAtTheFilesEnd: the rip's last track runs to the
// end of the file, so finished it leaves the whole-file track at the file's last moment.
func TestARipsLastTrackFinishedLeavesThePlaceAtTheFilesEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeCueRip(t, root, 17, "First", "Second") // track 2 opens at ten seconds and runs to the end
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	pb := lib.Playback()
	if err := pb.MarkPlayed(ctx, "", itemPIDByTitle(t, ctx, lib, "Second"), true, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "album.cue")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].DurationMS <= 10000 {
		t.Fatalf("items after the sheet went = %+v (err %v), want one whole-file track past ten seconds", items, err)
	}
	st, err := pb.State(ctx, "", items[0].PID)
	if err != nil || st.Finished || st.PositionMS != items[0].DurationMS-1 {
		t.Errorf("the whole-file track's state = %+v (err %v), want unfinished at %d, the file's last moment", st, err, items[0].DurationMS-1)
	}
}

// TestARipReadAgainAsABookKeepsItsState: a rip's file read again as a book, once its
// root is an audiobook root, folds its tracks into the book: the opening track's pid and
// star, and a position inside the second track as that place in the book.
func TestARipReadAgainAsABookKeepsItsState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeCueRip(t, root, 11, "First", "Second")
	open := func(media model.MediaType) *waxbin.Library {
		t.Helper()
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
			Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Media: media}}})
		if err != nil {
			t.Fatal(err)
		}
		return lib
	}
	lib := open(model.MediaMixed)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	first, second := itemPIDByTitle(t, ctx, lib, "First"), itemPIDByTitle(t, ctx, lib, "Second")
	pb := lib.Playback()
	if _, err := pb.SetStar(ctx, "", first, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.Checkpoint(ctx, "", second, 3000, nil); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	lib = open(model.MediaAudiobook)
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("items after the re-read = %+v (err %v), want the one book", items, err)
	}
	if items[0].PID != first {
		t.Errorf("the book's pid = %s, want the opening track's %s", items[0].PID, first)
	}
	st, err := lib.Playback().State(ctx, "", items[0].PID)
	if err != nil || !st.Starred || st.PositionMS != 13000 {
		t.Errorf("the book's state = %+v (err %v), want the star and the place thirteen seconds in", st, err)
	}
}
