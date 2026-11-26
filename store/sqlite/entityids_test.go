package sqlite

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestAFileNamingOnlyItsGroupLandsOnItsAlbum: an album's release group took its id while
// the files named none. A member retagged with the group's id and no release id lands on
// the album its untagged siblings sit on, under the same group, instead of an album of
// its own keyed on the group's id.
func TestAFileNamingOnlyItsGroupLandsOnItsAlbum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	spec := func(n, content, group string) trackSpec {
		return trackSpec{path: "/lib/Alpha/One/" + n + ".flac", essence: "e" + n, content: content, title: "T" + n,
			artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001, mbReleaseGroup: group, durationMS: 100}
	}
	putTrack(t, st, lib.ID, spec("1", "c1", ""))
	putTrack(t, st, lib.ID, spec("2", "c2", ""))
	album := albumPID(t, st)
	rg := model.PID(scalarStr(t, st, "SELECT pid FROM release_group"))
	const group = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	if _, err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rg, map[string]string{"mbid": group},
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("set the group's id: %v", err)
	}
	putTrack(t, st, lib.ID, spec("1", "c1-retagged", group))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums after the retag = %d, want the one", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release groups after the retag = %d, want the one", n)
	}
	if got := albumPID(t, st); got != album {
		t.Errorf("album = %s after the retag, want %s kept", got, album)
	}
	assertVerifyClean(t, st)
}

// TestEnrichmentIDHoldsAreWorkedOutOnce: the selects owing ids work out which albums and
// groups hold their ids once per statement, rather than reading an album's or a group's
// members again for every file of it.
func TestEnrichmentIDHoldsAreWorkedOutOnce(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	for name, stmt := range map[string]string{
		"items":  strings.Replace(enrichedTagSelect, "/*SCOPE*/", "", 1),
		"albums": strings.Replace(enrichedAlbumFilesQ, "/*SCOPE*/", "", 1),
	} {
		plan := explainPlan(t, st, stmt, false)
		for _, set := range []string{"MATERIALIZE held_albums", "MATERIALIZE held_groups"} {
			if !strings.Contains(plan, set) {
				t.Errorf("%s does not work out %s once:\n%s", name, strings.TrimPrefix(set, "MATERIALIZE "), plan)
			}
		}
	}
}

// TestATrackJoiningAnAlbumOwesTheIDsAUserSet: a user set an album's and its group's
// ids. A file landing on the album later without them owes both, as the members did at
// the edit, so a write-back of the ids reaches it too; one that names them owes nothing.
func TestATrackJoiningAnAlbumOwesTheIDsAUserSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	const release, group = "e3000000-0000-4000-8000-000000000001", "e3000000-0000-4000-8000-000000000002"
	spec := func(n, release, group string) trackSpec {
		return trackSpec{path: "/lib/Alpha/One/" + n + ".flac", essence: "e" + n, content: "c" + n, title: "T" + n,
			artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001, mbRelease: release, mbReleaseGroup: group, durationMS: 100}
	}
	putTrack(t, st, lib.ID, spec("1", "", ""))
	album := albumPID(t, st)
	rg := model.PID(scalarStr(t, st, "SELECT pid FROM release_group"))
	user := model.Attribution{Source: model.SourceUser}
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, album, map[string]string{"mbid": release}, user, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rg, map[string]string{"mbid": group}, user, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	bare := putTrack(t, st, lib.ID, spec("2", "", ""))
	named := putTrack(t, st, lib.ID, spec("3", release, group))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("albums = %d, want the joiners on the one", n)
	}
	got, err := owedKeysOf(ctx, st, bare.FilePID)
	if slices.Sort(got); err != nil || !slices.Equal(got, []string{model.OwedAlbumMBID, model.OwedReleaseGroupMBID}) {
		t.Errorf("a joiner naming no ids owes %v (err %v), want both ids", got, err)
	}
	if got, err := owedKeysOf(ctx, st, named.FilePID); err != nil || len(got) != 0 {
		t.Errorf("a joiner naming the ids owes %v (err %v), want nothing", got, err)
	}

	// Members moved in without a read owe both ids too: a track an edit moves, and the
	// tracks of an album merged into this one. An album is its folder's, so the album
	// they leave sits in the same one.
	other := func(n string) trackSpec {
		s := spec(n, "", "")
		s.album = "Two"
		return s
	}
	moved := putTrack(t, st, lib.ID, other("4"))
	merged := putTrack(t, st, lib.ID, other("5"))
	if err := st.EditItemFields(ctx, moved.ItemPID, map[string]string{"album": "One"}, user, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	two := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = 'Two'"))
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, album, two); err != nil {
		t.Fatal(err)
	}
	for name, res := range map[string]*model.ScanItemResult{"moved by an edit": moved, "moved by a merge": merged} {
		got, err := owedKeysOf(ctx, st, res.FilePID)
		slices.Sort(got)
		if err != nil || !slices.Contains(got, model.OwedAlbumMBID) || !slices.Contains(got, model.OwedReleaseGroupMBID) {
			t.Errorf("a file %s owes %v (err %v), want both ids", name, got, err)
		}
	}
}

