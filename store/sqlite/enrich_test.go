package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/colespringer/waxbin/identity"
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
	dbPath := sqlite.SeedCatalog(t, filepath.Join(t.TempDir(), "catalog.db"))
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

	targets, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
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
// TestApplyAlbumFieldsIgnoresAVanishedAlbum: the album can go away between the queue
// page and the apply (a merge or an orphan sweep in another writer), and a dead rowid
// gets nothing rather than failing the run or stranding a marker.
func TestApplyAlbumFieldsIgnoresAVanishedAlbum(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: 424242, PID: model.NewPID(), Matched: true, Provider: "discogs",
		Fields: map[string]string{"year": "1975", "label": "Harvest"},
	})
	if err != nil {
		t.Fatalf("ApplyAlbumFields on a vanished album: %v", err)
	}
	db := roConn(t, dbPath)
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'fields_album'"); n != 0 {
		t.Errorf("stranded fields_album markers = %d, want none", n)
	}
}

func TestAlbumsNeedingReleaseMatchGatesOnIdentifiers(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	albumTrack(t, st, lib.ID, "ess-a", "Has Barcode", "0075992739429", "")
	albumTrack(t, st, lib.ID, "ess-b", "Has CatNo", "", "SHVL 804")
	albumTrack(t, st, lib.ID, "ess-c", "Has Neither", "", "")

	queued, err := st.AlbumsNeedingReleaseMatch(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
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
	queued, err = st.AlbumsNeedingReleaseMatch(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if len(queued) != 1 || queued[0].Name != "Has CatNo" {
		t.Errorf("queued = %+v, want only Has CatNo", queued)
	}

	// Clearing the shared group's mbid leaves nothing to constrain a search to.
	setEntityMBID(t, st, model.MergeReleaseGroup,
		scalarQueryStr(t, db, "SELECT pid FROM release_group LIMIT 1"), "", false)
	queued, err = st.AlbumsNeedingReleaseMatch(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
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
	scoped, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, []int64{oneID})
	if err != nil {
		t.Fatalf("scoped ArtistsNeedingEnrichment: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != oneID {
		t.Fatalf("scoped artists = %+v, want only artist one", scoped)
	}
	all, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("unscoped artists = %d (err %v), want 2", len(all), err)
	}

	// The keyset shape holds under a scope: pages advance past the last id.
	page, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, oneID, 100, []int64{oneID, twoID})
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
	if got, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, []int64{oneID}); err != nil || len(got) != 0 {
		t.Fatalf("scoped unforced after mark = %+v (err %v), want empty", got, err)
	}
	if got, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 100, []int64{oneID}); err != nil || len(got) != 1 {
		t.Fatalf("scoped forced after mark = %+v (err %v), want artist one", got, err)
	}

	// Scoped release-group iteration mirrors the artist behavior.
	var rgOneID int64
	if err := db.QueryRow("SELECT id FROM release_group WHERE title='Album One'").Scan(&rgOneID); err != nil {
		t.Fatalf("resolve rg one: %v", err)
	}
	rgs, err := st.ReleaseGroupsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, false, []int64{rgOneID})
	if err != nil || len(rgs) != 1 || rgs[0].ID != rgOneID {
		t.Fatalf("scoped rgs = %+v (err %v), want only rg one", rgs, err)
	}

	// The scoped count covers exactly the phases a scoped run executes: one
	// artist + one release group here, and the empty album/book/lyrics lists add zero.
	scope := &model.EnrichScope{ArtistIDs: []int64{oneID}, ReleaseGroupIDs: []int64{rgOneID}}
	n, err := st.CountEntitiesNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, model.EnrichCountOptions{Identity: true, Albums: true, Lyrics: true}, scope)
	if err != nil {
		t.Fatalf("scoped count: %v", err)
	}
	if n != 2 {
		t.Errorf("scoped count = %d, want 2 (artist + release group, empty phases zero)", n)
	}
	// The unscoped count still covers the catalog (2 artists + 2 rgs; the tracks
	// need lyrics lookups too under includeLyrics).
	un, err := st.CountEntitiesNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, model.EnrichCountOptions{Identity: true}, nil)
	if err != nil || un != 4 {
		t.Fatalf("unscoped count = %d (err %v), want 4", un, err)
	}

	// Scoped lyrics iteration: only the scoped item, and an item that already has
	// lyrics stays excluded (the fill-when-empty predicate rides along).
	var itemAID int64
	if err := db.QueryRow("SELECT pi.id FROM playable_item pi WHERE pi.title='A'").Scan(&itemAID); err != nil {
		t.Fatalf("resolve item A: %v", err)
	}
	ly, err := st.ItemsNeedingLyrics(ctx, model.EnrichQueueOptions{}, 0, 100, []int64{itemAID})
	if err != nil || len(ly) != 1 || ly[0].ID != itemAID {
		t.Fatalf("scoped lyrics = %+v (err %v), want item A", ly, err)
	}

	// An EMPTY non-nil ids list is a scope with no targets and matches nothing;
	// only nil means "no scope". A scoped-to-nothing walk must not silently widen
	// into the full catalog.
	if got, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 100, []int64{}); err != nil || len(got) != 0 {
		t.Errorf("empty-scope artists = %+v (err %v), want none", got, err)
	}
	if got, err := st.ItemsNeedingLyrics(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 100, []int64{}); err != nil || len(got) != 0 {
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
	all, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
	if err != nil {
		t.Fatalf("unscoped artists: %v", err)
	}
	for _, a := range all {
		if a.ID == ghostID {
			t.Fatalf("unscoped walk returned the ghost artist %+v", a)
		}
	}
	scoped, err := st.ArtistsNeedingEnrichment(ctx, model.EnrichQueueOptions{}, 0, 100, []int64{ghostID})
	if err != nil || len(scoped) != 1 || scoped[0].ID != ghostID {
		t.Fatalf("scoped ghost walk = %+v (err %v), want the ghost artist", scoped, err)
	}

	// The scoped count stays in lockstep with the relaxed walk.
	n, err := st.CountEntitiesNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, model.EnrichCountOptions{Identity: true}, &model.EnrichScope{ArtistIDs: []int64{ghostID}})
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

