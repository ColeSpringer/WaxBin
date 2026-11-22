package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRenameEntityAlbumKeepsRow renames a whole album and asserts the row survived with
// its pid and everything hanging off it, which is what the implicit pre-pass could only
// manage when a caller's batch happened to cover every track.
func TestRenameEntityAlbumKeepsRow(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", year: 2001,
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='Album'"))
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid=?", string(albumPID)))
	if _, err := st.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	if _, err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, tinyPNG(t), "image/png",
		model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("art: %v", err)
	}

	rep, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New Title"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Errorf("outcome = %q, want renamed", rep.Outcome)
	}
	if rep.Members != 2 {
		t.Errorf("members = %d, want 2", rep.Members)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album count = %d, want 1 (renamed in place, not split)", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM album"); got != string(albumPID) {
		t.Errorf("album pid = %s, want the original %s", got, albumPID)
	}
	if got := scalarStr(t, st, "SELECT title FROM album"); got != "New Title" {
		t.Errorf("album title = %q, want the new one", got)
	}
	state, _ := st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID)
	if !state.Starred {
		t.Error("star did not survive the rename")
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", albumID); n == 0 {
		t.Error("art did not survive the rename")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 2 {
		t.Errorf("tracks on the album = %d, want both members", n)
	}
}

// TestRenameEntityCaseOnlyRefreshes covers the third outcome: the key does not move, so
// only the display columns do.
func TestRenameEntityCaseOnlyRefreshes(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "album",
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	rep, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "ALBUM"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenameRefreshed {
		t.Errorf("outcome = %q, want refreshed", rep.Outcome)
	}
	if got := scalarStr(t, st, "SELECT title FROM album"); got != "ALBUM" {
		t.Errorf("title = %q, want the new casing", got)
	}
}

// TestRenameEntityOntoTakenKeyMerges pins the merge outcome and the survivor the report
// hands back, since the caller's pid does not exist afterwards.
func TestRenameEntityOntoTakenKeyMerges(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/A/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Wrong Title",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/A/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Right Title",
	})
	losePID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='Wrong Title'"))
	keepPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='Right Title'"))

	rep, err := st.RenameEntity(ctx, model.MergeAlbum, losePID,
		map[string]string{"album": "Right Title"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenameMerged {
		t.Fatalf("outcome = %q, want merged", rep.Outcome)
	}
	if rep.MergedInto != keepPID {
		t.Errorf("merged into %s, want %s", rep.MergedInto, keepPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE pid=?", string(losePID)); n != 0 {
		t.Error("the renamed album row is still there after a merge")
	}
}

// TestRenameEntityRefusesSilentSplits checks each refusal that replaces a split nobody
// was told about.
func TestRenameEntityRefusesSilentSplits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album",
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	itemPID := model.PID(scalarStr(t, st,
		"SELECT pi.pid FROM playable_item pi JOIN track t ON t.item_id=pi.id ORDER BY pi.id LIMIT 1"))

	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "  "}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("rename to an empty name = %v, want CodeInvalid", err)
	}
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"genre": "Rock"}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("rename on a non-keying field = %v, want CodeInvalid", err)
	}
	if _, err := st.RenameEntity(ctx, model.MergeReleaseGroup, albumPID,
		map[string]string{"year": "2001"}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("year at the group rung = %v, want CodeInvalid", err)
	}
	// The year keys no album either, so the album rung refuses it too.
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"year": "2001"}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("year at the album rung = %v, want CodeInvalid", err)
	}

	// A locked keying field on one member is a refusal, never the skip that would break
	// the coverage count and split the album.
	if err := st.EditItemFields(ctx, itemPID, map[string]string{"album": "Album"},
		model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("lock a member's album: %v", err)
	}
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New"}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("rename over a locked member = %v, want CodeLocked", err)
	}
	// force overrides it and the album still moves as one row.
	rep, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New"}, model.Attribution{}, model.LockUnchanged, true)
	if err != nil {
		t.Fatalf("forced rename: %v", err)
	}
	if rep.Outcome != model.EntityRenamed || scalarInt(t, st, "SELECT COUNT(*) FROM album") != 1 {
		t.Errorf("forced rename outcome = %q with %d albums, want one renamed row",
			rep.Outcome, scalarInt(t, st, "SELECT COUNT(*) FROM album"))
	}
}

