package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
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

	targets, err := st.ArtistsNeedingEnrichment(ctx, false, 0, 100)
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