// TestApplyAlbumArtBackfillRespectsLocks: the album's whole "art" lock gates the front
// and every auxiliary role at once, and a per-role lock takes only its own slot, which is
// the approximation the queue's vacancy test leaves for the apply to settle.
func TestApplyAlbumArtBackfillRespectsLocks(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	albumTrack(t, st, lib.ID, "ess-a", "WholeLock", "0075992739429", "")
	albumTrack(t, st, lib.ID, "ess-b", "RoleLock", "5099902154251", "")

	apply := func(title string) int64 {
		t.Helper()
		id := albumIDByTitle(t, db, title)
		pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = ?", title)
		err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
			AlbumID: id, PID: model.PID(pid), Matched: true, Provider: "mock",
			Art: enrichArtImg("front-"+title, "mock"),
			AuxArt: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBack: enrichArtImg("back-"+title, "mock"),
				model.ArtRoleDisc: enrichArtImg("disc-"+title, "mock"),
			},
		})
		if err != nil {
			t.Fatalf("ApplyAlbumArtBackfill(%s): %v", title, err)
		}
		return id
	}
	rows := func(id int64, role string) int {
		t.Helper()
		return scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role=?", id, role)
	}

	wholePID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='WholeLock'")
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, model.PID(wholePID), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	wholeID := apply("WholeLock")
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", wholeID); n != 0 {
		t.Errorf("whole-locked album art rows = %d, want 0 (the lock gates front and aux)", n)
	}

	rolePID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='RoleLock'")
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, model.PID(rolePID), model.ArtRoleBack, true); err != nil {
		t.Fatalf("lock back: %v", err)
	}
	roleID := apply("RoleLock")
	if rows(roleID, "back") != 0 {
		t.Error("the locked back role took an image")
	}
	if rows(roleID, "front") != 1 || rows(roleID, "disc") != 1 {
		t.Errorf("front/disc rows = %d/%d, want 1 each beside the locked role",
			rows(roleID, "front"), rows(roleID, "disc"))
	}

	// The marker is written either way, so an album nothing serves costs one pass.
	for _, id := range []int64{wholeID, roleID} {
		if n := scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=?", id); n != 1 {
			t.Errorf("album %d art markers = %d, want 1", id, n)
		}
	}
	assertStoreVerifyClean(t, st)
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

	queued, err := st.ReleaseGroupsNeedingAuxArt(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
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
	withAux, err := st.CountEntitiesNeedingEnrichment(ctx, model.EnrichQueueOptions{}, model.EnrichCountOptions{AuxArt: true}, nil)
	if err != nil {
		t.Fatalf("count with aux: %v", err)
	}
	withoutAux, err := st.CountEntitiesNeedingEnrichment(ctx, model.EnrichQueueOptions{}, model.EnrichCountOptions{}, nil)
	if err != nil {
		t.Fatalf("count without aux: %v", err)
	}
	if withAux-withoutAux != len(queued) {
		t.Errorf("aux contribution to the count = %d, want the %d queued groups", withAux-withoutAux, len(queued))
	}

	// Force is what re-asks a marked group, mirroring every other queue.
	forced, err := st.ReleaseGroupsNeedingAuxArt(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 100, nil)
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
		targets, err := st.ReleaseGroupsNeedingAuxArt(ctx, model.EnrichQueueOptions{}, 0, 100, nil)
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
	res, err := st.PutScannedTrack(context.Background(), seedEnrichTrackInput(libID))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// seedEnrichTrackInput is seedEnrichTrack's scan input, so a test can rescan the file.
func seedEnrichTrackInput(libID int64) model.PutScannedTrackInput {
	return model.PutScannedTrackInput{
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
	}
}

// TestEnrichmentWritebackOwedUntilSettled: a file is owed a write while any enrichment
// value on its item is newer than the file's settle stamp, whatever pass filled it, and
// stops being owed once the write-back settles the stamp at that value's time. A later
// fill reopens the file with every field, so a value an earlier pass could not land rides
// along rather than being left behind. A value a rescan cleared still comes back, with
// nothing to write, so the caller can settle it instead of scanning it forever.
func TestEnrichmentWritebackOwedUntilSettled(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	pid := seedEnrichTrack(t, st, lib.ID)
	filePID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM file"))
	itemID := itemRowID(t, db, pid)

	owed := func(t *testing.T, scope *model.EnrichScope) []model.EnrichedTagRow {
		t.Helper()
		rows, err := st.EnrichmentWriteback(ctx, scope)
		if err != nil {
			t.Fatalf("writeback: %v", err)
		}
		return rows
	}
	if rows := owed(t, nil); len(rows) != 0 {
		t.Fatalf("rows = %d, want none before enrichment filled anything", len(rows))
	}
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemID, PID: pid, Matched: true, Provider: "early",
		Fields: map[string]string{"composer": "Roger Waters"},
	}); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	first := int64(scalarQueryInt(t, db, "SELECT updated_at FROM field_provenance WHERE field='composer'"))
	rows := owed(t, nil)
	if len(rows) != 1 || rows[0].FilePID != filePID || rows[0].Newest != first || rows[0].Fields["composer"] != "Roger Waters" {
		t.Fatalf("rows = %+v, want the file owed its composer at the fill's time", rows)
	}

	// A scoped run reaches only the files within its scope.
	if rows := owed(t, &model.EnrichScope{FieldsItemIDs: []int64{itemID + 1}}); len(rows) != 0 {
		t.Fatalf("rows = %d, want none for a scope that does not reach the item", len(rows))
	}
	if rows := owed(t, &model.EnrichScope{FieldsItemIDs: []int64{itemID}}); len(rows) != 1 {
		t.Fatalf("rows = %d, want the one file the scope reaches", len(rows))
	}

	// Settled at the fill's time, the file is no longer owed.
	if err := st.SettleEnrichmentWrite(ctx, filePID, first); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rows := owed(t, nil); len(rows) != 0 {
		t.Fatalf("rows = %d, want none once settled", len(rows))
	}
	// A later fill reopens it, and the earlier composer rides along.
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemID, PID: pid, Matched: true, Provider: "late",
		Fields: map[string]string{"bpm": "128"},
	}); err != nil {
		t.Fatalf("second fill: %v", err)
	}
	rows = owed(t, nil)
	if len(rows) != 1 || rows[0].Newest <= first {
		t.Fatalf("rows = %+v, want the file owed again at the later fill's time", rows)
	}
	if rows[0].Fields["bpm"] != "128" || rows[0].Fields["composer"] != "Roger Waters" {
		t.Errorf("fields = %v, want both the new bpm and the earlier composer", rows[0].Fields)
	}
	// Settling below the newest value leaves the file owed; settling at it does not.
	if err := st.SettleEnrichmentWrite(ctx, filePID, first); err != nil {
		t.Fatalf("settle again: %v", err)
	}
	if rows := owed(t, nil); len(rows) != 1 {
		t.Fatalf("rows = %d, want the file still owed the later fill", len(rows))
	}

	// A rescan that rebuilt the columns from a file without the values leaves the
	// provenance rows behind: the file comes back with nothing to write.
	in := seedEnrichTrackInput(lib.ID)
	in.File.ContentHash = "c-a2"
	in.File.MTimeNS = 2
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	rows = owed(t, nil)
	if len(rows) != 1 || len(rows[0].Fields) != 0 {
		t.Fatalf("rows = %+v, want the file owed with nothing left to write", rows)
	}
	if err := st.SettleEnrichmentWrite(ctx, filePID, rows[0].Newest); err != nil {
		t.Fatalf("settle cleared: %v", err)
	}
	if rows := owed(t, nil); len(rows) != 0 {
		t.Fatalf("rows = %d, want none after settling the cleared value", len(rows))
	}
}

