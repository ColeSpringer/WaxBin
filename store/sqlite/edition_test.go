package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
	_ "modernc.org/sqlite"
)

// editionTrack persists one track whose album carries the edition columns; each album
// gets its own folder, since the match key embeds it. rev is the file's revision, and a
// higher one is a retag: it moves the content hash and mtime, because a byte-identical
// rescan skips entity resolution entirely. The essence hash stays, so identity holds.
func editionTrack(t *testing.T, st *sqlite.Store, libID int64, essence, album string, rev int, tr model.Track) {
	t.Helper()
	path := "/lib/" + essence + "/1.mp3"
	tr.Artist, tr.AlbumArtist, tr.Album = "PF", "PF", album
	tr.TrackNo = 1
	tr.MBReleaseGroupID = relTestRGMBID
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

// albumColumns reads an album's two descriptive edition columns.
func albumColumns(t *testing.T, db *sql.DB, title string) (media, country string) {
	t.Helper()
	if err := db.QueryRow(
		"SELECT COALESCE(media,''), COALESCE(country,'') FROM album WHERE title = ?", title).
		Scan(&media, &country); err != nil {
		t.Fatalf("read album %q: %v", title, err)
	}
	return media, country
}

// markerRows returns an album's marker count, plus its provider and matched flag when
// there is exactly one.
func markerRows(t *testing.T, db *sql.DB, albumID int64) (n int, provider string, matched int) {
	t.Helper()
	n = scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album' AND entity_id=?", albumID)
	if n != 1 {
		return n, "", 0
	}
	if err := db.QueryRow(
		"SELECT provider, matched FROM entity_enrichment WHERE entity_type='album' AND entity_id=?",
		albumID).Scan(&provider, &matched); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return n, provider, matched
}

// TestResolveAlbumStoresAndTopsUpEditionColumns covers both halves of the scan write: an
// insert takes the first file's values, a later file fills only the empty columns.
func TestResolveAlbumStoresAndTopsUpEditionColumns(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Insert", 1, model.Track{Media: `12" Vinyl`, Country: "GB"})
	if media, country := albumColumns(t, db, "Insert"); media != `12" Vinyl` || country != "GB" {
		t.Errorf("inserted album = (%q, %q), want (12\" Vinyl, GB)", media, country)
	}

	// A file that gained a country tag tops it up; the stored medium is not overwritten.
	editionTrack(t, st, lib.ID, "ess-b", "TopUp", 1, model.Track{Media: "CD"})
	if media, country := albumColumns(t, db, "TopUp"); media != "CD" || country != "" {
		t.Fatalf("album before the top-up = (%q, %q), want (CD, empty)", media, country)
	}
	editionTrack(t, st, lib.ID, "ess-b", "TopUp", 2, model.Track{Media: "DVD", Country: "JP"})
	if media, country := albumColumns(t, db, "TopUp"); media != "CD" || country != "JP" {
		t.Errorf("album after the top-up = (%q, %q), want (CD, JP): an empty column fills, a full one keeps",
			media, country)
	}
}