// TestRenameReleaseGroupRefusesMixedEditions covers the group-rung refusal: retitling a
// group retitles every member, which would flatten editions that are deliberately named
// apart into one album.
func TestRenameReleaseGroupRefusesMixedEditions(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "A", artist: "Band",
		album: "Deluxe Edition", mbReleaseGroup: "11111111-2222-3333-4444-555555555555", mbRelease: "aaaaaaaa-2222-3333-4444-555555555555",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "B", artist: "Band",
		album: "Original Pressing", mbReleaseGroup: "11111111-2222-3333-4444-555555555555", mbRelease: "bbbbbbbb-2222-3333-4444-555555555555",
	})
	rgPID := model.PID(scalarStr(t, st, "SELECT pid FROM release_group"))
	if _, err := st.RenameEntity(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"album": "One Name"}, model.Attribution{}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("rename over mixed editions = %v, want CodeConflict", err)
	}
}

// TestRenameReleaseGroupMovesChain renames a group whose albums share a title and checks
// both rungs moved together, the group keeping its pid.
func TestRenameReleaseGroupMovesChain(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album2/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album",
	})
	rgPID := model.PID(scalarStr(t, st, "SELECT pid FROM release_group"))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album count = %d, want 2 editions under one group", n)
	}
	rep, err := st.RenameEntity(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"album_artist": "The Band"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Errorf("outcome = %q, want renamed", rep.Outcome)
	}
	if rep.Members != 2 {
		t.Errorf("members = %d, want 2", rep.Members)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release_group count = %d, want 1", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM release_group"); got != string(rgPID) {
		t.Errorf("release group pid = %s, want the original %s", got, rgPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM album WHERE release_group_id=(SELECT id FROM release_group)"); n != 2 {
		t.Errorf("albums still under the group = %d, want 2", n)
	}
}

// TestRenameEntityRefusesArchivedMember covers the refusal that today vetoes the whole
// in-place move silently: a member with no primary file has no folder to key on, so no
// scan of its restored files would ever compute the key the rename lands on.
func TestRenameEntityRefusesArchivedMember(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album",
	})
	if _, err := st.DetachFile(ctx, res.FilePID, model.DeletePermanent); err != nil {
		t.Fatalf("DetachFile: %v", err)
	}
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	_, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New"}, model.Attribution{}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("rename with an archived member = %v, want CodeConflict", err)
	}
	if !strings.Contains(err.Error(), string(res.ItemPID)) {
		t.Errorf("refusal %q does not name the archived member %s", err, res.ItemPID)
	}
}

// TestRenameArtistMovesEveryReference renames an artist that a track credits as its
// performer, another track credits as album-artist, and a book credits as author, and
// checks all three followed one row rather than forking a second artist.
func TestRenameArtistMovesEveryReference(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", albumArt: "Alpha", album: "Album",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Books/b.m4b", essence: "e2", content: "c2", title: "A Book", author: "Alpha",
	})
	artistPID := entityPID(t, st, "artist", "Alpha")
	artistID := int64(scalarInt(t, st, "SELECT id FROM artist WHERE pid=?", string(artistPID)))

	rep, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Errorf("outcome = %q, want renamed", rep.Outcome)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid=?", string(artistPID)); got != "Alpha Prime" {
		t.Errorf("artist name = %q, want the new one", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Alpha'"); n != 0 {
		t.Error("the old spelling forked a second artist row")
	}
	if got := scalarStr(t, st, "SELECT artist FROM track"); got != "Alpha Prime" {
		t.Errorf("track artist = %q, want the new name", got)
	}
	if got := scalarStr(t, st, "SELECT album_artist FROM track"); got != "Alpha Prime" {
		t.Errorf("track album_artist = %q, want the new name", got)
	}
	if got := scalarStr(t, st, "SELECT author FROM book"); got != "Alpha Prime" {
		t.Errorf("book author = %q, want the new name", got)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM item_contributor WHERE artist_id=?", artistID); n != 2 {
		t.Errorf("contributor rows on the renamed artist = %d, want the track and the book", n)
	}
}