// TestEnrichedAlbumLabelFiles: the album label fan-out is planned by one query joining
// the enrichment label rows to the member files still owed the label, not by a member
// lookup per enriched album. Each such file comes back once with the label, when
// enrichment wrote it, and what the write needs (the path and the on-disk state for the
// optimistic update). A settled file drops out; a non-present item's file never
// appears, since its write would fail on every pass; a shared or virtual file is
// flagged so the caller refuses it and settles the refusal rather than opening it. A
// member with enrichment fields of its own gets the label through the item select
// instead, folded into the one rewrite. A user-curated label is not the enrichment
// write-back's to write.
func TestEnrichedAlbumLabelFiles(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	files := map[string]model.PID{}
	for _, p := range []string{"a", "b"} {
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte("/lib/" + p + ".mp3"), DisplayPath: "/lib/" + p + ".mp3", RelPath: []byte(p + ".mp3"),
				Kind: model.FileAudio, Size: 100, MTimeNS: 7,
				ContentHash: "c-" + p, EssenceHash: "ess-" + p, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: "Track " + p,
				SortKey: model.SortKey("Track " + p), IdentityKey: "essence:ess-" + p,
			},
			Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals", TrackNo: 1},
		})
		if err != nil {
			t.Fatalf("PutScannedTrack %s: %v", p, err)
		}
		files[p] = res.FilePID
	}
	// One shared rip carved into two more members of the same album.
	var vts []model.VirtualTrack
	for i, title := range []string{"Rip 1", "Rip 2"} {
		start := int64(i) * 750
		var end int64
		if i == 0 {
			end = 750
		}
		vts = append(vts, model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: title,
				SortKey: model.SortKey(title), IdentityKey: identity.VirtualTrackKey("rip-e", i+1, start),
			},
			Track:       model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals", TrackNo: i + 3},
			StartFrames: start, EndFrames: end,
		})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/rip.flac"), DisplayPath: "/lib/rip.flac", RelPath: []byte("rip.flac"),
			Kind: model.FileAudio, Size: 600_000, MTimeNS: 1,
			ContentHash: "rip-c", EssenceHash: "rip-e", DurationMS: 30_000, ScanState: model.ScanIndexed,
		},
		Tracks: vts,
	}); err != nil {
		t.Fatalf("put virtual rip: %v", err)
	}
	ripPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM file WHERE display_path = '/lib/rip.flac'"))
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("albums = %d, want the rip's members on the same album as the files", n)
	}
	albumID := int64(scalarQueryInt(t, db, "SELECT id FROM album"))
	albumPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album"))

	plan := func(t *testing.T, scope *model.EnrichScope) map[model.PID]model.EntityFieldFile {
		t.Helper()
		rows, err := st.EnrichedAlbumLabelFiles(ctx, scope)
		if err != nil {
			t.Fatalf("EnrichedAlbumLabelFiles: %v", err)
		}
		out := map[model.PID]model.EntityFieldFile{}
		for _, r := range rows {
			if _, dup := out[r.FilePID]; dup {
				t.Fatalf("file %s planned twice: a shared file appears once", r.FilePID)
			}
			out[r.FilePID] = r
		}
		return out
	}
	if got := plan(t, nil); len(got) != 0 {
		t.Fatalf("planned %d files before any label was filled", len(got))
	}
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: albumID, PID: albumPID,
		Matched: true, Provider: "discogs", Fields: map[string]string{"label": "Harvest"},
	}); err != nil {
		t.Fatalf("ApplyAlbumFields: %v", err)
	}
	got := plan(t, nil)
	if len(got) != 3 {
		t.Fatalf("planned files = %d, want the two members and the shared rip", len(got))
	}
	for _, pid := range []model.PID{files["a"], files["b"], ripPID} {
		r, ok := got[pid]
		if !ok {
			t.Fatalf("file %s missing from the plan", pid)
		}
		if r.EntityType != model.MergeAlbum || r.Field != "label" || r.Value != "Harvest" || r.UpdatedAt == 0 {
			t.Errorf("file %s planned as %+v, want the album's label with its write time", pid, r)
		}
		if len(r.Path) == 0 || r.Size == 0 || r.MTimeNS == 0 {
			t.Errorf("file %s planned without its on-disk state: %+v", pid, r)
		}
		if r.Shared != (pid == ripPID) {
			t.Errorf("file %s shared = %v, want only the rip flagged", pid, r.Shared)
		}
	}
	labelAt := got[files["a"]].UpdatedAt

	// Settling a file at the label's time takes it out of the plan.
	if err := st.SettleEnrichmentWrite(ctx, files["a"], labelAt); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := st.SettleEnrichmentWrite(ctx, ripPID, labelAt); err != nil {
		t.Fatalf("settle rip: %v", err)
	}
	got = plan(t, nil)
	if len(got) != 1 || got[files["b"]].Value != "Harvest" {
		t.Fatalf("planned files after settling = %v, want b.mp3 alone", got)
	}

	// A scoped run plans only the files within its scope: its items, its albums'
	// members, and its release groups' albums' members.
	itemB := itemRowID(t, db, model.PID(scalarQueryStr(t, db, "SELECT pid FROM playable_item WHERE title = 'Track b'")))
	if got := plan(t, &model.EnrichScope{FieldsItemIDs: []int64{itemB + 1000}}); len(got) != 0 {
		t.Errorf("a scope reaching nothing planned %d files", len(got))
	}
	if got := plan(t, &model.EnrichScope{FieldsItemIDs: []int64{itemB}}); len(got) != 1 {
		t.Errorf("an item scope planned %d files, want b.mp3 alone", len(got))
	}
	if got := plan(t, &model.EnrichScope{AlbumIDs: []int64{albumID}}); len(got) != 1 {
		t.Errorf("an album scope planned %d files, want the one member still owed", len(got))
	}
	rgID := int64(scalarQueryInt(t, db, "SELECT release_group_id FROM album"))
	if got := plan(t, &model.EnrichScope{ReleaseGroupIDs: []int64{rgID}}); len(got) != 1 {
		t.Errorf("a release-group scope planned %d files, want the one member still owed", len(got))
	}

	// A member with enrichment fields of its own is the item select's to write: the
	// label rides that row, so one rewrite carries both.
	pidA := model.PID(scalarQueryStr(t, db, "SELECT pid FROM playable_item WHERE title = 'Track a'"))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemRowID(t, db, pidA), PID: pidA, Matched: true, Provider: "discogs",
		Fields: map[string]string{"bpm": "120"},
	}); err != nil {
		t.Fatalf("ApplyItemFields: %v", err)
	}
	rows, err := st.EnrichmentWriteback(ctx, nil)
	if err != nil {
		t.Fatalf("EnrichmentWriteback: %v", err)
	}
	if len(rows) != 1 || rows[0].FilePID != files["a"] || rows[0].Label != "Harvest" || rows[0].LabelUpdatedAt != labelAt {
		t.Fatalf("item rows = %+v, want a.mp3 owed its bpm with the album label riding along", rows)
	}
	if rows[0].Fields["bpm"] != "120" {
		t.Errorf("a.mp3 fields = %v, want the bpm", rows[0].Fields)
	}

	// A file whose item is no longer present is not planned: the write would fail on
	// every pass and could never settle.
	if _, err := st.MarkItemMissing(ctx, model.PID(scalarQueryStr(t, db, "SELECT pid FROM playable_item WHERE title = 'Track b'"))); err != nil {
		t.Fatalf("MarkItemMissing: %v", err)
	}
	if got := plan(t, nil); len(got) != 0 {
		t.Errorf("planned %d files with the only owed member missing, want none", len(got))
	}

	// A user's edit takes the label out of the enrichment write-back's hands, on the
	// item select's rows as much as the plan's.
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID, map[string]string{"label": "Harvest Records"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("EditEntityFields: %v", err)
	}
	if got = plan(t, nil); len(got) != 0 {
		t.Errorf("planned %d files for a user-curated label, want none", len(got))
	}
	rows, err = st.EnrichmentWriteback(ctx, nil)
	if err != nil {
		t.Fatalf("EnrichmentWriteback after the edit: %v", err)
	}
	if len(rows) != 1 || rows[0].Label != "" || rows[0].LabelUpdatedAt != 0 {
		t.Errorf("item rows after the edit = %+v, want a.mp3 with no label riding along", rows)
	}
}

