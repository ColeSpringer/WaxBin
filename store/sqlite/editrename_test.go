package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// renameFixture scans a two-track heuristic album "One" by Alpha and returns the
// member pids in track order.
func renameFixture(t *testing.T) (*Store, *model.Library, []model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "r1", content: "c1",
		title: "R1", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "r2", content: "c2",
		title: "R2", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
	})
	pids := []model.PID{
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='R1'")),
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='R2'")),
	}
	return st, lib, pids
}

func assertVerifyClean(t *testing.T, st *Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// memberAlbumID reads the album row id an item currently belongs to.
func memberAlbumID(t *testing.T, st *Store, pid model.PID) int {
	t.Helper()
	return scalarInt(t, st, `SELECT COALESCE(t.album_id, 0) FROM track t
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid=?`, string(pid))
}

// changeCount counts change_log rows for one entity type and op past a sequence.
func changeCount(t *testing.T, st *Store, seq int64, entityType string, op model.ChangeOp) int {
	t.Helper()
	return scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type=? AND op=?",
		seq, entityType, string(op))
}

// TestEditAlbumRenamesInPlace: a whole-set album rename rewrites the album and its
// release group in place, keeping ids, pids, curation, art, stars, and the matched
// enrichment marker, and emits entity updates rather than a create/delete pair.
func TestEditAlbumRenamesInPlace(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")
	albPID := albumPID(t, st)
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	rgPID := model.PID(scalarStr(t, st, "SELECT pid FROM release_group"))

	// The attachments that must survive: a locked curation row, a front cover, a
	// star, and a matched RG enrichment marker (matched explicitly, since an
	// unmatched one is cleared by design on a rename).
	if err := st.EditEntityFields(ctx, model.MergeAlbum, albPID, map[string]string{"barcode": "1234567890128"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	cover := testPNG(t, 64, 64)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albPID, model.ArtRoleFront, cover.Data, "",
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

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (renamed in place)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id = %d, want kept %d", id, albumID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(albPID) {
		t.Fatalf("album pid = %s, want kept %s", pid, albPID)
	}
	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Renamed")
	wantAlbumKey := identity.AlbumKey("", wantRGKey, 2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantAlbumKey {
		t.Fatalf("album match_key = %q, want %q", k, wantAlbumKey)
	}
	var title, sortKey string
	if err := st.read.QueryRowContext(ctx, "SELECT title, sort_key FROM album").Scan(&title, &sortKey); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if title != "Renamed" || sortKey != model.SortKey("Renamed") {
		t.Fatalf("album title/sort = %q/%q, want Renamed with matching sort", title, sortKey)
	}
	if id := scalarInt(t, st, "SELECT id FROM release_group"); id != rgID {
		t.Fatalf("rg id changed")
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group"); pid != string(rgPID) {
		t.Fatalf("rg pid changed")
	}
	var rgTitle, rgKey string
	if err := st.read.QueryRowContext(ctx, "SELECT title, match_key FROM release_group").Scan(&rgTitle, &rgKey); err != nil {
		t.Fatalf("read rg: %v", err)
	}
	if rgTitle != "Renamed" || rgKey != wantRGKey {
		t.Fatalf("rg title/key = %q/%q, want Renamed/%q", rgTitle, rgKey, wantRGKey)
	}

	// All four attachments survive.
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
		t.Errorf("matched marker rows = %d, want 1 (a matched marker survives a rename)", n)
	}

	// Entity updates, no create/delete pair.
	if n := changeCount(t, st, seq0, "album", model.OpUpdate); n != 1 {
		t.Errorf("album updates = %d, want 1", n)
	}
	if n := changeCount(t, st, seq0, "release_group", model.OpUpdate); n != 1 {
		t.Errorf("rg updates = %d, want 1", n)
	}
	for _, et := range []string{"album", "release_group", "artist"} {
		for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
			if n := changeCount(t, st, seq0, et, op); n != 0 {
				t.Errorf("%s %s deltas = %d, want 0", et, op, n)
			}
		}
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumRenameRequeuesUnmatchedRG: a rename is new search evidence, so a
// whole-set title rename deletes the RG's unmatched enrichment marker and the entity
// re-enters the enrichment queue.
func TestEditAlbumRenameRequeuesUnmatchedRG(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	if _, err := st.write.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES ('release_group', ?, 'musicbrainz', 0, NULL, 1)`, rgID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if id := scalarInt(t, st, "SELECT id FROM release_group"); id != rgID {
		t.Fatalf("rg id changed")
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group' AND entity_id=?", rgID); n != 0 {
		t.Errorf("unmatched marker rows = %d, want 0 (rename re-queues the entity)", n)
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumPartialMemberSplits pins today's contract: an edit that moves only
// some of an album's members forks those members off and the old entity stays.
func TestEditAlbumPartialMemberSplits(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")

	if err := st.EditItemField(ctx, pids[0], "album", "Renamed",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (split)", n)
	}
	if id := memberAlbumID(t, st, pids[1]); id != albumID {
		t.Errorf("unedited member album = %d, want kept %d", id, albumID)
	}
	if id := memberAlbumID(t, st, pids[0]); id == albumID {
		t.Errorf("edited member still on the old album")
	}
	assertVerifyClean(t, st)
}

// TestEditYearRekeysAlbumOnly: the year is part of the album key but not the RG key,
// so a whole-set year edit re-keys the album row in place and leaves the RG alone.
func TestEditYearRekeysAlbumOnly(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")
	rgKey0 := scalarStr(t, st, "SELECT match_key FROM release_group")

	if _, err := st.EditManyFields(ctx, pids, map[string]string{"year": "1999"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit year: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id = %d, want kept %d", id, albumID)
	}
	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", wantRGKey, 1999, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want %q", k, wantKey)
	}
	if y := scalarInt(t, st, "SELECT year FROM album"); y != 1999 {
		t.Fatalf("album year = %d, want 1999", y)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM release_group"); k != rgKey0 {
		t.Fatalf("rg match_key moved on a year edit: %q", k)
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumCaseOnlyRefreshesDisplay: a case-only rename folds to the same match
// key, so the row is untouched except for the display title and sort refresh.
func TestEditAlbumCaseOnlyRefreshesDisplay(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")
	key0 := scalarStr(t, st, "SELECT match_key FROM album")

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "ONE"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id changed")
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != key0 {
		t.Fatalf("match_key = %q, want unchanged %q", k, key0)
	}
	if title := scalarStr(t, st, "SELECT title FROM album"); title != "ONE" {
		t.Fatalf("title = %q, want ONE", title)
	}
	// The release group folds to the same key too, and its title is the facet
	// display, so the whole-set respelling refreshes it as well.
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "ONE" {
		t.Fatalf("rg title = %q, want ONE", title)
	}
	if n := changeCount(t, st, seq0, "album", model.OpUpdate); n != 1 {
		t.Errorf("album updates = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumCollisionMergesIntoIncumbent: renaming a whole release onto a key
// another row already owns auto-merges the old entity into the incumbent, which keeps
// its pid and attachments; the loser emits an OpDelete.
func TestEditAlbumCollisionMergesIntoIncumbent(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One folder for all three tracks, so the album keys differ only through the rg
	// (title) segment and the rename lands exactly on the incumbent's key.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/shared/a1.flac", essence: "a1", content: "a1",
		title: "A1", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/shared/a2.flac", essence: "a2", content: "a2",
		title: "A2", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/shared/b1.flac", essence: "b1", content: "b1",
		title: "B1", artist: "Alpha", albumArt: "Alpha", album: "Two", year: 2001,
	})
	pidA1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='A1'"))
	pidA2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='A2'"))
	bID := scalarInt(t, st, "SELECT id FROM album WHERE title='Two'")
	bPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='Two'"))
	aPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='One'"))
	if err := st.EditEntityFields(ctx, model.MergeAlbum, bPID, map[string]string{"barcode": "1234567890128"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed incumbent curation: %v", err)
	}
	cover := testPNG(t, 64, 64)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, bPID, model.ArtRoleFront, cover.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed incumbent art: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.EditManyFields(ctx, []model.PID{pidA1, pidA2}, map[string]string{"album": "Two"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("rename onto incumbent: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (merged into the incumbent)", n)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM album"); pid != string(bPID) {
		t.Fatalf("surviving album pid = %s, want the incumbent %s", pid, bPID)
	}
	for _, pid := range []model.PID{pidA1, pidA2} {
		if id := memberAlbumID(t, st, pid); id != bID {
			t.Errorf("member %s album = %d, want the incumbent %d", pid, id, bID)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1 (the loser's sole-backed rg merged away)", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND entity_id=?", bID); n != 1 {
		t.Errorf("incumbent curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", bID); n != 1 {
		t.Errorf("incumbent art rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='album' AND entity_pid=? AND op=?",
		seq0, string(aPID), string(model.OpDelete)); n != 1 {
		t.Errorf("loser OpDelete deltas = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditItemsFieldsNonUniformAlbumSplits: per-item maps with two different target
// titles fail the uniformity check and fall back to today's split.
func TestEditItemsFieldsNonUniformAlbumSplits(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")

	if _, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: pids[0], Fields: map[string]string{"album": "Beta"}},
		{ItemPID: pids[1], Fields: map[string]string{"album": "Gamma"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 3 {
		t.Fatalf("album rows = %d, want 3 (old ghost plus two new)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 0 {
		t.Errorf("old album still has %d members, want 0", n)
	}
	if a, b := memberAlbumID(t, st, pids[0]), memberAlbumID(t, st, pids[1]); a == b {
		t.Errorf("members share album %d, want distinct", a)
	}
	assertVerifyClean(t, st)
}

// TestEditSkipLockedMemberBreaksAllMembers: a skipped (locked) member is not a
// participant, so the all-members condition fails and the edited members split off
// while the locked one keeps the old entity.
func TestEditSkipLockedMemberBreaksAllMembers(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")
	key0 := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID)

	if err := st.LockField(ctx, pids[1], "album"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	res, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, true)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != pids[1] {
		t.Fatalf("skipped = %v, want [%s]", res.Skipped, pids[1])
	}
	if id := memberAlbumID(t, st, pids[1]); id != albumID {
		t.Errorf("locked member album = %d, want kept %d", id, albumID)
	}
	if id := memberAlbumID(t, st, pids[0]); id == albumID {
		t.Errorf("edited member still on the old album (should have split)")
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != key0 {
		t.Errorf("old album match_key = %q, want unchanged %q", k, key0)
	}
	assertVerifyClean(t, st)
}

// TestEditRenameAlongsideGenre: a rename combined with a genre edit renames in place
// while the genre links and rollups flow through the unchanged per-item path.
func TestEditRenameAlongsideGenre(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")

	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed", "genre": "Jazz"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id changed")
	}
	if title := scalarStr(t, st, "SELECT title FROM album"); title != "Renamed" {
		t.Fatalf("album title = %q, want Renamed", title)
	}
	for _, pid := range pids {
		if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_genre ig
			JOIN genre g ON g.id = ig.genre_id
			JOIN playable_item pi ON pi.id = ig.item_id
			WHERE pi.pid=? AND g.name='Jazz'`, string(pid)); n != 1 {
			t.Errorf("member %s Jazz links = %d, want 1", pid, n)
		}
	}
	assertVerifyClean(t, st)
}

