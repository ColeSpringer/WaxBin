package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// entityPIDByCol reads an entity's public id from a column lookup (test-only).
func entityPIDByCol(t *testing.T, st *Store, table, col, val string) model.PID {
	t.Helper()
	var pid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM "+table+" WHERE "+col+" = ?", val).Scan(&pid); err != nil {
		t.Fatalf("no %s with %s=%q: %v", table, col, val, err)
	}
	return model.PID(pid)
}

func entityIDByCol(t *testing.T, st *Store, table, col, val string) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT id FROM "+table+" WHERE "+col+" = ?", val).Scan(&id); err != nil {
		t.Fatalf("no %s with %s=%q: %v", table, col, val, err)
	}
	return id
}

func TestEditEntityAlbumIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Album", genre: "Rock", durationMS: 100,
	})
	albumPID := entityPIDByCol(t, st, "album", "title", "Album")

	if err := st.EditEntityFields(ctx, model.MergeAlbum, albumPID,
		map[string]string{"barcode": "0123456789", "catalog_number": "CAT-1", "label": "Indie Co"},
		model.SourceUser, true, false); err != nil {
		t.Fatalf("edit album: %v", err)
	}

	var barcode, catalog, label string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(barcode,''), COALESCE(catalog_number,''), COALESCE(label,'') FROM album WHERE pid=?",
		string(albumPID)).Scan(&barcode, &catalog, &label); err != nil {
		t.Fatalf("read album cols: %v", err)
	}
	if barcode != "0123456789" || catalog != "CAT-1" || label != "Indie Co" {
		t.Fatalf("album identifiers not written: barcode=%q catalog=%q label=%q", barcode, catalog, label)
	}

	rows, err := st.EntityCuration(ctx, model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	locked := map[string]bool{}
	for _, r := range rows {
		locked[r.Field] = r.Locked
	}
	for _, f := range []string{"barcode", "catalog_number", "label"} {
		if !locked[f] {
			t.Fatalf("field %q should be locked after a user edit", f)
		}
	}

	// db verify stays clean.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("db verify not consistent after entity edit: %+v", rep)
	}
}

func TestScanPopulatesAlbumIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A scanned track carries album-level release identifiers; they land on the album
	// entity, not the denormalized track row.
	in := model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/1.flac"), DisplayPath: "/lib/1.flac", RelPath: []byte("1.flac"),
			Kind: model.FileAudio, Size: 3, MTimeNS: 1, ContentHash: "c1", EssenceHash: "e1",
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "One",
			SortKey: model.SortKey("One"), IdentityKey: "essence:e1",
		},
		Track: model.Track{
			Artist: "Artist", Album: "Album",
			Barcode: "0123456789", Label: "Indie Co", CatalogNumber: "CAT-1",
		},
	}
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}
	var barcode, label, catalog string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(barcode,''), COALESCE(label,''), COALESCE(catalog_number,'') FROM album WHERE title='Album'").
		Scan(&barcode, &label, &catalog); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if barcode != "0123456789" || label != "Indie Co" || catalog != "CAT-1" {
		t.Fatalf("scan did not populate album identifiers: barcode=%q label=%q catalog=%q", barcode, label, catalog)
	}
}

func TestEditEntitySortOverride(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Miles Davis", album: "Album", genre: "Jazz", durationMS: 100,
	})
	artistPID := entityPIDByCol(t, st, "artist", "name", "Miles Davis")

	if err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"sort": "Davis, Miles"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit sort: %v", err)
	}
	var sortKey string
	if err := st.read.QueryRowContext(ctx, "SELECT sort_key FROM artist WHERE pid=?", string(artistPID)).Scan(&sortKey); err != nil {
		t.Fatalf("read sort_key: %v", err)
	}
	if want := model.SortKey("Davis, Miles"); sortKey != want {
		t.Fatalf("sort_key = %q, want %q", sortKey, want)
	}

	// A curated sort override must not count as db-verify drift.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.SortKeyDrift != 0 {
		t.Fatalf("curated sort override counted as drift: %+v", rep)
	}

	// Clearing the override regenerates the sort key from the display name.
	if err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"sort": ""}, model.SourceUser, false, true); err != nil {
		t.Fatalf("clear sort: %v", err)
	}
	if err := st.read.QueryRowContext(ctx, "SELECT sort_key FROM artist WHERE pid=?", string(artistPID)).Scan(&sortKey); err != nil {
		t.Fatalf("read sort_key: %v", err)
	}
	if want := model.SortKey("Miles Davis"); sortKey != want {
		t.Fatalf("cleared sort_key = %q, want regenerated %q", sortKey, want)
	}
}