// TestEnrichmentWritebackDropsALabelTheAlbumNoLongerHolds: the owed test's label term
// counts only while the album still holds a label. Merging an enriched album into one
// with no label of its own moves the label's curation row onto the survivor and leaves
// its label column empty, and a write settles a file at the label's time only when there
// is a label to write, so an unguarded term hands the same file back on every pass for
// good.
func TestEnrichmentWritebackDropsALabelTheAlbumNoLongerHolds(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	itemOf := map[string]model.PID{}
	for _, a := range []struct{ file, album string }{{"a", "Animals"}, {"b", "Meddle"}} {
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte("/lib/" + a.file + ".mp3"), DisplayPath: "/lib/" + a.file + ".mp3",
				RelPath: []byte(a.file + ".mp3"), Kind: model.FileAudio, Size: 100, MTimeNS: 7,
				ContentHash: "c-" + a.file, EssenceHash: "ess-" + a.file, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: "Track " + a.file,
				SortKey: model.SortKey("Track " + a.file), IdentityKey: "essence:ess-" + a.file,
			},
			Track: model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: a.album, TrackNo: 1},
		})
		if err != nil {
			t.Fatalf("PutScannedTrack %s: %v", a.file, err)
		}
		itemOf[a.album] = res.ItemPID
	}
	albumPID := func(name string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = '"+name+"'"))
	}
	albumID := func(name string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM album WHERE title = '"+name+"'"))
	}

	// Both items carry an enrichment field, so both files reach the item select.
	for album, pid := range itemOf {
		if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
			ItemID: itemRowID(t, db, pid), PID: pid, Matched: true, Provider: "p",
			Fields: map[string]string{"composer": "Roger Waters"},
		}); err != nil {
			t.Fatalf("fill %s: %v", album, err)
		}
	}
	// Only Animals is given a label.
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: albumID("Animals"), PID: albumPID("Animals"),
		Matched: true, Provider: "discogs", Fields: map[string]string{"label": "Harvest"},
	}); err != nil {
		t.Fatalf("ApplyAlbumFields: %v", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, albumPID("Meddle"), albumPID("Animals")); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// The state the guard is about: the survivor holds the enrichment label row with no
	// label of its own.
	survivor := albumID("Meddle")
	if got := scalarQueryStr(t, db, "SELECT COALESCE(label,'') FROM album WHERE id = "+strconv.FormatInt(survivor, 10)); got != "" {
		t.Fatalf("survivor label = %q, want the merge to leave it empty", got)
	}
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND field='label'"+
		" AND source='enrichment' AND entity_id = "+strconv.FormatInt(survivor, 10)); n != 1 {
		t.Fatalf("enrichment label rows on the survivor = %d, want the merge to have moved the loser's", n)
	}

	rows, err := st.EnrichmentWriteback(ctx, nil)
	if err != nil {
		t.Fatalf("writeback: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no files owed their composer")
	}
	// Settle each file the way the write-back does: at the newest value it could write,
	// which is the item's own, since there is no label to carry.
	for _, r := range rows {
		if r.Label != "" {
			t.Fatalf("row %s carries label %q, want none from an album with an empty label", r.FilePID, r.Label)
		}
		if err := st.SettleEnrichmentWrite(ctx, r.FilePID, r.Newest); err != nil {
			t.Fatalf("settle %s: %v", r.FilePID, err)
		}
	}
	again, err := st.EnrichmentWriteback(ctx, nil)
	if err != nil {
		t.Fatalf("writeback again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("still owed %+v after settling every writable value, want nothing", again)
	}
}

// TestEnrichmentWritebackFlagsASharedFile: a file several items back, or one carrying an
// offset window on the edge the select walks, survives the virtual-track gate and comes
// back flagged so the caller refuses it once. The settle stamp is per file while the
// newest value is per item, so rewriting the file for one item would settle it past the
// other's value and lose it.
func TestEnrichmentWritebackFlagsASharedFile(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	rw := writeConn(t, dbPath)
	pid := seedEnrichTrack(t, st, lib.ID)
	itemID := itemRowID(t, db, pid)
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemID, PID: pid, Matched: true, Provider: "p",
		Fields: map[string]string{"composer": "Roger Waters"},
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	owed := func(t *testing.T) model.EnrichedTagRow {
		t.Helper()
		rows, err := st.EnrichmentWriteback(ctx, nil)
		if err != nil {
			t.Fatalf("writeback: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want the one owed file", len(rows))
		}
		return rows[0]
	}
	if r := owed(t); r.Shared {
		t.Fatalf("a file backing one whole item reported shared")
	}

	// A second item on the same file. Its edge carries no offset, so the select's own
	// gate lets it through and only the shared test catches it.
	fileID := int64(scalarQueryInt(t, db, "SELECT id FROM file"))
	if _, err := rw.ExecContext(ctx, `INSERT INTO playable_item(pid, kind, state, title, sort_key, identity_key, created_at, updated_at)
		VALUES ('01J0SHARED0000000000000000','track','present','Second','second','essence:shared',1,1)`); err != nil {
		t.Fatalf("insert second item: %v", err)
	}
	if _, err := rw.ExecContext(ctx, `INSERT INTO item_file(item_id, file_id, role, position)
		SELECT id, ?, 'primary', 0 FROM playable_item WHERE pid = '01J0SHARED0000000000000000'`, fileID); err != nil {
		t.Fatalf("insert second edge: %v", err)
	}
	if r := owed(t); !r.Shared {
		t.Errorf("a file two items back reported unshared, so the write-back would rewrite it per item")
	}
}

// backdateMisses ages every no-match marker in one catalog and returns the stamp it
// wrote, so a test can pin a cutoff exactly against it. Never a sleep: the coarse
// Windows clock makes two stamps taken in one run equal.
func backdateMisses(t *testing.T, db *sql.DB, age time.Duration) int64 {
	t.Helper()
	stamp := time.Now().Add(-age).UnixNano()
	if _, err := db.Exec("UPDATE entity_enrichment SET enriched_at = ? WHERE matched = 0", stamp); err != nil {
		t.Fatalf("backdate misses: %v", err)
	}
	return stamp
}

