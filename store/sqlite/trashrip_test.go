package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// TestARipTrashedTwiceStaysRestorableUntilItsLastEntry: two copies of a rip trashed one
// after the other each restore every track, so purging one leaves every track trash.
func TestARipTrashedTwiceStaysRestorableUntilItsLastEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	rips, _ := ripCopies(t, st, lib, root, 2)
	var entries []model.PID
	for i, r := range rips {
		res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: r.FilePID,
			TrashPath: []byte(fmt.Sprintf("%s/t%d/album.flac", root, i)), TrashDisplay: fmt.Sprintf("%s/t%d/album.flac", root, i)})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, res.TrashPID)
	}
	if err := st.DeleteTrashRow(ctx, entries[0]); err != nil {
		t.Fatal(err)
	}
	for _, tr := range vtItems(t, st) {
		if tr.State != model.StateArchived || tr.ArchiveReason != model.ArchiveTrash {
			t.Errorf("%s = %s (%q) with the second copy still in the trash, want archived as trash", tr.Title, tr.State, tr.ArchiveReason)
		}
	}
	if err := st.DeleteTrashRow(ctx, entries[1]); err != nil {
		t.Fatal(err)
	}
	for _, tr := range vtItems(t, st) {
		if tr.ArchiveReason != model.ArchivePurge {
			t.Errorf("%s = %q with both copies purged, want purge", tr.Title, tr.ArchiveReason)
		}
	}
}

// TestATrashedRipAnswersForEveryTrack: every track of a trashed single-file rip is
// restorable from its one entry, and once the entry goes, by a purge, with its library or
// by a restore that brings nothing back, each track reads purge and is announced.
func TestATrashedRipAnswersForEveryTrack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		drop func(st *sqlite.Store, lib *model.Library, entry model.PID) error
	}{
		{"purged", func(st *sqlite.Store, _ *model.Library, entry model.PID) error { return st.DeleteTrashRow(ctx, entry) }},
		{"its library removed", func(st *sqlite.Store, lib *model.Library, _ model.PID) error {
			_, _, err := st.RemoveLibrary(ctx, lib.PID, nil)
			return err
		}},
		{"restored to nothing", func(st *sqlite.Store, _ *model.Library, entry model.PID) error {
			return st.MarkTrashRestored(ctx, entry)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st, lib, root := openCopyStore(t)
			rips, _ := ripCopies(t, st, lib, root, 1)
			res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: rips[0].FilePID,
				TrashPath: []byte(root + "/t/album.flac"), TrashDisplay: root + "/t/album.flac"})
			if err != nil {
				t.Fatal(err)
			}
			tracks := vtItems(t, st)
			if len(tracks) != 3 {
				t.Fatalf("tracks = %d, want 3", len(tracks))
			}
			var pids []model.PID
			for _, tr := range tracks {
				pids = append(pids, tr.PID)
			}
			entries, err := st.ActiveTrashForItems(ctx, pids)
			if err != nil {
				t.Fatal(err)
			}
			for _, tr := range tracks {
				if tr.ArchiveReason != model.ArchiveTrash || len(entries[tr.PID]) != 1 || entries[tr.PID][0].PID != res.TrashPID {
					t.Errorf("%s = %q with entries %+v, want trash restorable from %s", tr.Title, tr.ArchiveReason, entries[tr.PID], res.TrashPID)
				}
			}
			seq := headSeq(t, st)
			if err := c.drop(st, lib, res.TrashPID); err != nil {
				t.Fatal(err)
			}
			changes := changesAfter(t, st, seq)
			for _, tr := range vtItems(t, st) {
				if tr.ArchiveReason != model.ArchivePurge {
					t.Errorf("%s = %q, want purge", tr.Title, tr.ArchiveReason)
				}
				if !hasChange(changes, "item", tr.PID, model.OpUpdate) {
					t.Errorf("%s earned no item update", tr.Title)
				}
			}
		})
	}
}

