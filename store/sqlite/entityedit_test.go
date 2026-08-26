package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// entityPIDByCol reads an entity's public id from a column lookup (test-only).
func entityPIDByCol(t *testing.T, st *Store, table, col, val string) model.PID {
	t.Helper()
	var pid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM "+table+" WHERE "+col+" = ?", val).Scan(&pid); err != nil {
		t.Fatalf("no %s with %s=%q: %v", table, col, val, err)
	}
	return model.PID(pid)
}

func entityIDByCol(t *testing.T, st *Store, table, col, val string) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT id FROM "+table+" WHERE "+col+" = ?", val).Scan(&id); err != nil {
		t.Fatalf("no %s with %s=%q: %v", table, col, val, err)
	}
	return id
}

func TestEditEntityAlbumIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Album", genre: "Rock", durationMS: 100,
	})
	albumPID := entityPIDByCol(t, st, "album", "title", "Album")

	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "036000291452", "catalog_number": "CAT-1", "label": "Indie Co"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit album: %v", err)
	}

	var barcode, catalog, label string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(barcode,''), COALESCE(catalog_number,''), COALESCE(label,'') FROM album WHERE pid=?",
		string(albumPID)).Scan(&barcode, &catalog, &label); err != nil {
		t.Fatalf("read album cols: %v", err)
	}
	if barcode != "036000291452" || catalog != "CAT-1" || label != "Indie Co" {
		t.Fatalf("album identifiers not written: barcode=%q catalog=%q label=%q", barcode, catalog, label)
	}

	rows, err := st.EntityCuration(ctx, model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	locked := map[string]bool{}
	for _, r := range rows {
		locked[r.Field] = r.Locked
	}
	for _, f := range []string{"barcode", "catalog_number", "label"} {
		if !locked[f] {
			t.Fatalf("field %q should be locked after a user edit", f)
		}
	}

	// db verify stays clean.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("db verify not consistent after entity edit: %+v", rep)
	}
}

func TestScanPopulatesAlbumIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A scanned track carries album-level release identifiers; they land on the album
	// entity, not the denormalized track row.
	in := model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/1.flac"), DisplayPath: "/lib/1.flac", RelPath: []byte("1.flac"),
			Kind: model.FileAudio, Size: 3, MTimeNS: 1, ContentHash: "c1", EssenceHash: "e1",
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "One",
			SortKey: model.SortKey("One"), IdentityKey: "essence:e1",
		},
		Track: model.Track{
			Artist: "Artist", Album: "Album",
			Barcode: "0123456789", Label: "Indie Co", CatalogNumber: "CAT-1",
		},
	}
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}
	var barcode, label, catalog string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(barcode,''), COALESCE(label,''), COALESCE(catalog_number,'') FROM album WHERE title='Album'").
		Scan(&barcode, &label, &catalog); err != nil {
		t.Fatalf("read album: %v", err)
	}
	// The scanned barcode is stored verbatim, checksum or not: identifier
	// validation is an edit-surface contract, and the scan stays faithful to the
	// file's tags.
	if barcode != "0123456789" || label != "Indie Co" || catalog != "CAT-1" {
		t.Fatalf("scan did not populate album identifiers: barcode=%q label=%q catalog=%q", barcode, label, catalog)
	}
}

func TestEditEntitySortOverride(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Miles Davis", album: "Album", genre: "Jazz", durationMS: 100,
	})
	artistPID := entityPIDByCol(t, st, "artist", "name", "Miles Davis")

	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"sort": "Davis, Miles"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit sort: %v", err)
	}
	var sortKey string
	if err := st.read.QueryRowContext(ctx, "SELECT sort_key FROM artist WHERE pid=?", string(artistPID)).Scan(&sortKey); err != nil {
		t.Fatalf("read sort_key: %v", err)
	}
	if want := model.SortKey("Davis, Miles"); sortKey != want {
		t.Fatalf("sort_key = %q, want %q", sortKey, want)
	}

	// A curated sort override must not count as db-verify drift.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.SortKeyDrift != 0 {
		t.Fatalf("curated sort override counted as drift: %+v", rep)
	}

	// Clearing the override regenerates the sort key from the display name.
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"sort": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
		t.Fatalf("clear sort: %v", err)
	}
	if err := st.read.QueryRowContext(ctx, "SELECT sort_key FROM artist WHERE pid=?", string(artistPID)).Scan(&sortKey); err != nil {
		t.Fatalf("read sort_key: %v", err)
	}
	if want := model.SortKey("Miles Davis"); sortKey != want {
		t.Fatalf("cleared sort_key = %q, want regenerated %q", sortKey, want)
	}
}

