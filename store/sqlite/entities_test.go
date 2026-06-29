package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// entityFixture opens a store with one managed library for white-box assertions
// against the derived entity/rollup/FTS state.
func entityFixture(t *testing.T) (*Store, *model.Library) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return st, lib
}

type trackSpec struct {
	path, essence, content  string
	title, artist, albumArt string
	album, genre            string
	composer                string
	year                    int
	discTotal               int
	durationMS              int64
	compilation             bool
	mbRecording             string
	mbReleaseGroup          string
	mbRelease               string
}

func putTrack(t *testing.T, st *Store, libID int64, s trackSpec) *model.ScanItemResult {
	t.Helper()
	idKey := "essence:" + s.essence
	if s.mbRecording != "" {
		idKey = "mbid:" + s.mbRecording
	}
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(s.path), DisplayPath: s.path, RelPath: []byte(filepath.Base(s.path)),
			Kind: model.FileAudio, Size: int64(len(s.content)), MTimeNS: 1,
			ContentHash: s.content, EssenceHash: s.essence, DurationMS: s.durationMS,
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: s.title,
			SortKey: model.SortKey(s.title), IdentityKey: idKey,
		},
		Track: model.Track{
			Artist: s.artist, ArtistSort: model.SortKey(s.artist), Album: s.album,
			AlbumArtist: s.albumArt, Composer: s.composer, Genre: s.genre,
			Genres:           identity.SplitGenres(s.genre),
			Year:             s.year,
			DiscTotal:        s.discTotal,
			Compilation:      s.compilation,
			MBID:             s.mbRecording,
			MBReleaseGroupID: s.mbReleaseGroup,
			MBReleaseID:      s.mbRelease,
		},
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", s.path, err)
	}
	return res
}

func scalarInt(t *testing.T, st *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func TestEntityResolutionDedupes(t *testing.T) {
	st, lib := entityFixture(t)

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Radiohead", album: "OK Computer", genre: "Rock; Alternative",
		year: 1997, durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/02.flac", essence: "e2", content: "c2",
		title: "Paranoid Android", artist: "Radiohead", album: "OK Computer", genre: "Rock",
		year: 1997, durationMS: 200,
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Errorf("artist count = %d, want 1 (deduped by match key)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release_group count = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count = %d, want 1", n)
	}
	// "Rock; Alternative" + "Rock" => two distinct genres, Rock shared.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM genre"); n != 2 {
		t.Errorf("genre count = %d, want 2", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_genre"); n != 3 {
		t.Errorf("item_genre links = %d, want 3 (2+1)", n)
	}
}

// TestMBIDFirstReleaseGroupUnifies verifies that a shared MusicBrainz
// release-group id unifies two releases the heuristic key would have split
// (different titles/folders), while a different id keeps them separate.
func TestMBIDFirstReleaseGroupUnifies(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "A", artist: "Band",
		album: "Deluxe Edition", mbReleaseGroup: "rg-100", mbRelease: "rel-1",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "B", artist: "Band",
		album: "Original Pressing", mbReleaseGroup: "rg-100", mbRelease: "rel-2",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/c/3.flac", essence: "e3", content: "c3", title: "C", artist: "Band",
		album: "Other Work", mbReleaseGroup: "rg-200", mbRelease: "rel-3",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Errorf("release_group count = %d, want 2 (rg-100 unifies two, rg-200 separate)", n)
	}
	// The two MBID releases under rg-100 stay distinct albums; rg-200 is its own.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 3 {
		t.Errorf("album count = %d, want 3 (each MB release id is its own edition)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group WHERE mbid='rg-100'"); n != 1 {
		t.Errorf("rg-100 rows = %d, want 1 (mbid recorded)", n)
	}
}

// TestMusicColumnsPersist verifies the Gate B track columns round-trip.
// TestInconsistentDiscTotalNotFragmented verifies an album whose tracks carry
// inconsistent disc-total tags (some missing) is not split into multiple albums.
func TestInconsistentDiscTotalNotFragmented(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Set/d1t1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Box Set", discTotal: 2,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Set/d2t1.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Box Set", discTotal: 0, // missing disctotal tag
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count = %d, want 1 (inconsistent disc_total must not fragment)", n)
	}
}

func TestMusicColumnsPersist(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/v/1.flac", essence: "e1", content: "c1", title: "Aria",
		artist: "Various Artists", album: "Now That's Music", composer: "J.S. Bach",
		genre: "Classical", year: 1999, compilation: true,
	})
	var composer string
	var compilation int
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT composer, compilation FROM track LIMIT 1").Scan(&composer, &compilation); err != nil {
		t.Fatalf("read track columns: %v", err)
	}
	if composer != "J.S. Bach" {
		t.Errorf("composer = %q, want J.S. Bach", composer)
	}
	if compilation != 1 {
		t.Errorf("compilation = %d, want 1", compilation)
	}
}