// TestEditVirtualCueRipRenames: the pre-pass sees CUE-carved virtual tracks like any
// other members. Editing the album on all of them renames in place; editing one
// splits as today.
func TestEditVirtualCueRipRenames(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	windows := [][2]int64{{0, 300}, {300, 600}}
	tracks := make([]model.VirtualTrack, len(windows))
	for i, w := range windows {
		n := i + 1
		title := "VT" + string(rune('0'+n))
		tracks[i] = model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent,
				Title: title, SortKey: model.SortKey(title),
				IdentityKey: identity.VirtualTrackKey("ve", n, w[0]),
			},
			Track:       model.Track{Artist: "Rip Artist", AlbumArtist: "Rip Artist", Album: "Rip Album", TrackNo: n},
			StartFrames: w[0], EndFrames: w[1],
		}
	}
	if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/rip.flac"), DisplayPath: "/lib/rip.flac", RelPath: []byte("rip.flac"),
			Kind: model.FileAudio, Size: 4, MTimeNS: 1,
			ContentHash: "vc", EssenceHash: "ve", DurationMS: 8000, ScanState: model.ScanIndexed,
		},
		Tracks: tracks,
	}); err != nil {
		t.Fatalf("put vtracks: %v", err)
	}
	pids := []model.PID{
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='VT1'")),
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='VT2'")),
	}
	albumID := scalarInt(t, st, "SELECT id FROM album")

	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Rip Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("rename all: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (renamed in place)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id changed")
	}
	if title := scalarStr(t, st, "SELECT title FROM album"); title != "Rip Renamed" {
		t.Fatalf("album title = %q, want Rip Renamed", title)
	}

	if err := st.EditItemField(ctx, pids[0], "album", "Solo",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit one: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (single-member edit splits)", n)
	}
	if id := memberAlbumID(t, st, pids[1]); id != albumID {
		t.Errorf("unedited vtrack album = %d, want kept %d", id, albumID)
	}
	assertVerifyClean(t, st)
}

