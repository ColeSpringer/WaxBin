package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

func owedFields(t *testing.T, st *sqlite.Store, pid model.PID) map[string]bool {
	t.Helper()
	diags, err := st.FileDiagnostics(context.Background(), model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	out := map[string]bool{}
	for _, d := range diags {
		out[d.TagKey] = true
	}
	return out
}

// TestEditOwesNoFileAWriteCannotReach: an edit owes a write to a track's own file, but not
// to a rip's shared file, whose tags belong to every track cut from it, nor to an
// episode's, whose tags are its feed's.
func TestEditOwesNoFileAWriteCannotReach(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	if _, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song")); err != nil {
		t.Fatalf("put track: %v", err)
	}
	if _, err := st.PutScannedVirtualTracks(ctx,
		vtrackInput(lib.ID, "/lib/album.flac", "sha256:VE", "sha256:VC", 8000, [][2]int64{{0, 300}, {300, 600}})); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	feed, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Ep"))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, feed.PodcastPID, 0)
	podLib, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: eps[0].PID, LibraryID: podLib,
		File: model.File{Path: []byte("/podcasts/a.mp3"), DisplayPath: "/podcasts/a.mp3", RelPath: []byte("a.mp3"),
			Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed},
	}); err != nil {
		t.Fatalf("attach episode file: %v", err)
	}

	items := vtItems(t, st)
	var track, ripTrack model.PID
	for _, it := range items {
		switch {
		case it.Virtual && ripTrack == "":
			ripTrack = it.PID
		case it.Title == "Song":
			track = it.PID
		}
	}
	user := model.Attribution{Source: model.SourceUser}
	for _, pid := range []model.PID{track, ripTrack} {
		if err := st.EditItemFields(ctx, pid, map[string]string{"title": "Edited", "genre": "Jazz"}, user, model.LockUnchanged, false); err != nil {
			t.Fatalf("edit %s: %v", pid, err)
		}
	}
	if err := st.EditItemField(ctx, eps[0].PID, "title", "Edited", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit episode: %v", err)
	}
	if got := owedFields(t, st, track); !got["title"] || !got["genre"] || len(got) != 2 {
		t.Errorf("track owed = %v, want title and genre", got)
	}
	if got := owedFields(t, st, ripTrack); len(got) != 0 {
		t.Errorf("rip track owed = %v, want none on a shared file", got)
	}
	if got := owedFields(t, st, eps[0].PID); len(got) != 0 {
		t.Errorf("episode owed = %v, want none", got)
	}
}

// TestEditOwesWhatItChanged: an edit records its owed rows in its own transaction, one per
// value it changed, so a form that saves every field owes only the ones that moved. A
// credit set to the names already credited, or a field set to the value it holds, owes
// nothing.
func TestEditOwesWhatItChanged(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pid := res.ItemPID
	user := model.Attribution{Source: model.SourceUser}
	if err := st.EditItemFields(ctx, pid, map[string]string{"title": "Song", "album": "Album", "genre": "Jazz"},
		user, model.LockOn, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := owedFields(t, st, pid); !got["genre"] || len(got) != 1 {
		t.Fatalf("owed after the edit = %v, want genre alone", got)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleArtist, []string{"Artist"}, user, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set the credited artist: %v", err)
	}
	if got := owedFields(t, st, pid); got[model.CreditField(model.RoleArtist)] {
		t.Fatalf("owed after crediting the same artist = %v, want no artist row", got)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Producer"}, user, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set a producer: %v", err)
	}
	if got := owedFields(t, st, pid); !got[model.CreditField(model.RoleProducer)] {
		t.Fatalf("owed after a new producer = %v, want credit.producer", got)
	}
}