func TestEnrichRespectsReleaseGroupTypeLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Live Set", genre: "Rock", durationMS: 100,
	})
	rgPID := entityPIDByCol(t, st, "release_group", "title", "Live Set")
	rgID := entityIDByCol(t, st, "release_group", "title", "Live Set")

	// User curates the release-group type and locks it.
	if _, err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"type": "ep"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit type: %v", err)
	}

	// Enrichment tries to set a different type; the lock must hold.
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, Type: "album",
	}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var typ string
	if err := st.read.QueryRowContext(ctx, "SELECT COALESCE(type,'') FROM release_group WHERE id=?", rgID).Scan(&typ); err != nil {
		t.Fatalf("read type: %v", err)
	}
	if typ != "ep" {
		t.Fatalf("locked release-group type was overwritten by enrichment: got %q, want ep", typ)
	}

	// An invalid type is rejected.
	if _, err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"type": "bootleg"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("invalid type should be CodeInvalid, got %v", err)
	}
}

func TestEnrichRespectsArtistMBIDLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Album", genre: "Rock", durationMS: 100,
	})
	artistPID := entityPIDByCol(t, st, "artist", "name", "Artist")
	artistID := entityIDByCol(t, st, "artist", "name", "Artist")

	// User clears and locks the artist MBID (a locked-empty value the fill-when-empty
	// enrich guard would otherwise refill).
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"mbid": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit mbid: %v", err)
	}
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{
		ArtistID: artistID, PID: artistPID, Matched: true,
		MBID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var mbid string
	if err := st.read.QueryRowContext(ctx, "SELECT COALESCE(mbid,'') FROM artist WHERE id=?", artistID).Scan(&mbid); err != nil {
		t.Fatalf("read mbid: %v", err)
	}
	if mbid != "" {
		t.Fatalf("locked-empty artist mbid was refilled by enrichment: %q", mbid)
	}
}

func TestEditEntityRejectsDuplicateMBID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "A1", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Beta", album: "A2", genre: "Rock", durationMS: 100,
	})
	alpha := entityPIDByCol(t, st, "artist", "name", "Alpha")
	beta := entityPIDByCol(t, st, "artist", "name", "Beta")

	mbid := "22222222-2222-2222-2222-222222222222"
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, alpha,
		map[string]string{"mbid": mbid}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	// Setting the SAME mbid on another artist must be refused (enrichment relies on
	// entity-mbid uniqueness).
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, beta,
		map[string]string{"mbid": mbid}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("duplicate mbid should be CodeConflict, got %v", err)
	}
}

func TestMergeEntityCurationLockedWins(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Beatles", album: "A1", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "The Beatles", album: "A2", genre: "Rock", durationMS: 200,
	})
	survivor := entityPIDByCol(t, st, "artist", "name", "The Beatles")
	loser := entityPIDByCol(t, st, "artist", "name", "Beatles")

	// Survivor curates sort unlocked; loser curates the same field locked.
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, survivor,
		map[string]string{"sort": "SSort"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("edit survivor: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, loser,
		map[string]string{"sort": "LSort"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit loser: %v", err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}

	rows, err := st.EntityCuration(ctx, model.MergeArtist, survivor)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "sort" {
		t.Fatalf("expected one sort curation row after merge, got %+v", rows)
	}
	// Locked-wins: the survivor keeps its value but inherits the loser's lock.
	if rows[0].Value != "SSort" {
		t.Fatalf("survivor value should win on conflict: got %q", rows[0].Value)
	}
	if !rows[0].Locked {
		t.Fatalf("survivor lock should be unioned from the loser (locked-wins)")
	}
}

// TestEntityCurationCarriesAttributionAndLockChange is the entity-side twin of
// TestEditCarriesTheCallersAttribution and TestEditWithLockUnchangedLeavesTheLockStanding:
// entity_curation carries its own provider column and the same preserving upsert, so
// both are pinned here rather than left to the item side's coverage.
func TestEntityCurationCarriesAttributionAndLockChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Album", genre: "Rock", durationMS: 100,
	})
	albumPID := entityPIDByCol(t, st, "album", "title", "Album")

	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "036000291452"},
		model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}, model.LockOn, false); err != nil {
		t.Fatalf("stamped entity edit: %v", err)
	}
	rows, err := st.EntityCuration(ctx, model.MergeAlbum, albumPID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("curation = %+v (err %v), want one row", rows, err)
	}
	if rows[0].Source != model.SourceEnrichment || rows[0].Provider != "musicbrainz" || !rows[0].Locked {
		t.Fatalf("row = %+v, want a locked musicbrainz row", rows[0])
	}

	// A later edit that says nothing about the lock leaves it standing, and an unstated
	// origin is a user edit whose provider goes with the source it belonged to.
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "012345678905"},
		model.Attribution{}, model.LockUnchanged, true); err != nil {
		t.Fatalf("unchanged-lock entity edit: %v", err)
	}
	rows, err = st.EntityCuration(ctx, model.MergeAlbum, albumPID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("curation = %+v (err %v), want one row", rows, err)
	}
	if !rows[0].Locked || rows[0].Source != model.SourceUser || rows[0].Provider != "" {
		t.Errorf("row = %+v, want a still-locked user row with no provider", rows[0])
	}
	if rows[0].Value != "012345678905" {
		t.Errorf("value = %q, want the second edit to have applied", rows[0].Value)
	}

	// A fresh row under LockUnchanged inserts unlocked.
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"label": "Indie Co"}, model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("fresh unchanged-lock entity edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT locked FROM entity_curation WHERE field='label'"); n != 0 {
		t.Errorf("fresh label row locked = %d, want 0", n)
	}
}