// TestEnrichQueuesRetryAnExpiredMiss walks every queue through the four sweeps. The
// point of the window is that a no-match marker stops being permanent: a provider that
// had no picture of an artist last month is asked again this month, without the forced
// run that would re-ask about every matched entity too.
func TestEnrichQueuesRetryAnExpiredMiss(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	albumTrack(t, st, lib.ID, "ess-a", "Wish You Were Here", "0075992739429", "")

	artistID := int64(scalarQueryInt(t, db, "SELECT id FROM artist WHERE name='PF'"))
	artistPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM artist WHERE name='PF'"))
	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group"))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group"))
	albumID := albumIDByTitle(t, db, "Wish You Were Here")
	albumPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Wish You Were Here'"))
	itemID := int64(scalarQueryInt(t, db, "SELECT id FROM playable_item"))
	itemPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM playable_item"))

	// One queue per marker type, so a sweep that misses one of them is visible by name
	// rather than as a count that happens to add up.
	queues := []struct {
		name string
		walk func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error)
	}{
		{"artist", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ArtistsNeedingEnrichment(ctx, q, 0, 100, nil)
		}},
		{"release group", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ReleaseGroupsNeedingEnrichment(ctx, q, 0, 100, false, nil)
		}},
		{"album release", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.AlbumsNeedingReleaseMatch(ctx, q, 0, 100, nil)
		}},
		{"aux art", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ReleaseGroupsNeedingAuxArt(ctx, q, 0, 100, nil)
		}},
		{"artist art", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ArtistsNeedingArtBackfill(ctx, q, 0, 100, nil)
		}},
		{"lyrics", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ItemsNeedingLyrics(ctx, q, 0, 100, nil)
		}},
		{"track fields", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.ItemsNeedingFields(ctx, q, 0, 100, model.KindTrack, nil)
		}},
		{"album fields", func(q model.EnrichQueueOptions) ([]model.EnrichTarget, error) {
			return st.AlbumsNeedingFields(ctx, q, 0, 100, nil)
		}},
	}
	countAll := model.EnrichCountOptions{
		Identity: true, Albums: true, AuxArt: true, ArtistArt: true,
		Lyrics: true, TrackFields: true, AlbumFields: true,
	}
	walked := func(t *testing.T, q model.EnrichQueueOptions) map[string]int {
		t.Helper()
		got := map[string]int{}
		for _, queue := range queues {
			targets, err := queue.walk(q)
			if err != nil {
				t.Fatalf("%s queue: %v", queue.name, err)
			}
			got[queue.name] = len(targets)
		}
		return got
	}
	assertEach := func(t *testing.T, got map[string]int, want int, why string) {
		t.Helper()
		for _, queue := range queues {
			if got[queue.name] != want {
				t.Errorf("%s queue returned %d, want %d (%s)", queue.name, got[queue.name], want, why)
			}
		}
	}

	assertEach(t, walked(t, model.EnrichQueueOptions{}), 1, "nothing has been looked up yet")

	misses := []struct {
		name  string
		apply func() error
	}{
		{"artist", func() error {
			return st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{ArtistID: artistID, PID: artistPID})
		}},
		{"release group", func() error {
			return st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{ReleaseGroupID: rgID, PID: rgPID})
		}},
		{"album release", func() error {
			return st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{AlbumID: albumID, PID: albumPID})
		}},
		{"aux art", func() error {
			return st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{ReleaseGroupID: rgID, PID: rgPID})
		}},
		{"artist art", func() error {
			return st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: artistID, PID: artistPID})
		}},
		{"lyrics", func() error {
			return st.ApplyLyricsEnrichment(ctx, model.LyricsEnrichment{ItemID: itemID, PID: itemPID})
		}},
		{"track fields", func() error {
			return st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: itemPID})
		}},
		{"album fields", func() error {
			return st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{AlbumID: albumID, PID: albumPID})
		}},
	}
	markMisses := func(t *testing.T) {
		t.Helper()
		for _, m := range misses {
			if err := m.apply(); err != nil {
				t.Fatalf("mark %s miss: %v", m.name, err)
			}
		}
	}
	markMisses(t)
	assertEach(t, walked(t, model.EnrichQueueOptions{}), 0, "every target carries a marker")

	rw := writeConn(t, dbPath)
	stamp := backdateMisses(t, rw, 40*24*time.Hour)
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixNano()
	retry := model.EnrichQueueOptions{Sweep: model.SweepRetry, MissCutoff: cutoff}
	due := model.EnrichQueueOptions{Sweep: model.SweepDue, MissCutoff: cutoff}

	assertEach(t, walked(t, retry), 1, "the markers are older than the cutoff")
	assertEach(t, walked(t, due), 1, "due is the two sweeps' union")
	assertEach(t, walked(t, model.EnrichQueueOptions{Sweep: model.SweepFresh, MissCutoff: cutoff}), 0,
		"the fresh sweep never looks at markers")
	assertEach(t, walked(t, model.EnrichQueueOptions{Sweep: model.SweepRetry}), 0,
		"a zero cutoff expires nothing")

	// The count shares the predicate with the queues, so the denominator a heartbeat
	// divides by has to equal what the sweeps will actually walk.
	dueCount, err := st.CountEntitiesNeedingEnrichment(ctx, due, countAll, nil)
	if err != nil {
		t.Fatalf("due count: %v", err)
	}
	if dueCount != len(queues) {
		t.Errorf("SweepDue count = %d, want %d (one per queue)", dueCount, len(queues))
	}

	// The boundary: a marker stamped exactly at the cutoff has expired.
	assertEach(t, walked(t, model.EnrichQueueOptions{Sweep: model.SweepRetry, MissCutoff: stamp}), 1,
		"a stamp equal to the cutoff expires")
	assertEach(t, walked(t, model.EnrichQueueOptions{Sweep: model.SweepRetry, MissCutoff: stamp - 1}), 0,
		"a stamp one nanosecond after the cutoff is still inside the window")

	// Re-asking rewrites enriched_at, which is what re-arms the window: the same cutoff
	// stops selecting a target the run just asked about again.
	markMisses(t)
	assertEach(t, walked(t, retry), 0, "the re-ask refreshed every stamp")
	assertEach(t, walked(t, due), 0, "due follows the refreshed stamps")

	// A matched marker is durable. The artist-art backfill shows it without a second
	// fixture: a provider that answered but filled no slot leaves the vacancy the queue
	// gates on standing, so only the marker separates the sweeps here.
	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{
		ArtistID: artistID, PID: artistPID, Matched: true, Provider: "deezer",
	}); err != nil {
		t.Fatalf("mark an artist-art match: %v", err)
	}
	if _, err := rw.Exec("UPDATE entity_enrichment SET enriched_at = ? WHERE matched = 1", stamp); err != nil {
		t.Fatalf("backdate the match: %v", err)
	}
	if got := walked(t, retry)["artist art"]; got != 0 {
		t.Errorf("retry sweep returned %d matched artist-art targets, want 0 (a match is durable)", got)
	}
	if got := walked(t, due)["artist art"]; got != 0 {
		t.Errorf("due sweep returned %d matched artist-art targets, want 0", got)
	}
	if got := walked(t, model.EnrichQueueOptions{Sweep: model.SweepAll})["artist art"]; got != 1 {
		t.Errorf("forced sweep returned %d artist-art targets, want 1", got)
	}
	assertStoreVerifyClean(t, st)
}

// folderTrack persists one track at an explicit path and with no release-group mbid, so
// the album title is what keys its group and a test can put two albums in one folder.
// The album identity key embeds the folder, which is what lets a retag move the file onto
// a sibling album there and strand the one it left.
func folderTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence string, rev int, tr model.Track) {
	t.Helper()
	tr.Artist, tr.AlbumArtist = "PF", "PF"
	tr.TrackNo = 1
	_, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: int64(rev),
			ContentHash: "c-" + essence + "-" + strconv.Itoa(rev), EssenceHash: essence,
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + essence,
			SortKey: model.SortKey("T-" + essence), IdentityKey: "essence:" + essence,
		},
		Track: tr,
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
}

// albumArtMarkers counts one album's art-backfill markers.
func albumArtMarkers(t *testing.T, db *sql.DB, albumID int64) int {
	t.Helper()
	return scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=?", albumID)
}