// TestRenameArtistKeepsJointCreditNames covers the substitution: a track credited to two
// artists keeps the other one, which is also what makes the pre-pass's fold-back guard
// meaningful.
func TestRenameArtistKeepsJointCreditNames(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Joint/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha; Beta", artists: []string{"Alpha", "Beta"}, album: "Album",
	})
	artistPID := entityPID(t, st, "artist", "Alpha")
	if _, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if got := scalarStr(t, st, "SELECT artist FROM track"); got != "Alpha Prime; Beta" {
		t.Errorf("track artist = %q, want only Alpha replaced", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta'"); n != 1 {
		t.Error("Beta did not survive the rename of its co-credit")
	}
}

// TestRenameArtistMovesContributorRole: a producer credit has no item field to ride, so
// it moves on the credit surface inside the rename's own transaction. Leaving it behind
// would keep the row naming the old spelling while the artist's curation moved, which is
// why this used to be a refusal.
func TestRenameArtistMovesContributorRole(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "Album",
	})
	itemPID := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item"))
	if _, _, err := st.SetItemCredits(ctx, itemPID, model.RoleProducer, []string{"Alpha"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit Alpha as producer: %v", err)
	}
	artistPID := entityPID(t, st, "artist", "Alpha")
	rep, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Fatalf("outcome = %s, want the row moved in place", rep.Outcome)
	}
	if rep.Credits != 1 || len(rep.CreditEdits) != 1 {
		t.Fatalf("credits = %d, edits = %+v, want the producer credit reported", rep.Credits, rep.CreditEdits)
	}
	if rep.CreditEdits[0].Role != model.RoleProducer || rep.CreditEdits[0].ItemPID != itemPID {
		t.Errorf("credit edit = %+v, want the producer role on %s", rep.CreditEdits[0], itemPID)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid='"+string(artistPID)+"'"); got != "Alpha Prime" {
		t.Errorf("artist name = %q, want the row renamed rather than ghosted", got)
	}
	// The one artist row now backs both references, so nothing is left spelling the old
	// name for a rescan to fork back.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Alpha'"); n != 0 {
		t.Errorf("%d artist rows still spell the old name", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		WHERE ic.role='producer' AND a.pid='`+string(artistPID)+`'`); n != 1 {
		t.Errorf("producer rows pointing at the renamed artist = %d, want 1", n)
	}
}

// TestRenameArtistRefusesLockedCredit: the credit half checks its lock up front, with the
// field half, so a rename cannot move some of an artist's references and refuse the rest.
func TestRenameArtistRefusesLockedCredit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "Album",
	})
	itemPID := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item"))
	if _, _, err := st.SetItemCredits(ctx, itemPID, model.RoleProducer, []string{"Alpha"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("credit Alpha as producer: %v", err)
	}
	artistPID := entityPID(t, st, "artist", "Alpha")
	_, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("rename over a locked producer credit = %v, want CodeLocked", err)
	}
	if !strings.Contains(err.Error(), string(itemPID)) {
		t.Errorf("refusal %q does not name the item holding the credit", err)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid='"+string(artistPID)+"'"); got != "Alpha" {
		t.Errorf("artist name = %q, want the refused rename to have written nothing", got)
	}
	// Force overrides it, the way it overrides a locked keying field.
	if _, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, true); err != nil {
		t.Fatalf("forced rename: %v", err)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid='"+string(artistPID)+"'"); got != "Alpha Prime" {
		t.Errorf("artist name after force = %q, want the rename to have landed", got)
	}
}

// TestRenameArtistMovesFieldAndCreditTogether: an artist performing on one track and
// producing another moves both references in one transaction, which is what makes the
// coverage check pass and the row rename in place rather than ghost.
func TestRenameArtistMovesFieldAndCreditTogether(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "Album",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Other/Album/01.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Gamma", album: "Other",
	})
	produced := model.PID(scalarStr(t, st, "SELECT pi.pid FROM playable_item pi JOIN track t ON t.item_id=pi.id WHERE t.artist='Gamma'"))
	if _, _, err := st.SetItemCredits(ctx, produced, model.RoleProducer, []string{"Alpha"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit Alpha as producer: %v", err)
	}
	artistPID := entityPID(t, st, "artist", "Alpha")
	rep, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Fatalf("outcome = %s, want the row moved in place", rep.Outcome)
	}
	if rep.Members != 1 || rep.Credits != 1 {
		t.Errorf("members = %d, credits = %d, want one of each", rep.Members, rep.Credits)
	}
	if got := scalarStr(t, st, `SELECT t.artist FROM track t
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title='One'`); got != "Alpha Prime" {
		t.Errorf("performing credit = %q", got)
	}
	// producer carries no denormalized column, so the credit row is where it shows.
	if got := scalarStr(t, st, `SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE ic.role='producer' AND pi.title='Two'`); got != "Alpha Prime" {
		t.Errorf("producer credit = %q", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name IN ('Alpha','Alpha Prime')"); n != 1 {
		t.Errorf("artist rows spelling either name = %d, want the one renamed row", n)
	}
	assertVerifyClean(t, st)
}

// TestRenameEntityRefusesSplitAcrossFolders is the last silent fallback closed. Coverage
// alone does not make the in-place branch fire: the heuristic album key carries each
// member's folder, so an album whose members live in different folders, which a shared
// MusicBrainz id can produce, computes a different key per folder and splits.
func TestRenameEntityRefusesSplitAcrossFolders(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "dddddddd-1111-2222-3333-444444444444"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album", mbRelease: relMBID,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Elsewhere/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", mbRelease: relMBID,
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album count = %d, want the one release id unified them", n)
	}
	// The release id keys the album today, so the rename would drop to the heuristic keys
	// the two folders compute and land the members apart.
	if _, err := st.wdb().ExecContext(ctx,
		"UPDATE album SET match_key='al:rg:band\x1falbum\x1f\x1f\x1flib band album' WHERE pid=?",
		string(albumPID)); err != nil {
		t.Fatal(err)
	}
	_, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New"}, model.Attribution{}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("rename across folders = %v, want CodeConflict", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count = %d, want the refusal to have written nothing", n)
	}
}

// TestRenameArtistKeepsStatedCoCredits is the credit-loss regression. A file that states
// its own artist list resolves one entity per name while track.artist keeps the joined
// display, and SplitPerformerCredit does not break on a comma or an ampersand. Deriving
// the rename from that column found no name to replace, wrote it back untouched, and the
// pre-pass then renamed the artist row onto the whole credit string while the co-credit
// was dropped. The list now comes from the contributor rows instead.
func TestRenameArtistKeepsStatedCoCredits(t *testing.T) {
	t.Parallel()
	for _, display := range []string{"Alpha & Beta", "Alpha, Beta", "Alpha; Beta"} {
		t.Run(display, func(t *testing.T) {
			st, lib := entityFixture(t)
			ctx := context.Background()
			putTrack(t, st, lib.ID, trackSpec{
				path: "/lib/x/01.flac", essence: "e1", content: "c1", title: "One",
				artist: display, artists: []string{"Alpha", "Beta"}, album: "Album",
			})
			if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 2 {
				t.Fatalf("artist rows = %d, want the stated list to have resolved two", n)
			}
			pid := entityPID(t, st, "artist", "Alpha")
			if _, err := st.RenameEntity(ctx, model.MergeArtist, pid,
				map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false); err != nil {
				t.Fatalf("RenameEntity: %v", err)
			}
			if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid=?", string(pid)); got != "Alpha Prime" {
				t.Errorf("artist name = %q, want Alpha Prime and not the whole credit string", got)
			}
			if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta'"); n != 1 {
				t.Error("the co-credit Beta was dropped by the rename")
			}
			if n := scalarInt(t, st,
				"SELECT COUNT(*) FROM item_contributor WHERE role='artist'"); n != 2 {
				t.Errorf("artist contributor rows = %d, want both credits kept", n)
			}
			// The joined display comes back semicolon-separated, which is the only
			// separator resolution reads back into separate entities.
			if got := scalarStr(t, st, "SELECT artist FROM track"); got != "Alpha Prime; Beta" {
				t.Errorf("track artist = %q, want %q", got, "Alpha Prime; Beta")
			}
		})
	}
}

// TestRenameEntityValidatesAttributionAndLock: the rename is a curation write like every
// other one, so it defaults an empty attribution to a user edit and refuses values the
// other surfaces refuse, rather than storing them.
func TestRenameEntityValidatesAttributionAndLock(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))

	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "X"}, model.Attribution{Provider: "discogs"},
		model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("provider without an enrichment source = %v, want CodeInvalid", err)
	}
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "X"}, model.Attribution{},
		model.LockChange("maybe"), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown lock instruction = %v, want CodeInvalid", err)
	}
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "X"}, model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	// An empty attribution defaults to a user edit, as on every other curation surface.
	if got := scalarStr(t, st, `SELECT source FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE fp.field='album' LIMIT 1`); got != string(model.SourceUser) {
		t.Errorf("recorded provenance source = %q, want user", got)
	}
}

// TestRenameArtistSpansManyAlbums: an artist legitimately sits under as many release
// groups as it has albums, so the chain-rung expectation that every member lands under one
// group must not be applied here.
func TestRenameArtistSpansManyAlbums(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/First/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "First",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Second/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Alpha", album: "Second",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Fatalf("release groups = %d, want one per album title", n)
	}
	pid := entityPID(t, st, "artist", "Alpha")
	rep, err := st.RenameEntity(ctx, model.MergeArtist, pid,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity across two albums: %v", err)
	}
	if rep.Outcome != model.EntityRenamed || rep.Members != 2 {
		t.Errorf("report = %+v, want a two-member rename in place", rep)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid=?", string(pid)); got != "Alpha Prime" {
		t.Errorf("artist name = %q, want the new one", got)
	}
}

// TestRenameArtistRefusedWhenPrePassDeclines is the backstop for the coverage rules the
// pre-pass owns. artistRenameCoveredTx checks five reference kinds and declines silently
// on any it cannot satisfy, which used to surface as a successful "refreshed" while the
// entity sat where it was. A release group whose primary_artist_id points at the artist
// without the artist being on its tracks is one such reference, and the shape a
// credit-split transition leaves behind.
func TestRenameArtistRefusedWhenPrePassDeclines(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "Album",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Gamma/Other/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Gamma", album: "Other",
	})
	pid := entityPID(t, st, "artist", "Alpha")
	// Gamma's group now names Alpha as its primary artist, a reference no member of the
	// rename touches.
	if _, err := st.wdb().ExecContext(ctx, `UPDATE release_group
		SET primary_artist_id = (SELECT id FROM artist WHERE pid = ?)
		WHERE title = 'Other'`, string(pid)); err != nil {
		t.Fatal(err)
	}
	_, err := st.RenameEntity(ctx, model.MergeArtist, pid,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("rename with an uncovered reference = %v, want CodeConflict", err)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid=?", string(pid)); got != "Alpha" {
		t.Errorf("artist name = %q, want the refusal to have written nothing", got)
	}
}

// TestRenameArtistMovesASharedRole: two producers on one item. The credit surface's
// cardinality rule cannot say which of them a batch means, but a rename names one artist
// and one target, so it states the pair rather than leaving the rule to guess. Before
// that, this failed the coverage check and rolled back reporting an uncovered reference,
// which was not the reason.
func TestRenameArtistMovesASharedRole(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/G/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Gamma", album: "Album",
	})
	itemPID := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item"))
	if _, _, err := st.SetItemCredits(ctx, itemPID, model.RoleProducer, []string{"Alpha", "Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit two producers: %v", err)
	}
	artistPID := entityPID(t, st, "artist", "Alpha")
	rep, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed || rep.Credits != 1 {
		t.Fatalf("report = %+v, want the row moved with its one credit", rep)
	}
	if len(rep.CreditEdits) != 1 || len(rep.CreditEdits[0].Names) != 2 ||
		rep.CreditEdits[0].Names[0] != "Alpha Prime" || rep.CreditEdits[0].Names[1] != "Beta" {
		t.Fatalf("credit edit = %+v, want only Alpha replaced", rep.CreditEdits)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta'"); n != 1 {
		t.Error("Beta did not survive the rename of the artist sharing its role")
	}
	assertVerifyClean(t, st)
}

// TestRenameArtistRefusesAKeylessName: a name of nothing but punctuation passes the
// empty check and then folds to an empty match key. The artist stage skips such a target
// while the applies still run, and resolveArtist drops every name it cannot key, so the
// credits would be deleted and not rebuilt with the rename reporting success. An artist
// held by credits alone has no member for the member-key check to catch this on.
func TestRenameArtistRefusesAKeylessName(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/G/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Gamma", album: "Album",
	})
	itemPID := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item"))
	if _, _, err := st.SetItemCredits(ctx, itemPID, model.RoleProducer, []string{"Alpha"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit: %v", err)
	}
	artistPID := entityPID(t, st, "artist", "Alpha")
	_, err := st.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "???"}, model.Attribution{}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("rename to a keyless name = %v, want CodeInvalid", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_contributor"); n != 2 {
		t.Errorf("contributor rows = %d, want both kept: the refusal must write nothing", n)
	}
}

// rgPIDOf and albumPIDOf read the heuristic release group an artist and a title key, and
// the album of it whose members sit in folder.
func rgPIDOf(t *testing.T, st *Store, artist, title string) model.PID {
	t.Helper()
	return entityPIDByCol(t, st, "release_group", "match_key",
		identity.ReleaseGroupKey("", identity.MatchKey(artist), title))
}

func albumPIDOf(t *testing.T, st *Store, artist, title, folder string) model.PID {
	t.Helper()
	return entityPIDByCol(t, st, "album", "match_key",
		identity.AlbumKey("", identity.ReleaseGroupKey("", identity.MatchKey(artist), title), 0, folder))
}

// assertMerges compares a report's merges with want, in any order.
func assertMerges(t *testing.T, got, want []model.EntityMerge) {
	t.Helper()
	flat := func(ms []model.EntityMerge) []string {
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, string(m.EntityType)+" "+string(m.Loser)+" into "+string(m.Survivor))
		}
		slices.Sort(out)
		return out
	}
	if g, w := flat(got), flat(want); !slices.Equal(g, w) {
		t.Errorf("merges = %v, want %v", g, w)
	}
}

// assertMergeRows checks that the entity deletes the feed holds after seq are exactly the
// merges in want, each naming the row it went into.
func assertMergeRows(t *testing.T, st *Store, seq int64, want []model.EntityMerge) {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("ChangesSince: %v", err)
	}
	var got []model.EntityMerge
	for _, c := range changes {
		if c.Op == model.OpDelete && model.MergeEntity(c.EntityType).Valid() {
			got = append(got, model.EntityMerge{
				EntityType: model.MergeEntity(c.EntityType), Loser: c.EntityPID, Survivor: c.MergedInto,
			})
		}
	}
	assertMerges(t, got, want)
}

// TestRenameArtistReportsTheReleasesItMerged: an artist merging into another takes along
// its release groups whose titles the survivor already has, and an album whose edition
// shares a folder with the survivor's. The report names each with the entity it went
// into, and so does each delete in the change feed.
func TestRenameArtistReportsTheReleasesItMerged(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, s := range []struct{ path, artist, album string }{
		{"/lib/Shared/Same/01.flac", "Alpha", "Same"},
		{"/lib/Shared/Same/02.flac", "Beta", "Same"},
		{"/lib/Alpha/Other/03.flac", "Alpha", "Other"},
		{"/lib/Beta/Other/04.flac", "Beta", "Other"},
	} {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: s.path, essence: "e" + n, content: "c" + n, title: "T" + n,
			artist: s.artist, album: s.album})
	}
	alpha, beta := entityPID(t, st, "artist", "Alpha"), entityPID(t, st, "artist", "Beta")
	want := []model.EntityMerge{
		{EntityType: model.MergeArtist, Loser: alpha, Survivor: beta},
		{EntityType: model.MergeReleaseGroup, Loser: rgPIDOf(t, st, "Alpha", "Same"), Survivor: rgPIDOf(t, st, "Beta", "Same")},
		{EntityType: model.MergeReleaseGroup, Loser: rgPIDOf(t, st, "Alpha", "Other"), Survivor: rgPIDOf(t, st, "Beta", "Other")},
		{EntityType: model.MergeAlbum, Loser: albumPIDOf(t, st, "Alpha", "Same", "/lib/Shared/Same"),
			Survivor: albumPIDOf(t, st, "Beta", "Same", "/lib/Shared/Same")},
	}
	moved := albumPIDOf(t, st, "Alpha", "Other", "/lib/Alpha/Other")
	seq0 := latestSeq(t, st)

	rep, err := st.RenameEntity(ctx, model.MergeArtist, alpha, map[string]string{"name": "Beta"},
		model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenameMerged || rep.MergedInto != beta {
		t.Fatalf("outcome = %q into %q, want merged into %s", rep.Outcome, rep.MergedInto, beta)
	}
	assertMerges(t, rep.Merges, want)
	// Its group merged away, but the album itself sits in a folder of its own and stands.
	if !slices.Equal(rep.MovedAlbums, []model.PID{moved}) {
		t.Errorf("moved albums = %v, want %s alone", rep.MovedAlbums, moved)
	}
	assertMergeRows(t, st, seq0, want)
	assertVerifyClean(t, st)
}

// TestRenameReleaseGroupReportsTheAlbumItMerged: a group renamed onto a key another group
// holds merges into it, and its album merges into the incumbent's where the two editions
// sit in one folder.
func TestRenameReleaseGroupReportsTheAlbumItMerged(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Shared/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", albumArt: "Band", album: "Album"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Shared/Album/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "The Band", albumArt: "The Band", album: "Album"})
	loser, survivor := rgPIDOf(t, st, "Band", "Album"), rgPIDOf(t, st, "The Band", "Album")
	want := []model.EntityMerge{
		{EntityType: model.MergeReleaseGroup, Loser: loser, Survivor: survivor},
		{EntityType: model.MergeAlbum, Loser: albumPIDOf(t, st, "Band", "Album", "/lib/Shared/Album"),
			Survivor: albumPIDOf(t, st, "The Band", "Album", "/lib/Shared/Album")},
	}

	rep, err := st.RenameEntity(ctx, model.MergeReleaseGroup, loser, map[string]string{"album_artist": "The Band"},
		model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenameMerged || rep.MergedInto != survivor {
		t.Fatalf("outcome = %q into %q, want merged into %s", rep.Outcome, rep.MergedInto, survivor)
	}
	assertMerges(t, rep.Merges, want)
	if len(rep.MovedAlbums) != 0 {
		t.Errorf("moved albums = %v, want none: the album merged away", rep.MovedAlbums)
	}
	assertVerifyClean(t, st)
}

// TestRenameAlbumReportsTheGroupItMerged: an album renamed to a title its artist already
// has a group for takes its own group into that one, while the album, in a folder of its
// own, keeps its row and comes out under the incumbent's group.
func TestRenameAlbumReportsTheGroupItMerged(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Wrong/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Wrong"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Right/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Right"})
	album := albumPIDOf(t, st, "Band", "Wrong", "/lib/Band/Wrong")
	want := []model.EntityMerge{
		{EntityType: model.MergeReleaseGroup, Loser: rgPIDOf(t, st, "Band", "Wrong"), Survivor: rgPIDOf(t, st, "Band", "Right")},
	}

	rep, err := st.RenameEntity(ctx, model.MergeAlbum, album, map[string]string{"album": "Right"},
		model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	if rep.Outcome != model.EntityRenamed || rep.MergedInto != "" {
		t.Fatalf("outcome = %q into %q, want renamed in place", rep.Outcome, rep.MergedInto)
	}
	assertMerges(t, rep.Merges, want)
	if !slices.Equal(rep.MovedAlbums, []model.PID{album}) {
		t.Errorf("moved albums = %v, want %s", rep.MovedAlbums, album)
	}
	assertVerifyClean(t, st)
}

// leaveMemberless deletes an item the way a fold does, leaving its album with no tracks,
// the shape a split the re-key reconciliation cannot carry leaves until the orphan sweep.
func leaveMemberless(t *testing.T, st *Store, title string) {
	t.Helper()
	ctx := context.Background()
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE title = ?", title).Scan(&id); err != nil {
			return err
		}
		aff := newAffectedRollups()
		if err := aff.collect(ctx, tx, id); err != nil {
			return err
		}
		var batch entryHolders
		if _, err := deleteItemCascade(ctx, tx, id, &batch); err != nil {
			return err
		}
		if err := batch.settleTx(ctx, tx); err != nil {
			return err
		}
		return maintainRollupsTx(ctx, tx, aff, nowNS())
	})
	if err != nil {
		t.Fatalf("delete %q: %v", title, err)
	}
}

// TestRenameReleaseGroupPastAMemberlessAlbum: an album left with no tracks has nothing to
// carry the rename, so it does not hold the group back. The group moves in place, and the
// memberless album stays under it for the orphan sweep.
func TestRenameReleaseGroupPastAMemberlessAlbum(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Album 2/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album"})
	group := rgPIDOf(t, st, "Band", "Album")
	empty := albumPIDOf(t, st, "Band", "Album", "/lib/Band/Album 2")
	leaveMemberless(t, st, "Two")

	rep, err := st.RenameEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"album": "Renamed"},
		model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity past a memberless album: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Errorf("outcome = %q, want renamed in place", rep.Outcome)
	}
	if got := rgPIDOf(t, st, "Band", "Renamed"); got != group {
		t.Errorf("renamed group pid = %s, want the original %s", got, group)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE pid = ? AND release_group_id = (SELECT id FROM release_group WHERE pid = ?)",
		string(empty), string(group)); n != 1 {
		t.Errorf("the memberless album is gone or moved, want it left under the group")
	}
	assertVerifyClean(t, st)
}

// TestRenameArtistPastAMemberlessGroup: a release group whose albums hold no tracks has no
// anchor left to move, so it does not hold back a rename of the artist it points at.
func TestRenameArtistPastAMemberlessGroup(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Band/Other/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Other"})
	artist := entityPID(t, st, "artist", "Band")
	leaveMemberless(t, st, "Two")

	rep, err := st.RenameEntity(ctx, model.MergeArtist, artist, map[string]string{"name": "The Band"},
		model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("RenameEntity past a memberless group: %v", err)
	}
	if rep.Outcome != model.EntityRenamed {
		t.Errorf("outcome = %q, want renamed in place", rep.Outcome)
	}
	if got := scalarStr(t, st, "SELECT name FROM artist WHERE pid = ?", string(artist)); got != "The Band" {
		t.Errorf("artist name = %q, want the new one", got)
	}
	assertVerifyClean(t, st)
}

// TestRenameReleaseGroupPastAMemberlessEdition: an edition titled apart from the rest has
// its own title to keep, which refuses a group rename, unless it holds no tracks: then the
// rename retitles nothing of it.
func TestRenameReleaseGroupPastAMemberlessEdition(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "abababab-abab-abab-abab-abababababab"
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", mbReleaseGroup: rgMBID})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/Alpha/One Deluxe/01.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Alpha", albumArt: "Alpha", album: "One (Deluxe Edition)", mbReleaseGroup: rgMBID})
	group := entityPIDByCol(t, st, "release_group", "match_key", "mbid:"+rgMBID)
	leaveMemberless(t, st, "T2")

	if _, err := st.RenameEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"album": "One Remastered"},
		model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("RenameEntity past a memberless edition: %v", err)
	}
	if got := scalarStr(t, st, "SELECT title FROM release_group WHERE pid = ?", string(group)); got != "One Remastered" {
		t.Errorf("group title = %q, want the new one", got)
	}
	assertVerifyClean(t, st)
}
