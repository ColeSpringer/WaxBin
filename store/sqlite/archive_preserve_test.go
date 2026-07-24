package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// Removing a file archives the logical item, it does not delete it. Every
// file-removal path sets the item state to archived or remote and leaves the
// playable_item row in place, so the play_state ON DELETE CASCADE never fires and
// a user's rating, star, and play count survive. These regression tests lock that
// in, along with the change delta a WaxDeck tailer needs to refresh the item once
// its state flips.

// seedPlayState gives an item a non-trivial default-user play_state row: rating,
// star, and one play (play_count=1, played=1).
func seedPlayState(t *testing.T, ctx context.Context, st *sqlite.Store, itemPID model.PID) {
	t.Helper()
	rating := 80
	if err := st.SetRating(ctx, "", itemPID, &rating, nil); err != nil {
		t.Fatalf("SetRating: %v", err)
	}
	if err := st.SetStar(ctx, "", itemPID, true, nil); err != nil {
		t.Fatalf("SetStar: %v", err)
	}
	if err := st.MarkPlayed(ctx, "", itemPID, false); err != nil {
		t.Fatalf("MarkPlayed: %v", err)
	}
}

func assertPlayStatePreserved(t *testing.T, ctx context.Context, st *sqlite.Store, itemPID model.PID) {
	t.Helper()
	ps, err := st.PlayStateFor(ctx, "", itemPID)
	if err != nil {
		t.Fatalf("PlayStateFor after archive: %v", err)
	}
	if !ps.HasRating || ps.Rating != 80 {
		t.Fatalf("rating not preserved: %+v", ps)
	}
	if !ps.Starred {
		t.Fatalf("star not preserved: %+v", ps)
	}
	if ps.PlayCount != 1 || !ps.Played {
		t.Fatalf("play count/played not preserved: %+v", ps)
	}
}

// hasChange reports whether the change set contains an entry for the given
// entity/pid/op.
func hasChange(changes []model.Change, entity string, pid model.PID, op model.ChangeOp) bool {
	for _, c := range changes {
		if c.EntityType == entity && c.EntityPID == pid && c.Op == op {
			return true
		}
	}
	return false
}

func TestDetachFilePreservesPlayState(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	r, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	seedPlayState(t, ctx, st, r.ItemPID)

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.DetachFile(ctx, r.FilePID); err != nil {
		t.Fatalf("DetachFile: %v", err)
	}

	// The item survives, archived (not deleted), and stays readable.
	it, err := st.ItemByPID(ctx, r.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID after detach: %v (item must survive, not cascade-delete)", err)
	}
	if it.State != model.StateArchived {
		t.Fatalf("state = %s, want archived", it.State)
	}
	assertPlayStatePreserved(t, ctx, st, r.ItemPID)

	// The tailer must see the item flip to archived and the file removed.
	changes, err := st.ChangesSince(ctx, seq0)
	if err != nil {
		t.Fatalf("ChangesSince: %v", err)
	}
	if !hasChange(changes, "item", r.ItemPID, model.OpUpdate) {
		t.Fatalf("missing item OpUpdate delta for archived item; got %+v", changes)
	}
	if !hasChange(changes, "file", r.FilePID, model.OpDelete) {
		t.Fatalf("missing file OpDelete delta; got %+v", changes)
	}
}

func TestDropEpisodeFilePreservesPlayState(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)

	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	target := eps[0]

	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("EnsurePodcastLibrary: %v", err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: target.PID,
		LibraryID:  libID,
		File: model.File{
			Path: []byte("/podcasts/a.mp3"), DisplayPath: "/podcasts/a.mp3",
			RelPath: []byte("a.mp3"), Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed,
		},
	}); err != nil {
		t.Fatalf("AttachEpisodeFile: %v", err)
	}
	seedPlayState(t, ctx, st, target.PID)

	// The downloaded episode's backing file pid, captured before the drop.
	before, err := st.ItemByPID(ctx, target.PID)
	if err != nil {
		t.Fatalf("ItemByPID before drop: %v", err)
	}
	filePID := before.FilePID

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.DropEpisodeFile(ctx, target.PID); err != nil {
		t.Fatalf("DropEpisodeFile: %v", err)
	}

	// The episode item survives as remote (retention), not deleted.
	it, err := st.ItemByPID(ctx, target.PID)
	if err != nil {
		t.Fatalf("ItemByPID after drop: %v (episode must survive retention, not cascade-delete)", err)
	}
	if it.State != model.StateRemote {
		t.Fatalf("state = %s, want remote", it.State)
	}
	assertPlayStatePreserved(t, ctx, st, target.PID)

	changes, err := st.ChangesSince(ctx, seq0)
	if err != nil {
		t.Fatalf("ChangesSince: %v", err)
	}
	if !hasChange(changes, "item", target.PID, model.OpUpdate) {
		t.Fatalf("missing item OpUpdate delta for remote episode; got %+v", changes)
	}
	if !hasChange(changes, "file", filePID, model.OpDelete) {
		t.Fatalf("missing file OpDelete delta; got %+v", changes)
	}
}
