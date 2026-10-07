package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// cascadePlaylist creates a static playlist holding members in order.
func cascadePlaylist(t *testing.T, st *sqlite.Store, name string, members ...model.PID) model.PID {
	t.Helper()
	ctx := context.Background()
	pl, err := st.CreatePlaylist(ctx, name, "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := st.AddPlaylistItems(ctx, pl, members); err != nil {
		t.Fatalf("add to %s: %v", name, err)
	}
	return pl
}

func cascadeTitles(t *testing.T, st *sqlite.Store, pl model.PID) []string {
	t.Helper()
	items, err := st.PlaylistItems(context.Background(), pl, "")
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

// cascadeDeltas counts the changes after seq by entity type and pid.
func cascadeDeltas(t *testing.T, st *sqlite.Store, seq int64) map[string]map[model.PID]int {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	out := map[string]map[model.PID]int{}
	for _, c := range changes {
		if out[c.EntityType] == nil {
			out[c.EntityType] = map[model.PID]int{}
		}
		out[c.EntityType][c.EntityPID]++
	}
	return out
}

func cascadeUpdatedAt(t *testing.T, st *sqlite.Store, pl model.PID) int64 {
	t.Helper()
	p, err := st.PlaylistByPID(context.Background(), pl)
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	return p.UpdatedAt
}

func sameTitles(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestItemDeleteSettlesItsPlaylists: a cue track a re-carve drops leaves both playlists
// that listed it with dense positions, a moved updated_at and one delta each, and the
// queue that held it with a delta of its own.
func TestItemDeleteSettlesItsPlaylists(t *testing.T) {
	t.Parallel()
	st, lib := openTestStore(t)
	ctx := context.Background()
	in := vtrackInput(lib.ID, "/lib/rip.flac", "sha256:PE", "sha256:PC", 8000, [][2]int64{{0, 300}, {300, 600}, {600, 900}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	tracks := vtItems(t, st)
	t1, t2, t3 := tracks[0].PID, tracks[1].PID, tracks[2].PID
	first := cascadePlaylist(t, st, "First", t3, t1, t3)
	second := cascadePlaylist(t, st, "Second", t2, t3)
	untouched := cascadePlaylist(t, st, "Untouched", t1)
	if err := st.SetQueue(ctx, "", []model.PID{t3, t1}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	stamps := map[model.PID]int64{first: cascadeUpdatedAt(t, st, first), second: cascadeUpdatedAt(t, st, second)}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}

	in = vtrackInput(lib.ID, "/lib/rip.flac", "sha256:PE", "sha256:PC2", 8000, [][2]int64{{0, 300}, {300, 600}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("re-carve: %v", err)
	}
	if got := cascadeTitles(t, st, first); !sameTitles(got, "Track 1") {
		t.Errorf("First = %v, want [Track 1]", got)
	}
	if got := cascadeTitles(t, st, second); !sameTitles(got, "Track 2") {
		t.Errorf("Second = %v, want [Track 2]", got)
	}
	deltas := cascadeDeltas(t, st, seq)
	for pl, before := range stamps {
		if n := deltas["playlist"][pl]; n != 1 {
			t.Errorf("playlist %s: %d deltas, want 1", pl, n)
		}
		if after := cascadeUpdatedAt(t, st, pl); after <= before {
			t.Errorf("playlist %s: updated_at %d -> %d, want it moved", pl, before, after)
		}
	}
	if n := deltas["playlist"][untouched]; n != 0 {
		t.Errorf("a playlist not holding the track got %d deltas", n)
	}
	if len(deltas["play_queue"]) != 1 {
		t.Errorf("queue deltas = %v, want one for the default user", deltas["play_queue"])
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.PlaylistPositionDrift != 0 {
		t.Errorf("verify = %+v (err %v), want no playlist position drift", rep, err)
	}
}

// TestPodcastRemovalSettlesItsPlaylists: unsubscribing a show drops its episodes from a
// playlist that also lists a track, which keeps dense positions, moves updated_at and
// emits its delta.
func TestPodcastRemovalSettlesItsPlaylists(t *testing.T) {
	t.Parallel()
	st, lib := openTestStore(t)
	ctx := context.Background()
	feed, err := st.UpsertFeed(ctx, feedInput("http://feed.example/cascade", "E1", "E2"))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, feed.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	song, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.flac", "sha256:SE", "sha256:SC", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pl := cascadePlaylist(t, st, "Mixed", eps[0].PID, song.ItemPID, eps[1].PID)
	before := cascadeUpdatedAt(t, st, pl)
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}

	if _, err := st.RemovePodcast(ctx, feed.PodcastPID); err != nil {
		t.Fatalf("remove podcast: %v", err)
	}
	if got := cascadeTitles(t, st, pl); !sameTitles(got, "Song") {
		t.Errorf("playlist = %v, want [Song]", got)
	}
	if n := cascadeDeltas(t, st, seq)["playlist"][pl]; n != 1 {
		t.Errorf("playlist deltas = %d, want 1", n)
	}
	if after := cascadeUpdatedAt(t, st, pl); after <= before {
		t.Errorf("updated_at %d -> %d, want it moved", before, after)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.PlaylistPositionDrift != 0 {
		t.Errorf("verify = %+v (err %v), want no playlist position drift", rep, err)
	}
}

// TestItemDeletesSettleEachPlaylistOnce: a re-carve dropping two tracks one playlist and
// one queue hold settles each once, one delta apiece.
func TestItemDeletesSettleEachPlaylistOnce(t *testing.T) {
	t.Parallel()
	st, lib := openTestStore(t)
	ctx := context.Background()
	in := vtrackInput(lib.ID, "/lib/rip.flac", "sha256:OE", "sha256:OC", 8000, [][2]int64{{0, 300}, {300, 600}, {600, 900}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	tracks := vtItems(t, st)
	t1, t2, t3 := tracks[0].PID, tracks[1].PID, tracks[2].PID
	pl := cascadePlaylist(t, st, "All", t3, t1, t2, t3)
	if err := st.SetQueue(ctx, "", []model.PID{t2, t3, t1}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}

	in = vtrackInput(lib.ID, "/lib/rip.flac", "sha256:OE", "sha256:OC2", 8000, [][2]int64{{0, 300}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("re-carve: %v", err)
	}
	if got := cascadeTitles(t, st, pl); !sameTitles(got, "Track 1") {
		t.Errorf("playlist = %v, want [Track 1]", got)
	}
	deltas := cascadeDeltas(t, st, seq)
	if n := deltas["playlist"][pl]; n != 1 {
		t.Errorf("playlist deltas = %d, want 1", n)
	}
	queued := 0
	for _, n := range deltas["play_queue"] {
		queued += n
	}
	if queued != 1 {
		t.Errorf("queue deltas = %v, want one", deltas["play_queue"])
	}
}

// TestQueueDeltasNameTheUser: a queue delta names its user by pid however the queue was
// written, so the default user's queue is one key whether it was set as "" or changed
// by a delete.
func TestQueueDeltasNameTheUser(t *testing.T) {
	t.Parallel()
	st, lib := openTestStore(t)
	ctx := context.Background()
	song, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/q.flac", "sha256:QE", "sha256:QC", "Queued"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	users, err := st.Users(ctx)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	var def model.PID
	for _, u := range users {
		if u.IsDefault {
			def = u.PID
		}
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	if err := st.SetQueue(ctx, "", []model.PID{song.ItemPID}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if got := cascadeDeltas(t, st, seq)["play_queue"]; len(got) != 1 || got[def] != 1 {
		t.Errorf("queue deltas = %v, want one under the default user's pid %s", got, def)
	}
}
