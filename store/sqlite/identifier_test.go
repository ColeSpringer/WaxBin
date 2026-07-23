package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEditNormalizesISRC verifies the edit surface stores an identifier in its
// canonical form: the column and the provenance row both carry the normalized
// value, and a malformed one rejects the edit before any write.
func TestEditNormalizesISRC(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "A", album: "Alp",
	})

	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"isrc": "us-rc1-77-00001"},
		model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	var isrc string
	if err := st.read.QueryRowContext(ctx,
		"SELECT t.isrc FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(res.ItemPID)).Scan(&isrc); err != nil {
		t.Fatalf("read isrc: %v", err)
	}
	if isrc != "USRC17700001" {
		t.Errorf("stored isrc = %q, want the normalized USRC17700001", isrc)
	}
	prov, err := st.FieldProvenance(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	var provValue string
	for _, p := range prov {
		if p.Field == "isrc" {
			provValue = p.Value
		}
	}
	if provValue != "USRC17700001" {
		t.Errorf("provenance value = %q, want the normalized form", provValue)
	}

	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"isrc": "not-an-isrc"},
		model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("malformed isrc = %v, want CodeInvalid", err)
	}
	// The prior value survived the rejected edit.
	if err := st.read.QueryRowContext(ctx,
		"SELECT t.isrc FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(res.ItemPID)).Scan(&isrc); err != nil || isrc != "USRC17700001" {
		t.Errorf("isrc after rejected edit = %q (err %v), want USRC17700001", isrc, err)
	}

	// An empty value still clears (force past the lock the first edit set).
	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"isrc": ""},
		model.SourceUser, true, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := st.read.QueryRowContext(ctx,
		"SELECT t.isrc FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(res.ItemPID)).Scan(&isrc); err != nil || isrc != "" {
		t.Errorf("isrc after clear = %q (err %v), want empty", isrc, err)
	}
}

// TestEditNormalizesBookIdentifiers covers the book fields: isbn separators are
// stripped and asin uppercased, and a malformed isbn rejects the whole edit
// (the valid asin beside it included).
func TestEditNormalizesBookIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/book.m4b", essence: "be1", content: "bc1",
		title: "Memoir", author: "Jane Author", durationMS: 100,
	})

	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{
		"isbn": "978-0-13-468599-1", "asin": "b000fa5kk0",
	}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	var isbn, asin string
	if err := st.read.QueryRowContext(ctx,
		"SELECT isbn, asin FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?",
		string(res.ItemPID)).Scan(&isbn, &asin); err != nil {
		t.Fatalf("read book: %v", err)
	}
	if isbn != "9780134685991" || asin != "B000FA5KK0" {
		t.Errorf("stored isbn=%q asin=%q, want normalized forms", isbn, asin)
	}

	err := st.EditItemFields(ctx, res.ItemPID, map[string]string{
		"isbn": "1234", "publisher": "Legit Books",
	}, model.SourceUser, true, false)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("malformed isbn = %v, want CodeInvalid", err)
	}
	var publisher string
	if err := st.read.QueryRowContext(ctx,
		"SELECT publisher FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?",
		string(res.ItemPID)).Scan(&publisher); err != nil || publisher != "" {
		t.Errorf("publisher = %q (err %v), want empty: the malformed isbn must reject the whole edit", publisher, err)
	}
}

// TestEntityEditNormalizesBarcode verifies the album barcode is stored (and
// curated) in canonical digits.
func TestEntityEditNormalizesBarcode(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Artist", album: "Album",
	})
	albumPID := entityPIDByName(t, st, "album", "title", "Album")

	if err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "0 36000 29145 2"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	var barcode string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(barcode,'') FROM album WHERE pid=?", string(albumPID)).Scan(&barcode); err != nil {
		t.Fatalf("read barcode: %v", err)
	}
	if barcode != "036000291452" {
		t.Errorf("stored barcode = %q, want stripped digits", barcode)
	}
	rows, err := st.EntityCuration(ctx, model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("curation: %v", err)
	}
	for _, r := range rows {
		if r.Field == "barcode" && r.Value != "036000291452" {
			t.Errorf("curation value = %q, want the normalized form", r.Value)
		}
	}

	if err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "12345"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("malformed barcode = %v, want CodeInvalid", err)
	}
}

