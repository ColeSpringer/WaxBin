package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// detachFixture scans a two-track album whose files carry a release id, so the album is
// keyed on that id while the release group above it stays heuristic. It returns the
// album's row id and pid, the release group's row id and pid, and the two member pids.
func detachFixture(t *testing.T, relMBID string) (*Store, *model.Library, int, string, int, string, model.PID, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One",
			genre: "Rock", year: 2001, mbRelease: relMBID,
		})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != "mbid:"+relMBID {
		t.Fatalf("album match_key = %q, want the release id key", k)
	}
	albumID, albumPID := scalarInt(t, st, "SELECT id FROM album"), scalarStr(t, st, "SELECT pid FROM album")
	rgID, rgPID := scalarInt(t, st, "SELECT id FROM release_group"), scalarStr(t, st, "SELECT pid FROM release_group")
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T2'"))
	return st, lib, albumID, albumPID, rgID, rgPID, pid1, pid2
}

// TestDetachMovesMemberToHeuristicAlbum: one member of an identified album re-resolves
// onto the heuristic chain its own tags and folder imply, leaving the identified row and
// its other members alone. The release group is heuristic here, so the member stays under
// it rather than forking a second one.
func TestDetachMovesMemberToHeuristicAlbum(t *testing.T) {
	const relMBID = "44444444-4444-4444-4444-444444444444"
	st, _, albumID, albumPID, rgID, rgPID, pid1, pid2 := detachFixture(t, relMBID)
	ctx := context.Background()

	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	rep, err := st.DetachItemFromMBIDAlbum(ctx, pid1)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", rgKey, 2001, 0, "/lib/Alpha/One")
	newAlbumID := scalarInt(t, st, "SELECT id FROM album WHERE match_key=?", wantKey)
	if newAlbumID == albumID {
		t.Fatalf("the detached member landed back on the identified album %d", albumID)
	}
	if id := memberAlbumID(t, st, pid1); id != newAlbumID {
		t.Errorf("detached member's album = %d, want the fresh heuristic row %d", id, newAlbumID)
	}
	if id := memberAlbumID(t, st, pid2); id != albumID {
		t.Errorf("the other member moved to album %d, want the identified row %d", id, albumID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 1 {
		t.Errorf("members left on the identified album = %d, want 1", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != "mbid:"+relMBID {
		t.Errorf("identified album re-keyed to %q, want it untouched", k)
	}
	// The fresh row is bare: nothing carries across, since the member left an album it
	// still shares with others rather than draining one.
	if v := scalarStr(t, st, "SELECT COALESCE(mbid,'') FROM album WHERE id=?", newAlbumID); v != "" {
		t.Errorf("fresh album mbid = %q, want it empty", v)
	}

	// The release group is heuristic, so the detached member keeps it.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release_group rows = %d, want the one reused", n)
	}
	if id := scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", newAlbumID); id != rgID {
		t.Errorf("fresh album's release group = %d, want the reused %d", id, rgID)
	}

	newAlbumPID := scalarStr(t, st, "SELECT pid FROM album WHERE id=?", newAlbumID)
	want := model.DetachReport{
		ItemPID: pid1, OldAlbumPID: model.PID(albumPID),
		NewAlbumPID: model.PID(newAlbumPID), NewReleaseGroupPID: model.PID(rgPID),
	}
	if rep == nil || *rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}

	if n := changeCount(t, st, seq0, "album", model.OpCreate); n != 1 {
		t.Errorf("album creates = %d, want 1", n)
	}
	if n := changeCount(t, st, seq0, "item", model.OpUpdate); n != 1 {
		t.Errorf("item updates = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestDetachRefusesLastMember: detaching the only member would leave the identified
// album curated, pid intact, and empty, which is the ghost the scan reconciliation
// exists to avoid. The refusal points at the whole-album escape hatch instead.
func TestDetachRefusesLastMember(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "55555555-5555-5555-5555-555555555555"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Solo/01.flac", essence: "e1", content: "c1",
		title: "T1", artist: "Alpha", albumArt: "Alpha", album: "Solo",
		genre: "Rock", year: 2001, mbRelease: relMBID,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	albumPID := scalarStr(t, st, "SELECT pid FROM album")

	rep, err := st.DetachItemFromMBIDAlbum(ctx, pid)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("last-member detach = %v, want CodeInvalid", err)
	}
	if rep != nil {
		t.Errorf("refused detach returned %+v, want no report", rep)
	}
	if want := "waxbin entity edit album " + albumPID + " --set mbid="; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %q, want it to name %q", err, want)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album rows after the refusal = %d, want 1", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != "mbid:"+relMBID {
		t.Errorf("album match_key = %q, want it untouched", k)
	}
	assertVerifyClean(t, st)
}

// groupKeyedFixture scans a two-track album whose files carry a release-group id but no
// release id, the Picard shape: the album key embeds the group's id, so every re-resolve
// re-pins the member to the group even though the album row itself is heuristic.
func groupKeyedFixture(t *testing.T, rgMBID string) (*Store, *model.Library, int, string) {
	t.Helper()
	st, lib := entityFixture(t)
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One",
			genre: "Rock", year: 2001, mbReleaseGroup: rgMBID,
		})
	}
	wantKey := identity.AlbumKey("", "mbid:"+rgMBID, 2001, 0, "/lib/Alpha/One")
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want the group-carrying %q", k, wantKey)
	}
	return st, lib, scalarInt(t, st, "SELECT id FROM album"), scalarStr(t, st, "SELECT pid FROM album")
}