// TestEditionColumnLockSurvivesBothWriters: a curated media value keeps against the scan
// top-up, as a curated barcode does.
func TestEditionColumnLockSurvivesBothWriters(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Locked", 1, model.Track{})
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Locked'")
	// Locked deliberately empty, so only the lock probe keeps it.
	if err := st.EditEntityFields(ctx, model.MergeAlbum, model.PID(pid),
		map[string]string{"media": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("lock media: %v", err)
	}
	editionTrack(t, st, lib.ID, "ess-a", "Locked", 2, model.Track{Media: "CD", Country: "GB"})
	media, country := albumColumns(t, db, "Locked")
	if media != "" {
		t.Errorf("locked media = %q, want empty (a curated value keeps)", media)
	}
	if country != "GB" {
		t.Errorf("unlocked country = %q, want GB (the lock is per field)", country)
	}
}

// TestAlbumsNeedingReleaseMatchIncludesEditionEvidence: an album carrying only a medium
// or only a country is now worth a lookup; one carrying nothing still is not.
func TestAlbumsNeedingReleaseMatchIncludesEditionEvidence(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStoreAt(t)

	editionTrack(t, st, lib.ID, "ess-a", "Media Only", 1, model.Track{Media: "CD"})
	editionTrack(t, st, lib.ID, "ess-b", "Country Only", 1, model.Track{Country: "GB"})
	editionTrack(t, st, lib.ID, "ess-c", "Label Only", 1, model.Track{Label: "Harvest"})
	editionTrack(t, st, lib.ID, "ess-d", "Nothing", 1, model.Track{})

	queued, err := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	got := map[string]model.EnrichTarget{}
	for _, q := range queued {
		got[q.Name] = q
	}
	if _, ok := got["Media Only"]; !ok {
		t.Error("an album carrying only a medium should be queued")
	}
	if _, ok := got["Country Only"]; !ok {
		t.Error("an album carrying only a country should be queued")
	}
	// label feeds no tier, so filling it costs no lookup.
	if _, ok := got["Label Only"]; ok {
		t.Error("an album carrying only a label should not be queued")
	}
	if _, ok := got["Nothing"]; ok {
		t.Error("an album carrying no evidence should not be queued")
	}
	if q := got["Media Only"]; q.Media != "CD" {
		t.Errorf("queued target media = %q, want CD", q.Media)
	}
	if q := got["Country Only"]; q.Country != "GB" {
		t.Errorf("queued target country = %q, want GB", q.Country)
	}

	// The heartbeat denominator is built from the same list and must agree.
	n, err := st.CountEntitiesNeedingEnrichment(ctx, false, true, false, nil)
	if err != nil {
		t.Fatalf("CountEntitiesNeedingEnrichment: %v", err)
	}
	albums, err := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if n < len(albums) {
		t.Errorf("count %d is below the %d albums the walk returns; the predicates have drifted", n, len(albums))
	}
}

// TestScanClearsAnUnmatchedAlbumMarker is the lifecycle the widened gate needs: without
// it a barcode retag would never re-queue a media-only album short of --force.
func TestScanClearsAnUnmatchedAlbumMarker(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Retagged", 1, model.Track{Media: "CD"})
	id := albumIDByTitle(t, db, "Retagged")
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Retagged'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: model.PID(pid), Provider: "musicbrainz:edition",
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}
	if n, provider, matched := markerRows(t, db, id); n != 1 || provider != "musicbrainz:edition" || matched != 0 {
		t.Fatalf("marker = (%d, %q, %d), want one unmatched edition marker", n, provider, matched)
	}
	if queued, _ := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil); len(queued) != 0 {
		t.Fatalf("queued %d albums, want 0 while the marker stands", len(queued))
	}

	// The retag lands a barcode, which a stronger tier can decide on.
	editionTrack(t, st, lib.ID, "ess-a", "Retagged", 2, model.Track{Media: "CD", Barcode: "0075992739429"})
	if n, _, _ := markerRows(t, db, id); n != 0 {
		t.Errorf("marker rows = %d, want 0 (new evidence must re-queue the album)", n)
	}
	queued, err := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if len(queued) != 1 || queued[0].Barcode != "0075992739429" {
		t.Errorf("queued = %+v, want the retagged album carrying its new barcode", queued)
	}
}

// TestMatchedAlbumMarkerSurvivesNewEvidence is why the clear is conditional: a matched
// album has an mbid and is never queued again, so clearing it would decrement
// EnrichmentCoverage.Matched with no path back.
func TestMatchedAlbumMarkerSurvivesNewEvidence(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Matched", 1, model.Track{Media: "CD"})
	id := albumIDByTitle(t, db, "Matched")
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Matched'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: model.PID(pid), Matched: true, MBID: relTestOneMBID,
		Reason: "medium", Provider: "musicbrainz:edition",
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}

	editionTrack(t, st, lib.ID, "ess-a", "Matched", 2, model.Track{Media: "CD", Barcode: "0075992739429"})
	n, provider, matched := markerRows(t, db, id)
	if n != 1 || matched != 1 {
		t.Errorf("marker = (%d rows, matched %d), want one surviving matched marker", n, matched)
	}
	if provider != "musicbrainz:edition" {
		t.Errorf("marker provider = %q, want musicbrainz:edition so the weaker write stays findable", provider)
	}
}