// TestAHeldAlbumWithNoGroupHoldsNoOtherGroupsID: an album that holds its id and whose
// release group is gone (the column is set null when the group row goes) leaves another
// group's matched id owed to that group's files.
func TestAHeldAlbumWithNoGroupHoldsNoOtherGroupsID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	held := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Alpha/One/1.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Alpha", albumArt: "Alpha", album: "One", durationMS: 100})
	owed := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Beta/Two/2.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Beta", albumArt: "Beta", album: "Two", durationMS: 100})
	one := int64(scalarInt(t, st, "SELECT al.id FROM album al JOIN track t ON t.album_id = al.id JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?", string(held.ItemPID)))
	two := int64(scalarInt(t, st, "SELECT al.release_group_id FROM album al JOIN track t ON t.album_id = al.id JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?", string(owed.ItemPID)))
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{AlbumID: one,
		PID: model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE id = ?", one)), Matched: true,
		MBID: "f3000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{ReleaseGroupID: two,
		PID: model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE id = ?", two)), Matched: true,
		MBID: "f3000000-0000-4000-8000-000000000002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE playable_item SET state = 'missing' WHERE pid = ?", string(held.ItemPID)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET release_group_id = NULL WHERE id = ?", one); err != nil {
		t.Fatal(err)
	}
	files, err := st.EnrichedAlbumFiles(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FilePID != owed.FilePID || files[0].ReleaseGroupMBID == "" {
		t.Errorf("owed = %+v, want the other group's id owed to its file", files)
	}
}

// TestAUserRowNamingAnotherIDOwesNothing: the survivor of a merge holds the id its
// files name, and the loser's id, set by a user, came with the merge on a row of its
// own. The row names another id than the album holds, so it says nothing of the
// album's id: no file is owed it as a user's.
func TestAUserRowNamingAnotherIDOwesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	const tagged, set = "e6000000-0000-4000-8000-000000000001", "e6000000-0000-4000-8000-000000000002"
	spec := func(n, album, release string) trackSpec {
		return trackSpec{path: "/lib/Alpha/" + album + "/" + n + ".flac", essence: "e" + n, content: "c" + n, title: "T" + n,
			artist: "Alpha", albumArt: "Alpha", album: album, mbRelease: release, durationMS: 100}
	}
	survivor := putTrack(t, st, lib.ID, spec("1", "One", tagged))
	loser := putTrack(t, st, lib.ID, spec("2", "Two", ""))
	pidOf := func(item model.PID) model.PID {
		return model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(item)))
	}
	one, two := pidOf(survivor.ItemPID), pidOf(loser.ItemPID)
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, two, map[string]string{"mbid": set},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM file_diagnostic WHERE code = ?", string(model.DiagTagWriteOwed)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, one, two); err != nil {
		t.Fatal(err)
	}
	putTrack(t, st, lib.ID, spec("3", "One", ""))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE code = ? AND tag_key = ?",
		string(model.DiagTagWriteOwed), model.OwedAlbumMBID); n != 0 {
		t.Errorf("%d files owe the album's id as a user's, want none: the user's row names another id", n)
	}
}

