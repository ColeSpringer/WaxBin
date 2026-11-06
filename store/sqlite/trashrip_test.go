package sqlite_test

import (
	"context"
	"fmt"
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