func TestEnrichRespectsReleaseGroupTypeLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Live Set", genre: "Rock", durationMS: 100,
	})
	rgPID := entityPIDByCol(t, st, "release_group", "title", "Live Set")
	rgID := entityIDByCol(t, st, "release_group", "title", "Live Set")

	// User curates the release-group type and locks it.
	if err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"type": "ep"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit type: %v", err)
	}

	// Enrichment tries to set a different type; the lock must hold.
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, Type: "album",
	}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var typ string
	if err := st.read.QueryRowContext(ctx, "SELECT COALESCE(type,'') FROM release_group WHERE id=?", rgID).Scan(&typ); err != nil {
		t.Fatalf("read type: %v", err)
	}
	if typ != "ep" {
		t.Fatalf("locked release-group type was overwritten by enrichment: got %q, want ep", typ)
	}

	// An invalid type is rejected.
	if err := st.EditEntityFields(ctx, model.MergeReleaseGroup, rgPID,
		map[string]string{"type": "bootleg"}, model.SourceUser, true, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("invalid type should be CodeInvalid, got %v", err)
	}
}

func TestEnrichRespectsArtistMBIDLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Artist", album: "Album", genre: "Rock", durationMS: 100,
	})
	artistPID := entityPIDByCol(t, st, "artist", "name", "Artist")
	artistID := entityIDByCol(t, st, "artist", "name", "Artist")

	// User clears and locks the artist MBID (a locked-empty value the fill-when-empty
	// enrich guard would otherwise refill).
	if err := st.EditEntityFields(ctx, model.MergeArtist, artistPID,
		map[string]string{"mbid": ""}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit mbid: %v", err)
	}
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{
		ArtistID: artistID, PID: artistPID, Matched: true,
		MBID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var mbid string
	if err := st.read.QueryRowContext(ctx, "SELECT COALESCE(mbid,'') FROM artist WHERE id=?", artistID).Scan(&mbid); err != nil {
		t.Fatalf("read mbid: %v", err)
	}
	if mbid != "" {
		t.Fatalf("locked-empty artist mbid was refilled by enrichment: %q", mbid)
	}
}

func TestEditEntityRejectsDuplicateMBID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Alpha", album: "A1", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Beta", album: "A2", genre: "Rock", durationMS: 100,
	})
	alpha := entityPIDByCol(t, st, "artist", "name", "Alpha")
	beta := entityPIDByCol(t, st, "artist", "name", "Beta")

	mbid := "22222222-2222-2222-2222-222222222222"
	if err := st.EditEntityFields(ctx, model.MergeArtist, alpha,
		map[string]string{"mbid": mbid}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	// Setting the SAME mbid on another artist must be refused (enrichment relies on
	// entity-mbid uniqueness).
	if err := st.EditEntityFields(ctx, model.MergeArtist, beta,
		map[string]string{"mbid": mbid}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("duplicate mbid should be CodeConflict, got %v", err)
	}
}

func TestMergeEntityCurationLockedWins(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Beatles", album: "A1", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "The Beatles", album: "A2", genre: "Rock", durationMS: 200,
	})
	survivor := entityPIDByCol(t, st, "artist", "name", "The Beatles")
	loser := entityPIDByCol(t, st, "artist", "name", "Beatles")

	// Survivor curates sort unlocked; loser curates the same field locked.
	if err := st.EditEntityFields(ctx, model.MergeArtist, survivor,
		map[string]string{"sort": "SSort"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("edit survivor: %v", err)
	}
	if err := st.EditEntityFields(ctx, model.MergeArtist, loser,
		map[string]string{"sort": "LSort"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit loser: %v", err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}

	rows, err := st.EntityCuration(ctx, model.MergeArtist, survivor)
	if err != nil {
		t.Fatalf("entity curation: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "sort" {
		t.Fatalf("expected one sort curation row after merge, got %+v", rows)
	}
	// Locked-wins: the survivor keeps its value but inherits the loser's lock.
	if rows[0].Value != "SSort" {
		t.Fatalf("survivor value should win on conflict: got %q", rows[0].Value)
	}
	if !rows[0].Locked {
		t.Fatalf("survivor lock should be unioned from the loser (locked-wins)")
	}
}
