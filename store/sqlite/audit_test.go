package sqlite

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

func TestDuplicateArtistsByCollationKey(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// "Beatles" and "The Beatles" fold to the same sort key (the "The"-strip) but
	// keep distinct match keys, so they are two rows the audit should pair.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "A",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "B",
	})
	sets, err := st.DuplicateArtists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sets {
		if s.Reason == "same collation key" && len(s.Members) == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a collation-key duplicate pair, got %+v", sets)
	}
}

func TestDuplicateArtistsByMBID(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Weezer", album: "A",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "weezer band", album: "B",
	})
	// Enrichment resolved both heuristic rows to one MBID (a collision left for merge).
	if _, err := st.wdb().ExecContext(ctx, "UPDATE artist SET mbid='mb-weezer' WHERE name IN ('Weezer','weezer band')"); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateArtists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mbidSet *model.DuplicateSet
	for i := range sets {
		if sets[i].Reason == "shared MBID" {
			mbidSet = &sets[i]
		}
	}
	if mbidSet == nil || len(mbidSet.Members) != 2 {
		t.Fatalf("expected a shared-MBID pair, got %+v", sets)
	}
	// Survivor (first member) should be the higher track count.
	if mbidSet.Members[0].TrackCount < mbidSet.Members[1].TrackCount {
		t.Errorf("survivor should have the most tracks, got %+v", mbidSet.Members)
	}
}

func TestDuplicateAlbumsSurvivorHasMostTracks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One album with two tracks (folder a1) and one with a single track (folder a2),
	// same title/artist -> two album rows under one release group.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a1/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a1/2.flac", essence: "e2", content: "c2", title: "Two", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a2/1.flac", essence: "e3", content: "c3", title: "Three", artist: "A", albumArt: "A", album: "Hits"})
	// Enrichment resolved both album rows to one release id (the collision merge fixes).
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET mbid='mb-hits'"); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 {
		t.Fatalf("want one duplicate-album set of 2, got %+v", sets)
	}
	// The pair shares a name too, and is reported once, by its MBID.
	if sets[0].Reason != "shared MBID" {
		t.Errorf("reason = %q, want shared MBID", sets[0].Reason)
	}
	// The survivor (first member) must be the album backing the most tracks, so a
	// merge re-points the fewest tracks and keeps the larger album's PID.
	if sets[0].Members[0].TrackCount != 2 {
		t.Errorf("survivor track count = %d, want 2 (the larger album); members=%+v",
			sets[0].Members[0].TrackCount, sets[0].Members)
	}
}

// TestDuplicateAlbumsByName: two albums carrying one folded title under one album artist,
// in different folders and with no MusicBrainz ids, are a duplicate set, the larger first.
func TestDuplicateAlbumsByName(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Hits/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Hits Again/1.flac", essence: "e2", content: "c2", title: "Two", artist: "A", albumArt: "A", album: "HITS!"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Hits Again/2.flac", essence: "e3", content: "c3", title: "Three", artist: "A", albumArt: "A", album: "HITS!"})
	// Another artist's album of the same title is no duplicate of either.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/Hits/1.flac", essence: "e4", content: "c4", title: "Four", artist: "B", albumArt: "B", album: "Hits"})
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 {
		t.Fatalf("sets = %+v, want one pair", sets)
	}
	if sets[0].Reason != "same title and album artist" || sets[0].EntityType != model.MergeAlbum {
		t.Errorf("set = %s (%q), want an album set for the same title and album artist", sets[0].EntityType, sets[0].Reason)
	}
	if sets[0].Members[0].TrackCount != 2 || sets[0].Members[1].TrackCount != 1 {
		t.Errorf("members = %+v, want the two-track album first", sets[0].Members)
	}
}

// TestDuplicateAlbumsByNameAcrossKeyedGroups: a name pair is found whether its release
// groups are keyed by name or by MusicBrainz id, which is a partly tagged album's split.
func TestDuplicateAlbumsByNameAcrossKeyedGroups(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Hits/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Hits (Tagged)/1.flac", essence: "e2", content: "c2", title: "Two", artist: "A", albumArt: "A", album: "Hits",
		mbReleaseGroup: "aaaaaaaa-0000-4000-8000-000000000001"})
	if n := countRows(t, st, "release_group"); n != 2 {
		t.Fatalf("release groups = %d, want the name-keyed one and the id-keyed one", n)
	}
	sets, err := st.DuplicateAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 || sets[0].Reason != "same title and album artist" {
		t.Fatalf("sets = %+v, want one name pair", sets)
	}
}

