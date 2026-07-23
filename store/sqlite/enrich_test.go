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
		Track: model.Track{Artist: artist, AlbumArtist: artist, MBArtistID: mbArtistID, TrackNo: 1},
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
	if n := countEE(t, db, itemID); n != 1 {
		t.Fatalf("marker rows before delete = %d, want 1", n)
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

func countEE(t *testing.T, db *sql.DB, itemID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='book' AND entity_id=?", itemID).Scan(&n); err != nil {
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
	// artist + one release group here, and the empty book/lyrics lists add zero.
	scope := &model.EnrichScope{ArtistIDs: []int64{oneID}, ReleaseGroupIDs: []int64{rgOneID}}
	n, err := st.CountEntitiesNeedingEnrichment(ctx, true, true, scope)
	if err != nil {
		t.Fatalf("scoped count: %v", err)
	}
	if n != 2 {
		t.Errorf("scoped count = %d, want 2 (artist + release group, empty phases zero)", n)
	}
	// The unscoped count still covers the catalog (2 artists + 2 rgs; the tracks
	// need lyrics lookups too under includeLyrics).
	un, err := st.CountEntitiesNeedingEnrichment(ctx, true, false, nil)
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
	n, err := st.CountEntitiesNeedingEnrichment(ctx, true, false, &model.EnrichScope{ArtistIDs: []int64{ghostID}})
	if err != nil || n != 1 {
		t.Fatalf("scoped ghost count = %d (err %v), want 1", n, err)
	}
}

func scalarQueryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}