// TestAMergeOwesAGainedIDToTheSurvivorsOwnMembers: a loser carries a user's id, which its
// files took, and the survivor has none. The merge hands the survivor the id, so the
// survivor's own members owe it, and the loser's, which carry it, owe nothing. The same
// holds for a release group merged into another.
func TestAMergeOwesAGainedIDToTheSurvivorsOwnMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	user := model.Attribution{Source: model.SourceUser}
	put := func(n, artist, album string) *model.ScanItemResult {
		return putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + artist + "/" + album + "/" + n + ".flac", essence: "e" + n,
			content: "c" + n, title: "T" + n, artist: artist, albumArt: artist, album: album, durationMS: 100})
	}
	of := func(res *model.ScanItemResult, col string) model.PID {
		return model.PID(scalarStr(t, st, `SELECT `+col+` FROM track t JOIN album al ON al.id = t.album_id
			LEFT JOIN release_group rg ON rg.id = al.release_group_id JOIN playable_item pi ON pi.id = t.item_id
			WHERE pi.pid = ?`, string(res.ItemPID)))
	}
	owes := func(res *model.ScanItemResult, key string) bool {
		t.Helper()
		keys, err := owedKeysOf(ctx, st, res.FilePID)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(keys, key)
	}
	for _, tc := range []struct {
		name string
		et   model.MergeEntity
		col  string
		key  string
		id   string
	}{
		{"album", model.MergeAlbum, "al.pid", model.OwedAlbumMBID, "e7000000-0000-4000-8000-000000000001"},
		{"group", model.MergeReleaseGroup, "rg.pid", model.OwedReleaseGroupMBID, "e7000000-0000-4000-8000-000000000002"},
	} {
		loser := put(tc.name+"1", "Lose"+tc.name, "Lost "+tc.name)
		survivor := put(tc.name+"2", "Keep"+tc.name, "Kept "+tc.name)
		if _, err := st.EditEntityFields(ctx, tc.et, of(loser, tc.col), map[string]string{"mbid": tc.id}, user, model.LockOn, false); err != nil {
			t.Fatal(err)
		}
		if err := st.SettleTagWriteOwed(ctx, loser.FilePID, []string{tc.key}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.MergeEntity(ctx, tc.et, of(survivor, tc.col), of(loser, tc.col)); err != nil {
			t.Fatal(err)
		}
		if !owes(survivor, tc.key) {
			t.Errorf("%s merge: the survivor's own member does not owe the id it gained", tc.name)
		}
		if owes(loser, tc.key) {
			t.Errorf("%s merge: the loser's member, which carries the id, owes it", tc.name)
		}
	}
}

// TestARefusalLiftsWhenTheFileChanges: the writer refused a member as it was, which holds
// its album's id from every member. Other bytes at the same path lift the hold, even under
// the same mtime, as a copy that keeps file times leaves them.
func TestARefusalLiftsWhenTheFileChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	spec := func(content string) trackSpec {
		return trackSpec{path: "/lib/Alpha/One/1.flac", essence: "e1", content: content, title: "T1",
			artist: "Alpha", albumArt: "Alpha", album: "One", durationMS: 100}
	}
	res := putTrack(t, st, lib.ID, spec("c1"))
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{AlbumID: int64(scalarInt(t, st, "SELECT id FROM album")),
		PID: albumPID(t, st), Matched: true, MBID: "e8000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	owed := func() int {
		t.Helper()
		files, err := st.EnrichedAlbumFiles(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(files)
	}
	// A refusal met on bytes the catalog has not read is not recorded against its hash.
	if err := st.NoteTagWriteRefused(ctx, res.FilePID, 99, 1); err != nil {
		t.Fatal(err)
	}
	if n := owed(); n != 1 {
		t.Fatalf("files owed after a refusal of bytes the catalog has not read = %d, want the one", n)
	}
	if err := st.NoteTagWriteRefused(ctx, res.FilePID, int64(len("c1")), 1); err != nil {
		t.Fatal(err)
	}
	if n := owed(); n != 0 {
		t.Fatalf("files owed while the writer's refusal stands = %d, want none", n)
	}
	putTrack(t, st, lib.ID, spec("c1-copied-keeping-times"))
	if n := owed(); n != 1 {
		t.Errorf("files owed once other bytes replaced the refused file = %d, want the one", n)
	}
}