// TestDuplicateAlbumsByNameKeepsDistinctReleasesApart: albums that share a name but carry
// different MusicBrainz ids, of the release or of its group, are different releases (two
// "Greatest Hits" of one artist) and no duplicate, while an untagged third joins the
// larger of them.
func TestDuplicateAlbumsByNameKeepsDistinctReleasesApart(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/GH 1975/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "Greatest Hits",
		mbRelease: "bbbbbbbb-0000-4000-8000-000000000001"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/GH 1975/2.flac", essence: "e2", content: "c2", title: "Two", artist: "A", albumArt: "A", album: "Greatest Hits",
		mbRelease: "bbbbbbbb-0000-4000-8000-000000000001"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/GH 1990/1.flac", essence: "e3", content: "c3", title: "Three", artist: "A", albumArt: "A", album: "Greatest Hits",
		mbRelease: "bbbbbbbb-0000-4000-8000-000000000002"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/Live 1/1.flac", essence: "e4", content: "c4", title: "Four", artist: "B", albumArt: "B", album: "Live",
		mbReleaseGroup: "cccccccc-0000-4000-8000-000000000001"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/B/Live 2/1.flac", essence: "e5", content: "c5", title: "Five", artist: "B", albumArt: "B", album: "Live",
		mbReleaseGroup: "cccccccc-0000-4000-8000-000000000002"})
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Fatalf("sets = %+v, want none: every pair names two releases", sets)
	}

	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Greatest Hits/1.flac", essence: "e6", content: "c6", title: "Six", artist: "A", albumArt: "A", album: "Greatest Hits"})
	sets, err = st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 || sets[0].Members[0].TrackCount != 2 {
		t.Fatalf("sets = %+v, want the untagged album paired with the two-track release", sets)
	}
}

// trashItemFile moves one put track's file to the trash, archiving its item.
func trashItemFile(t *testing.T, st *Store, res *model.ScanItemResult) {
	t.Helper()
	path := []byte("/trash/" + string(res.FilePID) + ".flac")
	if _, err := st.TrashFile(context.Background(), model.TrashFileInput{
		FilePID: res.FilePID, TrashPath: path, TrashDisplay: string(path)}); err != nil {
		t.Fatalf("trash: %v", err)
	}
}

// replacedRipFixture is a rip of Hits whose three tracks were trashed, beside a new rip of
// it in another folder with two live tracks and a third rip of it with one trashed track
// and one live one.
func replacedRipFixture(t *testing.T, st *Store, libID int64) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		trashItemFile(t, st, putTrack(t, st, libID, trackSpec{path: "/lib/A/Hits/" + n + ".flac", essence: "old" + n, content: "oc" + n,
			title: "Old " + n, artist: "A", albumArt: "A", album: "Hits"}))
	}
	for i := 1; i <= 2; i++ {
		n := strconv.Itoa(i)
		putTrack(t, st, libID, trackSpec{path: "/lib/A/Hits [FLAC]/" + n + ".flac", essence: "new" + n, content: "nc" + n,
			title: "New " + n, artist: "A", albumArt: "A", album: "Hits"})
	}
	trashItemFile(t, st, putTrack(t, st, libID, trackSpec{path: "/lib/A/Hits (2)/1.flac", essence: "half1", content: "hc1",
		title: "Half 1", artist: "A", albumArt: "A", album: "Hits"}))
	putTrack(t, st, libID, trackSpec{path: "/lib/A/Hits (2)/2.flac", essence: "half2", content: "hc2",
		title: "Half 2", artist: "A", albumArt: "A", album: "Hits"})
}

// TestDuplicateAlbumsByNameCountLiveMembers: a trashed member is no part of its album, so
// an album whose members are all trashed is no duplicate, and a set ranks its albums by
// the members they still hold.
func TestDuplicateAlbumsByNameCountLiveMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	replacedRipFixture(t, st, lib.ID)
	sets, err := st.DuplicateAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 {
		t.Fatalf("sets = %+v, want one pair without the trashed rip", sets)
	}
	if got := sets[0].Members; got[0].TrackCount != 2 || got[1].TrackCount != 1 {
		t.Errorf("members = %+v, want the live rip (2) first and the half-trashed one (1)", got)
	}
	trashed := scalarStr(t, st, "SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id JOIN playable_item pi ON pi.id = t.item_id WHERE pi.title = 'Old 1'")
	for _, m := range sets[0].Members {
		if string(m.PID) == trashed {
			t.Errorf("set names the trashed rip %s", trashed)
		}
	}
}