// TestEditMBIDAlbumDisplayRefresh: on an mbid-keyed album a whole-set title edit
// cannot move the key, so it refreshes the display columns on the kept row.
func TestEditMBIDAlbumDisplayRefresh(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "44444444-4444-4444-4444-444444444444"
	const relMBID = "55555555-5555-5555-5555-555555555555"
	for i, title := range []string{"M1", "M2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/1" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One",
			year: 2001, mbReleaseGroup: rgMBID, mbRelease: relMBID,
		})
	}
	pids := []model.PID{
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='M1'")),
		model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='M2'")),
	}
	albumID := scalarInt(t, st, "SELECT id FROM album")

	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Fresh Title"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id changed")
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != "mbid:"+relMBID {
		t.Fatalf("album match_key = %q, want the mbid key kept", k)
	}
	if title := scalarStr(t, st, "SELECT title FROM album"); title != "Fresh Title" {
		t.Fatalf("album title = %q, want Fresh Title", title)
	}
	// The mbid-keyed release group cannot move its key either, so the whole-set
	// rename refreshes its display title the same way.
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "Fresh Title" {
		t.Fatalf("rg title = %q, want Fresh Title", title)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistWholeSetRenamesInPlace: when every reference to an artist moves at
// once to one new primary name, the artist row is rewritten in place with the old
// spelling preserved as an alias, and its curation, star, and RG primary pointer all
// survive.
func TestEditArtistWholeSetRenamesInPlace(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	artistID := scalarInt(t, st, "SELECT id FROM artist")
	artPID := model.PID(scalarStr(t, st, "SELECT pid FROM artist"))
	rgID := scalarInt(t, st, "SELECT id FROM release_group")

	if err := st.EditEntityFields(ctx, model.MergeArtist, artPID,
		map[string]string{"mbid": "66666666-6666-6666-6666-666666666666"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, artPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES ('artist', ?, 'musicbrainz', 0, NULL, 1)`, artistID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemFields(ctx, pid, map[string]string{"artist": "Beta", "album_artist": "Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1 (renamed in place)", n)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", artistID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Beta" || matchKey != identity.MatchKey("Beta") || gotPID != string(artPID) {
		t.Fatalf("artist = %q/%q/%s, want Beta with kept pid %s", name, matchKey, gotPID, artPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Alpha' AND is_primary=0", artistID); n != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", artistID); n != 1 {
		t.Errorf("star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='artist' AND entity_id=? AND field='mbid' AND locked=1", artistID); n != 1 {
		t.Errorf("curation rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT primary_artist_id FROM release_group WHERE id=?", rgID); id != artistID {
		t.Errorf("rg primary = %d, want still %d", id, artistID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist' AND entity_id=?", artistID); n != 0 {
		t.Errorf("unmatched artist marker rows = %d, want 0 (rename re-queues)", n)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 1 {
		t.Errorf("artist updates = %d, want 1", n)
	}
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq0, "artist", op); n != 0 {
			t.Errorf("artist %s deltas = %d, want 0", op, n)
		}
	}
	if v, _ := st.ItemByPID(ctx, pid); v.Artist != "Beta" {
		t.Errorf("denormalized artist = %q, want Beta", v.Artist)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistEmptyAlbumArtistMovesWholeChain pins the anchor-fallback finding: with
// a blank album_artist the release group anchors on the credited artist, so editing
// artist alone renames the artist, the release group, and the album in place.
func TestEditArtistEmptyAlbumArtistMovesWholeChain(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/one/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	artistID := scalarInt(t, st, "SELECT id FROM artist")
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	albumID := scalarInt(t, st, "SELECT id FROM album")
	if _, err := st.write.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES ('artist', ?, 'musicbrainz', 1, NULL, 1)`, artistID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	if err := st.EditItemField(ctx, pid, "artist", "Beta",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1", n)
	}
	if name := scalarStr(t, st, "SELECT name FROM artist WHERE id=?", artistID); name != "Beta" {
		t.Fatalf("artist name = %q, want Beta", name)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1", n)
	}
	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Beta"), "One")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != wantRGKey {
		t.Fatalf("rg match_key = %q, want %q", k, wantRGKey)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Fatalf("album id changed")
	}
	// A matched marker records a real write and survives the rename.
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist' AND entity_id=? AND matched=1", artistID); n != 1 {
		t.Errorf("matched artist marker rows = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistCollisionMergesIntoIncumbent: a whole-set rename onto a name another
// artist row already owns folds the old artist into the incumbent (stars fold, loser
// OpDelete), instead of leaving a ghost.
func TestEditArtistCollisionMergesIntoIncumbent(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Beta", albumArt: "Beta", album: "Other", year: 2002,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	alphaPID := entityPID(t, st, "artist", "Alpha")
	betaID := scalarInt(t, st, "SELECT id FROM artist WHERE name='Beta'")
	betaPID := entityPID(t, st, "artist", "Beta")
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, alphaPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemFields(ctx, pid1, map[string]string{"artist": "Beta", "album_artist": "Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1 (merged into the incumbent)", n)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM artist"); pid != string(betaPID) {
		t.Fatalf("surviving artist pid = %s, want the incumbent %s", pid, betaPID)
	}
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid1)); id != betaID {
		t.Errorf("track artist = %d, want the incumbent %d", id, betaID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", betaID); n != 1 {
		t.Errorf("folded star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='artist' AND entity_pid=? AND op=?",
		seq0, string(alphaPID), string(model.OpDelete)); n != 1 {
		t.Errorf("loser OpDelete deltas = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistRetainedAsFeatureSplits: a rename whose new credit still names the
// old artist ("Alpha" to "Beta feat. Alpha") must not rename Alpha, whose curation
// belongs to the still-referenced featured credit; the primary forks off as today.
func TestEditArtistRetainedAsFeatureSplits(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/one/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	alphaPID := entityPID(t, st, "artist", "Alpha")

	if err := st.EditItemField(ctx, pid, "artist", "Beta feat. Alpha",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 2 {
		t.Fatalf("artist rows = %d, want 2 (Alpha kept, Beta created)", n)
	}
	if pid := entityPID(t, st, "artist", "Alpha"); pid != alphaPID {
		t.Fatalf("Alpha pid = %s, want kept %s", pid, alphaPID)
	}
	betaID := scalarInt(t, st, "SELECT id FROM artist WHERE name='Beta'")
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); id != betaID {
		t.Errorf("track primary artist = %d, want Beta %d", id, betaID)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN artist a ON a.id=ic.artist_id WHERE a.name='Alpha' AND ic.role='artist'`); n != 1 {
		t.Errorf("Alpha featured credit rows = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistOutsideReferenceBlocks: a credit on an item outside the batch (here a
// curated producer credit) keeps the artist referenced, so the rename falls back to
// split and the old artist survives.
func TestEditArtistOutsideReferenceBlocks(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Other", albumArt: "Other", album: "Two", year: 2002,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S2'"))
	alphaPID := entityPID(t, st, "artist", "Alpha")
	if _, err := st.SetItemCredits(ctx, pid2, model.RoleProducer, []string{"Alpha"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed producer credit: %v", err)
	}

	if err := st.EditItemFields(ctx, pid1, map[string]string{"artist": "Beta", "album_artist": "Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Alpha survives under its old name; Beta is a fresh row (split, not rename).
	if pid := entityPID(t, st, "artist", "Alpha"); pid != alphaPID {
		t.Fatalf("Alpha pid = %s, want kept %s", pid, alphaPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta'"); n != 1 {
		t.Fatalf("Beta rows = %d, want 1", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN artist a ON a.id=ic.artist_id
		JOIN playable_item pi ON pi.id=ic.item_id
		WHERE a.name='Alpha' AND ic.role='producer' AND pi.pid=?`, string(pid2)); n != 1 {
		t.Errorf("producer credit rows = %d, want the credit still on Alpha", n)
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumRenameUnderMultiBackedRG: when the old release group backs another
// album, it keeps that album (no ghost) and the renamed album moves under a
// found-or-created new release group.
func TestEditAlbumRenameUnderMultiBackedRG(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two editions of "One" under one release group (same rg key, different album
	// keys through year and folder).
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2002,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1", n)
	}
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	album1 := memberAlbumID(t, st, pid1)

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemField(ctx, pid1, "album", "Two",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Fatalf("rg rows = %d, want 2 (new group created, old one kept)", n)
	}
	// The album said so twice: once for the group repoint (its membership is
	// readable state a delta consumer must refetch on) and once for the key rewrite.
	if n := changeCount(t, st, seq0, "album", model.OpUpdate); n != 2 {
		t.Errorf("album updates = %d, want 2 (repoint plus key rewrite)", n)
	}
	if id := memberAlbumID(t, st, pid1); id != album1 {
		t.Fatalf("album id = %d, want kept %d (renamed in place)", id, album1)
	}
	newRG := scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", album1)
	if newRG == rgID {
		t.Fatalf("renamed album still under the old rg")
	}
	if title := scalarStr(t, st, "SELECT title FROM album WHERE id=?", album1); title != "Two" {
		t.Fatalf("album title = %q, want Two", title)
	}
	// The old group keeps the other edition.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE release_group_id=?", rgID); n != 1 {
		t.Errorf("old rg albums = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditAlbumRenameMovesWholeMultiAlbumRG: a two-disc set is two album rows (one
// per folder) under one release group, and a batch renaming every track of both
// discs moves the whole group in place. A per-album walk would have disc one fork
// the group off to a fresh pid and disc two then merge the original into it,
// destroying the pid the pre-pass exists to preserve.
func TestEditAlbumRenameMovesWholeMultiAlbumRG(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, d := range []struct{ folder, essence, title string }{
		{"d1", "m1", "D1T1"}, {"d1", "m2", "D1T2"}, {"d2", "m3", "D2T1"}, {"d2", "m4", "D2T2"},
	} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/" + d.folder + "/" + d.essence + ".flac", essence: d.essence, content: d.essence,
			title: d.title, artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
		})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2 (one per disc folder)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1", n)
	}
	rgID := scalarInt(t, st, "SELECT id FROM release_group")
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group")
	albumIDs := []int{
		scalarInt(t, st, "SELECT MIN(id) FROM album"),
		scalarInt(t, st, "SELECT MAX(id) FROM album"),
	}
	var pids []model.PID
	for _, title := range []string{"D1T1", "D1T2", "D2T1", "D2T2"} {
		pids = append(pids, model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title=?", title)))
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1 (renamed in place)", n)
	}
	if id := scalarInt(t, st, "SELECT id FROM release_group"); id != rgID {
		t.Fatalf("rg id = %d, want kept %d", id, rgID)
	}
	if pid := scalarStr(t, st, "SELECT pid FROM release_group"); pid != rgPID {
		t.Fatalf("rg pid changed")
	}
	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "Renamed")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group"); k != wantRGKey {
		t.Fatalf("rg match_key = %q, want %q", k, wantRGKey)
	}
	if title := scalarStr(t, st, "SELECT title FROM release_group"); title != "Renamed" {
		t.Fatalf("rg title = %q, want Renamed", title)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want the same 2", n)
	}
	for _, id := range albumIDs {
		if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE id=? AND title='Renamed'", id); n != 1 {
			t.Errorf("album %d not renamed in place", id)
		}
	}
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq0, "release_group", op); n != 0 {
			t.Errorf("rg %s deltas = %d, want 0", op, n)
		}
	}
	assertVerifyClean(t, st)
}

// TestEditRenameWithYearSplitKeepsChainInPlace: a batch that renames the whole set's
// artist while re-dating one member still renames the artist and the release group
// in place (the year is an album-key segment, not an RG one); only the album splits.
// Before the batch-level RG stage the album-key non-uniformity vetoed the whole
// chain after the artist had already committed, leaving a ghost group double-counted
// in the artist's rollup.
func TestEditRenameWithYearSplitKeepsChainInPlace(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	artistID := scalarInt(t, st, "SELECT id FROM artist")
	rgID := scalarInt(t, st, "SELECT id FROM release_group")

	if _, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: pids[0], Fields: map[string]string{"artist": "Beta", "album_artist": "Beta", "year": "1999"}},
		{ItemPID: pids[1], Fields: map[string]string{"artist": "Beta", "album_artist": "Beta"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1 (renamed in place)", n)
	}
	if name := scalarStr(t, st, "SELECT name FROM artist WHERE id=?", artistID); name != "Beta" {
		t.Fatalf("artist name = %q, want Beta", name)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1 (renamed in place, no fork)", n)
	}
	wantRGKey := identity.ReleaseGroupKey("", identity.MatchKey("Beta"), "One")
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != wantRGKey {
		t.Fatalf("rg match_key = %q, want %q", k, wantRGKey)
	}
	// The members split by year into two albums, both under the renamed group.
	if a, b := memberAlbumID(t, st, pids[0]), memberAlbumID(t, st, pids[1]); a == b {
		t.Errorf("members share album %d, want a year split", a)
	}
	if n := scalarInt(t, st, `SELECT COUNT(DISTINCT al.release_group_id) FROM track t
		JOIN album al ON al.id = t.album_id`); n != 1 {
		t.Errorf("members span %d release groups, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestEditConflictingAnchorsBlockArtistRename: two album groups under one release
// group where one group's anchors are not even internally uniform must veto the
// artist rename deterministically, rather than letting map iteration order decide
// whether an earlier group's uniform value stands.
func TestEditConflictingAnchorsBlockArtistRename(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, d := range []struct{ folder, essence, title string }{
		{"d1", "c1", "A1T1"}, {"d2", "c2", "A2T1"}, {"d2", "c3", "A2T2"},
	} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/One/" + d.folder + "/" + d.essence + ".flac", essence: d.essence, content: d.essence,
			title: d.title, artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
		})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want 2", n)
	}
	alphaPID := entityPID(t, st, "artist", "Alpha")
	p1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='A1T1'"))
	p2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='A2T1'"))
	p3 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='A2T2'"))

	if _, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"artist": "Beta", "album_artist": "Beta"}},
		{ItemPID: p2, Fields: map[string]string{"artist": "Beta", "album_artist": "Beta"}},
		{ItemPID: p3, Fields: map[string]string{"artist": "Beta", "album_artist": "Gamma"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Alpha survives under its old name and pid: the second group's non-uniform
	// anchors blocked the release-group reference check.
	if pid := entityPID(t, st, "artist", "Alpha"); pid != alphaPID {
		t.Fatalf("Alpha pid = %s, want kept %s", pid, alphaPID)
	}
	assertVerifyClean(t, st)
}

// TestEditYearKeepsMergedAnchorSpelling: after a merge the member columns still
// spell the loser's name while the entity spells the survivor's, and an unrelated
// whole-set edit must not rename the survivor back to the column value through the
// vacuously-passing coverage checks. The anchor pair fires only when the edit moved
// the anchor.
func TestEditYearKeepsMergedAnchorSpelling(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// The survivor's only reference is the merged compilation itself: two loose
	// tracks create the entity (no album, so no chain), then a non-uniform batch
	// moves both credits away, which is the split path and leaves the ghost (a
	// uniform whole-set edit would rename it in place instead).
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/loose/t0.flac", essence: "t0", content: "t0",
		title: "T0", artist: "The Beatles",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/loose/t1.flac", essence: "t1", content: "t1",
		title: "T1", artist: "The Beatles",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/comp/01.flac", essence: "k1", content: "k1",
		title: "K1", artist: "Someone1", albumArt: "Beatles", album: "Comp", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/comp/02.flac", essence: "k2", content: "k2",
		title: "K2", artist: "Someone2", albumArt: "Beatles", album: "Comp", year: 2001,
	})
	p0 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T0'"))
	pl1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	if _, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p0, Fields: map[string]string{"artist": "Unrelated1"}},
		{ItemPID: pl1, Fields: map[string]string{"artist": "Unrelated2"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("orphan the survivor: %v", err)
	}
	survivorPID := entityPID(t, st, "artist", "The Beatles")
	loserPID := entityPID(t, st, "artist", "Beatles")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivorPID, loserPID); err != nil {
		t.Fatalf("merge: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	p1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='K1'"))
	p2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='K2'"))
	if _, err := st.EditManyFields(ctx, []model.PID{p1, p2}, map[string]string{"year": "1999"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("year edit: %v", err)
	}

	// The survivor keeps its spelling and pid: no pair fired, so the pre-pass never
	// renamed it back to the column value. The per-item loop still re-derives a
	// fresh "Beatles" row from the unchanged column, the standing contract that a
	// heuristic merge can be re-derived over (merge.go); what matters here is that
	// the surviving entity, with its stars and curation, was not rewritten.
	if pid := entityPID(t, st, "artist", "The Beatles"); pid != survivorPID {
		t.Fatalf("survivor pid = %s, want kept %s", pid, survivorPID)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 0 {
		t.Errorf("artist updates = %d, want 0 (a year edit renames no artist)", n)
	}
	assertVerifyClean(t, st)
}

// TestEditArchivedAlbumFallsBackToSplit: members with no primary file compute a
// folder-less album key no scan of the restored files would ever compute, so the
// pre-pass must not rewrite the original row onto it; the group falls back to the
// per-item split and the original keeps its real key.
func TestEditArchivedAlbumFallsBackToSplit(t *testing.T) {
	st, _, pids := renameFixture(t)
	ctx := context.Background()
	albumID := scalarInt(t, st, "SELECT id FROM album")
	key0 := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID)
	if _, err := st.write.ExecContext(ctx, "DELETE FROM item_file"); err != nil {
		t.Fatalf("archive members: %v", err)
	}

	if _, err := st.EditManyFields(ctx, pids, map[string]string{"album": "Renamed"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != key0 {
		t.Fatalf("archived album match_key = %q, want unchanged %q", k, key0)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 0 {
		t.Errorf("old album still has %d members, want 0 (split)", n)
	}
	assertVerifyClean(t, st)
}

// TestEditArtistCaseOnlyRespellKeepsMarker: a whole-set respelling that folds to the
// same match key refreshes the display name but keeps the unmatched enrichment
// marker, since MatchKey folds exactly what the MusicBrainz search is insensitive
// to and re-queueing would burn a rate-limited lookup on the same non-match.
func TestEditArtistCaseOnlyRespellKeepsMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/one/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "alpha", albumArt: "alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	artistID := scalarInt(t, st, "SELECT id FROM artist")
	if _, err := st.write.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES ('artist', ?, 'musicbrainz', 0, NULL, 1)`, artistID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if err := st.EditItemFields(ctx, pid, map[string]string{"artist": "ALPHA", "album_artist": "ALPHA"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if name := scalarStr(t, st, "SELECT name FROM artist WHERE id=?", artistID); name != "ALPHA" {
		t.Fatalf("artist name = %q, want the respelled ALPHA", name)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 1 {
		t.Errorf("artist updates = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist' AND entity_id=? AND matched=0", artistID); n != 1 {
		t.Errorf("unmatched marker rows = %d, want 1 (a same-key respell is not new search evidence)", n)
	}
	assertVerifyClean(t, st)
}
