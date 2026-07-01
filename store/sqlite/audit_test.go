package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestDuplicateArtistsByCollationKey(t *testing.T) {
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Weezer", album: "A",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "weezer band", album: "B",
	})
	// Enrichment resolved both heuristic rows to one MBID (a collision left for merge).
	if _, err := st.write.ExecContext(ctx, "UPDATE artist SET mbid='mb-weezer' WHERE name IN ('Weezer','weezer band')"); err != nil {
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One album with two tracks (folder a1) and one with a single track (folder a2),
	// same title/artist -> two album rows under one release group.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a1/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a1/2.flac", essence: "e2", content: "c2", title: "Two", artist: "A", albumArt: "A", album: "Hits"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a2/1.flac", essence: "e3", content: "c3", title: "Three", artist: "A", albumArt: "A", album: "Hits"})
	// Enrichment resolved both album rows to one release id (the collision merge fixes).
	if _, err := st.write.ExecContext(ctx, "UPDATE album SET mbid='mb-hits'"); err != nil {
		t.Fatal(err)
	}
	sets, err := st.DuplicateAlbums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Members) != 2 {
		t.Fatalf("want one duplicate-album set of 2, got %+v", sets)
	}
	// The survivor (first member) must be the album backing the most tracks, so a
	// merge re-points the fewest tracks and keeps the larger album's PID.
	if sets[0].Members[0].TrackCount != 2 {
		t.Errorf("survivor track count = %d, want 2 (the larger album); members=%+v",
			sets[0].Members[0].TrackCount, sets[0].Members)
	}
}

func TestSplitAlbumsDetectsFolderSplit(t *testing.T) {
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

func TestItemsMissingArt(t *testing.T) {
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A normal (analyzable) track with no loudness -> counts.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "A",
	})
	// A downloaded podcast episode lives in the internal podcast library, which the
	// analyze pass skips, so it must not be counted as missing ReplayGain. That
	// finding would be unfixable.
	res, err := st.write.ExecContext(ctx,
		"INSERT INTO library(pid, root, display_root, mode, profile, created_at) VALUES (?,?,?,'podcast','waxbin-native',1)",
		string(model.NewPID()), []byte("/pod"), "/pod")
	if err != nil {
		t.Fatal(err)
	}
	podLib, _ := res.LastInsertId()
	if _, err := st.write.ExecContext(ctx,
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