// TestSplitAlbumsCountLiveMembers: the split check reads the same way, so a trashed rip
// splits nothing and the live rip leads.
func TestSplitAlbumsCountLiveMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	replacedRipFixture(t, st, lib.ID)
	splits, err := st.SplitAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(splits) != 1 || len(splits[0].Albums) != 2 {
		t.Fatalf("splits = %+v, want one split of the two live rips", splits)
	}
	if got := splits[0].Albums; got[0].TrackCount != 2 || got[1].TrackCount != 1 {
		t.Errorf("albums = %+v, want the live rip (2) first", got)
	}
}

// TestDuplicateAlbumsByMBIDPutsTheLiveAlbumFirst: an id pair ranks its albums by live
// members too, so a merge keeps the album still in use.
func TestDuplicateAlbumsByMBIDPutsTheLiveAlbumFirst(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	replacedRipFixture(t, st, lib.ID)
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET mbid = 'eeeeeeee-0000-4000-8000-000000000001' WHERE match_key LIKE '%hits flac' OR match_key LIKE '%a hits'"); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) == 0 || sets[0].Reason != "shared MBID" || len(sets[0].Members) != 2 {
		t.Fatalf("sets = %+v, want the id pair first", sets)
	}
	if got := sets[0].Members; got[0].TrackCount != 2 || got[1].TrackCount != 0 {
		t.Errorf("members = %+v, want the live rip (2) as survivor over the trashed one (0)", got)
	}
}

// TestDuplicateAlbumsNameSetLeavesOutTheIDPairsLoser: an id pair and an untagged album of
// the same name are two findings that never name the same loser, so merging the id pair
// leaves the name finding naming albums that still exist.
func TestDuplicateAlbumsNameSetLeavesOutTheIDPairsLoser(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, folder := range []string{"/lib/A/Hits", "/lib/A/Hits", "/lib/A/Hits (CD)", "/lib/A/Hits (Rip)"} {
		n := strconv.Itoa(i + 1)
		putTrack(t, st, lib.ID, trackSpec{path: folder + "/" + n + ".flac", essence: "o" + n, content: "oc" + n,
			title: "T" + n, artist: "A", albumArt: "A", album: "Hits"})
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET mbid = 'ffffffff-0000-4000-8000-000000000001' WHERE match_key NOT LIKE '%rip'"); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 || sets[0].Reason != "shared MBID" || sets[1].Reason != model.ReasonSameAlbumName {
		t.Fatalf("sets = %+v, want the id pair and one name set", sets)
	}
	survivor, loser := sets[0].Members[0].PID, sets[0].Members[1].PID
	byName := sets[1].Members
	if len(byName) != 2 || byName[0].PID != survivor {
		t.Errorf("name set = %+v, want the id pair's survivor %s and the untagged album", byName, survivor)
	}
	for _, m := range byName {
		if m.PID == loser {
			t.Errorf("name set names the id pair's loser %s, which a merge of the pair deletes", loser)
		}
	}
}

// TestDuplicateAlbumsByNameReadsAGroupIDFromItsColumn: an album keyed by its release id
// under a name-keyed group whose id enrichment filled in the column is under that group,
// so it is no duplicate of a same-named album under another group's id.
func TestDuplicateAlbumsByNameReadsAGroupIDFromItsColumn(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/GH/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", albumArt: "A", album: "Greatest Hits", mbRelease: "bbbbbbbb-0000-4000-8000-000000000009"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/GH Two/1.flac", essence: "e2", content: "c2", title: "Two",
		artist: "A", albumArt: "A", album: "Greatest Hits", mbReleaseGroup: "cccccccc-0000-4000-8000-000000000009"})
	if _, err := st.wdb().ExecContext(ctx, `UPDATE release_group SET mbid = 'cccccccc-0000-4000-8000-000000000008'
		WHERE match_key NOT LIKE 'mbid:%'`); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Errorf("sets = %+v, want none: the two sit under different release groups", sets)
	}
}