// clearEntityMBID is the escape-hatch gesture the re-key tests drive: a user edit that
// blanks the entity's mbid and locks the emptiness. It returns the edit's report, which
// names a survivor when the re-key merged the entity away.
func clearEntityMBID(t *testing.T, st *Store, et model.MergeEntity, pid model.PID) *model.EntityEditReport {
	t.Helper()
	rep, err := st.EditEntityFields(context.Background(), et, pid, map[string]string{"mbid": ""},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if err != nil {
		t.Fatalf("clear %s mbid: %v", et, err)
	}
	return rep
}

// TestEntityEditClearAlbumMBIDRekeysHeuristic pins the escape hatch: clearing an album's
// release id rewrites its match key to the heuristic form a scan of its members computes,
// so a rescan of a member whose file lost the tag lands back on the same row instead of
// forking, and the edit path's key carryover derives the same key.
func TestEntityEditClearAlbumMBIDRekeysHeuristic(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "22222222-2222-2222-2222-222222222222"
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path:    "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac",
			essence: "e" + title, content: "c" + title, title: title,
			artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
			year: 2001, mbRelease: relMBID, durationMS: 100,
		})
	}
	albumPID := entityPIDByCol(t, st, "album", "title", "One")
	albumID := entityIDByCol(t, st, "album", "title", "One")
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != "mbid:"+relMBID {
		t.Fatalf("album match_key = %q, want the mbid form", k)
	}

	rep := clearEntityMBID(t, st, model.MergeAlbum, albumPID)
	if rep.MergedInto != "" {
		t.Fatalf("report = %+v, want no survivor: the row is still there", rep)
	}

	wantKey := identity.AlbumKey("", identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One"),
		2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != wantKey {
		t.Fatalf("album match_key = %q, want the heuristic %q", k, wantKey)
	}
	if p := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", albumID); model.PID(p) != albumPID {
		t.Fatalf("album pid changed: %q", p)
	}
	if m := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM album WHERE id=?", albumID); m != "" {
		t.Fatalf("album mbid column = %q, want cleared", m)
	}

	// A rescan of a member whose file no longer carries the release id resolves onto the
	// same row rather than creating a heuristic twin.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "eT1", content: "cT1b", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after rescan = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != int(albumID) {
		t.Fatalf("rescan landed on album %d, want %d", id, albumID)
	}

	// The edit path derives the same key from the row it sits on, so an unrelated
	// item edit does not fork the other member off.
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T2'"))
	if err := st.EditItemField(ctx, pid2, "genre", "Jazz",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after item edit = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 2 {
		t.Fatalf("members on the album = %d, want 2", n)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearAlbumMBIDKeepsRGCarryover pins the carryover the re-key makes: only
// the release id is dropped, so an album under an mbid-keyed release group lands on the
// al:mbid:<group> form a scan of the same files computes, and the group keeps its own id.
func TestEntityEditClearAlbumMBIDKeepsRGCarryover(t *testing.T) {
	st, lib := entityFixture(t)
	const relMBID = "99999999-9999-9999-9999-999999999999"
	const rgMBID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path:    "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac",
			essence: "e" + title, content: "c" + title, title: title,
			artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
			year: 2001, mbRelease: relMBID, mbReleaseGroup: rgMBID, durationMS: 100,
		})
	}
	albumPID := entityPIDByCol(t, st, "album", "match_key", "mbid:"+relMBID)
	albumID := entityIDByCol(t, st, "album", "match_key", "mbid:"+relMBID)
	rgID := entityIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)

	clearEntityMBID(t, st, model.MergeAlbum, albumPID)

	wantKey := identity.AlbumKey("", "mbid:"+rgMBID, 2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != wantKey {
		t.Fatalf("album match_key = %q, want the group-carrying %q", k, wantKey)
	}
	if p := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", albumID); model.PID(p) != albumPID {
		t.Fatalf("album pid changed: %q", p)
	}
	// Only the release was disowned, so the group keeps both its key and its column.
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != "mbid:"+rgMBID {
		t.Fatalf("release_group match_key = %q, want it untouched", k)
	}
	if m := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM release_group WHERE id=?", rgID); m != rgMBID {
		t.Fatalf("release_group mbid column = %q, want it untouched", m)
	}

	// A rescan of a member whose file lost the release id but kept the group's resolves
	// onto the re-keyed row rather than forking a twin.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "eT1", content: "cT1b", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbReleaseGroup: rgMBID, durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after rescan = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != int(albumID) {
		t.Fatalf("rescan landed on album %d, want %d", id, albumID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release_group rows after rescan = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearAlbumMBIDMergesIntoHeuristicTwin covers the taken-key half of the
// hatch: when a heuristic sibling already owns the key the clear derives, the disowned
// album folds into it rather than being left on the id it just gave up.
func TestEntityEditClearAlbumMBIDMergesIntoHeuristicTwin(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "33333333-3333-3333-3333-333333333333"
	// Same release group, same folder, same year: one member carries the release id and
	// the other does not, so the scan keyed them onto two album rows.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbRelease: relMBID, durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2", n)
	}
	heuristicKey := identity.AlbumKey("", identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One"),
		2001, 0, "/lib/Alpha/One")
	twinPID := entityPIDByCol(t, st, "album", "match_key", heuristicKey)
	twinID := entityIDByCol(t, st, "album", "match_key", heuristicKey)
	identified := entityPIDByCol(t, st, "album", "match_key", "mbid:"+relMBID)
	seq := latestSeq(t, st)

	rep := clearEntityMBID(t, st, model.MergeAlbum, identified)
	// The edited pid is gone, so the report has to name the row that absorbed it or the
	// caller goes on talking about a deleted entity.
	if rep.MergedInto != twinPID {
		t.Fatalf("report = %+v, want the survivor %q", rep, twinPID)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the clear = %d, want 1", n)
	}
	if p := scalarStr(t, st, "SELECT pid FROM album"); model.PID(p) != twinPID {
		t.Fatalf("surviving album pid = %q, want the heuristic twin %q", p, twinPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", twinID); n != 2 {
		t.Fatalf("members on the twin = %d, want both", n)
	}
	if n := changeCount(t, st, seq, "album", model.OpDelete); n != 1 {
		t.Fatalf("album delete deltas = %d, want 1", n)
	}
	// The locked-empty mbid the clear recorded moves with the merge, so enrichment does
	// not put the disowned id straight back.
	rows, err := st.EntityCuration(ctx, model.MergeAlbum, twinPID)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Field == "mbid" {
			found = true
			if r.Value != "" || !r.Locked {
				t.Fatalf("carried mbid curation = %+v, want a locked empty value", r)
			}
		}
	}
	if !found {
		t.Fatalf("no mbid curation row carried onto the survivor: %+v", rows)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearRGMBIDRekeysChainAndAlbumKeys pins the release-group half: the group
// key goes back to its heuristic form, the marker that recorded the match goes with it,
// and every dependent album key has the group segment swapped, without which each album
// would sit on a key no scan computes.
func TestEntityEditClearRGMBIDRekeysChainAndAlbumKeys(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "44444444-4444-4444-4444-444444444444"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbReleaseGroup: rgMBID, durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One Deluxe/01.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2002, mbReleaseGroup: rgMBID, durationMS: 100,
	})
	rgPID := entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	rgID := entityIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	albumID1 := entityIDByCol(t, st, "album", "match_key",
		identity.AlbumKey("", "mbid:"+rgMBID, 2001, 0, "/lib/Alpha/One"))
	albumID2 := entityIDByCol(t, st, "album", "match_key",
		identity.AlbumKey("", "mbid:"+rgMBID, 2002, 0, "/lib/Alpha/One Deluxe"))

	// A matched marker to take back with the id.
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: rgMBID, Type: "album",
	}); err != nil {
		t.Fatalf("enrich release group: %v", err)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group' AND entity_id=?", rgID); n != 1 {
		t.Fatalf("marker rows before the clear = %d, want 1", n)
	}
	// The aux-art backfill keeps its own marker under its own entity type, and it gates
	// the same queue, so the undo has to take that one back too.
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true,
	}); err != nil {
		t.Fatalf("mark aux art: %v", err)
	}

	clearEntityMBID(t, st, model.MergeReleaseGroup, rgPID)

	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", rgID); n != 0 {
		t.Errorf("aux-art marker rows after the clear = %d, want 0", n)
	}

	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != wantRGKey {
		t.Fatalf("release_group match_key = %q, want %q", k, wantRGKey)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group' AND entity_id=?", rgID); n != 0 {
		t.Fatalf("marker rows after the clear = %d, want 0", n)
	}
	for _, tc := range []struct {
		id     int64
		year   int
		folder string
	}{{albumID1, 2001, "/lib/Alpha/One"}, {albumID2, 2002, "/lib/Alpha/One Deluxe"}} {
		want := identity.AlbumKey("", wantRGKey, tc.year, 0, tc.folder)
		if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", tc.id); k != want {
			t.Fatalf("album %d match_key = %q, want %q", tc.id, k, want)
		}
	}

	// A rescan of a member whose file lost the group id lands on the same chain.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1b", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release_group rows after rescan = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows after rescan = %d, want 2", n)
	}
	if id := scalarInt(t, st, `SELECT t.album_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.title='T1'`); id != int(albumID1) {
		t.Fatalf("rescan landed T1 on album %d, want %d", id, albumID1)
	}
	assertVerifyClean(t, st)
}