// trashRip journals a rip's file into the trash and returns its entry.
func trashRip(t *testing.T, st *sqlite.Store, file model.PID, dst string) *model.TrashEntry {
	t.Helper()
	ctx := context.Background()
	res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: file, TrashPath: []byte(dst), TrashDisplay: dst})
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.ActiveTrashByPID(ctx, res.TrashPID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ripWindows is what a trashed rip's entry should list: each track with its window.
func ripWindows(tracks []*model.ItemView, windows [][2]int64) []model.TrashItem {
	out := make([]model.TrashItem, len(tracks))
	for i, tr := range tracks {
		out[i] = model.TrashItem{ItemPID: tr.PID, Virtual: true, StartFrames: windows[i][0], EndFrames: windows[i][1]}
	}
	return out
}

// TestATrashedRipCarriesItsEssenceAndEachTracksWindow: every read of a trashed rip's entry
// answers the file's audio and, in window order, each track with the window it played.
func TestATrashedRipCarriesItsEssenceAndEachTracksWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	rips, _ := ripCopies(t, st, lib, root, 1)
	tracks := vtItems(t, st)
	want := ripWindows(tracks, [][2]int64{{0, 9}, {9, 27}, {27, 0}})
	byPID := trashRip(t, st, rips[0].FilePID, root+"/t/album.flac")
	listed, err := st.TrashEntries(ctx, false, 0, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listing = %+v (err %v), want the one entry", listed, err)
	}
	var pids []model.PID
	for _, tr := range tracks {
		pids = append(pids, tr.PID)
	}
	byItem, err := st.ActiveTrashForItems(ctx, pids)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]model.TrashEntry{"by pid": *byPID, "listed": listed[0]}
	for _, tr := range tracks {
		if len(byItem[tr.PID]) != 1 {
			t.Fatalf("%s restorable from %+v, want one entry", tr.Title, byItem[tr.PID])
		}
		reads["under "+tr.Title] = byItem[tr.PID][0]
	}
	for name, e := range reads {
		if e.EssenceHash != "sha256:VE" || !slices.Equal(e.Items, want) {
			t.Errorf("%s: essence %q items %+v, want sha256:VE and %+v", name, e.EssenceHash, e.Items, want)
		}
		for i, tr := range tracks {
			if it, ok := reads[name].Item(tr.PID); !ok || it != want[i] {
				t.Errorf("%s: Item(%s) = %+v, %v, want %+v", name, tr.Title, it, ok, want[i])
			}
		}
		if _, ok := e.Item("NOSUCHITEM"); ok {
			t.Errorf("%s: Item found a pid the file never backed", name)
		}
	}
	if t.Failed() {
		return
	}
	byItem[pids[0]][0].Items[0].ItemPID = "CHANGED"
	if got := byItem[pids[1]][0].Items[0].ItemPID; got != pids[0] {
		t.Errorf("the entry under another track reads %s after a caller wrote to the first, want its own items", got)
	}
}

// TestATrashedRipCopyCarriesItsOwnWindows: a copy of a rip whose sheet ends a track
// elsewhere is journaled with the copy's windows, which the tracks' views never showed.
func TestATrashedRipCopyCarriesItsOwnWindows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	ripCopies(t, st, lib, root, 1)
	p := filepath.Join(root, "copy", "album.flac")
	touch(t, p)
	own := [][2]int64{{0, 8}, {9, 26}, {27, 0}}
	cp, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, p, "sha256:VE", "sha256:VC9", 600, own))
	if err != nil || !cp.AttachedAsCopy {
		t.Fatalf("copy put = %+v (err %v), want it attached as a copy", cp, err)
	}
	e := trashRip(t, st, cp.FilePID, root+"/t/album.flac")
	tracks := vtItems(t, st)
	if want := ripWindows(tracks, own); !slices.Equal(e.Items, want) {
		t.Errorf("items = %+v, want the copy's windows %+v", e.Items, want)
	}
	if tracks[0].EndFrames != 9 || tracks[1].EndFrames != 27 {
		t.Errorf("tracks end at %d and %d, want the original's 9 and 27", tracks[0].EndFrames, tracks[1].EndFrames)
	}
}