// TestAlbumsNeedingArtGuards pins the album-art queue's gate. An album is asked about
// only when it carries an identifier a provider can key on, since the releases of one
// group share a title and a title-keyed ask can only return the wrong edition; the
// vacancy it is asked about is per slot, so an install with no aux-capable provider does
// not mark an album for a slot nothing could answer; and a settled front (a member
// track's embedded cover included), a whole art lock, an existing marker, and the ghost
// heuristic each keep it out.
func TestAlbumsNeedingArtGuards(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-mbid", "ByMBID", 1, model.Track{})
	editionTrack(t, st, lib.ID, "ess-bc", "ByBarcode", 1, model.Track{Barcode: "0075992739429"})
	editionTrack(t, st, lib.ID, "ess-cat", "ByCatNo", 1, model.Track{CatalogNumber: "SHVL 804"})
	editionTrack(t, st, lib.ID, "ess-plain", "TitleOnly", 1, model.Track{})
	editionTrackWithCover(t, st, lib.ID, "ess-emb", "Embedded", 1,
		model.Track{Barcode: "5099902154251"}, pngFixture())
	editionTrack(t, st, lib.ID, "ess-lock", "Locked", 1, model.Track{Barcode: "0724382955528"})
	editionTrack(t, st, lib.ID, "ess-mark", "Marked", 1, model.Track{Barcode: "0724383024124"})
	// A ghost: identified while it had members, then stranded by a retag that cleared the
	// album tag and left its one track ungrouped. It qualifies on every other part of the
	// gate, which is what makes it the likeliest thing the heuristic has to catch.
	folderTrack(t, st, lib.ID, "/lib/ghost/1.mp3", "ess-ghost", 1,
		model.Track{Album: "Ghost", Barcode: "0724383024131"})
	folderTrack(t, st, lib.ID, "/lib/ghost/1.mp3", "ess-ghost", 2, model.Track{})
	if n := scalarQueryInt(t, db,
		`SELECT COUNT(*) FROM track t JOIN album al ON al.id = t.album_id WHERE al.title='Ghost'`); n != 0 {
		t.Fatalf("the Ghost album still backs %d tracks; the fixture did not strand it", n)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM album WHERE title='Ghost' AND COALESCE(barcode,'') <> ''"); n != 1 {
		t.Fatalf("the Ghost album row is gone; only the heuristic should be keeping it out")
	}

	albumPID := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title=?", title))
	}
	setEntityMBID(t, st, model.MergeAlbum, string(albumPID("ByMBID")), relTestOneMBID, false)
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, albumPID("Locked"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	markedID := albumIDByTitle(t, db, "Marked")
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: markedID, PID: albumPID("Marked"),
	}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	queued := func(slots model.AlbumArtSlots, q model.EnrichQueueOptions) map[string]bool {
		t.Helper()
		targets, err := st.AlbumsNeedingArt(ctx, q, 0, 100, slots, nil)
		if err != nil {
			t.Fatalf("AlbumsNeedingArt: %v", err)
		}
		got := map[string]bool{}
		for _, target := range targets {
			got[target.Name] = true
		}
		// The count is built from the same gate, so it has to agree exactly.
		n, err := st.CountEntitiesNeedingEnrichment(ctx, q, model.EnrichCountOptions{AlbumArt: slots}, nil)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != len(targets) {
			t.Errorf("count = %d for %d queued albums at slots %+v", n, len(targets), slots)
		}
		return got
	}

	front := queued(model.AlbumArtSlots{Front: true}, model.EnrichQueueOptions{})
	wantFront := map[string]bool{"ByMBID": true, "ByBarcode": true, "ByCatNo": true}
	for _, title := range []string{"ByMBID", "ByBarcode", "ByCatNo", "TitleOnly", "Embedded", "Locked", "Marked", "Ghost"} {
		if front[title] != wantFront[title] {
			t.Errorf("front sweep queued %q = %v, want %v", title, front[title], wantFront[title])
		}
	}

	// The embedded cover settles the front and nothing else, so the aux slots are still
	// a question worth asking.
	aux := queued(model.AlbumArtSlots{Aux: true}, model.EnrichQueueOptions{})
	if !aux["Embedded"] {
		t.Error("the aux sweep skipped an album whose front is settled but whose aux slots are empty")
	}
	for _, title := range []string{"TitleOnly", "Locked", "Marked", "Ghost"} {
		if aux[title] {
			t.Errorf("the aux sweep queued %q", title)
		}
	}

	// Neither slot askable is the phase not running at all.
	if got := queued(model.AlbumArtSlots{}, model.EnrichQueueOptions{}); len(got) != 0 {
		t.Errorf("queued %v with no askable slot, want none", got)
	}

	// A forced run reaches the marked album; only its marker was keeping it out.
	if !queued(model.AlbumArtSlots{Front: true}, model.EnrichQueueOptions{Sweep: model.SweepAll})["Marked"] {
		t.Error("a forced sweep did not re-queue the marked album")
	}

	// The request carries the identifiers, which is what the walk exists to send.
	targets, err := st.AlbumsNeedingArt(ctx, model.EnrichQueueOptions{}, 0, 100, model.AlbumArtSlots{Front: true}, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingArt: %v", err)
	}
	for _, target := range targets {
		if target.MBID == "" && target.Barcode == "" && target.CatalogNumber == "" {
			t.Errorf("queued %q with no identifier at all", target.Name)
		}
		if target.HasArt {
			t.Errorf("queued %q with HasArt set on the front sweep", target.Name)
		}
	}
}

// TestApplyAlbumArtBackfillFillsAndMarks: the front lands only where the art chain
// resolves nothing, so a member track's embedded cover stays the album's picture; the
// marker is written either way, so an album nothing serves costs one pass; the entity
// delta rides on an image actually landing; and an album merged away between the queue
// page and the write gets neither.
func TestApplyAlbumArtBackfillFillsAndMarks(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	editionTrack(t, st, lib.ID, "ess-bare", "Bare", 1, model.Track{Barcode: "0075992739429"})
	editionTrackWithCover(t, st, lib.ID, "ess-emb", "Embedded", 1,
		model.Track{Barcode: "5099902154251"}, pngFixture())

	albumDeltas := func() int {
		t.Helper()
		return scalarQueryInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='album' AND op='update'")
	}
	before := albumDeltas()

	bareID := albumIDByTitle(t, db, "Bare")
	barePID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Bare'"))
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: bareID, PID: barePID, Matched: true, Provider: "coverartarchive",
		Art: enrichArtImg("bare-front", "coverartarchive"),
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Bare): %v", err)
	}
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", bareID); got != "bare-front" {
		t.Errorf("bare album front = %q, want the fetched cover", got)
	}
	if albumDeltas() != before+1 {
		t.Error("a landed cover emitted no album delta")
	}

	// The embedded cover already answers, so the front is dropped and only the aux role
	// lands, and the album keeps deriving its picture from the track.
	embID := albumIDByTitle(t, db, "Embedded")
	embPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Embedded'"))
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: embID, PID: embPID, Matched: true, Provider: "fanart",
		Art:    enrichArtImg("late-front", "fanart"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("emb-back", "fanart")},
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Embedded): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", embID); n != 0 {
		t.Errorf("the album stored %d front rows; the track's cover already answers for it", n)
	}
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='album' AND entity_id=? AND role='back'", embID); got != "emb-back" {
		t.Errorf("album back = %q, want the offered one beside the settled front", got)
	}

	// Nothing offered still marks, and writes no delta.
	editionTrack(t, st, lib.ID, "ess-miss", "Missed", 1, model.Track{Barcode: "0724382955528"})
	missID := albumIDByTitle(t, db, "Missed")
	missPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Missed'"))
	deltas := albumDeltas()
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: missID, PID: missPID}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Missed): %v", err)
	}
	if albumArtMarkers(t, db, missID) != 1 {
		t.Error("a no-match wrote no marker; the album would be asked again every run")
	}
	if albumDeltas() != deltas {
		t.Error("a no-match emitted an album delta")
	}
	if got := scalarQueryStr(t, db,
		"SELECT provider FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=?", missID); got != "none" {
		t.Errorf("no-match marker provider = %q, want none", got)
	}

	// A rowid that went away between the queue page and the write takes nothing at all:
	// a marker there would silence whatever album inherits the id.
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: 424242, PID: model.NewPID(), Matched: true, Provider: "mock",
		Art: enrichArtImg("ghost-front", "mock"),
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill on a vanished album: %v", err)
	}
	if albumArtMarkers(t, db, 424242) != 0 {
		t.Error("a vanished album took a stranded marker")
	}
	assertStoreVerifyClean(t, st)
}