// TestDetachMovesReleaseGroupKeyedMember: an album keyed on the group id alone still pins
// its members to MusicBrainz on every re-resolve, so detach applies there too. The freed
// member lands on a chain with no identifier anywhere in it.
func TestDetachMovesReleaseGroupKeyedMember(t *testing.T) {
	const rgMBID = "77777777-7777-7777-7777-777777777777"
	st, _, albumID, albumPID := groupKeyedFixture(t, rgMBID)
	ctx := context.Background()
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T2'"))

	rep, err := st.DetachItemFromMBIDAlbum(ctx, pid1)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}

	rgKey := identity.ReleaseGroupKey("", identity.MatchKey("Alpha"), "One")
	wantKey := identity.AlbumKey("", rgKey, 2001, 0, "/lib/Alpha/One")
	newAlbumID := scalarInt(t, st, "SELECT id FROM album WHERE match_key=?", wantKey)
	if id := memberAlbumID(t, st, pid1); id != newAlbumID {
		t.Errorf("detached member's album = %d, want the heuristic row %d", id, newAlbumID)
	}
	if id := memberAlbumID(t, st, pid2); id != albumID {
		t.Errorf("the other member moved to album %d, want the identified row %d", id, albumID)
	}
	// The group above it is heuristic too: the member left the identified group behind,
	// which is the linkage the detach exists to cut.
	newRGID := scalarInt(t, st, "SELECT release_group_id FROM album WHERE id=?", newAlbumID)
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", newRGID); k != rgKey {
		t.Errorf("the freed member's release group match_key = %q, want %q", k, rgKey)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k !=
		identity.AlbumKey("", "mbid:"+rgMBID, 2001, 0, "/lib/Alpha/One") {
		t.Errorf("the album left behind re-keyed to %q, want it untouched", k)
	}
	newRGPID := scalarStr(t, st, "SELECT pid FROM release_group WHERE id=?", newRGID)
	want := model.DetachReport{
		ItemPID: pid1, OldAlbumPID: model.PID(albumPID),
		NewAlbumPID:        model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE id=?", newAlbumID)),
		NewReleaseGroupPID: model.PID(newRGPID),
	}
	if rep == nil || *rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	assertVerifyClean(t, st)
}