// TestEssenceAlgoUpgradePreservesItem verifies that re-scanning a byte-identical
// file whose essence hash changed by algorithm upgrade keeps the same item,
// preserving its pid and per-user play state.
func TestEssenceAlgoUpgradePreservesItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.wav", essence: "old-essence", content: "c1", title: "Song",
		artist: "X", album: "Al",
	}
	r1 := putTrack(t, st, lib.ID, spec)

	// User state on the item (the thing that must survive the upgrade).
	if err := st.SetStar(ctx, "", r1.ItemPID, true); err != nil {
		t.Fatalf("star: %v", err)
	}

	// Re-scan: identical bytes (same content hash) but a new essence digest, as if
	// the essence algorithm changed. Identity key follows the new essence.
	spec.essence = "new-essence" // content "c1" unchanged
	r2 := putTrack(t, st, lib.ID, spec)

	if r2.ItemPID != r1.ItemPID {
		t.Errorf("item pid changed across an essence-algo upgrade: %s -> %s", r1.ItemPID, r2.ItemPID)
	}
	if r2.ItemCreated {
		t.Error("a new item was created; the existing one should have been re-keyed in place")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
		t.Errorf("item count = %d, want 1 (no orphan/duplicate)", n)
	}
	// The star (play_state) survived because the item was preserved.
	stt, err := st.PlayStateFor(ctx, "", r1.ItemPID)
	if err != nil || !stt.Starred {
		t.Errorf("play state lost across the upgrade: %+v (err %v)", stt, err)
	}
	// The item is now keyed by the new essence, so a future scan matches it.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item WHERE identity_key = 'essence:new-essence'"); n != 1 {
		t.Error("item was not re-keyed to the new essence")
	}
}

// TestReencodeStillRekeys verifies the upgrade-preservation path does not apply
// to a real re-encode, where the content hash changes.
func TestReencodeStillRekeys(t *testing.T) {
	st, lib := entityFixture(t)
	spec := trackSpec{path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First", artist: "A", album: "Al"}
	r1 := putTrack(t, st, lib.ID, spec)
	// A re-encode changes both content and essence.
	spec.content, spec.essence, spec.title = "c2", "e2", "Second"
	r2 := putTrack(t, st, lib.ID, spec)
	if r2.ItemPID == r1.ItemPID {
		t.Error("a genuine re-encode (content changed) should re-key to a new item")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
		t.Errorf("item count = %d, want 1 (old orphan deleted)", n)
	}
}

func TestGenreMatchKeyDedup(t *testing.T) {
	st, lib := entityFixture(t)
	// Two display variants of one genre must resolve to a single entity.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "A",
		artist: "X", album: "Alp", genre: "Hip-Hop",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "B",
		artist: "Y", album: "Bet", genre: "hip hop",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM genre WHERE facet='genre'"); n != 1 {
		t.Errorf("genre count = %d, want 1 (Hip-Hop == hip hop)", n)
	}
	var display string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT name FROM genre LIMIT 1").Scan(&display); err != nil {
		t.Fatal(err)
	}
	if display != "Hip-Hop" {
		t.Errorf("genre display = %q, want first-seen Hip-Hop", display)
	}
}