// TestOwedEntityRowsLeaveWithTheItem: an album value owed to a member's file stops being
// owed once a rescan moves the member onto another album, which owes the file nothing of
// the album it left; a member still on the album keeps its row.
func TestOwedEntityRowsLeaveWithTheItem(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	one := input(lib.ID, "/lib/one.mp3", "sha256:E1", "sha256:C1", "One")
	two := input(lib.ID, "/lib/two.mp3", "sha256:E2", "sha256:C2", "Two")
	r1, err := st.PutScannedTrack(ctx, one)
	if err != nil {
		t.Fatalf("put one: %v", err)
	}
	r2, err := st.PutScannedTrack(ctx, two)
	if err != nil {
		t.Fatalf("put two: %v", err)
	}
	v, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("get one: %v", err)
	}
	user := model.Attribution{Source: model.SourceUser}
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"}, user, model.LockOn, false); err != nil {
		t.Fatalf("edit label: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, v.ArtistPID, map[string]string{"sort": "Artist, The"}, user, model.LockOn, false); err != nil {
		t.Fatalf("edit artist sort: %v", err)
	}
	for _, pid := range []model.PID{r1.ItemPID, r2.ItemPID} {
		if got := owedFields(t, st, pid); !got["album.label"] || !got["artist.sort"] {
			t.Fatalf("owed on %s = %v, want album.label and artist.sort", pid, got)
		}
	}

	one.File.ContentHash, one.Track.Album, one.Track.Artist = "sha256:C1b", "Elsewhere", "Someone Else"
	if _, err := st.PutScannedTrack(ctx, one); err != nil {
		t.Fatalf("rescan one onto another album and artist: %v", err)
	}
	if got := owedFields(t, st, r1.ItemPID); got["album.label"] || got["artist.sort"] {
		t.Errorf("owed on the moved member = %v, want neither album.label nor artist.sort", got)
	}
	if got := owedFields(t, st, r2.ItemPID); !got["album.label"] || !got["artist.sort"] {
		t.Errorf("owed on the member that stayed = %v, want both kept", got)
	}
}

// TestRipConversionDropsOwedRows: a whole-file track whose file becomes a cue rip drops
// the rows its edits owed the file, since a rip's shared file takes no write-back.
func TestRipConversionDropsOwedRows(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	r, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", "Whole File"))
	if err != nil {
		t.Fatalf("put whole-file track: %v", err)
	}
	if err := st.EditItemField(ctx, r.ItemPID, "genre", "Jazz", model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if n := owedRowCount(t, st); n != 1 {
		t.Fatalf("owed rows = %d, want the genre's", n)
	}
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC2", 360,
		[][2]int64{{0, 9}, {9, 27}})); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	if n := owedRowCount(t, st); n != 0 {
		t.Fatalf("owed rows after the rip = %d, want none", n)
	}
}

func owedRowCount(t *testing.T, st *sqlite.Store) int {
	t.Helper()
	diags, err := st.FileDiagnostics(context.Background(), model.DiagnosticFilter{Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	return len(diags)
}

// TestOwedEntityRowsFollowACarriedAlbum: a file moved to another folder re-keys its album,
// and the drained album's identity is carried, either onto the new key or into a curated
// album already there, so the album value its file still lacks stays owed.
func TestOwedEntityRowsFollowACarriedAlbum(t *testing.T) {
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	for _, intoCurated := range []bool{false, true} {
		st, lib := openTestStore(t)
		in := input(lib.ID, "/lib/a/one.mp3", "sha256:E1", "sha256:C1", "One")
		r, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		v, err := st.ItemByPID(ctx, r.ItemPID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if _, err := st.EditEntityFields(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"}, user, model.LockOn, false); err != nil {
			t.Fatalf("edit label: %v", err)
		}
		if intoCurated {
			r2, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/b/two.mp3", "sha256:E2", "sha256:C2", "Two"))
			if err != nil {
				t.Fatalf("put two: %v", err)
			}
			v2, err := st.ItemByPID(ctx, r2.ItemPID)
			if err != nil || v2.AlbumPID == v.AlbumPID {
				t.Fatalf("two's album = %q (err %v), want a second album", v2.AlbumPID, err)
			}
			if _, err := st.EditEntityFields(ctx, model.MergeAlbum, v2.AlbumPID, map[string]string{"catalog_number": "SHVL 804"}, user, model.LockOn, false); err != nil {
				t.Fatalf("curate the destination: %v", err)
			}
		}
		in.File.Path, in.File.DisplayPath = []byte("/lib/b/one.mp3"), "/lib/b/one.mp3"
		if moved, err := st.PutScannedTrack(ctx, in); err != nil || moved.ItemPID != r.ItemPID {
			t.Fatalf("move = %+v (err %v), want the item relinked", moved, err)
		}
		if got := owedFields(t, st, r.ItemPID); !got["album.label"] {
			t.Errorf("into a curated album %v: owed after the move = %v, want album.label kept", intoCurated, got)
		}
	}
}
