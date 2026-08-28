package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
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
	// artists is the split credit the scanner supplies alongside the raw artist
	// string. Leaving it nil is a pre-split caller, which the store re-splits.
	artists               []string
	preserveLocks         bool
	album, genre          string
	composer              string
	year                  int
	bpm                   int
	discTotal             int
	durationMS            int64
	compilation           bool
	mbRecording           string
	mbReleaseGroup        string
	mbRelease             string
	mbArtists             []string
	mbAlbumArtists        []string
	isrc                  string
	barcode, label, catNo string
	media, country        string
}

func putTrack(t *testing.T, st *Store, libID int64, s trackSpec) *model.ScanItemResult {
	t.Helper()
	idKey := "essence:" + s.essence
	if s.mbRecording != "" {
		idKey = "mbid:" + s.mbRecording
	}
	in := model.PutScannedTrackInput{
		LibraryID:     libID,
		PreserveLocks: s.preserveLocks,
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
			Artist: s.artist, Artists: s.artists, ArtistSort: model.SortKey(s.artist), Album: s.album,
			AlbumArtist: s.albumArt, Composer: s.composer, ComposerSort: model.SortKey(s.composer),
			Genre:            s.genre,
			Genres:           identity.SplitGenres(s.genre),
			Year:             s.year,
			BPM:              s.bpm,
			DiscTotal:        s.discTotal,
			Compilation:      s.compilation,
			MBID:             s.mbRecording,
			MBReleaseGroupID: s.mbReleaseGroup,
			MBReleaseID:      s.mbRelease,
			MBArtistIDs:      s.mbArtists,
			MBAlbumArtistIDs: s.mbAlbumArtists,
			ISRC:             s.isrc,
			Barcode:          s.barcode,
			Label:            s.label,
			CatalogNumber:    s.catNo,
			Media:            s.media,
			Country:          s.country,
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
	if _, err := st.SetStar(ctx, "", r1.ItemPID, true, nil); err != nil {
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

// TestArtistMBIDBackfill covers the late-arriving id: an artist row created before
// its files carried MUSICBRAINZ_ARTISTID never got one, however often the library was
// rescanned after a Picard pass. Each rescan here varies the content hash the way a
// retag does, since a byte-identical rescan skips entity resolution outright.
func TestArtistMBIDBackfill(t *testing.T) {
	st, lib := entityFixture(t)

	untagged := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer",
	}
	putTrack(t, st, lib.ID, untagged)
	if got := artistMBID(t, st, "Radiohead"); got != "" {
		t.Fatalf("artist mbid before the retag = %q, want empty", got)
	}

	tagged := untagged
	tagged.content = "c2"
	tagged.mbArtists, tagged.mbAlbumArtists = []string{"a1-mbid"}, []string{"a1-mbid"}
	putTrack(t, st, lib.ID, tagged)
	if got := artistMBID(t, st, "Radiohead"); got != "a1-mbid" {
		t.Fatalf("artist mbid after the retag = %q, want a1-mbid", got)
	}
	if n := artistChanges(t, st); n != 2 {
		t.Errorf("artist change_log rows = %d, want 2 (the create, then the backfill)", n)
	}

	// A later retag re-resolves the artist with the id already stored: no write, so
	// no delta. That is the standing invariant on every scan write path, and this
	// runs twice per scanned track.
	again := tagged
	again.content = "c3"
	putTrack(t, st, lib.ID, again)
	if n := artistChanges(t, st); n != 2 {
		t.Errorf("artist change_log rows after a no-op re-resolve = %d, want the same 2", n)
	}

	// A different id never displaces one already there: this fills gaps only.
	replacement := again
	replacement.content = "c4"
	replacement.mbArtists, replacement.mbAlbumArtists = []string{"a2-mbid"}, []string{"a2-mbid"}
	putTrack(t, st, lib.ID, replacement)
	if got := artistMBID(t, st, "Radiohead"); got != "a1-mbid" {
		t.Errorf("artist mbid after a conflicting tag = %q, want the stored a1-mbid", got)
	}
}

// TestArtistMBIDBackfillRespectsLock: a locked-empty mbid is a curated value, and the
// fill-when-empty WHERE clause alone would refill it.
func TestArtistMBIDBackfillRespectsLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer",
	}
	putTrack(t, st, lib.ID, spec)
	artistPID := entityPIDByName(t, st, "artist", "name", "Radiohead")
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"mbid": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("lock mbid empty: %v", err)
	}

	spec.content = "c2"
	spec.mbArtists, spec.mbAlbumArtists = []string{"a1-mbid"}, []string{"a1-mbid"}
	putTrack(t, st, lib.ID, spec)
	if got := artistMBID(t, st, "Radiohead"); got != "" {
		t.Errorf("locked-empty artist mbid = %q after a tagged rescan, want it left empty", got)
	}
}