// TestAlbumArtMarkerLifecycle walks every writer that has to drop the marker, because
// each one either opens a vacancy the marker says was asked about or hands a dead rowid
// to a new album.
func TestAlbumArtMarkerLifecycle(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	mark := func(title string) int64 {
		t.Helper()
		id := albumIDByTitle(t, db, title)
		pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title=?", title))
		if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: id, PID: pid}); err != nil {
			t.Fatalf("mark %s: %v", title, err)
		}
		if albumArtMarkers(t, db, id) != 1 {
			t.Fatalf("%s did not take a marker", title)
		}
		return id
	}

	// A retag that lands a barcode is new evidence: the earlier ask went out without one.
	editionTrack(t, st, lib.ID, "ess-scan", "Scanned", 1, model.Track{})
	scanID := mark("Scanned")
	editionTrack(t, st, lib.ID, "ess-scan", "Scanned", 2, model.Track{Barcode: "0075992739429"})
	if albumArtMarkers(t, db, scanID) != 0 {
		t.Error("a scan that filled the barcode left the art marker standing")
	}

	// So is an edited identifier, and an edited mbid on either side of the switch.
	editionTrack(t, st, lib.ID, "ess-edit", "Edited", 1, model.Track{})
	editID := mark("Edited")
	editPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Edited'"))
	if _, err := st.EditEntityFields(ctx, model.MergeAlbum, editPID,
		map[string]string{"barcode": "5099902154251"}, model.Attribution{Source: model.SourceUser},
		model.LockOf(false), false); err != nil {
		t.Fatalf("edit barcode: %v", err)
	}
	if albumArtMarkers(t, db, editID) != 0 {
		t.Error("an edited barcode left the art marker standing")
	}
	mark("Edited")
	setEntityMBID(t, st, model.MergeAlbum, string(editPID), relTestOneMBID, false)
	if albumArtMarkers(t, db, editID) != 0 {
		t.Error("an edited mbid left the art marker standing")
	}

	// Clearing the album's own front, and releasing its art lock, each open a vacancy.
	editionTrack(t, st, lib.ID, "ess-clear", "Cleared", 1, model.Track{Barcode: "0724382955528"})
	clearID := albumIDByTitle(t, db, "Cleared")
	clearPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Cleared'"))
	if err := st.SetEntityArt(ctx, model.ArtAlbum, clearPID, model.ArtRoleFront, pngFixture(), "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	mark("Cleared")
	if err := st.SetEntityArt(ctx, model.ArtAlbum, clearPID, model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear album art: %v", err)
	}
	if albumArtMarkers(t, db, clearID) != 0 {
		t.Error("a cleared album front left the art marker standing")
	}
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, clearPID, model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	mark("Cleared")
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, clearPID, model.ArtRoleFront, false); err != nil {
		t.Fatalf("unlock art: %v", err)
	}
	if albumArtMarkers(t, db, clearID) != 0 {
		t.Error("an unlocked album art field left the art marker standing")
	}

	// A member track's embedded cover is usually what answers the album's front, so
	// clearing one opens the album's vacancy too. Both writers of a track's front have
	// to do it: the CLI clears a track through SetItemArt and the proxy hands a track
	// type to SetEntityArt, so a rule in one of them alone fires for one client only.
	trackFrontPID := func(album string) model.PID {
		t.Helper()
		return model.PID(scalarQueryStr(t, db,
			`SELECT pi.pid FROM playable_item pi JOIN track t ON t.item_id = pi.id
			 JOIN album al ON al.id = t.album_id WHERE al.title = ?`, album))
	}
	editionTrackWithCover(t, st, lib.ID, "ess-memb", "Member", 1,
		model.Track{Barcode: "0724383024124"}, pngFixture())
	membID := mark("Member")
	if err := st.SetEntityArt(ctx, model.ArtTrack, trackFrontPID("Member"), model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear track art through SetEntityArt: %v", err)
	}
	if albumArtMarkers(t, db, membID) != 0 {
		t.Error("a cleared member front left the album's art marker standing (SetEntityArt)")
	}

	editionTrackWithCover(t, st, lib.ID, "ess-item", "ItemSurface", 1,
		model.Track{Barcode: "0724383024148"}, pngFixture())
	itemID := mark("ItemSurface")
	if err := st.SetItemArt(ctx, trackFrontPID("ItemSurface"), model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear track art through SetItemArt: %v", err)
	}
	if albumArtMarkers(t, db, itemID) != 0 {
		t.Error("a cleared member front left the album's art marker standing (SetItemArt)")
	}

	// A merge and the orphan sweep both hand the rowid on, so neither may leave a row.
	editionTrack(t, st, lib.ID, "ess-lose", "Loser", 1, model.Track{Barcode: "0724383024131"})
	loserID := mark("Loser")
	loserPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Loser'"))
	winnerPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Scanned'"))
	if _, err := st.MergeEntities(ctx, model.MergeAlbum, winnerPID, []model.PID{loserPID}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if albumArtMarkers(t, db, loserID) != 0 {
		t.Error("a merged-away album left its art marker behind for the next rowid")
	}
	assertStoreVerifyClean(t, st)
}

// TestMergeReopensTheSurvivorsArtQueue: a merge can hand the survivor the loser's release
// id, and the album-art walk gates on exactly that identifier. Only the loser's marker is
// dropped by the merge itself, so without this the survivor's own marker names the state
// before the union and it is never asked with the id it just gained.
func TestMergeReopensTheSurvivorsArtQueue(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	editionTrack(t, st, lib.ID, "ess-win", "Survivor", 1, model.Track{Barcode: "0075992739429"})
	editionTrack(t, st, lib.ID, "ess-lose", "Loser", 1, model.Track{Media: "CD"})

	winPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Survivor'"))
	losePID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Loser'"))
	winID := albumIDByTitle(t, db, "Survivor")
	setEntityMBID(t, st, model.MergeAlbum, string(losePID), relTestOneMBID, false)

	// The survivor was asked about while it carried only a barcode, and nothing answered.
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: winID, PID: winPID}); err != nil {
		t.Fatalf("mark survivor: %v", err)
	}
	if _, err := st.MergeEntities(ctx, model.MergeAlbum, winPID, []model.PID{losePID}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := scalarQueryStr(t, db, "SELECT COALESCE(mbid,'') FROM album WHERE id=?", winID); got != relTestOneMBID {
		t.Fatalf("survivor mbid = %q, want the loser's id unioned on", got)
	}
	if albumArtMarkers(t, db, winID) != 0 {
		t.Error("the survivor kept a marker naming the state before it gained the id")
	}
	assertStoreVerifyClean(t, st)
}

