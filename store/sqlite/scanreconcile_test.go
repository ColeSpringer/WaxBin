package sqlite

import (
	"context"
	"path/filepath"
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

// assertAlbumCarried checks that one album row survived a re-key with its id and pid,
// wearing the expected key and holding both members. The release group is read fresh
// rather than passed in: a retag re-keys the group too, and the carry folds the drained
// one into the group the new key names, so the seeded marker has to be found there.
func assertAlbumCarried(t *testing.T, st *Store, albumID int, albPID model.PID, wantKey string) {
	t.Helper()
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the re-keyed album carried)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id = %d, want kept %d", id, albumID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(albPID) {
		t.Fatalf("album pid = %s, want kept %s", pid, albPID)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want %q", k, wantKey)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 2 {
		t.Fatalf("members on the survivor = %d, want 2", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release group rows = %d, want 1 (the drained group folded in)", n)
	}
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	if id := scalarInt(t, st, "SELECT release_group_id FROM album"); id != rgID {
		t.Errorf("album release_group_id = %d, want the surviving group %d", id, rgID)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	assertVerifyClean(t, st)
}

// retagFolder is the folder every member of the moveFixture album sits in.
const retagFolder = "/lib/Alpha/One"

// TestScanTitleRetagCarriesAlbum: an external title retag of every member re-keys the
// release group as well, so the two album rows sit under different groups. The
// unchanged folder is what ties them together, and the old row carries onto the new
// key and follows it under the new group.
func TestScanTitleRetagCarriesAlbum(t *testing.T) {
	st, lib, albumID, albPID, _ := moveFixture(t)

	for _, s := range []trackSpec{
		{path: "/lib/Alpha/One/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: "/lib/Alpha/One/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "Two", "Rock", 2001
		putTrack(t, st, lib.ID, s)
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Two")
	assertAlbumCarried(t, st, albumID, albPID, identity.AlbumKey("", rgKey, 2001, 0, retagFolder))
	if title := scalarStr(t, st, "SELECT title FROM album"); title != "Two" {
		t.Errorf("album title = %q, want the retagged Two", title)
	}
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "Two" {
		t.Errorf("release group title = %q, want the retagged Two", title)
	}
}

// TestScanTitleRetagKeepsReleaseGroupPID: the group the retag lands on is a row this
// scan just created, holding nothing but the album that carried onto it. The
// established group is the one that survives the fold, keeping its pid and taking the
// new title, which is the album rung's bare-destination rule one level up.
func TestScanTitleRetagKeepsReleaseGroupPID(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", rgID)

	retagInto(t, st, lib, retagFolder)

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Two")
	assertAlbumCarried(t, st, albumID, albPID, identity.AlbumKey("", rgKey, 2001, 0, retagFolder))
	if id := scalarInt(t, st, "SELECT id FROM release_group"); id != rgID {
		t.Fatalf("release group id = %d, want the established %d", id, rgID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group"); pid != rgPID {
		t.Fatalf("release group pid = %s, want kept %s", pid, rgPID)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM release_group"); k != rgKey {
		t.Errorf("release group match_key = %q, want the retagged %q", k, rgKey)
	}
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "Two" {
		t.Errorf("release group title = %q, want the retagged Two", title)
	}
}

// TestScanAlbumArtistRetagIntoEstablishedAlbumKeepsGroupPID pins the fold path where
// the album and group rungs pick opposite survivors. The destination album wears art of
// its own, so the drained row merges into it, while the group above it is fresh and
// gives way: the established group keeps its pid and adopts the destination's key,
// title, and primary artist. Only the album_artist frames are retagged, so the two
// groups' primary artists differ. The old group's primary artist is a row no track
// frame references, the residue a pre-split combined credit or a departed last writer
// leaves behind, so nothing on the scan path re-marks it and the closing verify is what
// catches a rollup left stale by the primary-artist flip.
func TestScanAlbumArtistRetagIntoEstablishedAlbumKeepsGroupPID(t *testing.T) {
	st, lib, albumID, _, rgID := moveFixture(t)
	ctx := context.Background()
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", rgID)

	// The destination: same title, same folder, a different album artist, and art of
	// its own. Its group holds nothing but this album.
	putTrack(t, st, lib.ID, trackSpec{
		path: retagFolder + "/09.flac", essence: "m9", content: "k9",
		title: "M9", artist: "Beta", albumArt: "Beta", album: "One", genre: "Rock", year: 2001,
	})
	destID := scalarInt(t, st, "SELECT id FROM album WHERE id<>?", albumID)
	destPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE id=?", destID))
	destCover := testPNG(t, 32, 32)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, destPID, model.ArtRoleFront, destCover.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed destination art: %v", err)
	}
	// Re-point the old group's primary artist onto a row no frame references, then
	// repair the rollups so the retag starts from a clean slate.
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO artist(pid, name, sort_key, match_key) VALUES (?,?,?,?)",
		string(model.NewPID()), "Gamma", model.SortKey("Gamma"), identity.MatchKey("Gamma")); err != nil {
		t.Fatalf("seed canonical artist: %v", err)
	}
	gammaID := scalarInt(t, st, "SELECT id FROM artist WHERE name='Gamma'")
	if _, err := st.write.ExecContext(ctx,
		"UPDATE release_group SET primary_artist_id=? WHERE id=?", gammaID, rgID); err != nil {
		t.Fatalf("re-point primary artist: %v", err)
	}
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh rollups: %v", err)
	}
	assertVerifyClean(t, st)

	for _, s := range []trackSpec{
		{path: retagFolder + "/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: retagFolder + "/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Beta", "One", "Rock", 2001
		putTrack(t, st, lib.ID, s)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (the drained row folded in)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != destID {
		t.Fatalf("surviving album id = %d, want the incumbent %d", id, destID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", destID); n != 3 {
		t.Fatalf("members on the incumbent = %d, want 3", n)
	}
	if h := scalarStr(t, st,
		"SELECT source_hash FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", destID); h != destCover.Hash {
		t.Errorf("front art = %q, want the incumbent's own %q", h, destCover.Hash)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release group rows = %d, want 1 (the fresh group gave way)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM release_group"); id != rgID {
		t.Fatalf("release group id = %d, want the established %d", id, rgID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group"); pid != rgPID {
		t.Fatalf("release group pid = %s, want kept %s", pid, rgPID)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Beta"), "One")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group"); k != rgKey {
		t.Errorf("release group match_key = %q, want the destination's %q", k, rgKey)
	}
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "One" {
		t.Errorf("release group title = %q, want One", title)
	}
	betaID := scalarInt(t, st, "SELECT id FROM artist WHERE name='Beta'")
	if id := scalarInt(t, st, "SELECT COALESCE(primary_artist_id,0) FROM release_group"); id != betaID {
		t.Errorf("release group primary_artist_id = %d, want the destination's %d", id, betaID)
	}
	assertVerifyClean(t, st)
}

// TestScanArtistRetagCarriesAlbum: an artist retag re-keys the release group through
// its artist segment instead of its title, and carries on the same folder evidence.
func TestScanArtistRetagCarriesAlbum(t *testing.T) {
	st, lib, albumID, albPID, _ := moveFixture(t)

	for _, s := range []trackSpec{
		{path: "/lib/Alpha/One/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: "/lib/Alpha/One/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Beta", "Beta", "One", "Rock", 2001
		putTrack(t, st, lib.ID, s)
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Beta"), "One")
	assertAlbumCarried(t, st, albumID, albPID, identity.AlbumKey("", rgKey, 2001, 0, retagFolder))
}

// TestScanRetagWithMoveCarriesAlbum: a retag that also moves the release out of its
// folder shares neither release group nor folder with the old row, so the carry rests
// on the essence relink alone: the file came from the folder the old key names.
func TestScanRetagWithMoveCarriesAlbum(t *testing.T) {
	st, lib, albumID, albPID, _ := moveFixture(t)

	for _, s := range []trackSpec{
		{path: "/lib/Moved/Two/01.flac", essence: "m1", content: "k1", title: "M1"},
		{path: "/lib/Moved/Two/02.flac", essence: "m2", content: "k2", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "Two", "Rock", 2001
		res := putTrack(t, st, lib.ID, s)
		if !res.Relinked {
			t.Fatalf("put %s did not relink; the test needs the prior path", s.path)
		}
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Two")
	assertAlbumCarried(t, st, albumID, albPID, identity.AlbumKey("", rgKey, 2001, 0, "/lib/Moved/Two"))
}

// TestScanOrganizedMoveThenRetagCarriesAlbum: an organize move rewrites the file paths
// without re-keying anything, so the re-key waits for this retag scan, which finds the
// files by their new path and never relinks. The organize journal is the only evidence
// left, and its committed row carries the album.
func TestScanOrganizedMoveThenRetagCarriesAlbum(t *testing.T) {
	st, lib, albumID, albPID, _ := moveFixture(t)

	for _, n := range []string{"01", "02"} {
		organizeMove(t, st, "/lib/Alpha/One/"+n+".flac", "/lib/Organized/One/"+n+".flac")
	}
	for _, s := range []trackSpec{
		{path: "/lib/Organized/One/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: "/lib/Organized/One/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "Two", "Rock", 2001
		res := putTrack(t, st, lib.ID, s)
		if res.Relinked {
			t.Fatalf("put %s relinked; the organize move should have left a path match", s.path)
		}
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Two")
	assertAlbumCarried(t, st, albumID, albPID, identity.AlbumKey("", rgKey, 2001, 0, "/lib/Organized/One"))
}

// TestScanStaleOrganizeJournalStillSplits pins the refusal on the journal's source end.
// Nothing prunes the organize journal, so a file's newest committed row can describe a
// later hop than the one that took it out of the album's folder. Its destination still
// lines up with the new key, and that alone corroborates nothing.
func TestScanStaleOrganizeJournalStillSplits(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)

	for _, n := range []string{"01", "02"} {
		organizeMove(t, st, "/lib/Alpha/One/"+n+".flac", "/lib/Staging/One/"+n+".flac")
		organizeMove(t, st, "/lib/Staging/One/"+n+".flac", "/lib/Organized/One/"+n+".flac")
	}
	retagInto(t, st, lib, "/lib/Organized/One")
	assertAlbumGhosted(t, st, albumID, albPID, rgID)
}

// TestScanRelinkFromAnotherFolderStillSplits pins the other two ends: an organize hop
// the scan never caught up with, then a move made by hand. The relink says the file came
// from the staging folder rather than the one the old key names, and the journal's newest
// row ends there rather than where the file is now, so neither corroborates.
func TestScanRelinkFromAnotherFolderStillSplits(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)

	for _, n := range []string{"01", "02"} {
		organizeMove(t, st, "/lib/Alpha/One/"+n+".flac", "/lib/Staging/One/"+n+".flac")
	}
	retagInto(t, st, lib, "/lib/Organized/One")
	assertAlbumGhosted(t, st, albumID, albPID, rgID)
}

// retagInto rescans both members of the moveFixture album under folder, retitled, which
// re-keys them onto a fresh album under a fresh release group.
func retagInto(t *testing.T, st *Store, lib *model.Library, folder string) {
	t.Helper()
	for _, s := range []trackSpec{
		{path: folder + "/01.flac", essence: "m1", content: "k1b", title: "M1"},
		{path: folder + "/02.flac", essence: "m2", content: "k2b", title: "M2"},
	} {
		s.artist, s.albumArt, s.album, s.genre, s.year = "Alpha", "Alpha", "Two", "Rock", 2001
		putTrack(t, st, lib.ID, s)
	}
}

// assertAlbumGhosted checks that the drained row was left alone: still there, still
// keyed on its own folder, and still holding everything that hangs off it.
func assertAlbumGhosted(t *testing.T, st *Store, albumID int, albPID model.PID, rgID int) {
	t.Helper()
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (nothing corroborated the re-key)", n)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", albumID); pid != string(albPID) {
		t.Fatalf("old album pid = %s, want kept %s", pid, albPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 0 {
		t.Errorf("members left on the old album = %d, want 0", n)
	}
	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	oldKey := identity.AlbumKey("", rgKey, 2001, 0, retagFolder)
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != oldKey {
		t.Errorf("old album match_key = %q, want the unchanged %q", k, oldKey)
	}
	assertAlbumAttachments(t, st, albumID, rgID)
	assertVerifyClean(t, st)
}

// sameFolderAlbum scans one more track into the moveFixture album's folder, tagged with
// the title retagInto uses. The row a later retag drains onto therefore already exists,
// under a release group of its own, and can be given an identity first.
func sameFolderAlbum(t *testing.T, st *Store, lib *model.Library) (albumID, rgID int) {
	t.Helper()
	putTrack(t, st, lib.ID, trackSpec{
		path: retagFolder + "/09.flac", essence: "m9", content: "k9",
		title: "M9", artist: "Alpha", albumArt: "Alpha", album: "Two", genre: "Rock", year: 2001,
	})
	return scalarInt(t, st, "SELECT id FROM album WHERE title='Two'"),
		scalarInt(t, st, "SELECT id FROM release_group WHERE title='Two'")
}

// setEntityMBIDColumn writes an mbid straight onto a row, the way enrichment does. The
// id lands without a re-key, so the row keeps the heuristic key a scan computes for it.
func setEntityMBIDColumn(t *testing.T, st *Store, table string, id int, mbid string) {
	t.Helper()
	if _, err := st.write.ExecContext(context.Background(),
		"UPDATE "+table+" SET mbid=? WHERE id=?", mbid, id); err != nil {
		t.Fatalf("set %s mbid: %v", table, err)
	}
}

// TestScanRetagRefusesConflictingAlbumMBIDs: both rows carry a MusicBrainz release id
// while their keys stay heuristic, which is what enrichment leaves behind. Two ids are
// two releases, so the shared folder is a coincidence and the carry is refused.
func TestScanRetagRefusesConflictingAlbumMBIDs(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)
	destID, _ := sameFolderAlbum(t, st, lib)
	destPID := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", destID)
	destKey := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", destID)
	setEntityMBIDColumn(t, st, "album", albumID, "11111111-1111-1111-1111-111111111111")
	setEntityMBIDColumn(t, st, "album", destID, "22222222-2222-2222-2222-222222222222")

	retagInto(t, st, lib, retagFolder)

	assertAlbumGhosted(t, st, albumID, albPID, rgID)
	if pid := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", destID); pid != destPID {
		t.Errorf("destination album pid = %s, want the untouched %s", pid, destPID)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", destID); k != destKey {
		t.Errorf("destination album match_key = %q, want the untouched %q", k, destKey)
	}
	if m := scalarStr(t, st, "SELECT mbid FROM album WHERE id=?", albumID); m != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("drained album mbid = %q, want its own kept", m)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", destID); n != 3 {
		t.Errorf("members on the destination = %d, want 3", n)
	}
}

// TestScanRetagRefusesConflictingReleaseGroupMBIDs: the album rows are heuristic and
// unidentified, but the groups above them are not. An album stays heuristic under an
// mbid-keyed group, so folder evidence can reach two identified groups, and folding
// them on it would merge two different MusicBrainz releases.
func TestScanRetagRefusesConflictingReleaseGroupMBIDs(t *testing.T) {
	st, lib, albumID, albPID, rgID := moveFixture(t)
	destID, destRGID := sameFolderAlbum(t, st, lib)
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", rgID)
	destRGPID := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", destRGID)
	setEntityMBIDColumn(t, st, "release_group", rgID, "33333333-3333-3333-3333-333333333333")
	setEntityMBIDColumn(t, st, "release_group", destRGID, "44444444-4444-4444-4444-444444444444")

	retagInto(t, st, lib, retagFolder)

	assertAlbumGhosted(t, st, albumID, albPID, rgID)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Fatalf("release group rows = %d, want 2 (neither group folded)", n)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", rgID); pid != rgPID {
		t.Errorf("drained group pid = %s, want kept %s", pid, rgPID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", destRGID); pid != destRGPID {
		t.Errorf("destination group pid = %s, want kept %s", pid, destRGPID)
	}
	if id := scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", albumID); id != rgID {
		t.Errorf("drained album release_group_id = %d, want its own %d", id, rgID)
	}
	if id := scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", destID); id != destRGID {
		t.Errorf("destination album release_group_id = %d, want its own %d", id, destRGID)
	}
}

// organizeMove relocates a file the way an organize job does: a planned journal row,
// then a commit that rewrites the path columns and marks the row committed. No scan
// runs, so the album keeps the key its old folder gave it.
func organizeMove(t *testing.T, st *Store, src, dst string) {
	t.Helper()
	ctx := context.Background()
	f, err := st.FileByPath(ctx, []byte(src))
	if err != nil {
		t.Fatalf("file %s: %v", src, err)
	}
	in := model.RelocateInput{
		FilePID: f.PID, JobPID: model.NewPID(), SrcPath: []byte(src),
		NewPath: []byte(dst), NewDisplayPath: dst, NewRelPath: []byte(filepath.Base(dst)),
	}
	jpid, err := st.PlanMove(ctx, in)
	if err != nil {
		t.Fatalf("plan move %s: %v", src, err)
	}
	if err := st.CommitMove(ctx, jpid, in); err != nil {
		t.Fatalf("commit move %s: %v", src, err)
	}
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