// TestEntityEditClearsAnUnmatchedAlbumMarker: the second writer of an album's evidence
// clears the marker on the same rule. Editing label does not, since it feeds no tier.
func TestEntityEditClearsAnUnmatchedAlbumMarker(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Edited", 1, model.Track{Media: "CD"})
	editionTrack(t, st, lib.ID, "ess-b", "Untouched", 1, model.Track{Media: "CD"})
	editedID := albumIDByTitle(t, db, "Edited")
	untouchedID := albumIDByTitle(t, db, "Untouched")
	for _, title := range []string{"Edited", "Untouched"} {
		id := albumIDByTitle(t, db, title)
		pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = ?", title)
		if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
			AlbumID: id, PID: model.PID(pid), Provider: "musicbrainz:edition",
		}); err != nil {
			t.Fatalf("ApplyAlbumReleaseMatch(%s): %v", title, err)
		}
	}

	editedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Edited'")
	if err := st.EditEntityFields(ctx, model.MergeAlbum, model.PID(editedPID),
		map[string]string{"barcode": "0075992739429"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("edit barcode: %v", err)
	}
	if n, _, _ := markerRows(t, db, editedID); n != 0 {
		t.Errorf("marker rows after a barcode edit = %d, want 0", n)
	}

	untouchedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Untouched'")
	if err := st.EditEntityFields(ctx, model.MergeAlbum, model.PID(untouchedPID),
		map[string]string{"label": "Harvest"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("edit label: %v", err)
	}
	if n, _, _ := markerRows(t, db, untouchedID); n != 1 {
		t.Errorf("marker rows after a label edit = %d, want 1 (label feeds no tier)", n)
	}
}

// TestEntityEditNormalizesCountry pins the edit/scan asymmetry: an edit asserts one
// country and folds alpha-3, a scan stores the tag. media has no normalizer.
func TestEntityEditNormalizesCountry(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	// An untidy scanned value the edit below would refuse.
	editionTrack(t, st, lib.ID, "ess-a", "Untidy", 1, model.Track{Country: "US & Europe"})
	if _, country := albumColumns(t, db, "Untidy"); country != "US & Europe" {
		t.Errorf("scanned country = %q, want the tag verbatim", country)
	}
	pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Untidy'"))

	if err := st.EditEntityFields(ctx, model.MergeAlbum, pid,
		map[string]string{"country": "usa", "media": "2xCD"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	media, country := albumColumns(t, db, "Untidy")
	if country != "US" {
		t.Errorf("edited country = %q, want US (an alpha-3 code folds)", country)
	}
	if media != "2xCD" {
		t.Errorf("edited media = %q, want it stored as given", media)
	}

	err := st.EditEntityFields(ctx, model.MergeAlbum, pid,
		map[string]string{"country": "US & Europe"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false)
	if err == nil {
		t.Fatal("editing country to a multi-value list should be refused")
	}
	// The message names the accepted form, since entity info displays what it refuses.
	if !strings.Contains(err.Error(), "GB") {
		t.Errorf("refusal %q should name the accepted form", err)
	}
}

// TestEntityInfoReadsEditionColumns checks both entityinfo SQL sites, which mirror each
// other column for column.
func TestEntityInfoReadsEditionColumns(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Readable", 1, model.Track{Media: `12" Vinyl`, Country: "GB"})
	pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Readable'"))

	info, err := st.EntityByPID(ctx, read.EntityAlbum, pid)
	if err != nil {
		t.Fatalf("EntityByPID: %v", err)
	}
	if info.Media != `12" Vinyl` || info.Country != "GB" {
		t.Errorf("EntityByPID = (%q, %q), want (12\" Vinyl, GB)", info.Media, info.Country)
	}

	page, err := st.EntityPage(ctx, read.EntityAlbum, "", 10)
	if err != nil {
		t.Fatalf("EntityPage: %v", err)
	}
	var found bool
	for _, e := range page.Entities {
		if e.PID != pid {
			continue
		}
		found = true
		if e.Media != info.Media || e.Country != info.Country {
			t.Errorf("EntityPage = (%q, %q), want the same as EntityByPID (%q, %q)",
				e.Media, e.Country, info.Media, info.Country)
		}
	}
	if !found {
		t.Error("the album did not appear in its own kind's page")
	}
}

// TestDeclinedMBIDWriteTakesNoArt: the cover rides on the id landing. A locked mbid and
// a collision both leave album.mbid alone, and stamping the matched pressing's art on a
// row that never took its id would be the wrong picture with nothing recording why.
func TestDeclinedMBIDWriteTakesNoArt(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Locked", 1, model.Track{Media: "CD"})
	editionTrack(t, st, lib.ID, "ess-b", "Holder", 1, model.Track{Media: "CD"})
	editionTrack(t, st, lib.ID, "ess-c", "Taken", 1, model.Track{Media: "CD"})

	lockedPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Locked'")
	holderPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Holder'")
	setEntityMBID(t, st, model.MergeAlbum, lockedPID, "", true)
	setEntityMBID(t, st, model.MergeAlbum, holderPID, relTestOneMBID, false)

	cover := &model.ArtImage{Data: []byte("cover-bytes"), Hash: "h-cover", Format: "png", Width: 4, Height: 4,
		Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}}
	for _, tc := range []struct{ title, mbid string }{
		{"Locked", relTestTwoMBID},
		{"Taken", relTestOneMBID}, // already held by Holder
	} {
		id := albumIDByTitle(t, db, tc.title)
		pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = ?", tc.title)
		if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
			AlbumID: id, PID: model.PID(pid), Matched: true, MBID: tc.mbid,
			Reason: "medium", Art: cover,
		}); err != nil {
			t.Fatalf("ApplyAlbumReleaseMatch(%s): %v", tc.title, err)
		}
		if n := scalarQueryInt(t, db,
			"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", id); n != 0 {
			t.Errorf("%s took %d art rows despite a declined mbid write", tc.title, n)
		}
	}

	// A write that does land still takes its cover, so the gate is specific.
	editionTrack(t, st, lib.ID, "ess-d", "Fresh", 1, model.Track{Media: "CD"})
	freshID := albumIDByTitle(t, db, "Fresh")
	freshPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Fresh'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: freshID, PID: model.PID(freshPID), Matched: true, MBID: relTestTwoMBID,
		Reason: "medium", Art: cover,
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch(Fresh): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", freshID); n != 1 {
		t.Errorf("a landed match stored %d art rows, want 1", n)
	}
}