func artistMBID(t *testing.T, st *Store, name string) string {
	t.Helper()
	var mbid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT COALESCE(mbid,'') FROM artist WHERE name = ?", name).Scan(&mbid); err != nil {
		t.Fatalf("read artist %q mbid: %v", name, err)
	}
	return mbid
}

func artistChanges(t *testing.T, st *Store) int {
	t.Helper()
	return scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE entity_type = 'artist'")
}

// TestAlbumIdentifierBackfill: barcode/label/catalog_number are not part of
// identity.AlbumKey, so a late tag pass hits the existing row and never reaches the
// insert that carries them.
func TestAlbumIdentifierBackfill(t *testing.T) {
	st, lib := entityFixture(t)

	untagged := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Airbag",
		artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer", year: 1997,
	}
	putTrack(t, st, lib.ID, untagged)
	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")

	tagged := untagged
	tagged.content = "c2"
	tagged.barcode, tagged.label, tagged.catNo = "724385522925", "Parlophone", "CDNODATA 02"
	putTrack(t, st, lib.ID, tagged)

	album, err := st.EntityByPID(context.Background(), read.EntityAlbum, albumPID)
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Barcode != "724385522925" || album.Label != "Parlophone" || album.CatalogNumber != "CDNODATA 02" {
		t.Fatalf("album identifiers after the retag = %q/%q/%q, want the tagged values",
			album.Barcode, album.Label, album.CatalogNumber)
	}
	if n := albumChanges(t, st); n != 2 {
		t.Errorf("album change_log rows = %d, want 2 (the create, then the backfill)", n)
	}

	replacement := tagged
	replacement.content = "c3"
	replacement.barcode, replacement.label, replacement.catNo = "036000291452", "Other", "OTHER-1"
	putTrack(t, st, lib.ID, replacement)
	album, err = st.EntityByPID(context.Background(), read.EntityAlbum, albumPID)
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Barcode != "724385522925" || album.Label != "Parlophone" {
		t.Errorf("stored identifiers were displaced by a later tag: %q/%q", album.Barcode, album.Label)
	}
	if n := albumChanges(t, st); n != 2 {
		t.Errorf("album change_log rows after a no-op re-resolve = %d, want the same 2", n)
	}
}

func albumChanges(t *testing.T, st *Store) int {
	t.Helper()
	return scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE entity_type = 'album'")
}