// TestATrashedRipCarriesAWindowAsItLastStood: a cue edit that moves a track's end under
// the same pid is what the entry records.
func TestATrashedRipCarriesAWindowAsItLastStood(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	_, paths := ripCopies(t, st, lib, root, 1)
	moved := [][2]int64{{0, 9}, {9, 25}, {27, 0}}
	r, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, paths[0], "sha256:VE", "sha256:VC1", 600, moved))
	if err != nil {
		t.Fatal(err)
	}
	e := trashRip(t, st, r.FilePID, root+"/t/album.flac")
	if want := ripWindows(vtItems(t, st), moved); !slices.Equal(e.Items, want) {
		t.Errorf("items = %+v, want %+v", e.Items, want)
	}
}

// TestARipsOpeningTrackIsNamedByItsWindow: a sheet that gains an earlier track on a rescan
// gives the opening track the highest item id, and the journal, FileOwner and
// FilesByEssence name it all the same.
func TestARipsOpeningTrackIsNamedByItsWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p := filepath.Join(root, "album.flac")
	touch(t, p)
	full := vtrackInput(lib.ID, p, "sha256:VE", "sha256:VC1", 600, [][2]int64{{0, 9}, {9, 27}, {27, 0}})
	later := full
	later.Tracks = full.Tracks[1:]
	if _, err := st.PutScannedVirtualTracks(ctx, later); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutScannedVirtualTracks(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	opener := vtItems(t, st)[0].PID
	if owner, _, err := st.FileOwner(ctx, r.FilePID); err != nil || owner != opener {
		t.Errorf("FileOwner = %s (err %v), want the opening track %s", owner, err, opener)
	}
	if occ, err := st.FilesByEssence(ctx, "sha256:VE"); err != nil || len(occ) != 1 || occ[0].ItemPID != opener {
		t.Errorf("FilesByEssence = %+v (err %v), want the opening track %s", occ, err, opener)
	}
	if e := trashRip(t, st, r.FilePID, root+"/t/album.flac"); e.ItemPID != opener {
		t.Errorf("entry names %s, want the opening track %s", e.ItemPID, opener)
	}
}

// TestARipCopysOpeningTrackIsNamedWhateverItsEdges: a copy that took one track over as its
// primary and backs the rest as alternates is still named by the track that opens it.
func TestARipCopysOpeningTrackIsNamedWhateverItsEdges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	rips, paths := ripCopies(t, st, lib, root, 2)
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, paths[0], "sha256:VE", "sha256:VC1", 600, [][2]int64{{0, 9}, {9, 27}})); err != nil {
		t.Fatal(err)
	}
	tracks := vtItems(t, st)
	if len(tracks) != 3 || tracks[2].FilePID != rips[1].FilePID || tracks[0].FilePID != rips[0].FilePID {
		t.Fatalf("tracks = %+v, want the copy playing track 3 alone", tracks)
	}
	opener, copied := tracks[0].PID, rips[1].FilePID
	if owner, _, err := st.FileOwner(ctx, copied); err != nil || owner != opener {
		t.Errorf("FileOwner = %s (err %v), want the opening track %s", owner, err, opener)
	}
	occ, err := st.FilesByEssence(ctx, "sha256:VE")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range occ {
		if o.FilePID == copied && o.ItemPID != opener {
			t.Errorf("FilesByEssence names %s for the copy, want the opening track %s", o.ItemPID, opener)
		}
	}
	e := trashRip(t, st, copied, root+"/t/album.flac")
	if e.ItemPID != opener || len(e.Items) != 3 || e.Items[0].ItemPID != opener {
		t.Errorf("entry names %s with items %+v, want the opening track %s first", e.ItemPID, e.Items, opener)
	}
}
