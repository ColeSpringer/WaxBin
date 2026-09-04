package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// openStoreAt is like openTestStore but returns the DB path so a test can open a
// read-only connection for assertion queries.
func openStoreAt(t *testing.T) (*sqlite.Store, string, *model.Library) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: dbPath, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return st, dbPath, lib
}

func roConn(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func trackWithArtist(libID int64, path, essence, artist, mbArtistID string) model.PutScannedTrackInput {
	return model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + essence,
			SortKey: model.SortKey("T-" + essence), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: artist, AlbumArtist: artist, MBArtistIDs: []string{mbArtistID}, TrackNo: 1},
	}
}

// TestApplyArtistEnrichmentRelationDirection checks that an inbound relation is
// stored member -> band, the opposite orientation from a naive src=enriched edge.
func TestApplyArtistEnrichmentRelationDirection(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)

	// The band (enriched here) and a member already in the catalog with an MBID.
	if _, err := st.PutScannedTrack(ctx, trackWithArtist(lib.ID, "/lib/a.mp3", "ess-a", "Pink Floyd", "")); err != nil {
		t.Fatalf("seed band: %v", err)
	}
	if _, err := st.PutScannedTrack(ctx, trackWithArtist(lib.ID, "/lib/b.mp3", "ess-b", "David Gilmour", "gilmour-mbid")); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	targets, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("ArtistsNeedingEnrichment: %v", err)
	}
	var band, member model.EnrichTarget
	for _, tg := range targets {
		switch tg.Name {
		case "Pink Floyd":
			band = tg
		case "David Gilmour":
			member = tg
		}
	}
	if band.ID == 0 || member.ID == 0 {
		t.Fatalf("missing seeded artists: band=%+v member=%+v", band, member)
	}

	// Enrich the BAND with an inbound "member of band" relation to the member. It
	// must be stored member -> band.
	err = st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{
		ArtistID: band.ID, PID: band.PID, Matched: true, MBID: "pf-mbid",
		Relations: []model.ArtistRelationInput{
			{TargetMBID: "gilmour-mbid", Kind: model.RelationMemberOf, Inbound: true},
		},
	})
	if err != nil {
		t.Fatalf("ApplyArtistEnrichment: %v", err)
	}

	db := roConn(t, dbPath)
	var srcID, dstID int64
	err = db.QueryRow(`SELECT src_id, dst_id FROM artist_relation WHERE kind='member_of'`).Scan(&srcID, &dstID)
	if err != nil {
		t.Fatalf("read artist_relation: %v", err)
	}
	if srcID != member.ID || dstID != band.ID {
		t.Fatalf("relation stored src=%d dst=%d, want member(%d) -> band(%d)", srcID, dstID, member.ID, band.ID)
	}
}

// TestEntityEnrichmentClearedOnItemDelete checks that deleting an item (here by
// re-keying its file onto a new track item, which orphans the old book item) drops
// the book's polymorphic entity_enrichment marker, so a reused rowid cannot inherit
// a stale "already enriched" state.
func TestEntityEnrichmentClearedOnItemDelete(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)

	// Seed a single-file book at /lib/book.m4b.
	bookIn := model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/book.m4b"), DisplayPath: "/lib/book.m4b", RelPath: []byte("book.m4b"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-book1", EssenceHash: "ess-book1", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "A Book",
			SortKey: model.SortKey("A Book"), IdentityKey: "book:a book",
		},
		Book: model.Book{Authors: []string{"An Author"}},
	}
	res, err := st.PutScannedBook(ctx, bookIn)
	if err != nil {
		t.Fatalf("PutScannedBook: %v", err)
	}

	db := roConn(t, dbPath)
	var itemID int64
	if err := db.QueryRow("SELECT id FROM playable_item WHERE pid=?", string(res.ItemPID)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item id: %v", err)
	}

	// Mark the book enriched (creates the polymorphic entity_enrichment('book') row).
	if err := st.ApplyBookEnrichment(ctx, model.BookEnrichment{BookItemID: itemID, PID: res.ItemPID, Matched: true, MBID: "rel-x"}); err != nil {
		t.Fatalf("ApplyBookEnrichment: %v", err)
	}
	// The fields walk's marker is keyed by the same item id, so it has to go too.
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: res.ItemPID}); err != nil {
		t.Fatalf("ApplyItemFields: %v", err)
	}
	if n := countEE(t, db, itemID); n != 2 {
		t.Fatalf("marker rows before delete = %d, want 2 (book + fields)", n)
	}

	// Re-scan the SAME path as a track with a different essence: the file re-keys to
	// a new track item, orphaning the book item, which deleteItemCascade removes.
	if _, err := st.PutScannedTrack(ctx, trackWithArtist(lib.ID, "/lib/book.m4b", "ess-track2", "Someone", "")); err != nil {
		t.Fatalf("re-key scan: %v", err)
	}
	if n := countEE(t, db, itemID); n != 0 {
		t.Fatalf("marker rows after item delete = %d, want 0 (orphan not cleaned)", n)
	}
}

