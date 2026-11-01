package sqlite_test

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/internal/testimage"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
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
	t.Parallel()
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
		EpisodePID: eps[0].PID, LibraryID: podLib.ID,
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// setTag sets one custom tag through SetItemTags.
func setTag(ctx context.Context, st *sqlite.Store, pid model.PID, key string, values []string, attr model.Attribution, lock model.LockChange, force bool) (string, int, error) {
	res, err := st.SetItemTags(ctx, pid, []model.TagEdit{{Key: key, Values: values}}, attr, lock, force)
	if err != nil {
		return "", 0, err
	}
	return res[0].Key, res[0].Stored, nil
}

// owedRows returns an item's owed rows by key.
func owedRows(t *testing.T, st *sqlite.Store, pid model.PID) map[string]model.FileDiagnostic {
	t.Helper()
	diags, err := st.FileDiagnostics(context.Background(), model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	out := map[string]model.FileDiagnostic{}
	for _, d := range diags {
		out[d.TagKey] = d
	}
	return out
}

// TestOwedRowsCarryTheirSetter: an owed row names who set the value it carries, from the
// edit's attribution, and a later edit of the same key takes the row over. The row also
// names the item its file backs.
func TestOwedRowsCarryTheirSetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pid := res.ItemPID
	for _, e := range []struct {
		field, value string
		attr         model.Attribution
	}{
		{"genre", "Jazz", model.Attribution{Source: model.SourceUser}},
		{"title", "Song (Live)", model.Attribution{Source: model.SourceNormalize}},
		{"year", "1999", model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}},
	} {
		if err := st.EditItemField(ctx, pid, e.field, e.value, e.attr, model.LockUnchanged, false); err != nil {
			t.Fatalf("edit %s: %v", e.field, err)
		}
	}
	want := map[string][2]string{
		"genre": {"user", ""},
		"title": {"normalize", ""},
		"year":  {"enrichment", "musicbrainz"},
	}
	rows := owedRows(t, st, pid)
	if len(rows) != len(want) {
		t.Fatalf("owed rows = %v, want %v", rows, want)
	}
	for key, w := range want {
		d := rows[key]
		if string(d.Source) != w[0] || d.Provider != w[1] {
			t.Errorf("%s setter = %q/%q, want %q/%q", key, d.Source, d.Provider, w[0], w[1])
		}
		if d.ItemPID != pid {
			t.Errorf("%s item = %q, want %q", key, d.ItemPID, pid)
		}
	}
	if err := st.EditItemField(ctx, pid, "genre", "Blues",
		model.Attribution{Source: model.SourceEnrichment, Provider: "discogs"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit genre again: %v", err)
	}
	if d := owedRows(t, st, pid)["genre"]; d.Source != model.SourceEnrichment || d.Provider != "discogs" {
		t.Errorf("genre setter after a second edit = %q/%q, want enrichment/discogs", d.Source, d.Provider)
	}
}