// rgArtFixture scans one grouped track under an mbid-keyed release group and returns the
// group's pid and row id, for the art-takeback cases.
func rgArtFixture(t *testing.T, rgMBID string) (*Store, model.PID, int64) {
	t.Helper()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbReleaseGroup: rgMBID, durationMS: 100,
	})
	return st, entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID),
		entityIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
}

// rgEnrichArt is an enrichment-attributed image with a distinct content address, the
// shape the release-group apply stores.
func rgEnrichArt(hash string) *model.ArtImage {
	return &model.ArtImage{
		Data: []byte("img-" + hash), Hash: hash, Format: "png", Width: 4, Height: 4,
		Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "mock"},
	}
}

// rgArtRoles counts a release group's art rows per role.
func rgArtRoles(t *testing.T, st *Store, rgID int64, role string) int {
	t.Helper()
	return scalarInt(t, st,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role=?", rgID, role)
}

// TestEntityEditClearRGMBIDTakesBackMatchedArt: a release group holds art of its own, so
// the clear gives back what the match wrote there, its front cover and the aux slots
// enrichment filled, and leaves a hand-set cover standing. An unmatched marker means
// nothing was written to take back, so everything stays.
func TestEntityEditClearRGMBIDTakesBackMatchedArt(t *testing.T) {
	ctx := context.Background()
	const rgMBID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	seedArt := func(t *testing.T, st *Store, rgPID model.PID, rgID int64) {
		t.Helper()
		if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleBack,
			[]byte("user-back"), "png", model.Attribution{Source: model.SourceUser},
			model.LockOf(false), false); err != nil {
			t.Fatalf("seed user back: %v", err)
		}
		if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
			ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: rgMBID, Type: "album",
			Art:    rgEnrichArt("rg-front"),
			AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleDisc: rgEnrichArt("rg-disc")},
		}); err != nil {
			t.Fatalf("enrich release group: %v", err)
		}
		for _, role := range []string{"front", "disc", "back"} {
			if n := rgArtRoles(t, st, rgID, role); n != 1 {
				t.Fatalf("%s rows before the clear = %d, want 1", role, n)
			}
		}
	}

	matched, rgPID, rgID := rgArtFixture(t, rgMBID)
	seedArt(t, matched, rgPID, rgID)

	clearEntityMBID(t, matched, model.MergeReleaseGroup, rgPID)

	for _, role := range []string{"front", "disc"} {
		if n := rgArtRoles(t, matched, rgID, role); n != 0 {
			t.Errorf("%s rows after the clear = %d, want 0 (the match wrote them)", role, n)
		}
	}
	if n := rgArtRoles(t, matched, rgID, "back"); n != 1 {
		t.Errorf("back rows after the clear = %d, want the hand-set cover kept", n)
	}
	assertVerifyClean(t, matched)

	// The same shape with the marker recording no match: nothing there came from the id
	// being disowned, so nothing is taken back.
	unmatched, rgPID2, rgID2 := rgArtFixture(t, rgMBID)
	seedArt(t, unmatched, rgPID2, rgID2)
	if err := unmatched.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID2, PID: rgPID2,
	}); err != nil {
		t.Fatalf("re-mark unmatched: %v", err)
	}

	clearEntityMBID(t, unmatched, model.MergeReleaseGroup, rgPID2)

	for _, role := range []string{"front", "disc", "back"} {
		if n := rgArtRoles(t, unmatched, rgID2, role); n != 1 {
			t.Errorf("%s rows after an unmatched clear = %d, want 1", role, n)
		}
	}
	assertVerifyClean(t, unmatched)
}