// TestReleaseCoverDoesNotOverwriteADerivedTrackCover is the case that matters, because
// an album normally owns no art_map row at all and answers from a member track's embedded
// cover. Probing only for the album's own row would call almost every album empty and
// quietly replace the file's own artwork on the first release match.
func TestReleaseCoverDoesNotOverwriteADerivedTrackCover(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrackWithCover(t, st, lib.ID, "ess-a", "Embedded", 1, model.Track{Media: "CD"}, pngFixture())
	id := albumIDByTitle(t, db, "Embedded")
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Embedded'")

	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: model.PID(pid), Matched: true, MBID: relTestOneMBID, Reason: "medium",
		Art: &model.ArtImage{Data: []byte("provider-bytes"), Hash: "h-provider", Format: "png", Width: 4, Height: 4,
			Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}},
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", id); n != 0 {
		t.Errorf("album took %d stored art rows; the track's cover already answers for it", n)
	}
	// And the album still resolves the track's cover, unchanged.
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: model.PID(pid)}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("ResolveArt: %v", err)
	}
	if !blob.Derived {
		t.Error("album art should still be derived from the track cover")
	}

	// An album with nothing at all does take the release's cover.
	editionTrack(t, st, lib.ID, "ess-b", "Bare", 1, model.Track{Media: "CD"})
	bareID := albumIDByTitle(t, db, "Bare")
	barePID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Bare'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: bareID, PID: model.PID(barePID), Matched: true, MBID: relTestTwoMBID, Reason: "medium",
		Art: &model.ArtImage{Data: []byte("provider-bytes"), Hash: "h-provider", Format: "png", Width: 4, Height: 4,
			Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}},
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch(Bare): %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", bareID); n != 1 {
		t.Errorf("a bare album stored %d art rows, want 1", n)
	}
}

// TestReleaseCoverDoesNotOverwriteACuratedOne: entity art takes no lock, so
// fill-when-empty is the only thing protecting a cover a user deliberately set, which is
// what SetEntityArt's own doc promises of enrichment.
func TestReleaseCoverDoesNotOverwriteACuratedOne(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Curated", 1, model.Track{Media: "CD"})
	pid := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Curated'"))
	if err := st.SetEntityArt(ctx, model.ArtAlbum, pid, model.ArtRoleFront, pngFixture(), model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}
	before := scalarQueryStr(t, db, `SELECT am.source_hash FROM art_map am
		JOIN album al ON al.id = am.entity_id
		WHERE am.entity_type='album' AND am.role='front' AND al.title='Curated'`)

	id := albumIDByTitle(t, db, "Curated")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: pid, Matched: true, MBID: relTestOneMBID, Reason: "medium",
		Art: &model.ArtImage{Data: []byte("provider-bytes"), Hash: "h-provider", Format: "png", Width: 4, Height: 4,
			Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}},
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}
	after := scalarQueryStr(t, db, `SELECT am.source_hash FROM art_map am
		JOIN album al ON al.id = am.entity_id
		WHERE am.entity_type='album' AND am.role='front' AND al.title='Curated'`)
	if after != before {
		t.Errorf("curated album cover changed from %q to %q; enrichment fills only when empty", before, after)
	}
}