// countEE counts every item-keyed enrichment marker on one item, so the cascade test
// covers each marker a reused rowid could inherit rather than only the book's.
func countEE(t *testing.T, db *sql.DB, itemID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entity_enrichment
		WHERE entity_type IN ('book','lyrics','fields') AND entity_id=?`, itemID).Scan(&n); err != nil {
		t.Fatalf("count entity_enrichment: %v", err)
	}
	return n
}

// scopeTrack persists one track with a distinct artist and album artist so the
// item scope resolver has two artists to collect.
func scopeTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, artist, albumArtist, album string) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: artist, AlbumArtist: albumArtist, Album: album, TrackNo: 1},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// The album release match works in UUIDs, so its fixtures do too.
const (
	relTestRGMBID  = "b0000000-0000-4000-8000-000000000002"
	relTestOneMBID = "c0000000-0000-4000-8000-000000000003"
	relTestTwoMBID = "d0000000-0000-4000-8000-000000000004"
)

// albumTrack persists one track whose album carries release identifiers. Each album
// gets its own folder, since the album match key embeds it and these would otherwise
// collapse into one row under their shared release group.
func albumTrack(t *testing.T, st *sqlite.Store, libID int64, essence, album, barcode, catNo string) {
	t.Helper()
	path := "/lib/" + essence + "/1.mp3"
	_, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + essence,
			SortKey: model.SortKey("T-" + essence), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{
			Artist: "PF", AlbumArtist: "PF", Album: album, TrackNo: 1,
			MBReleaseGroupID: relTestRGMBID, Barcode: barcode, CatalogNumber: catNo,
		},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
}

func albumIDByTitle(t *testing.T, db *sql.DB, title string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow("SELECT id FROM album WHERE title = ?", title).Scan(&id); err != nil {
		t.Fatalf("no album titled %q: %v", title, err)
	}
	return id
}

func setEntityMBID(t *testing.T, st *sqlite.Store, et model.MergeEntity, pid, mbid string, lock bool) {
	t.Helper()
	if _, err := st.EditEntityFields(context.Background(), et, model.PID(pid),
		map[string]string{"mbid": mbid}, model.Attribution{Source: model.SourceUser}, model.LockOf(lock), false); err != nil {
		t.Fatalf("set %s mbid: %v", et, err)
	}
}

// TestAlbumsNeedingReleaseMatchGatesOnIdentifiers pins the four-part queue gate: an
// album is queued only when it has no mbid of its own, its release group has one, and
// it carries a barcode or a catalog number.
func TestAlbumsNeedingReleaseMatchGatesOnIdentifiers(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	albumTrack(t, st, lib.ID, "ess-a", "Has Barcode", "0075992739429", "")
	albumTrack(t, st, lib.ID, "ess-b", "Has CatNo", "", "SHVL 804")
	albumTrack(t, st, lib.ID, "ess-c", "Has Neither", "", "")

	queued, err := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	got := map[string]bool{}
	for _, q := range queued {
		got[q.Name] = true
		if q.ReleaseGroupMBID != relTestRGMBID {
			t.Errorf("queued %q under group %q, want %s", q.Name, q.ReleaseGroupMBID, relTestRGMBID)
		}
	}
	if !got["Has Barcode"] || !got["Has CatNo"] || got["Has Neither"] {
		t.Errorf("queued albums = %v, want the two carrying an identifier", got)
	}

	// An album that already has a release id drops out: entity MBIDs fill only when
	// empty, so there is nothing left for a match to write.
	setEntityMBID(t, st, model.MergeAlbum,
		scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Has Barcode'"), relTestOneMBID, false)
	queued, err = st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if len(queued) != 1 || queued[0].Name != "Has CatNo" {
		t.Errorf("queued = %+v, want only Has CatNo", queued)
	}

	// Clearing the shared group's mbid leaves nothing to constrain a search to.
	setEntityMBID(t, st, model.MergeReleaseGroup,
		scalarQueryStr(t, db, "SELECT pid FROM release_group LIMIT 1"), "", false)
	queued, err = st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if len(queued) != 0 {
		t.Errorf("queued = %+v, want none", queued)
	}
}

// TestAlbumReleaseMatchRespectsLockAndDuplicate covers the two refusals the apply
// shares with setReleaseGroupMBIDTx: a curated (locked) mbid keeps, and an id another
// album already holds is left for the merge primitive rather than duplicated. The
// marker is still recorded in both cases, so neither is re-searched every run.
func TestAlbumReleaseMatchRespectsLockAndDuplicate(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	albumTrack(t, st, lib.ID, "ess-a", "Locked", "0075992739429", "")
	albumTrack(t, st, lib.ID, "ess-b", "Taken", "5099902154251", "")
	albumTrack(t, st, lib.ID, "ess-c", "Holder", "", "SHVL 804")

	lockedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Locked'")
	takenPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Taken'")
	holderPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Holder'")

	// A lock on a deliberately empty value: the fill-when-empty WHERE alone would
	// refill it, so only the lock probe keeps it.
	setEntityMBID(t, st, model.MergeAlbum, lockedPID, "", true)
	setEntityMBID(t, st, model.MergeAlbum, holderPID, relTestOneMBID, false)

	for _, tc := range []struct {
		name, pid, mbid string
	}{
		{"Locked", lockedPID, relTestTwoMBID},
		{"Taken", takenPID, relTestOneMBID}, // already held by Holder
	} {
		id := albumIDByTitle(t, db, tc.name)
		err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
			AlbumID: id, PID: model.PID(tc.pid), Matched: true, MBID: tc.mbid, Reason: "barcode",
		})
		if err != nil {
			t.Fatalf("ApplyAlbumReleaseMatch(%s): %v", tc.name, err)
		}
		if got := scalarQueryStr(t, db, "SELECT COALESCE(mbid,'') FROM album WHERE title = ?", tc.name); got != "" {
			t.Errorf("%s album mbid = %q, want empty (refused)", tc.name, got)
		}
		if n := scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album' AND entity_id=?", id); n != 1 {
			t.Errorf("%s marker rows = %d, want 1", tc.name, n)
		}
	}

	// The refusals are specific: the holder kept the id, so nothing was clobbered.
	if got := scalarQueryStr(t, db, "SELECT COALESCE(mbid,'') FROM album WHERE title='Holder'"); got != relTestOneMBID {
		t.Errorf("Holder album mbid = %q, want %s", got, relTestOneMBID)
	}
}

// TestEnrichScopeForItem checks the per-kind scope resolution: a track scopes to
// its (distinct) artist and album artist, its release group, and its own lyrics
// lookup; a book to its contributors and its own identifier fill; an episode is
// refused; an unknown pid is CodeNotFound.
func TestEnrichScopeForItem(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	trackPID := scopeTrack(t, st, lib.ID, "/lib/t.mp3", "ess-t", "Song", "Solo Act", "Various Artists", "Comp")
	scope, err := st.EnrichScopeForItem(ctx, trackPID)
	if err != nil {
		t.Fatalf("EnrichScopeForItem(track): %v", err)
	}
	if len(scope.ArtistIDs) != 2 {
		t.Errorf("track artist scope = %v, want the artist and the distinct album artist", scope.ArtistIDs)
	}
	if len(scope.ReleaseGroupIDs) != 1 {
		t.Errorf("track release-group scope = %v, want the album's release group", scope.ReleaseGroupIDs)
	}
	var wantRG int64
	if err := db.QueryRow(`SELECT al.release_group_id FROM album al
		JOIN track tr ON tr.album_id = al.id JOIN playable_item pi ON pi.id = tr.item_id
		WHERE pi.pid = ?`, string(trackPID)).Scan(&wantRG); err != nil {
		t.Fatalf("resolve release group: %v", err)
	}
	if len(scope.ReleaseGroupIDs) == 1 && scope.ReleaseGroupIDs[0] != wantRG {
		t.Errorf("release-group scope = %d, want %d", scope.ReleaseGroupIDs[0], wantRG)
	}
	var itemID int64
	if err := db.QueryRow("SELECT id FROM playable_item WHERE pid = ?", string(trackPID)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item id: %v", err)
	}
	if len(scope.LyricsItemIDs) != 1 || scope.LyricsItemIDs[0] != itemID {
		t.Errorf("track lyrics scope = %v, want [%d]", scope.LyricsItemIDs, itemID)
	}
	if len(scope.BookItemIDs) != 0 {
		t.Errorf("track scope carries book ids: %v", scope.BookItemIDs)
	}

	// A track whose artist and album artist are the same entity collects it once.
	samePID := scopeTrack(t, st, lib.ID, "/lib/s.mp3", "ess-s", "Same", "One Band", "One Band", "Album")
	sameScope, err := st.EnrichScopeForItem(ctx, samePID)
	if err != nil {
		t.Fatalf("EnrichScopeForItem(same artist): %v", err)
	}
	if len(sameScope.ArtistIDs) != 1 {
		t.Errorf("same-artist track scope = %v, want one artist id", sameScope.ArtistIDs)
	}

	// A track with no primary artist (NULL artist_id) still scopes to its album
	// artist alone.
	onlyAlbumPID := scopeTrack(t, st, lib.ID, "/lib/o.mp3", "ess-o", "Only", "", "Album Only Band", "Album O")
	onlyScope, err := st.EnrichScopeForItem(ctx, onlyAlbumPID)
	if err != nil {
		t.Fatalf("EnrichScopeForItem(album artist only): %v", err)
	}
	var albumOnlyID int64
	if err := db.QueryRow("SELECT id FROM artist WHERE name='Album Only Band'").Scan(&albumOnlyID); err != nil {
		t.Fatalf("resolve album-only artist: %v", err)
	}
	if len(onlyScope.ArtistIDs) != 1 || onlyScope.ArtistIDs[0] != albumOnlyID {
		t.Errorf("album-artist-only track scope = %v, want [%d]", onlyScope.ArtistIDs, albumOnlyID)
	}

	// Book: contributors (author + narrator) and the book's own identifier fill.
	bookRes, err := st.PutScannedBook(ctx, model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/book.m4b"), DisplayPath: "/lib/book.m4b", RelPath: []byte("book.m4b"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-book", EssenceHash: "ess-book", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "A Book",
			SortKey: model.SortKey("A Book"), IdentityKey: "book:a book",
		},
		Book: model.Book{Authors: []string{"An Author"}, Narrators: []string{"A Narrator"}},
	})
	if err != nil {
		t.Fatalf("PutScannedBook: %v", err)
	}
	bookScope, err := st.EnrichScopeForItem(ctx, bookRes.ItemPID)
	if err != nil {
		t.Fatalf("EnrichScopeForItem(book): %v", err)
	}
	if len(bookScope.ArtistIDs) != 2 {
		t.Errorf("book contributor scope = %v, want author + narrator", bookScope.ArtistIDs)
	}
	var bookItemID int64
	if err := db.QueryRow("SELECT id FROM playable_item WHERE pid = ?", string(bookRes.ItemPID)).Scan(&bookItemID); err != nil {
		t.Fatalf("resolve book item id: %v", err)
	}
	if len(bookScope.BookItemIDs) != 1 || bookScope.BookItemIDs[0] != bookItemID {
		t.Errorf("book scope = %v, want [%d]", bookScope.BookItemIDs, bookItemID)
	}
	if len(bookScope.LyricsItemIDs) != 0 {
		t.Errorf("book scope carries lyrics ids: %v", bookScope.LyricsItemIDs)
	}

	// Episode: feed-owned metadata, not enrichable.
	feedRes, err := st.UpsertFeed(ctx, feedInput("http://feed.example/scope", "Ep"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, feedRes.PodcastPID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %v (err %v), want 1", eps, err)
	}
	if _, err := st.EnrichScopeForItem(ctx, eps[0].PID); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("EnrichScopeForItem(episode) err = %v, want CodeUnsupported", err)
	}

	if _, err := st.EnrichScopeForItem(ctx, "01J0NONEXISTENT0000000000"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("EnrichScopeForItem(unknown) err = %v, want CodeNotFound", err)
	}
}

// TestEnrichScopeForEntity checks the entity resolution: artist and release
// group scope to themselves, an album to its parent release group, and the
// kinds enrichment has no provider for are refused.
func TestEnrichScopeForEntity(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	scopeTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Song", "Pink Floyd", "Pink Floyd", "Wish You Were Here")

	var artistID int64
	var artistPID string
	if err := db.QueryRow("SELECT id, pid FROM artist WHERE name='Pink Floyd'").Scan(&artistID, &artistPID); err != nil {
		t.Fatalf("resolve artist: %v", err)
	}
	var rgID int64
	var rgPID string
	if err := db.QueryRow("SELECT id, pid FROM release_group WHERE title='Wish You Were Here'").Scan(&rgID, &rgPID); err != nil {
		t.Fatalf("resolve release group: %v", err)
	}
	var albumPID string
	if err := db.QueryRow("SELECT pid FROM album WHERE title='Wish You Were Here'").Scan(&albumPID); err != nil {
		t.Fatalf("resolve album: %v", err)
	}

	scope, err := st.EnrichScopeForEntity(ctx, read.EntityArtist, model.PID(artistPID))
	if err != nil || len(scope.ArtistIDs) != 1 || scope.ArtistIDs[0] != artistID {
		t.Errorf("artist scope = %+v (err %v), want [%d]", scope, err, artistID)
	}
	scope, err = st.EnrichScopeForEntity(ctx, read.EntityReleaseGroup, model.PID(rgPID))
	if err != nil || len(scope.ReleaseGroupIDs) != 1 || scope.ReleaseGroupIDs[0] != rgID {
		t.Errorf("release-group scope = %+v (err %v), want [%d]", scope, err, rgID)
	}
	// An album resolves to its parent release group: enrichment works at RG grain.
	scope, err = st.EnrichScopeForEntity(ctx, read.EntityAlbum, model.PID(albumPID))
	if err != nil || len(scope.ReleaseGroupIDs) != 1 || scope.ReleaseGroupIDs[0] != rgID {
		t.Errorf("album scope = %+v (err %v), want parent release group [%d]", scope, err, rgID)
	}

	if _, err := st.EnrichScopeForEntity(ctx, read.EntityGenre, "any"); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("genre scope err = %v, want CodeUnsupported", err)
	}
	if _, err := st.EnrichScopeForEntity(ctx, read.EntitySeries, "any"); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("series scope err = %v, want CodeUnsupported", err)
	}
	if _, err := st.EnrichScopeForEntity(ctx, read.EntityArtist, "01J0NONEXISTENT0000000000"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown artist scope err = %v, want CodeNotFound", err)
	}
}

// TestScopedEnrichmentQueries checks the ids filter on the iteration queries and
// the scoped count: only in-scope rows return, the keyset shape still advances,
// force still bypasses markers inside the scope, and the count mirrors the
// phases a scoped run would execute (an empty list contributes zero).
func TestScopedEnrichmentQueries(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	scopeTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "A", "Artist One", "Artist One", "Album One")
	scopeTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "B", "Artist Two", "Artist Two", "Album Two")

	var oneID, twoID int64
	if err := db.QueryRow("SELECT id FROM artist WHERE name='Artist One'").Scan(&oneID); err != nil {
		t.Fatalf("resolve artist one: %v", err)
	}
	if err := db.QueryRow("SELECT id FROM artist WHERE name='Artist Two'").Scan(&twoID); err != nil {
		t.Fatalf("resolve artist two: %v", err)
	}

	// Scoped iteration returns only the scoped artist; nil ids returns both.
	scoped, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, []int64{oneID})
	if err != nil {
		t.Fatalf("scoped ArtistsNeedingEnrichment: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != oneID {
		t.Fatalf("scoped artists = %+v, want only artist one", scoped)
	}
	all, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("unscoped artists = %d (err %v), want 2", len(all), err)
	}

	// The keyset shape holds under a scope: pages advance past the last id.
	page, err := st.ArtistsNeedingEnrichment(ctx, false, oneID, 100, []int64{oneID, twoID})
	if err != nil {
		t.Fatalf("keyset page: %v", err)
	}
	if len(page) != 1 || page[0].ID != twoID {
		t.Fatalf("keyset page after %d = %+v, want only artist two", oneID, page)
	}

	// A marked artist drops out of the scoped walk unless force, which is how a
	// scoped run (force implied) retries a previously-missed target.
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{ArtistID: oneID, PID: scoped[0].PID, Matched: false}); err != nil {
		t.Fatalf("mark artist one: %v", err)
	}
	if got, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, []int64{oneID}); err != nil || len(got) != 0 {
		t.Fatalf("scoped unforced after mark = %+v (err %v), want empty", got, err)
	}
	if got, err := st.ArtistsNeedingEnrichment(ctx, true, 0, 100, []int64{oneID}); err != nil || len(got) != 1 {
		t.Fatalf("scoped forced after mark = %+v (err %v), want artist one", got, err)
	}

	// Scoped release-group iteration mirrors the artist behavior.
	var rgOneID int64
	if err := db.QueryRow("SELECT id FROM release_group WHERE title='Album One'").Scan(&rgOneID); err != nil {
		t.Fatalf("resolve rg one: %v", err)
	}
	rgs, err := st.ReleaseGroupsNeedingEnrichment(ctx, false, 0, 100, false, []int64{rgOneID})
	if err != nil || len(rgs) != 1 || rgs[0].ID != rgOneID {
		t.Fatalf("scoped rgs = %+v (err %v), want only rg one", rgs, err)
	}

	// The scoped count covers exactly the phases a scoped run executes: one
	// artist + one release group here, and the empty album/book/lyrics lists add zero.
	scope := &model.EnrichScope{ArtistIDs: []int64{oneID}, ReleaseGroupIDs: []int64{rgOneID}}
	n, err := st.CountEntitiesNeedingEnrichment(ctx, true, model.EnrichCountOptions{Identity: true, Albums: true, Lyrics: true}, scope)
	if err != nil {
		t.Fatalf("scoped count: %v", err)
	}
	if n != 2 {
		t.Errorf("scoped count = %d, want 2 (artist + release group, empty phases zero)", n)
	}
	// The unscoped count still covers the catalog (2 artists + 2 rgs; the tracks
	// need lyrics lookups too under includeLyrics).
	un, err := st.CountEntitiesNeedingEnrichment(ctx, true, model.EnrichCountOptions{Identity: true}, nil)
	if err != nil || un != 4 {
		t.Fatalf("unscoped count = %d (err %v), want 4", un, err)
	}

	// Scoped lyrics iteration: only the scoped item, and an item that already has
	// lyrics stays excluded (the fill-when-empty predicate rides along).
	var itemAID int64
	if err := db.QueryRow("SELECT pi.id FROM playable_item pi WHERE pi.title='A'").Scan(&itemAID); err != nil {
		t.Fatalf("resolve item A: %v", err)
	}
	ly, err := st.ItemsNeedingLyrics(ctx, false, 0, 100, []int64{itemAID})
	if err != nil || len(ly) != 1 || ly[0].ID != itemAID {
		t.Fatalf("scoped lyrics = %+v (err %v), want item A", ly, err)
	}

	// An EMPTY non-nil ids list is a scope with no targets and matches nothing;
	// only nil means "no scope". A scoped-to-nothing walk must not silently widen
	// into the full catalog.
	if got, err := st.ArtistsNeedingEnrichment(ctx, true, 0, 100, []int64{}); err != nil || len(got) != 0 {
		t.Errorf("empty-scope artists = %+v (err %v), want none", got, err)
	}
	if got, err := st.ItemsNeedingLyrics(ctx, true, 0, 100, []int64{}); err != nil || len(got) != 0 {
		t.Errorf("empty-scope lyrics = %+v (err %v), want none", got, err)
	}
}

// TestScopedEnrichmentReachesGhostEntities verifies the backs-items heuristic is
// dropped for an explicitly scoped walk: a full pass skips an artist left
// backing nothing by a retag, but a caller who names that artist reaches it.
func TestScopedEnrichmentReachesGhostEntities(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	// Seed one track, then retag it (same path and essence, new content hash and
	// mtime, new artist): the old artist row stays behind, backing nothing.
	scopeTrack(t, st, lib.ID, "/lib/g.mp3", "ess-g", "Song", "Ghost Band", "Ghost Band", "Ghost Album")
	if _, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/g.mp3"), DisplayPath: "/lib/g.mp3", RelPath: []byte("g.mp3"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 2,
			ContentHash: "c-g-retagged", EssenceHash: "ess-g", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Song",
			SortKey: model.SortKey("Song"), IdentityKey: "essence:ess-g",
		},
		Track: model.Track{Artist: "Real Band", AlbumArtist: "Real Band", Album: "Ghost Album", TrackNo: 1},
	}); err != nil {
		t.Fatalf("retag: %v", err)
	}

	var ghostID int64
	if err := db.QueryRow("SELECT id FROM artist WHERE name='Ghost Band'").Scan(&ghostID); err != nil {
		t.Fatalf("resolve ghost artist (retag should leave the row): %v", err)
	}
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM track WHERE artist_id=? OR album_artist_id=?", ghostID, ghostID); n != 0 {
		t.Fatalf("ghost still backs %d tracks, fixture broken", n)
	}

	// The full pass skips the ghost; the scoped walk reaches it.
	all, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("unscoped artists: %v", err)
	}
	for _, a := range all {
		if a.ID == ghostID {
			t.Fatalf("unscoped walk returned the ghost artist %+v", a)
		}
	}
	scoped, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100, []int64{ghostID})
	if err != nil || len(scoped) != 1 || scoped[0].ID != ghostID {
		t.Fatalf("scoped ghost walk = %+v (err %v), want the ghost artist", scoped, err)
	}

	// The scoped count stays in lockstep with the relaxed walk.
	n, err := st.CountEntitiesNeedingEnrichment(ctx, true, model.EnrichCountOptions{Identity: true}, &model.EnrichScope{ArtistIDs: []int64{ghostID}})
	if err != nil || n != 1 {
		t.Fatalf("scoped ghost count = %d (err %v), want 1", n, err)
	}
}

func scalarQueryStr(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func scalarQueryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// enrichArtImg builds an enrichment-attributed art image with a distinct content
// address, the shape gatherArt hands the store.
func enrichArtImg(hash, provider string) *model.ArtImage {
	return &model.ArtImage{
		Data: []byte("img-" + hash), Hash: hash, Format: "png", Width: 4, Height: 4,
		Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: provider},
	}
}

// TestApplyReleaseGroupEnrichmentAuxRoles: enrichment's non-front roles land
// fill-when-empty at the release-group rung with their attribution, a pre-existing
// user image in a role is never replaced, and the entity art lock skips every
// enrichment art write.
func TestApplyReleaseGroupEnrichmentAuxRoles(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	albumTrack(t, st, lib.ID, "ess-a", "One", "", "")
	rgID := scalarQueryInt(t, db, "SELECT id FROM release_group")
	rgPID := scalarQueryStr(t, db, "SELECT pid FROM release_group")

	// A hand-set back cover already in place.
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, model.PID(rgPID), model.ArtRoleBack,
		[]byte("user-back"), "png", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("seed user back: %v", err)
	}
	userBackHash := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='back'")

	err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: int64(rgID), PID: model.PID(rgPID), Matched: true, MBID: relTestRGMBID,
		AuxArt: map[model.ArtRole]*model.ArtImage{
			model.ArtRoleBack: enrichArtImg("enr-back", "mock"),
			model.ArtRoleDisc: enrichArtImg("enr-disc", "mock"),
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The empty disc slot filled with enrichment attribution.
	var source, provider string
	if err := db.QueryRow(`SELECT source, provider FROM art_map
		WHERE entity_type='release_group' AND entity_id=? AND role='disc'`, rgID).Scan(&source, &provider); err != nil {
		t.Fatalf("read disc row: %v", err)
	}
	if source != string(model.SourceEnrichment) || provider != "mock" {
		t.Errorf("disc attribution = %q/%q, want enrichment/mock", source, provider)
	}
	// The hand-set back cover was not replaced.
	if h := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='back'"); h != userBackHash {
		t.Errorf("back hash = %q, want the user's %q (fill-when-empty per role)", h, userBackHash)
	}

	// Under the entity art lock nothing lands, front or aux.
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, model.PID(rgPID), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	err = st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: int64(rgID), PID: model.PID(rgPID), Matched: true, MBID: relTestRGMBID,
		Art: enrichArtImg("enr-front", "mock"),
		AuxArt: map[model.ArtRole]*model.ArtImage{
			model.ArtRoleBooklet: enrichArtImg("enr-booklet", "mock"),
		},
	})
	if err != nil {
		t.Fatalf("apply under lock: %v", err)
	}
	for _, role := range []string{"front", "booklet"} {
		if n := scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND role=?", role); n != 0 {
			t.Errorf("%s rows = %d, want 0 (the art lock gates every enrichment art write)", role, n)
		}
	}
}

// TestApplyAlbumReleaseMatchAuxRidesOnID: aux art rides on the mbid landing the same
// way the front does; a declined write (a locked mbid) applies neither.
func TestApplyAlbumReleaseMatchAuxRidesOnID(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	albumTrack(t, st, lib.ID, "ess-a", "Declined", "0075992739429", "")
	albumTrack(t, st, lib.ID, "ess-b", "Landed", "5099902154251", "")
	declinedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Declined'")
	landedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Landed'")

	// A locked-empty mbid declines the write; neither front nor aux lands.
	setEntityMBID(t, st, model.MergeAlbum, declinedPID, "", true)
	declinedID := albumIDByTitle(t, db, "Declined")
	err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: declinedID, PID: model.PID(declinedPID), Matched: true, MBID: relTestOneMBID, Reason: "barcode",
		Art:    enrichArtImg("front-a", "mock"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("back-a", "mock")},
	})
	if err != nil {
		t.Fatalf("apply declined: %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", declinedID); n != 0 {
		t.Errorf("declined album art rows = %d, want 0 (aux rides on the id landing)", n)
	}

	// A landed id applies both.
	landedID := albumIDByTitle(t, db, "Landed")
	err = st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: landedID, PID: model.PID(landedPID), Matched: true, MBID: relTestTwoMBID, Reason: "barcode",
		Art:    enrichArtImg("front-b", "mock"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("back-b", "mock")},
	})
	if err != nil {
		t.Fatalf("apply landed: %v", err)
	}
	for _, role := range []string{"front", "back"} {
		if n := scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role=?", landedID, role); n != 1 {
			t.Errorf("landed album %s rows = %d, want 1", role, n)
		}
	}

	// The album's own art lock skips front and aux even when the mbid lands.
	albumTrack(t, st, lib.ID, "ess-c", "LockedArt", "", "SHVL 804")
	lockedArtPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='LockedArt'")
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, model.PID(lockedArtPID), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	lockedArtID := albumIDByTitle(t, db, "LockedArt")
	err = st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: lockedArtID, PID: model.PID(lockedArtPID), Matched: true, MBID: relTestRGMBID, Reason: "catalog number",
		Art:    enrichArtImg("front-c", "mock"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("back-c", "mock")},
	})
	if err != nil {
		t.Fatalf("apply locked art: %v", err)
	}
	if got := scalarQueryStr(t, db, "SELECT COALESCE(mbid,'') FROM album WHERE id=?", lockedArtID); got != relTestRGMBID {
		t.Errorf("locked-art album mbid = %q, want the id landed", got)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", lockedArtID); n != 0 {
		t.Errorf("locked-art album art rows = %d, want 0 (the art lock gates front and aux)", n)
	}
}

// auxRGTrack persists one track under its own artist, album, and release group, so
// each call gives the aux-art queue a distinct group to judge. An empty mbid leaves
// the group unidentified. Re-calling it with the same name and a new album retags the
// one file, which is how a test strands the group it was under.
func auxRGTrack(t *testing.T, st *sqlite.Store, libID int64, name, album, mbid string) {
	t.Helper()
	path := "/lib/" + name + "/1.mp3"
	_, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte("1.mp3"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-" + album, EssenceHash: "e-" + name, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + name,
			SortKey: model.SortKey("T-" + name), IdentityKey: "essence:e-" + name,
		},
		Track: model.Track{
			Artist: name, AlbumArtist: name, Album: album, TrackNo: 1, MBReleaseGroupID: mbid,
		},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack %q: %v", name, err)
	}
}

// auxRGMBID is a distinct well-formed release-group id per fixture.
func auxRGMBID(n int) string {
	return "e0000000-0000-4000-8000-00000000001" + string(rune('0'+n))
}

// setRGArt stores one release-group art role from raw bytes, the way a user's
// `art set` does.
func setRGArt(t *testing.T, st *sqlite.Store, pid model.PID, role model.ArtRole, data string) {
	t.Helper()
	err := st.SetEntityArt(context.Background(), model.ArtReleaseGroup, pid, role,
		[]byte(data), "png", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false)
	if err != nil {
		t.Fatalf("set %s art: %v", role, err)
	}
}

func assertStoreVerifyClean(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// TestReleaseGroupsNeedingAuxArtGuards pins the backfill queue's guards: a titled group
// with a vacancy is queued no matter how settled its front is or whether it carries an
// mbid, while a whole-entity art lock, an existing marker, a full set of aux slots, and
// the shared ghost heuristic each keep a group out. A per-role lock deliberately does
// not: the queue cannot cheaply tell a role held empty from an empty one, so the group
// is queued and the apply skips the role.
func TestReleaseGroupsNeedingAuxArtGuards(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	names := []string{"Settled", "Whole", "Marked", "Full", "RoleLock"}
	for i, name := range names {
		auxRGTrack(t, st, lib.ID, name, name, auxRGMBID(i))
	}
	auxRGTrack(t, st, lib.ID, "NoID", "NoID", "")

	// A ghost group: enriched into an mbid while it still had members, then stranded by
	// a retag that cleared the album tag and left its one track ungrouped. It qualifies
	// on every other part of the gate, which is what makes it the likeliest thing the
	// ghost heuristic has to catch here. A retag onto another title would not do it any
	// more: the scan-side re-key reconciliation carries the group onto the new one.
	auxRGTrack(t, st, lib.ID, "Ghost", "Ghost", "")
	ghostRGPID := scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title='Ghost'")
	setEntityMBID(t, st, model.MergeReleaseGroup, ghostRGPID, auxRGMBID(6), false)
	auxRGTrack(t, st, lib.ID, "Ghost", "", "")
	if n := scalarQueryInt(t, db, `SELECT COUNT(*) FROM album al JOIN track t ON t.album_id = al.id
		JOIN release_group rg ON rg.id = al.release_group_id WHERE rg.title='Ghost'`); n != 0 {
		t.Fatalf("the Ghost group still backs %d tracks; the fixture did not strand it", n)
	}

	rgPID := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title=?", title))
	}
	rgID := func(title string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title=?", title))
	}

	// A settled front is what the release-group pass leaves behind, and it must not
	// keep the group out: re-asking about the empty aux slots is the whole phase.
	setRGArt(t, st, rgPID("Settled"), model.ArtRoleFront, "settled-front")
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, rgPID("Whole"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: rgID("Marked"), PID: rgPID("Marked"),
	}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	for _, role := range []model.ArtRole{
		model.ArtRoleBack, model.ArtRoleDisc, model.ArtRoleBooklet, model.ArtRoleBackground,
	} {
		setRGArt(t, st, rgPID("Full"), role, "full-"+string(role))
	}
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, rgPID("RoleLock"), model.ArtRoleBack, true); err != nil {
		t.Fatalf("lock back: %v", err)
	}

	queued, err := st.ReleaseGroupsNeedingAuxArt(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("ReleaseGroupsNeedingAuxArt: %v", err)
	}
	got := map[string]bool{}
	for _, q := range queued {
		got[q.Name] = true
		// The title is what a name-keyed provider is asked with, so a queued group
		// without one would be an ask that can only ever miss.
		if q.Name == "" {
			t.Error("queued a group with no title; there is nothing to ask a provider with")
		}
	}
	want := map[string]bool{"Settled": true, "RoleLock": true, "NoID": true}
	for _, name := range append(names, "NoID", "Ghost") {
		if got[name] != want[name] {
			t.Errorf("%q queued = %v, want %v", name, got[name], want[name])
		}
	}

	// The heartbeat denominator is built from the same gate, so turning the phase on
	// adds exactly the queued groups and nothing else.
	withAux, err := st.CountEntitiesNeedingEnrichment(ctx, false, model.EnrichCountOptions{AuxArt: true}, nil)
	if err != nil {
		t.Fatalf("count with aux: %v", err)
	}
	withoutAux, err := st.CountEntitiesNeedingEnrichment(ctx, false, model.EnrichCountOptions{}, nil)
	if err != nil {
		t.Fatalf("count without aux: %v", err)
	}
	if withAux-withoutAux != len(queued) {
		t.Errorf("aux contribution to the count = %d, want the %d queued groups", withAux-withoutAux, len(queued))
	}

	// Force is what re-asks a marked group, mirroring every other queue.
	forced, err := st.ReleaseGroupsNeedingAuxArt(ctx, true, 0, 100, nil)
	if err != nil {
		t.Fatalf("forced walk: %v", err)
	}
	var sawMarked bool
	for _, q := range forced {
		sawMarked = sawMarked || q.Name == "Marked"
	}
	if !sawMarked {
		t.Error("a forced walk skipped the marked group")
	}

	// The per-role lock is re-checked at apply: the locked slot stays empty while the
	// role beside it fills.
	err = st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: rgID("RoleLock"), PID: rgPID("RoleLock"), Matched: true, Provider: "mock",
		AuxArt: map[model.ArtRole]*model.ArtImage{
			model.ArtRoleBack: enrichArtImg("rl-back", "mock"),
			model.ArtRoleDisc: enrichArtImg("rl-disc", "mock"),
		},
	})
	if err != nil {
		t.Fatalf("apply under a role lock: %v", err)
	}
	lockedID := rgID("RoleLock")
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='back'", lockedID); n != 0 {
		t.Errorf("locked back rows = %d, want 0", n)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='disc'", lockedID); n != 1 {
		t.Errorf("disc rows = %d, want 1 (the role beside the lock still fills)", n)
	}
	assertStoreVerifyClean(t, st)
}

// auxMarkerFixture seeds one identified release group with a settled front cover and
// returns its row id, its pid, and readers for the marker count and the queue.
func auxMarkerFixture(t *testing.T, title string, mbidN int) (*sqlite.Store, int64, model.PID, func() int, func() bool) {
	t.Helper()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	auxRGTrack(t, st, lib.ID, title, title, auxRGMBID(mbidN))
	id := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title=?", title))
	pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title=?", title))
	setRGArt(t, st, pid, model.ArtRoleFront, "settled-front")

	markers := func() int {
		return scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", id)
	}
	queued := func() bool {
		t.Helper()
		targets, err := st.ReleaseGroupsNeedingAuxArt(ctx, false, 0, 100, nil)
		if err != nil {
			t.Fatalf("queue walk: %v", err)
		}
		for _, tgt := range targets {
			if tgt.ID == id {
				return true
			}
		}
		return false
	}
	return st, id, pid, markers, queued
}

// markAuxArt records the backfill marker the way a run that found nothing does.
func markAuxArt(t *testing.T, st *sqlite.Store, id int64, pid model.PID) {
	t.Helper()
	if err := st.ApplyReleaseGroupAuxArt(context.Background(),
		model.ReleaseGroupAuxArt{ReleaseGroupID: id, PID: pid}); err != nil {
		t.Fatalf("mark: %v", err)
	}
}

// TestAuxArtMarkerClearsOnUnlock: the marker says the group's vacancies were asked
// about once, so releasing a lock that was holding a slot shut has to drop it. Without
// that the group is out of the queue for good short of --force, and the documented
// unlock-then-enrich walk fills nothing.
func TestAuxArtMarkerClearsOnUnlock(t *testing.T) {
	ctx := context.Background()
	st, id, pid, markers, queued := auxMarkerFixture(t, "Opened", 0)

	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBack, true); err != nil {
		t.Fatalf("lock back: %v", err)
	}
	markAuxArt(t, st, id, pid)
	if n := markers(); n != 1 {
		t.Fatalf("markers after the pass = %d, want 1", n)
	}
	if queued() {
		t.Fatal("the marked group is still queued; the fixture proves nothing")
	}

	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBack, false); err != nil {
		t.Fatalf("unlock back: %v", err)
	}
	if n := markers(); n != 0 {
		t.Errorf("markers after the role unlock = %d, want the marker dropped", n)
	}
	if !queued() {
		t.Error("the group was not re-queued after the unlock that opened its back slot")
	}

	// Nothing opens while the whole-entity lock stands, so releasing one role under it
	// leaves the marker alone.
	markAuxArt(t, st, id, pid)
	for _, role := range []model.ArtRole{model.ArtRoleFront, model.ArtRoleBack} {
		if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, role, true); err != nil {
			t.Fatalf("lock %s: %v", role, err)
		}
	}
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBack, false); err != nil {
		t.Fatalf("unlock back under the whole lock: %v", err)
	}
	if n := markers(); n != 1 {
		t.Errorf("markers after a role unlock under the whole lock = %d, want it kept", n)
	}
	// Releasing the whole lock does open the roles.
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, model.ArtRoleFront, false); err != nil {
		t.Fatalf("unlock whole art: %v", err)
	}
	if n := markers(); n != 0 {
		t.Errorf("markers after the whole unlock = %d, want the marker dropped", n)
	}
	assertStoreVerifyClean(t, st)
}

// TestAuxArtMarkerClearsOnAuxClear: clearing an auxiliary image without locking the
// slot behind it opens a vacancy, which is the other write that outdates the marker.
// The default clear locks the slot, and then nothing opened. A set that releases the
// front's lock frees every role at once, since that lock is the whole-entity one, so it
// clears the marker the way `art unlock` does.
func TestAuxArtMarkerClearsOnAuxClear(t *testing.T) {
	ctx := context.Background()
	st, id, pid, markers, _ := auxMarkerFixture(t, "Cleared", 1)

	setRGArt(t, st, pid, model.ArtRoleBack, "back-image")
	markAuxArt(t, st, id, pid)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBack, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("clear back: %v", err)
	}
	if n := markers(); n != 0 {
		t.Errorf("markers after an unlocked clear = %d, want the marker dropped", n)
	}

	setRGArt(t, st, pid, model.ArtRoleDisc, "disc-image")
	markAuxArt(t, st, id, pid)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleDisc, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("clear and lock disc: %v", err)
	}
	if n := markers(); n != 1 {
		t.Errorf("markers after a clear that locked the slot = %d, want it kept", n)
	}

	// The --keep-lock spelling on a slot carrying no lock is a fillable clear too, so it
	// drops the marker the way --no-lock does.
	setRGArt(t, st, pid, model.ArtRoleBooklet, "booklet-image")
	markAuxArt(t, st, id, pid)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBooklet, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear booklet leaving its lock alone: %v", err)
	}
	if n := markers(); n != 0 {
		t.Errorf("markers after a keep-lock clear of an unlocked slot = %d, want the marker dropped", n)
	}

	// Under the whole-entity lock the cleared role is not fillable either, so the marker
	// stays. That is artFillBlockedTx's whole-lock branch, which the disc case above did
	// not reach: it was blocked by the role's own lock.
	setRGArt(t, st, pid, model.ArtRoleBackground, "background-image")
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid, model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock whole art: %v", err)
	}
	markAuxArt(t, st, id, pid)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleBackground, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear background under the whole lock: %v", err)
	}
	if n := markers(); n != 1 {
		t.Errorf("markers after a clear under the whole art lock = %d, want it kept", n)
	}

	// The front role's lock is the plain "art" field, so a set that releases it opens
	// every role not held by its own lock. Both sets need force, since the whole lock
	// taken just above is also what refuses them.
	markAuxArt(t, st, id, pid)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleFront, []byte("kept-front"), "png",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, true); err != nil {
		t.Fatalf("set front leaving the lock alone: %v", err)
	}
	if n := markers(); n != 1 {
		t.Errorf("markers after a front set that left the lock standing = %d, want it kept", n)
	}
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid, model.ArtRoleFront, []byte("freed-front"), "png",
		model.Attribution{Source: model.SourceUser}, model.LockOff, true); err != nil {
		t.Fatalf("set front with --no-lock: %v", err)
	}
	if n := markers(); n != 0 {
		t.Errorf("markers after a front set released the whole lock = %d, want the marker dropped", n)
	}
	assertStoreVerifyClean(t, st)
}

// TestApplyReleaseGroupAuxArtFillsAndMarks: the marker is written either way and
// always names a provider, while the entity delta rides on an image actually landing.
func TestApplyReleaseGroupAuxArtFillsAndMarks(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	auxRGTrack(t, st, lib.ID, "Marks", "Marks", auxRGMBID(0))
	id := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title='Marks'"))
	pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title='Marks'"))

	rgUpdates := func() int {
		return scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM change_log WHERE entity_type='release_group' AND op='update'")
	}
	before := rgUpdates()

	// A run nothing answered still marks, so the group costs one pass rather than one
	// lookup per run.
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: id, PID: pid,
	}); err != nil {
		t.Fatalf("apply no-match: %v", err)
	}
	if p := scalarQueryStr(t, db,
		"SELECT provider FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", id); p == "" {
		t.Error("no-match marker provider is empty; the column is NOT NULL and a reader cannot tell that from a missing value")
	}
	if n := scalarQueryInt(t, db,
		"SELECT matched FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", id); n != 0 {
		t.Errorf("no-match marker matched = %d, want 0", n)
	}
	if n := rgUpdates(); n != before {
		t.Errorf("release_group updates = %d, want %d (a no-match changes nothing)", n, before)
	}

	// A real fill emits exactly one entity delta and records the supplying provider.
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: id, PID: pid, Matched: true, Provider: "mock",
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("fill-back", "mock")},
	}); err != nil {
		t.Fatalf("apply fill: %v", err)
	}
	if n := rgUpdates(); n != before+1 {
		t.Errorf("release_group updates = %d, want %d (one fill, one delta)", n, before+1)
	}
	if p := scalarQueryStr(t, db,
		"SELECT provider FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", id); p != "mock" {
		t.Errorf("marker provider = %q, want mock", p)
	}
	backHash := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='back'", id)

	// A second offer for the filled slot writes nothing, so it emits no delta either.
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: id, PID: pid, Matched: true, Provider: "mock",
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("second-back", "mock")},
	}); err != nil {
		t.Fatalf("apply second: %v", err)
	}
	if n := rgUpdates(); n != before+1 {
		t.Errorf("release_group updates = %d, want %d (fill-when-empty wrote nothing)", n, before+1)
	}
	if h := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='back'", id); h != backHash {
		t.Errorf("back hash = %q, want the first image %q", h, backHash)
	}

	// The images decide the fill, not the match flag. The in-repo service sets both
	// together, but the method is on the exported port, and a caller handing over aux
	// art without a match must not silently get a marker and no pictures.
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
		ReleaseGroupID: id, PID: pid, Provider: "mock",
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleDisc: enrichArtImg("unmatched-disc", "mock")},
	}); err != nil {
		t.Fatalf("apply unmatched fill: %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='disc'", id); n != 1 {
		t.Errorf("disc rows after an unmatched fill = %d, want 1", n)
	}
	if n := rgUpdates(); n != before+2 {
		t.Errorf("release_group updates = %d, want %d (the unmatched fill landed an image)", n, before+2)
	}
	assertStoreVerifyClean(t, st)
}

// artMarkerCount reads how many art backfill markers of one type stand.
func artMarkerCount(t *testing.T, db *sql.DB, markerType string) int {
	t.Helper()
	return scalarQueryInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = ?", markerType)
}

// TestArtBackfillMarkersReopenOnNewEvidence: the art walks key on the name and their
// markers are permanent, so a marker earned by an id-less request has to be dropped when
// evidence a provider could have used arrives. Three writers land an artist mbid (the
// scan's tag fill, the identity phase, and the entity edit) and all three go through the
// one helper; a release group's lands through the identity phase alone.
func TestArtBackfillMarkersReopenOnNewEvidence(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	auxRGTrack(t, st, lib.ID, "Enriched", "Enriched", "")
	auxRGTrack(t, st, lib.ID, "Scanned", "Scanned", "")
	artistID := func(name string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM artist WHERE name = ?", name))
	}
	artistPID := func(name string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM artist WHERE name = ?", name))
	}
	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title = 'Enriched'"))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title = 'Enriched'"))

	mark := func() {
		t.Helper()
		for _, name := range []string{"Enriched", "Scanned"} {
			if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{
				ArtistID: artistID(name), PID: artistPID(name),
			}); err != nil {
				t.Fatalf("mark %s artist art: %v", name, err)
			}
		}
		if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{
			ReleaseGroupID: rgID, PID: rgPID,
		}); err != nil {
			t.Fatalf("mark aux art: %v", err)
		}
	}
	mark()
	if n := artMarkerCount(t, db, "artist_art"); n != 2 {
		t.Fatalf("artist_art markers = %d, want the 2 just written", n)
	}

	// The identity phase filling an artist's mbid, which is the case that matters after
	// a contact-less run: the art marker stands with no identity marker beside it.
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{
		ArtistID: artistID("Enriched"), PID: artistPID("Enriched"), Matched: true,
		MBID: auxRGMBID(1),
	}); err != nil {
		t.Fatalf("ApplyArtistEnrichment: %v", err)
	}
	if n := artMarkerCount(t, db, "artist_art"); n != 1 {
		t.Errorf("artist_art markers after an identity fill = %d, want the marked artist re-opened", n)
	}

	// The same phase one rung over, for the aux-art marker.
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: auxRGMBID(2),
	}); err != nil {
		t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
	}
	if n := artMarkerCount(t, db, "aux_art"); n != 0 {
		t.Errorf("aux_art markers after an identity fill = %d, want 0", n)
	}

	// A retag that supplies an artist mbid the row lacked. The scan is the most common
	// way one lands late, so it carries the same rule.
	retagArtistMBID(t, st, lib.ID, "Scanned", auxRGMBID(3))
	if n := artMarkerCount(t, db, "artist_art"); n != 0 {
		t.Errorf("artist_art markers after a retag = %d, want the scanned artist re-opened", n)
	}
}

// TestArtBackfillMarkersReopenOnRename: a rename moves the key a name-keyed provider was
// asked with, which is new evidence whether or not the entity carries an mbid. Both
// markers go through the whole-album edit's rename pre-pass.
func TestArtBackfillMarkersReopenOnRename(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	auxRGTrack(t, st, lib.ID, "Typo Band", "Typo Album", "")
	artistID := int64(scalarQueryInt(t, db, "SELECT id FROM artist WHERE name = 'Typo Band'"))
	artistPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM artist WHERE name = 'Typo Band'"))
	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title = 'Typo Album'"))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title = 'Typo Album'"))
	itemPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM playable_item WHERE title = 'T-Typo Band'"))

	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: artistID, PID: artistPID}); err != nil {
		t.Fatalf("mark artist art: %v", err)
	}
	if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{ReleaseGroupID: rgID, PID: rgPID}); err != nil {
		t.Fatalf("mark aux art: %v", err)
	}

	if err := st.EditItemFields(ctx, itemPID,
		map[string]string{"artist": "Typo Bandd", "album_artist": "Typo Bandd", "album": "Typo Albumm"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("rename edit: %v", err)
	}
	if n := artMarkerCount(t, db, "artist_art"); n != 0 {
		t.Errorf("artist_art markers after a rename = %d, want 0", n)
	}
	if n := artMarkerCount(t, db, "aux_art"); n != 0 {
		t.Errorf("aux_art markers after a rename = %d, want 0", n)
	}
	assertStoreVerifyClean(t, st)
}

// retagArtistMBID rescans auxRGTrack's file with the artist mbid a retag would have put
// in the tag, which is how an id lands on an artist the catalog already knows.
func retagArtistMBID(t *testing.T, st *sqlite.Store, libID int64, name, mbid string) {
	t.Helper()
	path := "/lib/" + name + "/1.mp3"
	_, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte("1.mp3"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 2,
			ContentHash: "c2-" + name, EssenceHash: "e-" + name, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + name,
			SortKey: model.SortKey("T-" + name), IdentityKey: "essence:e-" + name,
		},
		Track: model.Track{
			Artist: name, AlbumArtist: name, Album: name, TrackNo: 1,
			MBArtistIDs: []string{mbid}, MBAlbumArtistIDs: []string{mbid},
		},
	})
	if err != nil {
		t.Fatalf("retag %q: %v", name, err)
	}
}

// TestEnrichmentWritebackCarriesEarlierFields: the sinceNS bound decides which items are
// reopened, not which of their fields are written. A value an earlier pass filled but
// never got onto disk (the write failed, or write-tags was off) has to ride the next pass
// that reopens the file, or it stays catalog-only for good, which is the loss the
// write-back exists to prevent.
func TestEnrichmentWritebackCarriesEarlierFields(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	pid := seedEnrichTrack(t, st, lib.ID)

	// An earlier pass fills the composer, then time moves on.
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemRowID(t, db, pid), PID: pid, Matched: true, Provider: "early",
		Fields: map[string]string{"composer": "Roger Waters"},
	}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	between := scalarQueryInt(t, db,
		"SELECT updated_at FROM field_provenance WHERE field='composer'") + 1

	// Nothing since that point: the item is not reopened at all.
	rows, err := st.EnrichmentWriteback(ctx, int64(between))
	if err != nil {
		t.Fatalf("writeback (nothing new): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want none: no field was written since the bound", len(rows))
	}

	// A later pass fills the bpm. The item is reopened for it, and the composer the
	// earlier pass filled rides along rather than being left behind.
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemRowID(t, db, pid), PID: pid, Matched: true, Provider: "late",
		Fields: map[string]string{"bpm": "128"},
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	rows, err = st.EnrichmentWriteback(ctx, int64(between))
	if err != nil {
		t.Fatalf("writeback: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one reopened item", len(rows))
	}
	if got := rows[0].Fields["bpm"]; got != "128" {
		t.Errorf("bpm = %q, want the field this pass wrote", got)
	}
	if got := rows[0].Fields["composer"]; got != "Roger Waters" {
		t.Errorf("composer = %q, want the earlier pass's value carried along", got)
	}
}

// TestApplyItemFieldsStampsEachProviderSeparately: two providers commonly split a fields
// answer, and the provenance row is where a consumer attributes a value, so each field
// names the provider that actually supplied it rather than whichever answered first.
func TestApplyItemFieldsStampsEachProviderSeparately(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	pid := seedEnrichTrack(t, st, lib.ID)

	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemRowID(t, db, pid), PID: pid, Matched: true, Provider: "deezer",
		Fields:    map[string]string{"bpm": "128", "composer": "Roger Waters"},
		Providers: map[string]string{"bpm": "deezer", "composer": "discogs"},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for field, want := range map[string]string{"bpm": "deezer", "composer": "discogs"} {
		got := scalarQueryStr(t, db,
			"SELECT COALESCE(provider,'') FROM field_provenance WHERE field = ?", field)
		if got != want {
			t.Errorf("%s provenance provider = %q, want %q", field, got, want)
		}
	}
	// The marker names who answered at all, which is the caller's own choice of label.
	if p := scalarQueryStr(t, db,
		"SELECT provider FROM entity_enrichment WHERE entity_type='fields'"); p != "deezer" {
		t.Errorf("marker provider = %q, want deezer", p)
	}
	assertStoreVerifyClean(t, st)
}

// seedEnrichTrack persists one plain track and returns its pid.
func seedEnrichTrack(t *testing.T, st *sqlite.Store, libID int64) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/a.mp3"), DisplayPath: "/lib/a.mp3", RelPath: []byte("a.mp3"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1,
			ContentHash: "c-a", EssenceHash: "ess-a", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Shine On",
			SortKey: model.SortKey("Shine On"), IdentityKey: "essence:ess-a",
		},
		Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "WYWH", TrackNo: 1},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}