// TestEntityEditClearRGMBIDReparentsDifferentlyTitledAlbum: two differently-titled
// editions share one mbid-keyed group. A heuristic album key carries its group's key,
// which carries the album title, so the two cannot land under one heuristic group. The
// clear derives each dependent's chain from its own members, and the sibling gets a group
// of its own rather than a key spliced from the representative's title that nothing would
// ever recompute.
func TestEntityEditClearRGMBIDReparentsDifferentlyTitledAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	const rgMBID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbReleaseGroup: rgMBID,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One Deluxe/01.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One (Deluxe Edition)", genre: "Rock",
		year: 2002, mbReleaseGroup: rgMBID,
	})
	rgPID := entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	rgID := entityIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	plainKey0 := identity.AlbumKey("", "mbid:"+rgMBID, 2001, 0, "/lib/Alpha/One")
	deluxeKey0 := identity.AlbumKey("", "mbid:"+rgMBID, 2002, 0, "/lib/Alpha/One Deluxe")
	plainID, plainPID := entityIDByCol(t, st, "album", "match_key", plainKey0),
		entityPIDByCol(t, st, "album", "match_key", plainKey0)
	deluxeID, deluxePID := entityIDByCol(t, st, "album", "match_key", deluxeKey0),
		entityPIDByCol(t, st, "album", "match_key", deluxeKey0)
	seq0 := latestSeq(t, st)

	clearEntityMBID(t, st, model.MergeReleaseGroup, rgPID)

	plainRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	deluxeRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One (Deluxe Edition)")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != plainRGKey {
		t.Fatalf("release_group match_key = %q, want %q", k, plainRGKey)
	}
	deluxeParent := int64(scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", deluxeID))
	if deluxeParent == rgID {
		t.Fatalf("the deluxe album stayed under the re-keyed group, which its own title cannot key")
	}
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", deluxeParent); k != deluxeRGKey {
		t.Fatalf("the deluxe album's group match_key = %q, want %q", k, deluxeRGKey)
	}
	for _, tc := range []struct {
		id     int64
		rgKey  string
		year   int
		folder string
	}{{plainID, plainRGKey, 2001, "/lib/Alpha/One"}, {deluxeID, deluxeRGKey, 2002, "/lib/Alpha/One Deluxe"}} {
		want := identity.AlbumKey("", tc.rgKey, tc.year, 0, tc.folder)
		if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", tc.id); k != want {
			t.Errorf("album %d match_key = %q, want the %q its own members compute", tc.id, k, want)
		}
	}
	if p := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", plainID); model.PID(p) != plainPID {
		t.Errorf("album pid = %q, want the kept %q", p, plainPID)
	}
	if p := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", deluxeID); model.PID(p) != deluxePID {
		t.Errorf("deluxe album pid = %q, want the kept %q", p, deluxePID)
	}
	// An item read serves its release group's pid, so both members changed as far as a
	// delta consumer is concerned: T1's group was re-keyed under it, and T2's is a
	// different row entirely.
	for _, title := range []string{"T1", "T2"} {
		pid := scalarStr(t, st, "SELECT pid FROM playable_item WHERE title=?", title)
		if n := changeCountFor(t, st, seq0, "item", pid, model.OpUpdate); n < 1 {
			t.Errorf("item updates for %s = %d, want at least 1", title, n)
		}
	}

	// A re-put of each member with the group tag stripped resolves onto the same rows,
	// which is the whole point of deriving the keys a scan would compute.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1b", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One Deluxe/01.flac", essence: "e2", content: "c2b", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One (Deluxe Edition)", genre: "Rock", year: 2002,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Errorf("album rows after the rescan = %d, want 2", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Errorf("release_group rows after the rescan = %d, want 2", n)
	}
	for _, tc := range []struct {
		title string
		id    int64
	}{{"T1", plainID}, {"T2", deluxeID}} {
		if id := scalarInt(t, st, `SELECT t.album_id FROM track t
			JOIN playable_item pi ON pi.id=t.item_id WHERE pi.title=?`, tc.title); int64(id) != tc.id {
			t.Errorf("rescan landed %s on album %d, want %d", tc.title, id, tc.id)
		}
	}
	assertVerifyClean(t, st)
}