// TestDetachRefusesLastMemberOfGroupKeyedAlbum: the last-member refusal holds for the
// group-keyed shape too, and names the hatch that fits it, which is the release group's
// own mbid rather than the album's.
func TestDetachRefusesLastMemberOfGroupKeyedAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "88888888-8888-8888-8888-888888888888"
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Solo/01.flac", essence: "e1", content: "c1",
		title: "T1", artist: "Alpha", albumArt: "Alpha", album: "Solo",
		genre: "Rock", year: 2001, mbReleaseGroup: rgMBID,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group")

	_, err := st.DetachItemFromMBIDAlbum(ctx, pid)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("last-member detach = %v, want CodeInvalid", err)
	}
	if want := "waxbin entity edit release_group " + rgPID + " --set mbid="; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %q, want it to name %q", err, want)
	}
	assertVerifyClean(t, st)
}

// TestDetachRefusesHeuristicAlbum: a member of an album keyed on tags and folder already
// resolves from its own tags, so there is nothing to detach it from.
func TestDetachRefusesHeuristicAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", year: 2001,
		})
	}
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	albumID := memberAlbumID(t, st, pid)

	if _, err := st.DetachItemFromMBIDAlbum(ctx, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("heuristic-album detach = %v, want CodeInvalid", err)
	}
	if id := memberAlbumID(t, st, pid); id != albumID {
		t.Errorf("the refused detach moved the member to album %d, want %d", id, albumID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album rows after the refusal = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestDetachRefusesNonTrack: only a track sits on an album chain, so a book is refused
// rather than sent through the track re-resolve.
func TestDetachRefusesNonTrack(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Books/Dune/01.m4b", essence: "b1", content: "bc1",
		title: "Dune", author: "Frank Herbert", durationMS: 100,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Dune'"))

	if _, err := st.DetachItemFromMBIDAlbum(ctx, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("book detach = %v, want CodeInvalid", err)
	}
	assertVerifyClean(t, st)
}

// TestDetachRetagRescanReadopts pins both sides of the durability caveat the CLI warns
// about. The detach is a catalog re-resolve, and the file's tags still name the release,
// so a scan that re-resolves entities re-adopts the member. That scan is gated on a
// created, new, relinked, or content-changed file: a byte-identical rescan does not
// re-resolve and leaves the detach standing, while a retag (same essence, new bytes)
// puts the member straight back on the identified album.
func TestDetachRetagRescanReadopts(t *testing.T) {
	const relMBID = "66666666-6666-6666-6666-666666666666"
	st, lib, albumID, _, _, _, pid1, _ := detachFixture(t, relMBID)
	ctx := context.Background()

	if _, err := st.DetachItemFromMBIDAlbum(ctx, pid1); err != nil {
		t.Fatalf("detach: %v", err)
	}
	detachedTo := memberAlbumID(t, st, pid1)
	if detachedTo == albumID {
		t.Fatalf("detach left the member on the identified album %d", albumID)
	}

	// A byte-identical re-put: same path, same essence, same content hash. The entity
	// block never runs, so the release id in the file's tags is not consulted.
	member := trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "eT1", content: "cT1",
		title: "T1", artist: "Alpha", albumArt: "Alpha", album: "One",
		genre: "Rock", year: 2001, mbRelease: relMBID,
	}
	putTrack(t, st, lib.ID, member)
	if id := memberAlbumID(t, st, pid1); id != detachedTo {
		t.Errorf("a byte-identical rescan moved the member to album %d, want the detached %d", id, detachedTo)
	}

	// A retag: the same audio essence with different bytes, which is what a tag write
	// leaves behind. That re-resolves, and the file's release id wins again.
	member.content = "cT1-retagged"
	putTrack(t, st, lib.ID, member)
	if id := memberAlbumID(t, st, pid1); id != albumID {
		t.Errorf("after a retag rescan the member sits on album %d, want the identified %d", id, albumID)
	}
	assertVerifyClean(t, st)
}