// TestBookEnrichmentSkipsInvalidIdentifier verifies the enrichment apply drops a
// malformed identifier (the barcode-as-isbn case) with the rest of the apply
// intact: valid fields fill, the marker is written, nothing aborts.
func TestBookEnrichmentSkipsInvalidIdentifier(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/book.m4b", essence: "be1", content: "bc1",
		title: "Memoir", author: "Jane Author", durationMS: 100,
	})
	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid=?", string(res.ItemPID)).Scan(&itemID); err != nil {
		t.Fatal(err)
	}

	if err := st.ApplyBookEnrichment(ctx, model.BookEnrichment{
		BookItemID: itemID, PID: res.ItemPID, Matched: true, MBID: "mb-1",
		// A genuine checksum-valid product barcode in the isbn slot, the exact
		// shape MusicBrainz releases carry; only the Bookland prefix check
		// tells it apart from an ISBN-13.
		ISBN:      "4006381333931",
		ASIN:      "B000FA5KK0",
		Publisher: "Recorded Books",
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var isbn, asin, publisher string
	if err := st.read.QueryRowContext(ctx,
		"SELECT isbn, asin, publisher FROM book WHERE item_id=?", itemID).Scan(&isbn, &asin, &publisher); err != nil {
		t.Fatalf("read book: %v", err)
	}
	if isbn != "" {
		t.Errorf("isbn = %q, want empty: the malformed value must be skipped, not stored", isbn)
	}
	if asin != "B000FA5KK0" || publisher != "Recorded Books" {
		t.Errorf("asin=%q publisher=%q, want the valid fields filled", asin, publisher)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='book' AND entity_id=?", itemID); n != 1 {
		t.Errorf("enrichment marker rows = %d, want 1 (the apply must complete)", n)
	}
}

// TestItemsByContentHash covers the byte-identity probe: hit, miss, a multi-file
// book resolvable from any part, a CUE rip returning every virtual sibling, and
// the divergence from the essence lookup after a tag write changes the bytes.
func TestItemsByContentHash(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	track := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "te1", content: "tc1", title: "Song", artist: "A", album: "Alp",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/part1.m4b", essence: "bpe1", content: "bpc1",
		title: "Long Book", author: "Jane Author", position: 0, durationMS: 100,
	})
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/part2.m4b", essence: "bpe2", content: "bpc2",
		title: "Long Book", author: "Jane Author", position: 1, durationMS: 100,
	})

	// Hit and miss.
	if items, err := st.ItemsByContentHash(ctx, "tc1"); err != nil || len(items) != 1 || items[0].PID != track.ItemPID {
		t.Errorf("track hit = %v (err %v), want the one track", items, err)
	}
	if items, err := st.ItemsByContentHash(ctx, "no-such-hash"); err != nil || len(items) != 0 {
		t.Errorf("miss = %v (err %v), want empty and no error", items, err)
	}

	// Any part of a multi-file book resolves the book.
	if items, err := st.ItemsByContentHash(ctx, "bpc2"); err != nil || len(items) != 1 ||
		items[0].PID != book.ItemPID || items[0].Kind != model.KindBook {
		t.Errorf("book part hit = %v (err %v), want the book", items, err)
	}

	// A CUE rip's shared file resolves every virtual sibling.
	vt := model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/rip/album.flac"), DisplayPath: "/lib/rip/album.flac",
			RelPath: []byte(filepath.Base("/lib/rip/album.flac")), Kind: model.FileAudio,
			Size: 3, MTimeNS: 1, ContentHash: "ripc1", EssenceHash: "ripe1",
			DurationMS: 60_000, ScanState: model.ScanIndexed,
		},
	}
	for n := 1; n <= 2; n++ {
		start := int64((n - 1) * 2250)
		title := "Rip Track " + string(rune('0'+n))
		vt.Tracks = append(vt.Tracks, model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: title,
				SortKey: model.SortKey(title), IdentityKey: identity.VirtualTrackKey("ripe1", n, start),
			},
			Track:       model.Track{Artist: "Rip Artist", Album: "Rip Album", TrackNo: n},
			StartFrames: start,
		})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, vt); err != nil {
		t.Fatalf("virtual tracks: %v", err)
	}
	if items, err := st.ItemsByContentHash(ctx, "ripc1"); err != nil || len(items) != 2 {
		t.Errorf("cue hit = %d items (err %v), want both virtual siblings", len(items), err)
	}

	// A retag changes the content hash but not the essence: the essence lookup
	// keeps resolving the item, the old content hash goes stale, the new one hits.
	retag := trackSpec{path: "/lib/a/1.flac", essence: "te1", content: "tc1-retagged",
		title: "Song", artist: "A", album: "Alp"}
	putTrack(t, st, lib.ID, retag)
	if items, err := st.ItemsByEssence(ctx, "te1"); err != nil || len(items) != 1 {
		t.Errorf("essence after retag = %d items (err %v), want 1 (the dedup oracle)", len(items), err)
	}
	if items, err := st.ItemsByContentHash(ctx, "tc1"); err != nil || len(items) != 0 {
		t.Errorf("stale content hash = %d items (err %v), want 0 (bytes changed)", len(items), err)
	}
	if items, err := st.ItemsByContentHash(ctx, "tc1-retagged"); err != nil || len(items) != 1 {
		t.Errorf("new content hash = %d items (err %v), want 1", len(items), err)
	}
}