func TestRetagReplacesGenreLinks(t *testing.T) {
	st, lib := entityFixture(t)
	spec := trackSpec{
		path: "/lib/a/1.mp3", essence: "stable", content: "c1", title: "Song",
		artist: "X", album: "Alp", genre: "Rock; Pop",
	}
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_genre"); n != 2 {
		t.Fatalf("initial item_genre = %d, want 2", n)
	}
	// Retag (same essence/path) to a single different genre.
	spec.content = "c2"
	spec.genre = "Jazz"
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_genre"); n != 1 {
		t.Errorf("item_genre after retag = %d, want 1", n)
	}
	var name string
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT g.name FROM item_genre ig JOIN genre g ON g.id=ig.genre_id`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Jazz" {
		t.Errorf("linked genre after retag = %q, want Jazz", name)
	}
}

func TestSearchFTSMaintained(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Paranoid Android",
		artist: "Radiohead", album: "OK Computer", genre: "Rock",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts"); n != 1 {
		t.Fatalf("search_fts rows = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'paranoid'"); n != 1 {
		t.Errorf("FTS match 'paranoid' = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'radiohead'"); n != 1 {
		t.Errorf("FTS match 'radiohead' (artist column) = %d, want 1", n)
	}
}

func TestFTSRowRemovedWithItem(t *testing.T) {
	st, lib := entityFixture(t)
	// Same essence at two live paths would dedup to one item; instead re-key the
	// single file's essence so the prior item is orphaned and deleted.
	spec := trackSpec{path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First", artist: "A", album: "Alp"}
	putTrack(t, st, lib.ID, spec)
	spec.essence = "e2" // new identity for the same path -> old item orphaned
	spec.content = "c2"
	spec.title = "Second"
	putTrack(t, st, lib.ID, spec)
	// Exactly one item remains, and exactly one FTS row (no stale orphan).
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts"); n != 1 {
		t.Errorf("search_fts rows = %d, want 1 (orphan FTS cleaned)", n)
	}
}

func TestRefreshRollups(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/r/1.flac", essence: "e1", content: "c1", title: "T1",
		artist: "Radiohead", album: "OK Computer", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/r/2.flac", essence: "e2", content: "c2", title: "T2",
		artist: "Radiohead", album: "OK Computer", genre: "Rock", durationMS: 250,
	})
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	var tracks, rgs int
	var dur int64
	if err := st.read.QueryRowContext(ctx, `SELECT ar.track_count, ar.release_group_count, ar.total_duration_ms
		FROM artist_rollup ar JOIN artist a ON a.id=ar.artist_id WHERE a.name='Radiohead'`).
		Scan(&tracks, &rgs, &dur); err != nil {
		t.Fatalf("read artist_rollup: %v", err)
	}
	if tracks != 2 || rgs != 1 || dur != 350 {
		t.Errorf("artist_rollup = {tracks %d, rgs %d, dur %d}, want {2,1,350}", tracks, rgs, dur)
	}

	var gTracks int
	var gDur int64
	if err := st.read.QueryRowContext(ctx, `SELECT track_count, total_duration_ms
		FROM genre_rollup gr JOIN genre g ON g.id=gr.genre_id WHERE g.name='Rock'`).
		Scan(&gTracks, &gDur); err != nil {
		t.Fatalf("read genre_rollup: %v", err)
	}
	if gTracks != 2 || gDur != 350 {
		t.Errorf("genre_rollup Rock = {tracks %d, dur %d}, want {2,350}", gTracks, gDur)
	}
}

// TestNoopRescanStaysSilent verifies that entity resolution preserves the
// existing no-op rescan contract: an identical rescan must not emit change_log
// rows. New entities are the only entity-side deltas, and a no-op rescan creates
// none.
func TestNoopRescanStaysSilent(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Radiohead", album: "OK Computer", genre: "Rock; Pop",
	}
	putTrack(t, st, lib.ID, spec)
	seq1, _ := st.LatestChangeSeq(ctx)
	putTrack(t, st, lib.ID, spec) // byte-identical re-scan
	seq2, _ := st.LatestChangeSeq(ctx)
	if seq2 != seq1 {
		t.Fatalf("no-op rescan emitted %d change_log rows; want 0", seq2-seq1)
	}
}

// TestNoopRescanSkipsEntityWork verifies that a byte-identical rescan does not
// rebuild the FTS row or re-resolve entities, while a content change does. It
// deletes the FTS row, then checks that a no-op rescan leaves it gone but a
// content-changed rescan restores it.
func TestNoopRescanSkipsEntityWork(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "stable", content: "c1", title: "Song",
		artist: "Radiohead", album: "OK Computer", genre: "Rock",
	}
	putTrack(t, st, lib.ID, spec)
	if _, err := st.write.ExecContext(ctx, "DELETE FROM search_fts"); err != nil {
		t.Fatalf("delete fts: %v", err)
	}

	putTrack(t, st, lib.ID, spec) // byte-identical no-op
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts"); n != 0 {
		t.Errorf("no-op rescan rebuilt FTS (%d rows); entity work should be skipped", n)
	}

	spec.content = "c2" // content change -> entity resolution runs again
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts"); n != 1 {
		t.Errorf("content-changed rescan did not rebuild FTS (%d rows), want 1", n)
	}
}

func TestUntaggedAlbumNotGrouped(t *testing.T) {
	st, lib := entityFixture(t)
	// Two fully artist-less albums sharing a title must stay separate; a title-only
	// release-group key would collide them.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/x/1.mp3", essence: "e1", content: "c1", title: "T1",
		artist: "", albumArt: "", album: "Greatest Hits",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/y/2.mp3", essence: "e2", content: "c2", title: "T2",
		artist: "", albumArt: "", album: "Greatest Hits",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 0 {
		t.Errorf("artist-less albums were grouped into %d release_groups, want 0", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 0 {
		t.Errorf("artist-less albums created %d album rows, want 0", n)
	}
}

func TestNonAlbumSingleNotGrouped(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/loose/1.mp3", essence: "e1", content: "c1", title: "Loose",
		artist: "Someone", album: "", genre: "",
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 0 {
		t.Errorf("release_group count = %d, want 0 for a titleless single", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 0 {
		t.Errorf("album count = %d, want 0 for a titleless single", n)
	}
	// The artist is still resolved even without an album.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Errorf("artist count = %d, want 1", n)
	}
}