func TestSplitAlbumsDetectsFolderSplit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Same album by the same artist across two folders -> two album rows (the album
	// key embeds the folder), a split.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/PF/Wall_D1/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Pink Floyd", albumArt: "Pink Floyd", album: "The Wall",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/PF/Wall_D2/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Pink Floyd", albumArt: "Pink Floyd", album: "The Wall",
	})
	splits, err := st.SplitAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(splits) != 1 || len(splits[0].Albums) != 2 {
		t.Fatalf("want one split with two albums, got %+v", splits)
	}
	if splits[0].Title != "The Wall" || splits[0].Artist != "Pink Floyd" {
		t.Errorf("split fields = %q by %q", splits[0].Title, splits[0].Artist)
	}
}

func TestInconsistentAlbumsCompilationFlag(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two tracks in ONE album (same folder/artist/year) with a mismatched
	// compilation flag.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/V/al/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", albumArt: "VA", album: "Mix", year: 2000, compilation: true,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/V/al/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "B", albumArt: "VA", album: "Mix", year: 2000, compilation: false,
	})
	issues, err := st.InconsistentAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("want 1 inconsistent album, got %d: %+v", len(issues), issues)
	}
	if issues[0].Problem == "" {
		t.Error("expected a non-empty problem description")
	}
}

// TestInconsistentAlbumsYear: the year keys no album, so members disagreeing on it share
// one album, and that album reports the disagreement.
func TestInconsistentAlbumsYear(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, year := range []int{2015, 2015, 2016} {
		n := string(rune('1' + i))
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/AP/Malibu/" + n + ".flac", essence: "m" + n, content: "c" + n, title: "T" + n,
			artist: "Anderson .Paak", albumArt: "Anderson .Paak", album: "Malibu", year: year,
		})
	}
	issues, err := st.InconsistentAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Title != "Malibu" {
		t.Fatalf("issues = %+v, want one for Malibu", issues)
	}
	if issues[0].Problem != "2 distinct years" {
		t.Errorf("problem = %q, want %q", issues[0].Problem, "2 distinct years")
	}
}

// TestInconsistentAlbumsRepeatedTrackNumbers: two same-titled records by one artist kept
// in one folder are one album now that the year keys nothing, and their track numbers,
// each claimed twice on one disc, are what says so. Another disc's track 1 repeats
// nothing, a member with no disc counts as disc 1, and a trashed member claims nothing.
func TestInconsistentAlbumsRepeatedTrackNumbers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, tr := range []struct{ no, disc, year int }{
		{1, 0, 1994}, {2, 0, 1994}, {1, 1, 2001}, {2, 0, 2001}, {1, 2, 1994}, {3, 0, 1994}, {3, 0, 2001},
	} {
		n := strconv.Itoa(i + 1)
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Weezer/" + n + ".flac", essence: "w" + n, content: "wc" + n,
			title: "Song " + n, artist: "Weezer", albumArt: "Weezer", album: "Weezer", year: tr.year, trackNo: tr.no, discNo: tr.disc})
		if i == 6 {
			trashItemFile(t, st, res)
		}
	}
	if n := countRows(t, st, "album"); n != 1 {
		t.Fatalf("albums = %d, want the two records on one album", n)
	}
	issues, err := st.InconsistentAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want one for Weezer", issues)
	}
	if issues[0].RepeatedPositions != 2 || issues[0].Problem != "2 distinct years, 2 repeated track numbers" {
		t.Errorf("issue = %+v, want 2 repeated track numbers beside the 2 years", issues[0])
	}
}

// TestInconsistentAlbumsIgnoreTrashedMembers: a trashed member's year is no part of its
// album, so it reports no disagreement.
func TestInconsistentAlbumsIgnoreTrashedMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, year := range []int{2015, 2015, 2016} {
		n := strconv.Itoa(i + 1)
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/AP/Malibu/" + n + ".flac", essence: "m" + n, content: "c" + n, title: "T" + n,
			artist: "Anderson .Paak", albumArt: "Anderson .Paak", album: "Malibu", year: year, trackNo: i + 1})
		if year == 2016 {
			trashItemFile(t, st, res)
		}
	}
	issues, err := st.InconsistentAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %+v, want none once the odd track is trashed", issues)
	}
}

func TestItemsMissingArt(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Coverless", artist: "X", album: "A",
	})
	items, total, err := st.ItemsMissingArt(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("missing-art total=%d sample=%d, want 1/1", total, len(items))
	}
	if items[0].Title != "Coverless" {
		t.Errorf("missing-art item = %q", items[0].Title)
	}
}