// TestEveryOwedWriteNamesItsSetter: the credit, entity, cover, custom tag and acquisition
// writes name their setter on the rows they owe, and a write-back that leaves a locked
// album_sort owed names whoever set that lock's value.
func TestEveryOwedWriteNamesItsSetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pid := res.ItemPID
	views, err := st.ItemsByPIDs(ctx, []model.PID{pid})
	if err != nil || len(views) != 1 {
		t.Fatalf("item view: %v (%d)", err, len(views))
	}
	album := views[0].AlbumPID
	mb := model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Composer"}, mb, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credits: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, album, map[string]string{"label": "Label"},
		model.Attribution{Source: model.SourceNormalize}, model.LockUnchanged, false); err != nil {
		t.Fatalf("album label: %v", err)
	}
	if _, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, testimage.TexturedPNG(t, 8, 8, 0), "",
		model.Attribution{Source: model.SourceGenerated}, model.LockUnchanged, false); err != nil {
		t.Fatalf("art: %v", err)
	}
	if _, _, err := setTag(ctx, st, pid, "MOOD", []string{"calm"},
		model.Attribution{Source: model.SourceEnrichment, Provider: "lastfm"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := st.SetAcquisition(ctx, pid, model.AcquisitionInput{SourceType: model.SourceManual}, model.LockUnchanged, false); err != nil {
		t.Fatalf("acquisition: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "album_sort", "Album, The",
		model.Attribution{Source: model.SourceEnrichment, Provider: "discogs"}, model.LockOn, false); err != nil {
		t.Fatalf("album_sort: %v", err)
	}
	if err := st.SettleTagWriteOwed(ctx, res.FilePID, []string{"album_sort"}); err != nil {
		t.Fatalf("settle album_sort: %v", err)
	}
	if err := st.FollowAlbumSortWrite(ctx, album, []model.PID{res.FilePID}, "Album"); err != nil {
		t.Fatalf("follow album_sort: %v", err)
	}
	want := map[string][2]string{
		model.CreditField(model.RoleComposer): {"enrichment", "musicbrainz"},
		"album.label":                         {"normalize", ""},
		model.OwedArt:                         {"generated", ""},
		"tag.MOOD":                            {"enrichment", "lastfm"},
		model.OwedAcquisition:                 {"user", ""},
		"album_sort":                          {"enrichment", "discogs"},
	}
	rows := owedRows(t, st, pid)
	for key, w := range want {
		d, ok := rows[key]
		if !ok {
			t.Errorf("no owed row for %s (rows %v)", key, rows)
			continue
		}
		if string(d.Source) != w[0] || d.Provider != w[1] {
			t.Errorf("%s setter = %q/%q, want %q/%q", key, d.Source, d.Provider, w[0], w[1])
		}
	}
}

// TestDiagnosticsFilterBySource: Source keeps the rows one setter wrote, ExcludeSource
// leaves its rows out and keeps everything else, rows naming no setter included, and an
// unknown source is refused rather than matching nothing.
func TestDiagnosticsFilterBySource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pid := res.ItemPID
	if err := st.EditItemField(ctx, pid, "genre", "Jazz", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("user edit: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "title", "Song (Live)", model.Attribution{Source: model.SourceNormalize}, model.LockUnchanged, false); err != nil {
		t.Fatalf("normalize edit: %v", err)
	}
	if err := st.PutFileDiagnostics(ctx, res.FilePID, model.OriginScan, []model.FileDiagnostic{
		{Code: model.DiagSortNameFallback, Severity: model.SeverityInfo, Detail: "title from the file name"},
	}); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	keys := func(f model.DiagnosticFilter) []string {
		t.Helper()
		ds, err := st.FileDiagnostics(ctx, f)
		if err != nil {
			t.Fatalf("diagnostics %+v: %v", f, err)
		}
		var out []string
		for _, d := range ds {
			out = append(out, string(d.Code)+":"+d.TagKey)
		}
		return out
	}
	if got := keys(model.DiagnosticFilter{ExcludeSource: model.SourceNormalize}); !slices.Equal(got,
		[]string{"tag_write_owed:genre", "sort_name_fallback:"}) {
		t.Errorf("excluding normalize = %v, want the scan row and the user's genre", got)
	}
	if got := keys(model.DiagnosticFilter{Source: model.SourceNormalize}); !slices.Equal(got, []string{"tag_write_owed:title"}) {
		t.Errorf("normalize alone = %v, want its title", got)
	}
	sum, err := st.DiagnosticSummary(ctx, model.DiagnosticFilter{Code: model.DiagTagWriteOwed, ExcludeSource: model.SourceNormalize})
	if err != nil || len(sum) != 1 || sum[0].Count != 1 {
		t.Errorf("summary excluding normalize = %+v (err %v), want one owed row", sum, err)
	}
	for _, f := range []model.DiagnosticFilter{{Source: "robot"}, {ExcludeSource: "robot"}} {
		if _, err := st.FileDiagnostics(ctx, f); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("filter %+v = %v, want CodeInvalid", f, err)
		}
	}
}

// TestAClearedTagKeepsItsSetterOnTheOwedRow: a clear drops the tag's provenance row, so the
// owed row it leaves is the only record of who cleared it.
func TestAClearedTagKeepsItsSetterOnTheOwedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	pid := res.ItemPID
	if _, _, err := setTag(ctx, st, pid, "MOOD", []string{"calm"}, model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, _, err := setTag(ctx, st, pid, "MOOD", nil, model.Attribution{Source: model.SourceNormalize}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	prov, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	for _, p := range prov {
		if p.Field == "tag.MOOD" {
			t.Fatalf("the clear left a provenance row: %+v", p)
		}
	}
	if d := owedRows(t, st, pid)["tag.MOOD"]; d.Source != model.SourceNormalize {
		t.Errorf("owed tag.MOOD setter = %q, want normalize (the clear's)", d.Source)
	}
}

// TestDiagnosticsNameTheirItem: a row names the item its file backs, and none on a file
// several items share, such as a rip's.
func TestDiagnosticsNameTheirItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	rip, err := st.PutScannedVirtualTracks(ctx,
		vtrackInput(lib.ID, "/lib/album.flac", "sha256:VE", "sha256:VC", 8000, [][2]int64{{0, 300}, {300, 600}}))
	if err != nil {
		t.Fatalf("put rip: %v", err)
	}
	row := []model.FileDiagnostic{{Code: model.DiagSortNameFallback, Severity: model.SeverityInfo}}
	for _, f := range []model.PID{res.FilePID, rip.FilePID} {
		if err := st.PutFileDiagnostics(ctx, f, model.OriginScan, row); err != nil {
			t.Fatalf("diagnostic on %s: %v", f, err)
		}
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	got := map[model.PID]model.PID{}
	for _, d := range ds {
		got[d.FilePID] = d.ItemPID
	}
	if got[res.FilePID] != res.ItemPID || got[rip.FilePID] != "" || len(got) != 2 {
		t.Errorf("items by file = %v, want %s on the track's file and none on the rip's", got, res.ItemPID)
	}
}

// TestDiagnosticsCanLeaveTheItemOut: a reader that does not use a row's item can ask for
// the rows without it, sparing the per-row lookup.
func TestDiagnosticsCanLeaveTheItemOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/song.mp3", "sha256:E", "sha256:C", "Song"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	if err := st.PutFileDiagnostics(ctx, res.FilePID, model.OriginScan, []model.FileDiagnostic{
		{Code: model.DiagSortNameFallback, Severity: model.SeverityInfo},
	}); err != nil {
		t.Fatal(err)
	}
	for _, omit := range []bool{false, true} {
		ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{OmitItemPID: omit})
		if err != nil || len(ds) != 1 {
			t.Fatalf("omit %v: rows = %+v (err %v)", omit, ds, err)
		}
		if want := map[bool]model.PID{false: res.ItemPID, true: ""}[omit]; ds[0].ItemPID != want {
			t.Errorf("omit %v: item = %q, want %q", omit, ds[0].ItemPID, want)
		}
	}
}