// TestClearingAnAlbumMBIDUndoesTheMatch: the edition provider marker exists so a weak
// write can be reverted, and reverting means the album is re-decidable. The marker has
// to go with the id or the queue keeps skipping it.
func TestClearingAnAlbumMBIDUndoesTheMatch(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Wrong", 1, model.Track{Media: "CD"})
	id := albumIDByTitle(t, db, "Wrong")
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Wrong'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: model.PID(pid), Matched: true, MBID: relTestOneMBID,
		Reason: "medium", Provider: "musicbrainz:edition",
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}
	if queued, _ := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil); len(queued) != 0 {
		t.Fatalf("queued %d albums while matched, want 0", len(queued))
	}

	if err := st.EditEntityFields(ctx, model.MergeAlbum, model.PID(pid),
		map[string]string{"mbid": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
		t.Fatalf("clear mbid: %v", err)
	}
	if n, _, _ := markerRows(t, db, id); n != 0 {
		t.Errorf("marker rows after the undo = %d, want 0", n)
	}
	queued, err := st.AlbumsNeedingReleaseMatch(ctx, false, 0, 100, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingReleaseMatch: %v", err)
	}
	if len(queued) != 1 || queued[0].Name != "Wrong" {
		t.Errorf("queued = %+v, want the album back in the queue", queued)
	}
}

// TestUndoTakesTheMatchedCoverWithIt: the cover came from the pressing being disowned, so
// leaving it would make the undo cosmetic. A member track's embedded cover is untouched,
// since nothing here wrote it.
func TestUndoTakesTheMatchedCoverWithIt(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	editionTrack(t, st, lib.ID, "ess-a", "Wrong", 1, model.Track{Media: "CD"})
	id := albumIDByTitle(t, db, "Wrong")
	pid := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title='Wrong'")
	if err := st.ApplyAlbumReleaseMatch(ctx, model.AlbumReleaseMatch{
		AlbumID: id, PID: model.PID(pid), Matched: true, MBID: relTestOneMBID,
		Reason: "medium", Provider: "musicbrainz:edition",
		Art: &model.ArtImage{Data: []byte("provider-bytes"), Hash: "h-provider", Format: "png", Width: 4, Height: 4,
			Attribution: model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}},
	}); err != nil {
		t.Fatalf("ApplyAlbumReleaseMatch: %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", id); n != 1 {
		t.Fatalf("the match stored %d art rows, want 1", n)
	}

	if err := st.EditEntityFields(ctx, model.MergeAlbum, model.PID(pid),
		map[string]string{"mbid": ""}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
		t.Fatalf("clear mbid: %v", err)
	}
	if n := scalarQueryInt(t, db,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?", id); n != 0 {
		t.Errorf("album art rows after the undo = %d, want 0", n)
	}
}

// editionTrackWithCover is editionTrack with an embedded front cover on the track, which
// is what an album's art normally derives from.
func editionTrackWithCover(t *testing.T, st *sqlite.Store, libID int64, essence, album string, rev int, tr model.Track, art []byte) {
	t.Helper()
	path := "/lib/" + essence + "/1.mp3"
	tr.Artist, tr.AlbumArtist, tr.Album = "PF", "PF", album
	tr.TrackNo = 1
	tr.MBReleaseGroupID = relTestRGMBID
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
		CoverArt: &model.ArtImage{Data: art, Hash: "h-embedded", Format: "png", Width: 2, Height: 2,
			Attribution: model.Attribution{Source: model.SourceTag}},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
}

// pngFixture is a tiny valid PNG for the entity-art set path, which probes the bytes.
func pngFixture() []byte {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, G: 10, B: 10, A: 255})
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