// TestEntityEditSetMBIDStaysColumnOnly pins the other direction: setting an entity mbid
// fills the column and leaves the key alone, so a member whose own file never carried the
// id is not forked off on its next scan.
func TestEntityEditSetMBIDStaysColumnOnly(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	albumPID := entityPIDByCol(t, st, "album", "title", "One")
	rgPID := entityPIDByCol(t, st, "release_group", "title", "One")
	albumKey := scalarStr(t, st, "SELECT match_key FROM album WHERE pid=?", string(albumPID))
	rgKey := scalarStr(t, st, "SELECT match_key FROM release_group WHERE pid=?", string(rgPID))

	attr := model.Attribution{Source: model.SourceUser}
	const relMBID = "55555555-5555-5555-5555-555555555555"
	const rgMBID = "66666666-6666-6666-6666-666666666666"
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"mbid": relMBID}, attr, model.LockOf(true), false); err != nil {
		t.Fatalf("set album mbid: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"mbid": rgMBID}, attr, model.LockOf(true), false); err != nil {
		t.Fatalf("set release-group mbid: %v", err)
	}

	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE pid=?", string(albumPID)); k != albumKey {
		t.Fatalf("album match_key = %q, want the unchanged %q", k, albumKey)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE pid=?", string(rgPID)); k != rgKey {
		t.Fatalf("release_group match_key = %q, want the unchanged %q", k, rgKey)
	}
	if m := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM album WHERE pid=?", string(albumPID)); m != relMBID {
		t.Fatalf("album mbid column = %q, want %q", m, relMBID)
	}
	if m := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM release_group WHERE pid=?", string(rgPID)); m != rgMBID {
		t.Fatalf("release_group mbid column = %q, want %q", m, rgMBID)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearMBIDSkipsUngroupableChain covers the not-grouped guard: when the id
