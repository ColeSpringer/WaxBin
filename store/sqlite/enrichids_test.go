package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

const (
	enrichedRelease = "e1000000-0000-4000-8000-000000000005"
	enrichedGroup   = "f1000000-0000-4000-8000-000000000006"
)

// TestEnrichmentIDsAreOwedToEveryMemberOrNone: the ids enrichment matched for an album
// and its release group are owed to the member files while the curation rows still name
// the columns' ids, and only while every member file can take them. A copy in a
// read-only library holds them from all of the files. A file settled for its fields while
// they were held is still owed them when the hold lifts, and one settled for its ids is
// not. An edition-tier match is never owed, and neither is a row that names another id.
func TestEnrichmentIDsAreOwedToEveryMemberOrNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	other, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	put := func(libID int64, path, ess, title, album string) *model.ScanItemResult {
		t.Helper()
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: libID,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(path[1:]),
				Kind: model.FileAudio, Size: 100, MTimeNS: 7,
				ContentHash: "c-" + path, EssenceHash: ess, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title,
				IdentityKey: "essence:" + ess},
			Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: album, TrackNo: 1},
		})
		if err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
		return res
	}
	a := put(lib.ID, "/lib/Animals/a.mp3", "ess-a", "Dogs", "Animals")
	b := put(lib.ID, "/lib/Animals/b.mp3", "ess-b", "Pigs", "Animals")
	cp := put(other.ID, "/other/a.mp3", "ess-a", "Dogs", "Animals")
	if cp.ItemPID != a.ItemPID {
		t.Fatalf("the copy landed on %s, want the item %s", cp.ItemPID, a.ItemPID)
	}
	m := put(lib.ID, "/lib/Meddle/m.mp3", "ess-m", "Echoes", "Meddle")
	if _, err := st.SetLibraryReadOnly(ctx, other.PID, true); err != nil {
		t.Fatal(err)
	}
	albumID := func(title string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM album WHERE title = ?", title))
	}
	albumPID := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = ?", title))
	}
	rgID := int64(scalarQueryInt(t, db, "SELECT release_group_id FROM album WHERE title = 'Animals'"))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE id = ?", rgID))
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: enrichedGroup}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: albumID("Animals"), PID: albumPID("Animals"), Matched: true, MBID: enrichedRelease}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: albumID("Meddle"), PID: albumPID("Meddle"), Matched: true,
		MBID: "a2000000-0000-4000-8000-000000000007", Provider: model.EnrichProviderMBEdition}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemRowID(t, db, a.ItemPID), PID: a.ItemPID,
		Matched: true, Provider: "p", Fields: map[string]string{"composer": "Roger Waters"}}); err != nil {
		t.Fatal(err)
	}

	type owed struct{ album, group string }
	plan := func(when string) map[model.PID]owed {
		t.Helper()
		out := map[model.PID]owed{}
		rows, err := st.EnrichmentWriteback(ctx, nil)
		if err != nil {
			t.Fatalf("%s: EnrichmentWriteback: %v", when, err)
		}
		for _, r := range rows {
			if r.AlbumMBID != "" || r.ReleaseGroupMBID != "" {
				out[r.FilePID] = owed{r.AlbumMBID, r.ReleaseGroupMBID}
			}
		}
		files, err := st.EnrichedAlbumFiles(ctx, nil)
		if err != nil {
			t.Fatalf("%s: EnrichedAlbumFiles: %v", when, err)
		}
		for _, f := range files {
			if f.AlbumMBID != "" || f.ReleaseGroupMBID != "" {
				out[f.FilePID] = owed{f.AlbumMBID, f.ReleaseGroupMBID}
			}
		}
		return out
	}
	if got := plan("held"); len(got) != 0 {
		t.Fatalf("owed while a copy is read-only = %v, want nothing", got)
	}
	held := func() int {
		t.Helper()
		pids, err := st.EnrichmentIDsHeld(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(pids)
	}
	if n := held(); n != 2 {
		t.Errorf("files held from their ids = %d, want a and b, the copy counted once, as read-only", n)
	}
	// The item's field is written while its ids are held.
	newest := int64(scalarQueryInt(t, db, "SELECT MAX(updated_at) FROM field_provenance WHERE source = 'enrichment'"))
	if err := st.SettleEnrichmentWrite(ctx, a.FilePID, newest, 0, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetLibraryReadOnly(ctx, other.PID, false); err != nil {
		t.Fatal(err)
	}
	if n := held(); n != 0 {
		t.Errorf("files held from their ids once writable = %d, want none", n)
	}
	want := owed{enrichedRelease, enrichedGroup}
	got := plan("lifted")
	if len(got) != 3 || got[a.FilePID] != want || got[b.FilePID] != want || got[cp.FilePID] != want {
		t.Fatalf("owed once writable = %v, want both ids on a, b and the copy", got)
	}
	if _, ok := got[m.FilePID]; ok {
		t.Error("an edition-tier id is owed to its files")
	}

	if err := st.SettleEnrichmentWrite(ctx, b.FilePID, 0, newest+1_000_000_000_000, newest+1_000_000_000_000); err != nil {
		t.Fatal(err)
	}
	if _, ok := plan("settled")[b.FilePID]; ok {
		t.Error("a file settled for its ids is still owed them")
	}

	// What a merge leaves when it moves a loser's row onto a survivor holding another id.
	if _, err := writeConn(t, dbPath).ExecContext(ctx, `UPDATE entity_curation SET value = 'b9000000-0000-4000-8000-000000000009'
		WHERE entity_type = 'release_group' AND field = 'mbid'`); err != nil {
		t.Fatal(err)
	}
	for pid, o := range plan("stale group row") {
		if o.group != "" {
			t.Errorf("file %s is owed a group id %q its row no longer names", pid, o.group)
		}
	}
}

// TestAnAlbumIDLandsWhileItsGroupIDIsHeld: a release group has two albums, and a copy of
// one track of the second sits in a read-only library, which holds the group's id from
// every file of both. The first album's own id, matched after the group's, is owed to its
// files, and once a write settles them it is owed no more while the group's stays held.
func TestAnAlbumIDLandsWhileItsGroupIDIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	other, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	put := func(libID int64, path, ess, title, album, rg string) *model.ScanItemResult {
		t.Helper()
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: libID,
			File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(path[1:]), Kind: model.FileAudio,
				Size: 100, MTimeNS: 7, ContentHash: "c-" + path, EssenceHash: ess, ScanState: model.ScanIndexed},
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title, IdentityKey: "essence:" + ess},
			Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: album, TrackNo: 1,
				MBReleaseGroupID: rg},
		})
		if err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
		return res
	}
	const rg = "f1000000-0000-4000-8000-0000000000aa"
	a := put(lib.ID, "/lib/Animals/a.mp3", "ess-a", "Dogs", "Animals", rg)
	put(lib.ID, "/lib/Animals Deluxe/b.mp3", "ess-b", "Pigs", "Animals", rg)
	put(other.ID, "/other/b.mp3", "ess-b", "Pigs", "Animals", rg)
	if _, err := st.SetLibraryReadOnly(ctx, other.PID, true); err != nil {
		t.Fatal(err)
	}
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("albums = %d, want the two editions", n)
	}
	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group"))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group"))
	if _, err := writeConn(t, dbPath).ExecContext(ctx, "UPDATE release_group SET mbid = NULL"); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: rg}); err != nil {
		t.Fatal(err)
	}
	albumID := int64(scalarQueryInt(t, db, "SELECT al.id FROM album al JOIN track t ON t.album_id = al.id JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'Dogs'"))
	albumPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE id = ?", albumID))
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: albumID, PID: albumPID, Matched: true, MBID: enrichedRelease}); err != nil {
		t.Fatal(err)
	}
	owedTo := func() map[model.PID]model.EnrichedAlbumFile {
		t.Helper()
		files, err := st.EnrichedAlbumFiles(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.PID]model.EnrichedAlbumFile{}
		for _, f := range files {
			out[f.FilePID] = f
		}
		return out
	}
	f, ok := owedTo()[a.FilePID]
	if !ok || f.AlbumMBID != enrichedRelease || f.ReleaseGroupMBID != "" {
		t.Fatalf("a.mp3 owed %+v (%v), want the album's id alone", f, ok)
	}
	if err := st.SettleEnrichmentWrite(ctx, a.FilePID, 0, f.AlbumMBIDAt, f.ReleaseGroupMBIDAt); err != nil {
		t.Fatal(err)
	}
	if f, ok := owedTo()[a.FilePID]; ok {
		t.Errorf("a.mp3 is owed %+v again after its write settled", f)
	}
}