func TestCountItemsMissingReplayGain(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "A",
	})
	n, err := st.CountItemsMissingReplayGain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("missing-RG count = %d, want 1 (unanalyzed)", n)
	}
}

func TestCountItemsMissingReplayGainExcludesPodcasts(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A normal (analyzable) track with no loudness -> counts.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "A",
	})
	// A downloaded podcast episode lives in the internal podcast library, which the
	// analyze pass skips, so it must not be counted as missing ReplayGain. That
	// finding would be unfixable.
	res, err := st.wdb().ExecContext(ctx,
		"INSERT INTO library(pid, root, display_root, mode, profile, created_at) VALUES (?,?,?,'podcast','waxbin-native',1)",
		string(model.NewPID()), []byte("/pod"), "/pod")
	if err != nil {
		t.Fatal(err)
	}
	podLib, _ := res.LastInsertId()
	if _, err := st.wdb().ExecContext(ctx,
		`INSERT INTO file(pid, library_id, path, display_path, rel_path, kind, size, mtime_ns,
			content_hash, essence_hash, scan_state, first_seen, last_seen)
		 VALUES (?,?,?,?,?, 'audio', 1, 1, 'pc', 'pe', 'indexed', 1, 1)`,
		string(model.NewPID()), podLib, []byte("/pod/ep.mp3"), "/pod/ep.mp3", []byte("ep.mp3")); err != nil {
		t.Fatal(err)
	}

	n, err := st.CountItemsMissingReplayGain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("missing-RG count = %d, want 1 (the podcast episode is excluded)", n)
	}
}

func TestAuditFilesReturnsRows(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "A",
	})
	files, err := st.AuditFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Kind != model.FileAudio || files[0].ContentHash != "c1" || files[0].ItemPID == "" {
		t.Errorf("file row = %+v", files[0])
	}
}

// TestAuditFilesYieldsOneRowPerSharedRip: a single-file rip is backed by one primary
// edge per virtual track, and these are FILE-level checks, so it must still come back
// once. An ungated join returned it N times, which made the path-conflict check report
// the file as colliding with itself at error severity (a non-zero exit for anyone with
// a rip) and made integrity re-hash the same bytes once per track.
func TestAuditFilesYieldsOneRowPerSharedRip(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	// One rip carved into three virtual tracks: three primary edges onto one file.
	tracks := make([]model.VirtualTrack, 3)
	for i, w := range [][2]int64{{0, 9}, {9, 27}, {27, 0}} {
		n := i + 1
		title := fmt.Sprintf("R%d", n)
		tracks[i] = model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: title,
				SortKey: model.SortKey(title), IdentityKey: identity.VirtualTrackKey("eRip", n, w[0]),
			},
			Track:       model.Track{Artist: "Band", AlbumArtist: "Band", Album: "Rip", TrackNo: n},
			StartFrames: w[0], EndFrames: w[1],
		}
	}
	if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/rip.flac"), DisplayPath: "/lib/rip.flac", RelPath: []byte("rip.flac"),
			Kind: model.FileAudio, Size: 5, MTimeNS: 1, ContentHash: "cRip", EssenceHash: "eRip",
			DurationMS: 360, ScanState: model.ScanIndexed,
		},
		Tracks: tracks,
	}); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	// A plain whole-file track alongside it, to pin that the gate did not cost the
	// ordinary case its owning item.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/plain.flac", essence: "e2", content: "c2", title: "Plain", artist: "X", album: "A",
	})

	files, err := st.AuditFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("audit files = %d, want 2 (the rip counts once despite backing 3 items): %+v",
			len(files), files)
	}
	byPath := make(map[string]model.AuditFileInfo, len(files))
	for _, f := range files {
		byPath[f.DisplayPath] = f
	}
	rip, ok := byPath["/lib/rip.flac"]
	if !ok {
		t.Fatal("the rip's file is missing from the audit entirely; the gate must not drop it")
	}
	// No single item owns a shared rip, so the audit names none rather than picking an
	// arbitrary sibling to pin a finding on.
	if rip.ItemPID != "" {
		t.Errorf("rip owning item = %q, want empty (3 virtual tracks share it; none owns it)", rip.ItemPID)
	}
	if rip.ContentHash != "cRip" {
		t.Errorf("rip content hash = %q, want cRip (the integrity check needs it)", rip.ContentHash)
	}
	if plain := byPath["/lib/plain.flac"]; plain.ItemPID == "" {
		t.Error("a whole-file track lost its owning item to the gate")
	}
}