// was the chain's only grouping evidence, there is no heuristic key to fall back to, so
// the column clears and the key stands rather than the row moving somewhere no scan looks.
func TestEntityEditClearMBIDSkipsUngroupableChain(t *testing.T) {
	st, lib := entityFixture(t)
	const rgMBID = "77777777-7777-7777-7777-777777777777"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/x/01.flac", essence: "e1", content: "c1", title: "T1",
		genre: "Rock", mbReleaseGroup: rgMBID, durationMS: 100,
	})
	rgPID := entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	rgID := entityIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	albumKey := scalarStr(t, st, "SELECT match_key FROM album")

	clearEntityMBID(t, st, model.MergeReleaseGroup, rgPID)

	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != "mbid:"+rgMBID {
		t.Fatalf("release_group match_key = %q, want the key kept", k)
	}
	if m := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM release_group WHERE id=?", rgID); m != "" {
		t.Fatalf("release_group mbid column = %q, want cleared", m)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != albumKey {
		t.Fatalf("album match_key = %q, want the unchanged %q", k, albumKey)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearMBIDSkipsArchivedRepresentative: the member a re-key derives from
// has to be one with a file, since its folder anchors the album key. An archived lowest
// member is not the entity's answer while a sibling still has one, so the clear derives
// from the sibling instead of skipping half-applied.
func TestEntityEditClearMBIDSkipsArchivedRepresentative(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path:    "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac",
			essence: "e" + title, content: "c" + title, title: title,
			artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
			year: 2001, mbRelease: relMBID,
		})
	}
	albumPID := entityPIDByCol(t, st, "album", "match_key", "mbid:"+relMBID)
	albumID := entityIDByCol(t, st, "album", "match_key", "mbid:"+relMBID)
	// Dropping the file row is what archiving leaves behind; these members carry no
	// duration, so the maintained rollups stay where the scan put them.
	lowest := scalarInt(t, st, "SELECT MIN(item_id) FROM track WHERE album_id=?", albumID)
	if _, err := st.write.ExecContext(ctx, "DELETE FROM item_file WHERE item_id=?", lowest); err != nil {
		t.Fatalf("archive the lowest member: %v", err)
	}

	clearEntityMBID(t, st, model.MergeAlbum, albumPID)

	wantKey := identity.AlbumKey("", identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One"),
		2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != wantKey {
		t.Fatalf("album match_key = %q, want the %q its filed member computes", k, wantKey)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearRGMBIDMergesIntoHeuristicTwin drives the taken-key branch at both
// levels at once, the shape a partly-tagged library produces: the group folds into the
// heuristic twin it re-keys onto, and its dependent album, whose swapped key the twin's
// own album already owns, folds in behind it.
func TestEntityEditClearRGMBIDMergesIntoHeuristicTwin(t *testing.T) {
	st, lib := entityFixture(t)
	const rgMBID = "88888888-8888-8888-8888-888888888888"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbReleaseGroup: rgMBID, durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Fatalf("release_group rows = %d, want 2", n)
	}
	twinRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	twinRGPID := entityPIDByCol(t, st, "release_group", "match_key", twinRGKey)
	twinAlbumPID := entityPIDByCol(t, st, "album", "match_key",
		identity.AlbumKey("", twinRGKey, 2001, 0, "/lib/Alpha/One"))
	identified := entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)

	clearEntityMBID(t, st, model.MergeReleaseGroup, identified)

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release_group rows after the clear = %d, want 1", n)
	}
	if p := scalarStr(t, st, "SELECT pid FROM release_group"); model.PID(p) != twinRGPID {
		t.Fatalf("surviving release_group pid = %q, want %q", p, twinRGPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the clear = %d, want 1", n)
	}
	if p := scalarStr(t, st, "SELECT pid FROM album"); model.PID(p) != twinAlbumPID {
		t.Fatalf("surviving album pid = %q, want %q", p, twinAlbumPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id IS NOT NULL"); n != 2 {
		t.Fatalf("members still on an album = %d, want 2", n)
	}
	assertVerifyClean(t, st)
}

// TestEntityEditClearMBIDWithSiblingFieldsRefusesOnlyOnMerge pins the narrow refusal: a
// clear that merges the entity away takes its sibling field writes with it, so the whole
// call is refused and rolled back, while the far more common clear that simply re-keys in
// place still carries them.
func TestEntityEditClearMBIDWithSiblingFieldsRefusesOnlyOnMerge(t *testing.T) {
	ctx := context.Background()
	attr := model.Attribution{Source: model.SourceUser}

	// Re-keying in place: no twin owns the derived key, so the row survives and the
	// sibling label lands on it.
	free, lib := entityFixture(t)
	putTrack(t, free, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbRelease: "99999999-9999-9999-9999-999999999999", durationMS: 100,
	})
	albumPID := entityPIDByCol(t, free, "album", "title", "One")
	if _, err := free.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"mbid": "", "label": "Indie Co"}, attr, model.LockOf(true), false); err != nil {
		t.Fatalf("clear plus label on a free key: %v", err)
	}
	wantKey := identity.AlbumKey("", identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One"),
		2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, free, "SELECT match_key FROM album WHERE pid=?", string(albumPID)); k != wantKey {
		t.Fatalf("album match_key = %q, want the heuristic %q", k, wantKey)
	}
	if l := scalarStr(t, free, "SELECT COALESCE(label,'') FROM album WHERE pid=?", string(albumPID)); l != "Indie Co" {
		t.Fatalf("sibling label = %q, want it applied", l)
	}
	assertVerifyClean(t, free)

	// Merging: the label would have been written to a row the merge deletes, and a merge
	// carries curation rows but no column values, so the call is refused whole.
	taken, lib2 := entityFixture(t)
	const relMBID = "12121212-1212-1212-1212-121212121212"
	putTrack(t, taken, lib2.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, mbRelease: relMBID, durationMS: 100,
	})
	putTrack(t, taken, lib2.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
		year: 2001, durationMS: 100,
	})
	identified := entityPIDByCol(t, taken, "album", "match_key", "mbid:"+relMBID)
	_, err := taken.EditEntityFields(ctx, model.MergeAlbum, identified,
		map[string]string{"mbid": "", "label": "Blue Note"}, attr, model.LockOf(true), false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("clear plus label on a taken key = %v, want CodeConflict", err)
	}
	// Nothing committed: the merge, the key rewrite, the cleared column, and the label
	// all roll back together.
	if n := scalarInt(t, taken, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows after the refusal = %d, want 2", n)
	}
	if k := scalarStr(t, taken, "SELECT match_key FROM album WHERE pid=?", string(identified)); k != "mbid:"+relMBID {
		t.Fatalf("album match_key after the refusal = %q, want it untouched", k)
	}
	if m := scalarStr(t, taken, "SELECT COALESCE(mbid,'') FROM album WHERE pid=?", string(identified)); m != relMBID {
		t.Fatalf("album mbid after the refusal = %q, want it untouched", m)
	}
	if n := scalarInt(t, taken, "SELECT COUNT(*) FROM entity_curation WHERE field='label'"); n != 0 {
		t.Fatalf("label curation rows after the refusal = %d, want 0", n)
	}
	// The clear on its own still goes through, which is what the refusal message asks for.
	clearEntityMBID(t, taken, model.MergeAlbum, identified)
	if n := scalarInt(t, taken, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the lone clear = %d, want 1", n)
	}
	assertVerifyClean(t, taken)
}