// TestMBIDAdoptionJoinsEnrichedEntity covers the resolve-time adoption: enrichment
// fills the album and release-group mbid columns without moving either match_key, so a
// file that later arrives tagged with those ids computes an mbid: key that matches
// nothing. It must join the rows already holding its identity, keeping their pids and
// everything attached to them, rather than forking a second pair.
func TestMBIDAdoptionJoinsEnrichedEntity(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album", year: 2001,
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='Album'"))
	rgPID := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title='Album'"))
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid=?", string(albumPID)))
	rgID := int64(scalarInt(t, st, "SELECT id FROM release_group WHERE pid=?", string(rgPID)))

	if _, err := st.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil {
		t.Fatalf("star album: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, tinyPNG(t), "image/png",
		model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"label": "Curated Label"}, model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("curate album: %v", err)
	}

	// Exactly what enrichment does: the column moves, the key does not. Uppercase on
	// the way in, so the folding on write is under test too.
	const rgMBID = "A1B2C3D4-1111-2222-3333-444455556666"
	const relMBID = "B2C3D4E5-1111-2222-3333-444455556666"
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: rgMBID,
	}); err != nil {
		t.Fatalf("enrich release group: %v", err)
	}
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: albumID, PID: albumPID, Matched: true, MBID: relMBID, Provider: "musicbrainz",
	}); err != nil {
		t.Fatalf("enrich album: %v", err)
	}
	if got := scalarStr(t, st, "SELECT mbid FROM album WHERE pid=?", string(albumPID)); got != strings.ToLower(relMBID) {
		t.Fatalf("album mbid = %q, want the lowercased id", got)
	}

	// A second file, in a folder of its own so no heuristic key could unify it, tagged
	// with the same ids in the casing MusicBrainz hands out.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Other Folder/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", year: 2001,
		mbReleaseGroup: strings.ToLower(rgMBID), mbRelease: strings.ToLower(relMBID),
	})

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count = %d, want 1 (the tagged file adopts the enriched row)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release_group count = %d, want 1", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM album"); got != string(albumPID) {
		t.Errorf("album pid = %s, want the original %s", got, albumPID)
	}
	if got := scalarStr(t, st, "SELECT pid FROM release_group"); got != string(rgPID) {
		t.Errorf("release_group pid = %s, want the original %s", got, rgPID)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id=?", albumID); n != 2 {
		t.Errorf("tracks on the album = %d, want 2", n)
	}
	// Everything hanging off the adopted row survived, which is the point of adopting
	// rather than forking.
	state, err := st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("read album state: %v", err)
	}
	if !state.Starred {
		t.Error("album star did not survive adoption")
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", albumID); n == 0 {
		t.Error("album art did not survive adoption")
	}
	if got := scalarStr(t, st, "SELECT COALESCE(label,'') FROM album WHERE id=?", albumID); got != "Curated Label" {
		t.Errorf("album label = %q, want the curated value to survive adoption", got)
	}
}

// TestMBIDAdoptionFoldsTagCasing pins the other half of the casing rule: the column
// holds the canonical lowercase id, and a file tagged with the uppercase spelling still
// adopts, because identity's keys lowercase before the probe compares.
func TestMBIDAdoptionFoldsTagCasing(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "c3d4e5f6-1111-2222-3333-444455556666"

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	albumPID := scalarStr(t, st, "SELECT pid FROM album WHERE title='Album'")
	if _, err := st.write.ExecContext(ctx, "UPDATE album SET mbid=? WHERE pid=?", relMBID, albumPID); err != nil {
		t.Fatal(err)
	}
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Elsewhere/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", mbRelease: strings.ToUpper(relMBID),
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count = %d, want 1 (an uppercase tag names the same release)", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM album"); got != albumPID {
		t.Errorf("album pid = %s, want the original %s", got, albumPID)
	}
}

// TestAdoptedMemberSurvivesAnEdit closes the other half of resolve-time adoption. A member
// joined to a heuristically keyed album by the id in its own file has no release id
// anywhere in the catalog except that album's column, and the edit path rebuilds a track's
// tags from the catalog. Reading only the match_key prefix left the member with no id, so
// re-resolution computed its own folder's heuristic key, missed, and forked it onto an
// album of its own on any entity-touching edit.
func TestAdoptedMemberSurvivesAnEdit(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const relMBID = "eeeeeeee-1111-2222-3333-444444444444"

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "Album",
	})
	albumPID := scalarStr(t, st, "SELECT pid FROM album")
	if _, err := st.write.ExecContext(ctx, "UPDATE album SET mbid=? WHERE pid=?", relMBID, albumPID); err != nil {
		t.Fatal(err)
	}
	// A member in another folder, joined to that album only through the id in its file.
	adopted := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Elsewhere/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "Album", mbRelease: relMBID,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album count = %d, want the adopted member on one album", n)
	}

	if err := st.EditItemFields(ctx, adopted.ItemPID, map[string]string{"genre": "Rock"},
		model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit the adopted member: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album count after the edit = %d, want the member to have stayed put", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM album"); got != albumPID {
		t.Errorf("album pid = %s, want the original %s", got, albumPID)
	}
	// And the escape hatch still works for it: detach is how one member comes off an
	// album an id ties it to, adopted or keyed.
	rep, err := st.DetachItemFromMBIDAlbum(ctx, adopted.ItemPID)
	if err != nil {
		t.Fatalf("detach the adopted member: %v", err)
	}
	if rep.NewAlbumPID == "" || rep.NewAlbumPID == rep.OldAlbumPID {
		t.Errorf("detach report = %+v, want the member on an album of its own", rep)
	}
}