// TestItemsMissingMBID pins the chain the predicate walks. A track need not carry its
// own recording id to count as covered: its album's release id or its release group's
// id also resolves it, the way the missing-art predicate walks its own fallback chain.
func TestItemsMissingMBID(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Own Recording ID", artist: "A", albumArt: "A", album: "Al1", mbRecording: "rec-1"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Album Release ID", artist: "B", albumArt: "B", album: "Al2", mbRelease: "rel-2"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "Group ID Only", artist: "C", albumArt: "C", album: "Al3", mbReleaseGroup: "rg-3"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/4.flac", essence: "e4", content: "c4",
		title: "Bare Track", artist: "D", albumArt: "D", album: "Al4"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b1.m4b", essence: "be1", content: "bc1",
		title: "Tagged Book", author: "Auth One", mbid: "rel-book"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b2.m4b", essence: "be2", content: "bc2",
		title: "Bare Book", author: "Auth Two"})
	// An episode is never reported: a podcast's identity is a feed GUID.
	putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")

	items, total, err := st.ItemsMissingMBID(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("missing-mbid total=%d sample=%d, want 2/2", total, len(items))
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.Title] = true
	}
	if !got["Bare Track"] || !got["Bare Book"] {
		t.Errorf("missing-mbid items = %v, want the bare track and the bare book", got)
	}

	// The sample is capped while the total still counts everything.
	sample, total, err := st.ItemsMissingMBID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) != 1 || total != 2 {
		t.Errorf("limited call = %d sampled of %d, want 1 of 2", len(sample), total)
	}
}

// TestDuplicateEntitiesByEffectiveMBID builds the split the resolve-time adoption
// exists to prevent, one row holding the id in its column and its twin holding it in an
// mbid: match_key, and asserts both finders group them. Grouping on the column alone
// reports nothing here, which is the whole reason the effective-id expression exists.
func TestDuplicateEntitiesByEffectiveMBID(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "11111111-2222-3333-4444-555555555555"
	const relMBID = "66666666-7777-8888-9999-aaaaaaaaaaaa"

	// The heuristic pair enrichment later stamped.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	if _, err := st.wdb().ExecContext(ctx,
		"UPDATE album SET mbid=? WHERE title='Album'", relMBID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx,
		"UPDATE release_group SET mbid=? WHERE title='Album'", rgMBID); err != nil {
		t.Fatal(err)
	}
	// The mbid-keyed twin a pre-adoption build would have forked.
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO release_group(pid, title, sort_key, type, match_key)
		VALUES ('rg-twin','Album','album','album',?)`, "mbid:"+rgMBID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO album(pid, title, sort_key, match_key)
		VALUES ('al-twin','Album','album',?)`, "mbid:"+relMBID); err != nil {
		t.Fatal(err)
	}

	albums, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || len(albums[0].Members) != 2 {
		t.Fatalf("DuplicateAlbums = %+v, want one pair", albums)
	}
	if albums[0].EntityType != model.MergeAlbum {
		t.Errorf("album set type = %q, want album", albums[0].EntityType)
	}
	// Survivor first: the row backing a track, not the childless twin.
	if albums[0].Members[0].PID == "al-twin" {
		t.Errorf("survivor = %s, want the album holding the track", albums[0].Members[0].PID)
	}

	groups, err := st.DuplicateReleaseGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("DuplicateReleaseGroups = %+v, want one pair", groups)
	}
	if groups[0].EntityType != model.MergeReleaseGroup {
		t.Errorf("group set type = %q, want release_group", groups[0].EntityType)
	}
	if groups[0].Members[0].PID == "rg-twin" {
		t.Errorf("survivor = %s, want the group holding the album", groups[0].Members[0].PID)
	}
}

// TestDuplicateFindersIgnoreDistinctIDs guards the effective-id expression against
// pairing rows that merely both lack an id, which a COALESCE to ” would do.
func TestDuplicateFindersIgnoreDistinctIDs(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "One", artist: "A", album: "One",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/B/Two/02.flac", essence: "e2", content: "c2", title: "Two", artist: "B", album: "Two",
	})
	albums, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := st.DuplicateReleaseGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 0 || len(groups) != 0 {
		t.Errorf("unenriched rows reported as duplicates: albums=%+v groups=%+v", albums, groups)
	}
}
