package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// moveFixture scans a two-track heuristic album "One" by Alpha under /lib/Alpha/One
// and seeds the attachments a re-key must carry: a locked curation row, a front
// cover, a star, and a matched release-group marker.
func moveFixture(t *testing.T) (*Store, *model.Library, int, model.PID, int) {
	t.Helper()
	ctx := context.Background()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "m1", content: "k1",
		title: "M1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "m2", content: "k2",
		title: "M2", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	albumID := scalarInt(t, st, "SELECT id FROM album")
	albPID := albumPID(t, st)
	rgID := scalarInt(t, st, "SELECT id FROM release_group")

	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albPID, map[string]string{"barcode": "1234567890128"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albPID, model.ArtRoleFront, testPNG(t, 64, 64).Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed art: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeAlbum, albPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES ('release_group', ?, 'musicbrainz', 1, NULL, 1)`, rgID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	return st, lib, albumID, albPID, rgID
}

// assertAlbumAttachments checks that the four seeded attachments still hang off one
// album row.
func assertAlbumAttachments(t *testing.T, st *Store, albumID, rgID int) {
	t.Helper()
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND entity_id=? AND field='barcode' AND locked=1", albumID); n != 1 {
		t.Errorf("curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", albumID); n != 1 {
		t.Errorf("art rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='album' AND entity_id=? AND starred_at IS NOT NULL", albumID); n != 1 {
		t.Errorf("star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group' AND entity_id=? AND matched=1", rgID); n != 1 {
		t.Errorf("matched marker rows = %d, want 1", n)
	}
}

// TestScanMoveCarriesAlbumIdentity: rescanning a whole album out of one folder and
// into another re-keys it, and the reconciliation moves the surviving row onto the new
// key instead of letting the old one ghost with its pid and attachments.
func TestScanMoveCarriesAlbumIdentity(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)
	ctx := context.Background()

	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Moved/One/01.flac", essence: "m1", content: "k1",
		title: "M1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Moved/One/02.flac", essence: "m2", content: "k2",
		title: "M2", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the moved album carried)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id = %d, want kept %d", id, albumID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(albPID) {
		t.Fatalf("album pid = %s, want kept %s", pid, albPID)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", rgKey, 2001, 0, "/lib/Moved/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want %q", k, wantKey)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 2 {
		t.Fatalf("members on the survivor = %d, want 2", n)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	if n := changeCount(t, st, seq0, "album", model.OpUpdate); n < 1 {
		t.Errorf("album updates = %d, want at least 1 (the survivor moved)", n)
	}
	assertVerifyClean(t, st)
}

// TestScanMoveIntoEstablishedAlbumMergesAttachments: moving a release into a folder
// that already holds a curated edition of the same release group folds the drained row
// into the incumbent, which keeps its own pid and art and takes the locks the old row
// held.
func TestScanMoveIntoEstablishedAlbumMergesAttachments(t *testing.T) {
	st, lib, oldID, oldPID, _ := moveFixture(t)
	ctx := context.Background()

	// A second edition of the same release under its own folder, curated in its own
	// right: it shares the release group but keys on "two".
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/09.flac", essence: "m9", content: "k9",
		title: "M9", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	destID := scalarInt(t, st, "SELECT id FROM album WHERE id<>?", oldID)
	destPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE id=?", destID))
	destCover := testPNG(t, 32, 32)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, destPID, model.ArtRoleFront, destCover.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed destination art: %v", err)
	}

	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/01.flac", essence: "m1", content: "k1",
		title: "M1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/02.flac", essence: "m2", content: "k2",
		title: "M2", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the drained row folded in)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != destID {
		t.Fatalf("surviving album id = %d, want the incumbent %d", id, destID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", destID); n != 3 {
		t.Fatalf("members on the incumbent = %d, want 3", n)
	}
	// Survivor-wins on the image, locked-wins on the curation the old row held.
	if h := scalarStr(t, st,
		"SELECT source_hash FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", destID); h != destCover.Hash {
		t.Errorf("front art = %q, want the incumbent's own %q", h, destCover.Hash)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND entity_id=? AND field='barcode' AND locked=1", destID); n != 1 {
		t.Errorf("carried curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='album' AND entity_id=? AND starred_at IS NOT NULL", destID); n != 1 {
		t.Errorf("carried star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='album' AND op=? AND entity_pid=?",
		seq0, string(model.OpDelete), string(oldPID)); n != 1 {
		t.Errorf("drained album delete deltas = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='album' AND op=? AND entity_pid=?",
		seq0, string(model.OpUpdate), string(destPID)); n < 1 {
		t.Errorf("incumbent update deltas = %d, want at least 1", n)
	}
	assertVerifyClean(t, st)
}

// TestScanMoveOntoBareButPopulatedAlbum pins what the bare branch actually keys on. The
// destination here is not a row this scan created: it is a pre-existing album under the
// same release group that simply carries no attachments. It reads bare, so it is the one
// merged away, and the drained curated row keeps its pid and takes over its folder key.
func TestScanMoveOntoBareButPopulatedAlbum(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)
	ctx := context.Background()

	// A second edition of the same release under its own folder, with nothing attached
	// to it: no art, no curation, no star.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/09.flac", essence: "m9", content: "k9",
		title: "M9", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	bareID := scalarInt(t, st, "SELECT id FROM album WHERE id<>?", albumID)
	barePID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE id=?", bareID))

	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/01.flac", essence: "m1", content: "k1",
		title: "M1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/02.flac", essence: "m2", content: "k2",
		title: "M2", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the bare row was merged away)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("surviving album id = %d, want the curated %d", id, albumID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(albPID) {
		t.Fatalf("album pid = %s, want kept %s", pid, albPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 3 {
		t.Fatalf("members on the survivor = %d, want 3", n)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", rgKey, 2001, 0, "/lib/Alpha/Two")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want the destination folder's %q", k, wantKey)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='album' AND op=? AND entity_pid=?",
		seq0, string(model.OpDelete), string(barePID)); n != 1 {
		t.Errorf("bare album delete deltas = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestScanYearRetagCarriesAlbum: a year is part of the album key but not the release
// group's, so an external year retag of every member re-keys the album in place and the
// row adopts the new year.
func TestScanYearRetagCarriesAlbum(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)

	for _, s := range []trackSpec{
		{path: "/lib/Alpha/One/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: "/lib/Alpha/One/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "One", "Rock", 2002
		putTrack(t, st, lib.ID, s)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the retagged album carried)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id = %d, want kept %d", id, albumID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(albPID) {
		t.Fatalf("album pid = %s, want kept %s", pid, albPID)
	}
	if y := scalarInt(t, st, "SELECT year FROM album"); y != 2002 {
		t.Errorf("album year = %d, want the retagged 2002", y)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", rgKey, 2002, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want %q", k, wantKey)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	assertVerifyClean(t, st)
}

// TestScanTitleRetagStillSplits pins the residue: a title retag re-keys the release
// group as well, so nothing ties the old and new albums together and the old row still
// ghosts with its attachments.
func TestScanTitleRetagStillSplits(t *testing.T) {
	st, lib, albumID, _, rgID := moveFixture(t)

	for _, s := range []trackSpec{
		{path: "/lib/Alpha/One/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: "/lib/Alpha/One/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "Two", "Rock", 2001
		putTrack(t, st, lib.ID, s)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (a retitled release is a different release group)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 0 {
		t.Errorf("members left on the old album = %d, want 0", n)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	assertVerifyClean(t, st)
}

// TestScanPartialMoveDoesNotReconcile: the carry waits for the file that drains the old
// row, so moving one member of two leaves both albums standing.
func TestScanPartialMoveDoesNotReconcile(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Moved/One/01.flac", essence: "m1", content: "k1",
		title: "M1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (a partial move splits)", n)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", albumID); pid != string(albPID) {
		t.Fatalf("old album pid = %s, want kept %s", pid, albPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 1 {
		t.Errorf("members left on the old album = %d, want 1", n)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	oldKey := identity.AlbumKey("", rgKey, 2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != oldKey {
		t.Errorf("old album match_key = %q, want the unchanged %q", k, oldKey)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	assertVerifyClean(t, st)
}