// TestCountEntitiesNeedingEnrichmentCountsAForcedPhaseUnderSweepAll: the heartbeat
// denominator has to cover a phase-scoped force's own walk, or the ratio never reaches
// one; the phases it does not name stay on the run's sweep.
func TestCountEntitiesNeedingEnrichmentCountsAForcedPhaseUnderSweepAll(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	auxRGTrack(t, st, lib.ID, "Matched", "Matched Album", "")
	auxRGTrack(t, st, lib.ID, "Missed", "Missed Album", "")
	artistID := func(name string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM artist WHERE name = ?", name))
	}
	artistPID := func(name string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM artist WHERE name = ?", name))
	}
	for _, in := range []model.ArtistArtBackfill{
		{ArtistID: artistID("Matched"), PID: artistPID("Matched"), Matched: true, Provider: "deezer"},
		{ArtistID: artistID("Missed"), PID: artistPID("Missed")},
	} {
		if err := st.ApplyArtistArtBackfill(ctx, in); err != nil {
			t.Fatalf("mark artist art: %v", err)
		}
	}

	fresh := model.EnrichQueueOptions{Sweep: model.SweepFresh}
	count := func(t *testing.T, opts model.EnrichCountOptions) int {
		t.Helper()
		n, err := st.CountEntitiesNeedingEnrichment(ctx, fresh, opts, nil)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count(t, model.EnrichCountOptions{ArtistArt: true}); n != 0 {
		t.Errorf("fresh count = %d, want 0: both artists carry a marker", n)
	}
	if n := count(t, model.EnrichCountOptions{ArtistArt: true,
		Forced: []model.EnrichPhase{model.EnrichPhaseArtistArt}}); n != 2 {
		t.Errorf("forced count = %d, want 2 (matched and missed alike)", n)
	}
	if n := count(t, model.EnrichCountOptions{ArtistArt: true,
		Forced: []model.EnrichPhase{model.EnrichPhaseLyrics}}); n != 0 {
		t.Errorf("count with an unrelated phase forced = %d, want 0", n)
	}
}

// TestApplyAlbumArtBackfillCopiesTheGroupFront: the album takes the group's row rather
// than a picture, under the same guards a fetched cover passes, and a copy that finds
// nothing leaves the marker unmatched so a still-vacant album is asked again.
func TestApplyAlbumArtBackfillCopiesTheGroupFront(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	editionTrack(t, st, lib.ID, "ess-a", "Taken", 1, model.Track{Barcode: "0075992739429"})
	editionTrack(t, st, lib.ID, "ess-b", "Locked", 1, model.Track{Barcode: "5099902154251"})

	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE mbid = ?", relTestRGMBID))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE mbid = ?", relTestRGMBID))
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: relTestRGMBID,
		Art: enrichArtImg("group-front", "coverartarchive"),
	}); err != nil {
		t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
	}

	albumDeltas := func() int {
		t.Helper()
		return scalarQueryInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='album' AND op='update'")
	}
	takenID := albumIDByTitle(t, db, "Taken")
	takenPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Taken'"))
	copyIn := model.AlbumArtBackfill{
		AlbumID: takenID, PID: takenPID, Matched: true, Provider: "coverartarchive",
		FrontFromGroup: true, GroupFrontHash: "group-front",
	}
	before := albumDeltas()
	if err := st.ApplyAlbumArtBackfill(ctx, copyIn); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Taken): %v", err)
	}
	if got := scalarQueryStr(t, db,
		`SELECT source_hash||'/'||source||'/'||provider FROM art_map
			WHERE entity_type='album' AND entity_id=? AND role='front'`, takenID); got != "group-front/enrichment/coverartarchive" {
		t.Errorf("album front = %q, want the group's row copied whole", got)
	}
	if albumDeltas() != before+1 {
		t.Error("the copied cover emitted no album delta")
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=? AND matched=1", takenID); n != 1 {
		t.Error("the copy wrote an unmatched marker")
	}

	// A second call has nothing to copy: the album already resolves a front.
	before = albumDeltas()
	if err := st.ApplyAlbumArtBackfill(ctx, copyIn); err != nil {
		t.Fatalf("second ApplyAlbumArtBackfill(Taken): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", takenID); n != 1 {
		t.Errorf("album front rows = %d, want the one copy", n)
	}
	if albumDeltas() != before {
		t.Error("a copy that wrote nothing still emitted a delta")
	}

	// A hash the group no longer holds copies nothing and leaves the marker unmatched.
	editionTrack(t, st, lib.ID, "ess-c", "Stale", 1, model.Track{Barcode: "0724382955528"})
	staleID := albumIDByTitle(t, db, "Stale")
	stalePID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Stale'"))
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: staleID, PID: stalePID, Matched: true, Provider: "coverartarchive",
		FrontFromGroup: true, GroupFrontHash: "stale",
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Stale): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", staleID); n != 0 {
		t.Errorf("stale album front rows = %d, want none", n)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=? AND matched=1", staleID); n != 0 {
		t.Error("a copy that found nothing wrote a matched marker; the album would never be asked again")
	}

	// A stale hash beside a landed auxiliary role still marks unmatched, since the front
	// the album was queued for is still vacant. The aux row lands either way.
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: staleID, PID: stalePID, Matched: true, Provider: "coverartarchive",
		FrontFromGroup: true, GroupFrontHash: "stale",
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("stale-back", "fanart")},
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Stale, with aux): %v", err)
	}
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='album' AND entity_id=? AND role='back'", staleID); got != "stale-back" {
		t.Errorf("album back = %q, want the offered one", got)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=? AND matched=1", staleID); n != 0 {
		t.Error("a still-vacant front wrote a matched marker beside the aux fill; it would never be asked again")
	}

	// A front no longer open leaves no vacancy, so the marker stands on what the caller
	// said, the way a fetched cover the same guards drop already does.
	lockedID := albumIDByTitle(t, db, "Locked")
	lockedPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Locked'"))
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, lockedPID, model.ArtRoleFront, true); err != nil {
		t.Fatalf("SetArtLock: %v", err)
	}
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{
		AlbumID: lockedID, PID: lockedPID, Matched: true, Provider: "coverartarchive",
		FrontFromGroup: true, GroupFrontHash: "group-front",
	}); err != nil {
		t.Fatalf("ApplyAlbumArtBackfill(Locked): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=? AND role='front'", lockedID); n != 0 {
		t.Errorf("locked album front rows = %d, want none", n)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND entity_id=? AND matched=1", lockedID); n != 1 {
		t.Error("a locked album left the marker unmatched; there is no vacancy to re-ask about")
	}
}

// TestAlbumsNeedingArtCarriesTheGroupFrontHash: the queue hands a provider its group's
// id and the hash of the front the catalog holds, so it can offer the reuse; a cover
// chosen by hand carries no hash, since it is nothing the provider fetched.
func TestAlbumsNeedingArtCarriesTheGroupFrontHash(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	editionTrack(t, st, lib.ID, "ess-a", "Queued", 1, model.Track{Barcode: "0075992739429"})

	slots := model.AlbumArtSlots{Front: true}
	only := func(t *testing.T) model.EnrichTarget {
		t.Helper()
		got, err := st.AlbumsNeedingArt(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 10, slots, nil)
		if err != nil {
			t.Fatalf("AlbumsNeedingArt: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("queued %d albums, want 1", len(got))
		}
		return got[0]
	}
	if tgt := only(t); tgt.ReleaseGroupMBID != relTestRGMBID || tgt.GroupFrontHash != "" {
		t.Errorf("with no group front: mbid %q / hash %q, want the mbid and an empty hash",
			tgt.ReleaseGroupMBID, tgt.GroupFrontHash)
	}

	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE mbid = ?", relTestRGMBID))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE mbid = ?", relTestRGMBID))
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: relTestRGMBID,
		Art: enrichArtImg("group-front", "coverartarchive"),
	}); err != nil {
		t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
	}
	if tgt := only(t); tgt.GroupFrontHash != "group-front" {
		t.Errorf("with an enrichment front: hash %q, want group-front", tgt.GroupFrontHash)
	}

	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, pngFixture(), "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("hand-set the group front: %v", err)
	}
	if tgt := only(t); tgt.GroupFrontHash != "" {
		t.Errorf("with a user-set front: hash %q, want empty", tgt.GroupFrontHash)
	}
}

// TestReleaseGroupsNeedingEnrichmentCarriesTheGroupFrontHash: the identity queue carries
// the hash of the front the catalog holds, which is what lets a forced re-fetch ask the
// archive conditionally; a hand-set cover carries none, so it is fetched plainly.
func TestReleaseGroupsNeedingEnrichmentCarriesTheGroupFrontHash(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	editionTrack(t, st, lib.ID, "ess-a", "Queued", 1, model.Track{Barcode: "0075992739429"})

	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE mbid = ?", relTestRGMBID))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE mbid = ?", relTestRGMBID))
	only := func(t *testing.T) model.EnrichTarget {
		t.Helper()
		got, err := st.ReleaseGroupsNeedingEnrichment(ctx, model.EnrichQueueOptions{Sweep: model.SweepAll}, 0, 10, false, nil)
		if err != nil {
			t.Fatalf("ReleaseGroupsNeedingEnrichment: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("queued %d groups, want 1", len(got))
		}
		return got[0]
	}
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: relTestRGMBID,
		Art: enrichArtImg("group-front", "coverartarchive"),
	}); err != nil {
		t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
	}
	if tgt := only(t); tgt.GroupFrontHash != "group-front" {
		t.Errorf("with an enrichment front: hash %q, want group-front", tgt.GroupFrontHash)
	}

	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, pngFixture(), "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("hand-set the group front: %v", err)
	}
	if tgt := only(t); tgt.GroupFrontHash != "" {
		t.Errorf("with a user-set front: hash %q, want empty", tgt.GroupFrontHash)
	}
}