// TestAMatchedIDIsOwedAgainWhereAFileLacksIt: every file was settled for its album's
// matched id. One read again without the id is owed it again, and so is a file that
// moves onto an album whose id was matched before the file was settled for its own,
// whether a read, an edit of its album, or a merge moved it.
func TestAMatchedIDIsOwedAgainWhereAFileLacksIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	put := func(name, title, album, content string) *model.ScanItemResult {
		t.Helper()
		path := "/lib/mixed/" + name + ".mp3"
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(path[1:]),
				Kind: model.FileAudio, Size: 100, MTimeNS: 7,
				ContentHash: content, EssenceHash: "ess-" + name, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title,
				IdentityKey: "essence:ess-" + name},
			Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: album, TrackNo: 1},
		})
		if err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
		return res
	}
	const meddle, animals, wish = "f2000000-0000-4000-8000-000000000001", "f2000000-0000-4000-8000-000000000002",
		"f2000000-0000-4000-8000-000000000003"
	put("m", "Echoes", "Meddle", "c-m")
	l := put("l", "Fearless", "Meddle", "c-l")
	a := put("a", "Dogs", "Animals", "c-a")
	p := put("p", "Pigs", "Animals", "c-p")
	w := put("w", "Shine On", "Wish", "c-w")
	albumPID := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = ?", title))
	}
	for _, m := range []struct{ title, mbid string }{{"Meddle", meddle}, {"Animals", animals}, {"Wish", wish}} {
		if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
			AlbumID: int64(scalarQueryInt(t, db, "SELECT id FROM album WHERE title = ?", m.title)),
			PID:     albumPID(m.title), Matched: true, MBID: m.mbid}); err != nil {
			t.Fatal(err)
		}
	}
	owed := func() map[model.PID]model.EnrichedAlbumFile {
		t.Helper()
		files, err := st.EnrichedAlbumFiles(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.PID]model.EnrichedAlbumFile{}
		for _, f := range files {
			out[f.FilePID] = f
		}
		return out
	}
	for pid, f := range owed() {
		if err := st.SettleEnrichmentWrite(ctx, pid, 0, f.AlbumMBIDAt, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := owed(); len(got) != 0 {
		t.Fatalf("owed once every file is settled = %v, want nothing", got)
	}

	put("l", "Fearless", "Meddle", "c-l2")
	put("a", "Dogs", "Meddle", "c-a2")
	if err := st.EditItemFields(ctx, w.ItemPID, map[string]string{"album": "Meddle"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, albumPID("Meddle"), albumPID("Animals")); err != nil {
		t.Fatal(err)
	}
	got := owed()
	for name, pid := range map[string]model.PID{"read without it": l.FilePID, "retagged onto it": a.FilePID,
		"moved by an edit": w.FilePID, "moved by a merge": p.FilePID} {
		if f, ok := got[pid]; !ok || f.AlbumMBID != meddle {
			t.Errorf("a file %s is owed %q, want Meddle's id", name, f.AlbumMBID)
		}
	}
	if len(got) != 4 {
		t.Errorf("owed %d files, want the four that lack the id", len(got))
	}
}
