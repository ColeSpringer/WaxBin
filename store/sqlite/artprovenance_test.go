package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// coverPNG encodes a distinct PNG per seed value, so two covers in one test are two
// different content hashes.
func coverPNG(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{uint8(x * seed), uint8(y), uint8(seed), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// stamped builds the ingest carrier a producer hands the store, hashed and probed the
// way every real producer does before the write.
func stamped(t *testing.T, raw []byte, source model.ProvenanceSource, provider, srcURL string) *model.ArtImage {
	t.Helper()
	info := art.Describe(raw)
	// Decoded, not merely recognized: an exotic source describes with zero dimensions by
	// design, and a fixture built that way would send every later assertion down the
	// serve-unscaled branch without saying so.
	if info.Format == "" || info.Width == 0 || info.Height == 0 {
		t.Fatalf("probe cover: %+v, want a decoded image", info)
	}
	return &model.ArtImage{
		Data: raw, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: model.Attribution{Source: source, Provider: provider, SourceURL: srcURL},
	}
}

// putCoveredTrack seeds one track carrying a scanned cover, the shape every ingest
// producer eventually reaches.
func putCoveredTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, album string, cover *model.ArtImage) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 1000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track:    model.Track{Artist: "Alpha", AlbumArtist: "Alpha", Album: album, TrackNo: 1},
		CoverArt: cover,
	})
	if err != nil {
		t.Fatalf("PutScannedTrack %s: %v", path, err)
	}
	return res.ItemPID
}

// artRow reads one art_map row's provenance straight out of the catalog.
func artRow(t *testing.T, db *sql.DB, entityType string, entityID int64) (source, provider, srcURL string) {
	t.Helper()
	if err := db.QueryRow(
		"SELECT source, provider, source_url FROM art_map WHERE entity_type=? AND entity_id=? AND role='front'",
		entityType, entityID).Scan(&source, &provider, &srcURL); err != nil {
		t.Fatalf("read art_map %s/%d: %v", entityType, entityID, err)
	}
	return source, provider, srcURL
}

func itemRowID(t *testing.T, db *sql.DB, pid model.PID) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow("SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&id); err != nil {
		t.Fatalf("item rowid for %s: %v", pid, err)
	}
	return id
}

// TestDedupedSourceKeepsSeparateProvenance is the test that proves the mapping was the
// right home for provenance. Two entities carry byte-identical covers, so the
// content-addressed blob store holds exactly one art_source row, and each entity still
// answers for itself.
func TestDedupedSourceKeepsSeparateProvenance(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	raw := coverPNG(t, 3)

	tagged := putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "Tagged", "A",
		stamped(t, raw, model.SourceTag, "", ""))
	sidecar := putCoveredTrack(t, st, lib.ID, "/lib/b.flac", "ess-b", "Sidecar", "B",
		stamped(t, raw, model.SourceSidecar, "", ""))

	db := roConn(t, dbPath)
	if n := scalarInt64(t, db, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Fatalf("art_source rows = %d, want 1 (the covers are byte-identical)", n)
	}
	if got, _, _ := artRow(t, db, "track", itemRowID(t, db, tagged)); got != string(model.SourceTag) {
		t.Errorf("tagged track source = %q, want tag", got)
	}
	if got, _, _ := artRow(t, db, "track", itemRowID(t, db, sidecar)); got != string(model.SourceSidecar) {
		t.Errorf("sidecar track source = %q, want sidecar", got)
	}

	// And the read surface agrees, from the one shared blob.
	for _, tc := range []struct {
		pid  model.PID
		want model.ProvenanceSource
	}{{tagged, model.SourceTag}, {sidecar, model.SourceSidecar}} {
		blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: tc.pid}, model.ArtRoleFront, 0)
		if err != nil {
			t.Fatalf("ResolveArt %s: %v", tc.pid, err)
		}
		if blob.Source != tc.want {
			t.Errorf("ResolveArt(%s).Source = %q, want %q", tc.pid, blob.Source, tc.want)
		}
	}
}

// TestArtProvenancePerOrigin walks one cover from each producer through to the stored
// mapping: a tag, a sidecar, a user set, an enrichment fetch, and a podcast feed.
func TestArtProvenancePerOrigin(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	tagged := putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "Tagged", "A",
		stamped(t, coverPNG(t, 1), model.SourceTag, "", ""))
	if got, _, _ := artRow(t, db, "track", itemRowID(t, db, tagged)); got != "tag" {
		t.Errorf("embedded cover source = %q, want tag", got)
	}

	dirCover := putCoveredTrack(t, st, lib.ID, "/lib/b.flac", "ess-b", "DirCover", "B",
		stamped(t, coverPNG(t, 2), model.SourceSidecar, "", ""))
	if got, _, url := artRow(t, db, "track", itemRowID(t, db, dirCover)); got != "sidecar" || url != "" {
		t.Errorf("directory cover = %q/%q, want sidecar with no source_url", got, url)
	}

	if err := st.SetItemArt(ctx, tagged, model.ArtRoleFront, coverPNG(t, 4), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetItemArt: %v", err)
	}
	if got, _, _ := artRow(t, db, "track", itemRowID(t, db, tagged)); got != "user" {
		t.Errorf("user-set cover source = %q, want user", got)
	}

	// An enrichment cover, carrying the provider and the archive request URL the
	// provider reported.
	const caaURL = "https://coverartarchive.org/release-group/rg-mbid/front"
	rgID := scalarInt64(t, db, "SELECT release_group_id FROM album WHERE title = 'A'")
	rgPID := scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE id = ?", rgID)
	err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: model.PID(rgPID), Matched: true, MBID: "rg-mbid",
		Art: stamped(t, coverPNG(t, 5), model.SourceEnrichment, "coverartarchive", caaURL),
	})
	if err != nil {
		t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
	}
	src, provider, url := artRow(t, db, "release_group", rgID)
	if src != "enrichment" || provider != "coverartarchive" || url != caaURL {
		t.Errorf("fetched cover = %q/%q/%q, want enrichment/coverartarchive/%s", src, provider, url, caaURL)
	}

	// A podcast feed image.
	const feedImageURL = "http://feed.example/art/show.png"
	feed, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:     "http://feed.example/f",
		IdentityKey: identity.PodcastKey("", "http://feed.example/f"),
		Feed:        model.Feed{Title: "Cast", ImageURL: feedImageURL},
		FetchedAtNS: 1,
		Image:       stamped(t, coverPNG(t, 6), model.SourceFeed, "", feedImageURL),
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	podID := scalarInt64(t, db, "SELECT id FROM podcast WHERE pid = ?", string(feed.PodcastPID))
	src, _, url = artRow(t, db, "podcast", podID)
	if src != "feed" || url != feedImageURL {
		t.Errorf("feed image = %q/%q, want feed/%s", src, url, feedImageURL)
	}
}

// TestUnstampedArtIsRefused pins the single-chokepoint guard: an image reaching the
// store with no provenance fails at the write rather than storing a row no consumer
// can attribute. setEntityArtRoleTx raises CodeInvalid; the scan ingest re-wraps it
// as CodeIO the way it wraps every write failure, so the assertion walks the chain
// for the code the guard itself raised.
func TestUnstampedArtIsRefused(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	unstamped := stamped(t, coverPNG(t, 7), "", "", "")

	_, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/x.flac"), DisplayPath: "/lib/x.flac", RelPath: []byte("x.flac"),
			Kind: model.FileAudio, ContentHash: "c-x", EssenceHash: "ess-x", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "X",
			SortKey: model.SortKey("X"), IdentityKey: "essence:ess-x",
		},
		Track:    model.Track{Artist: "Alpha", Album: "A"},
		CoverArt: unstamped,
	})
	if err == nil {
		t.Fatal("an unstamped cover was accepted, want a refusal")
	}
	if !hasCode(err, waxerr.CodeInvalid) {
		t.Errorf("unstamped cover error = %v, want CodeInvalid somewhere in the chain", err)
	}
	if n := scalarInt64(t, roConn(t, dbPath), "SELECT COUNT(*) FROM art_map"); n != 0 {
		t.Errorf("art_map rows = %d after a refused write, want 0", n)
	}
}

// hasCode reports whether any error in the chain carries code, so a test can pin the
// code a store-internal guard raised under a caller's own wrap.
func hasCode(err error, code waxerr.Code) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		var we *waxerr.Error
		if errors.As(e, &we) && we.Code == code {
			return true
		}
	}
	return false
}

// sizedCoverPNG encodes a PNG at the requested size, for the cases that need a source
// big enough to actually thumbnail.
func sizedCoverPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 64, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// storedArt reads the art_source row one entity's front cover maps to.
func storedArt(t *testing.T, db *sql.DB, entityType string, entityID int64) (hash, format string, w, h, size int) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT s.hash, s.format, s.width, s.height, s.size FROM art_map m
		 JOIN art_source s ON s.hash = m.source_hash
		 WHERE m.entity_type=? AND m.entity_id=? AND m.role='front'`,
		entityType, entityID).Scan(&hash, &format, &w, &h, &size); err != nil {
		t.Fatalf("read stored art %s/%d: %v", entityType, entityID, err)
	}
	return hash, format, w, h, size
}

// TestUndescribedCoverIsStoredNotDropped is the fix for a producer that hands the store
// good bytes and describes none of them, which every injected cover provider did: the
// write used to be discarded with no error and no log. The address, format and
// dimensions are all derivable from the bytes the store is already holding, so it
// derives them.
func TestUndescribedCoverIsStoredNotDropped(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	raw := coverPNG(t, 11)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/u.flac", "ess-u", "Undescribed", "U",
		&model.ArtImage{Data: raw, Attribution: model.Attribution{Source: model.SourceTag}})

	db := roConn(t, dbPath)
	hash, format, w, h, size := storedArt(t, db, "track", itemRowID(t, db, pid))
	if hash != art.Hash(raw) {
		t.Errorf("stored under %q, want the address the bytes imply (%q)", hash, art.Hash(raw))
	}
	if format != "png" || w != 8 || h != 8 || size != len(raw) {
		t.Errorf("stored source = %s %dx%d %d bytes, want png 8x8 %d", format, w, h, size, len(raw))
	}
}

// TestCoverWithFormatButNoDimensionsStillThumbnails is the half-described case, which
// is worse than the undescribed one because it looks stored: zero dimensions read as
// undecodable to ResolveArt, which then serves the source unscaled forever. Filling
// each field independently is what keeps the thumbnail path reachable.
func TestCoverWithFormatButNoDimensionsStillThumbnails(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	raw := sizedCoverPNG(t, 400, 300)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/half.flac", "ess-half", "Half", "H",
		&model.ArtImage{Data: raw, Hash: art.Hash(raw), Format: "png",
			Attribution: model.Attribution{Source: model.SourceTag}})

	db := roConn(t, dbPath)
	if _, _, w, h, _ := storedArt(t, db, "track", itemRowID(t, db, pid)); w != 400 || h != 300 {
		t.Fatalf("stored dimensions = %dx%d, want the bytes' own 400x300", w, h)
	}
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 100)
	if err != nil {
		t.Fatalf("ResolveArt: %v", err)
	}
	if !blob.Thumbnail || blob.Width != 100 {
		t.Errorf("resolve at 100 = thumbnail %v %dx%d, want a scaled thumbnail",
			blob.Thumbnail, blob.Width, blob.Height)
	}
}

// TestUndescribedCoverMatchingTheStoredOneReattributes pins the second behaviour change
// filling the address brings: the comparison that decides whether the slot moved now
// happens against a real address, so a cover whose bytes match the stored one refreshes
// the attribution in place instead of being dropped before the comparison ran.
func TestUndescribedCoverMatchingTheStoredOneReattributes(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	raw := coverPNG(t, 12)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/r.flac", "ess-r", "Rescan", "R",
		stamped(t, raw, model.SourceTag, "", ""))
	db := roConn(t, dbPath)
	id := itemRowID(t, db, pid)
	before, _, _, _, _ := storedArt(t, db, "track", id)

	// The same picture from a new origin, described by nothing.
	putCoveredTrack(t, st, lib.ID, "/lib/r.flac", "ess-r", "Rescan", "R",
		&model.ArtImage{Data: raw, Attribution: model.Attribution{Source: model.SourceSidecar}})

	if got, _, _ := artRow(t, db, "track", id); got != string(model.SourceSidecar) {
		t.Errorf("source after the undescribed rescan = %q, want sidecar", got)
	}
	after, _, _, _, _ := storedArt(t, db, "track", id)
	if after != before {
		t.Errorf("source hash moved from %q to %q; the picture did not change", before, after)
	}
	if n := scalarInt64(t, db, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows = %d, want 1 (the bytes are identical)", n)
	}
}

// TestThumbnailCacheDoesNotCacheProvenance is the regression the thumbnail cache
// invites: it is keyed by (source hash, size) alone, so a cached entry must not carry
// the first requester's provenance into a second entity's answer.
func TestThumbnailCacheDoesNotCacheProvenance(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStoreAt(t)
	raw := coverPNG(t, 8)
	tagged := putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "Tagged", "A",
		stamped(t, raw, model.SourceTag, "", ""))
	sidecar := putCoveredTrack(t, st, lib.ID, "/lib/b.flac", "ess-b", "Sidecar", "B",
		stamped(t, raw, model.SourceSidecar, "", ""))

	resolve := func(pid model.PID) *model.ArtBlob {
		t.Helper()
		blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 4)
		if err != nil {
			t.Fatalf("ResolveArt %s: %v", pid, err)
		}
		if !blob.Thumbnail {
			t.Fatalf("ResolveArt %s did not return a thumbnail", pid)
		}
		return blob
	}
	// The second resolve of the same (source, size) comes from the cache.
	if got := resolve(tagged).Source; got != model.SourceTag {
		t.Errorf("first thumbnail source = %q, want tag", got)
	}
	if got := resolve(tagged).Source; got != model.SourceTag {
		t.Errorf("cached thumbnail source = %q, want tag", got)
	}
	// Same cached bytes, different entity, different answer.
	if got := resolve(sidecar).Source; got != model.SourceSidecar {
		t.Errorf("cached thumbnail for the second entity = %q, want sidecar", got)
	}
}

// TestDerivedAlbumCoverReportsMemberProvenance: an album with no durable cover answers
// from a member track, and reports that track's provenance rather than inventing one.
func TestDerivedAlbumCoverReportsMemberProvenance(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "One", "A",
		stamped(t, coverPNG(t, 9), model.SourceSidecar, "", ""))

	db := roConn(t, dbPath)
	albumPID := scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = 'A'")
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: model.PID(albumPID)}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("ResolveArt album: %v", err)
	}
	if !blob.Derived {
		t.Fatalf("album cover Derived = false, want true (no durable album row)")
	}
	if blob.Source != model.SourceSidecar {
		t.Errorf("derived album cover source = %q, want the member track's sidecar", blob.Source)
	}
}

// TestLockedEntityCoverSurvivesEnrichment: a chosen release-group cover is not replaced
// by a fetched one, forced run or not, while an unlocked one still fills.
func TestLockedEntityCoverSurvivesEnrichment(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "One", "A",
		stamped(t, coverPNG(t, 10), model.SourceTag, "", ""))
	db := roConn(t, dbPath)
	rgID := scalarInt64(t, db, "SELECT release_group_id FROM album WHERE title = 'A'")
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE id = ?", rgID))

	chosen := coverPNG(t, 11)
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, chosen, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}
	chosenHash := art.Hash(chosen)

	apply := func() {
		t.Helper()
		err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
			ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: "rg-mbid",
			Art: stamped(t, coverPNG(t, 12), model.SourceEnrichment, "coverartarchive", "http://caa.example/front"),
		})
		if err != nil {
			t.Fatalf("ApplyReleaseGroupEnrichment: %v", err)
		}
	}
	apply()
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='front'", rgID); got != chosenHash {
		t.Fatalf("locked cover was replaced by enrichment")
	}
	// A second pass is what `enrich --force` reaches the store as: the force flag lives
	// in the enrichment run, which re-fetches and calls this same write path again.
	apply()
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='release_group' AND entity_id=? AND role='front'", rgID); got != chosenHash {
		t.Fatalf("locked cover was replaced by a repeated enrichment pass")
	}
	if got, _, _ := artRow(t, db, "release_group", rgID); got != "user" {
		t.Errorf("locked cover source = %q, want user", got)
	}

	// Unlocking lets the next pass fill it.
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, chosen, model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
		t.Fatalf("SetEntityArt unlock: %v", err)
	}
	apply()
	if got, provider, _ := artRow(t, db, "release_group", rgID); got != "enrichment" || provider != "coverartarchive" {
		t.Errorf("unlocked cover = %q/%q, want enrichment/coverartarchive", got, provider)
	}
}

// TestLockedShowCoverSurvivesFeedSync: a chosen show cover is not replaced when the
// feed's image URL changes, while an unlocked one follows the feed.
func TestLockedShowCoverSurvivesFeedSync(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	sync := func(imageURL string, seed int) *model.UpsertFeedResult {
		t.Helper()
		res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
			FeedURL:     "http://feed.example/f",
			IdentityKey: identity.PodcastKey("", "http://feed.example/f"),
			Feed:        model.Feed{Title: "Cast", ImageURL: imageURL},
			FetchedAtNS: 1,
			Image:       stamped(t, coverPNG(t, seed), model.SourceFeed, "", imageURL),
		})
		if err != nil {
			t.Fatalf("UpsertFeed: %v", err)
		}
		return res
	}
	feed := sync("http://feed.example/one.png", 20)
	db := roConn(t, dbPath)
	podID := scalarInt64(t, db, "SELECT id FROM podcast WHERE pid = ?", string(feed.PodcastPID))

	chosen := coverPNG(t, 21)
	if err := st.SetEntityArt(ctx, model.ArtPodcast, feed.PodcastPID, model.ArtRoleFront, chosen, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}
	sync("http://feed.example/two.png", 22)
	if got := scalarQueryStr(t, db,
		"SELECT source_hash FROM art_map WHERE entity_type='podcast' AND entity_id=? AND role='front'", podID); got != art.Hash(chosen) {
		t.Fatalf("locked show cover was replaced by a feed sync")
	}

	if err := st.SetEntityArt(ctx, model.ArtPodcast, feed.PodcastPID, model.ArtRoleFront, chosen, model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
		t.Fatalf("SetEntityArt unlock: %v", err)
	}
	sync("http://feed.example/three.png", 23)
	if got, _, url := artRow(t, db, "podcast", podID); got != "feed" || url != "http://feed.example/three.png" {
		t.Errorf("unlocked show cover = %q/%q, want the feed's third image", got, url)
	}
}

// TestArtLockIsFrontRoleOnly: the lock flag applies to the front cover alone, so a
// non-front set records nothing and refuses nothing.
func TestArtLockIsFrontRoleOnly(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "One", "A",
		stamped(t, coverPNG(t, 30), model.SourceTag, "", ""))
	db := roConn(t, dbPath)
	albumPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = 'A'"))
	albumID := scalarInt64(t, db, "SELECT id FROM album WHERE title = 'A'")

	// A back cover asking to be locked records no lock at all.
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleBack, coverPNG(t, 32), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set back: %v", err)
	}
	if n := scalarInt64(t, db,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND entity_id=? AND field='art'",
		albumID); n != 0 {
		t.Fatalf("a back-role set recorded %d art lock rows, want 0", n)
	}
	// Which is why a second back set is not refused, where a front one would be.
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleBack, coverPNG(t, 33), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Errorf("a second back set was refused, so the front-only scope leaked: %v", err)
	}

	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t, 31), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t, 34), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("second front set = %v, want CodeLocked", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtAlbum, PID: albumPID})
	if err != nil {
		t.Fatalf("ArtRoles: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %d, want 2", len(roles))
	}
	for _, r := range roles {
		if r.Role == model.ArtRoleFront && (!r.Locked || r.Source != model.SourceUser) {
			t.Errorf("front role = locked %v source %q, want locked user", r.Locked, r.Source)
		}
	}
}

// TestItemArtLockHasOneHome pins the routing the two set paths share: a track cover
// locked through either API is refused through the other and read back by both
// surfaces. Before it had one, an embedder's set_entity_art lock landed in
// entity_curation, where the scan and SetItemArt never looked.
func TestItemArtLockHasOneHome(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	pid := putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "One", "A",
		stamped(t, coverPNG(t, 60), model.SourceTag, "", ""))
	db := roConn(t, dbPath)

	// Locked through the entity API, refused through the item API.
	if err := st.SetEntityArt(ctx, model.ArtTrack, pid, model.ArtRoleFront, coverPNG(t, 61), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t, 62), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("SetItemArt after an entity-API lock = %v, want CodeLocked", err)
	}
	// It landed in the item-scoped table, which is the one the scan reads.
	if n := scalarInt64(t, db,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='track' AND field='art'"); n != 0 {
		t.Errorf("a track art lock landed in entity_curation (%d rows), where nothing reads it", n)
	}
	if got := scalarQueryStr(t, db, `SELECT fp.field FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE pi.pid = ? AND fp.field = 'art'`, string(pid)); got != "art" {
		t.Errorf("no item-scoped art lock row after an entity-API set")
	}

	// And back the other way, with both read surfaces agreeing.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t, 63), model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("forced SetItemArt: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtTrack, pid, model.ArtRoleFront, coverPNG(t, 64), model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("SetEntityArt after an item-API lock = %v, want CodeLocked", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil || len(roles) != 1 {
		t.Fatalf("ArtRoles = %v (err %v), want one front role", roles, err)
	}
	if !roles[0].Locked {
		t.Errorf("ArtRoles reports the item cover unlocked while every write path refuses it")
	}
}

// TestEpisodeArtLockSurvivesRedownload: a chosen episode cover is not re-pointed by the
// next download, which re-attaches the feed's episode image every time.
func TestEpisodeArtLockSurvivesRedownload(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	feed, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:     "http://feed.example/f",
		IdentityKey: identity.PodcastKey("", "http://feed.example/f"),
		Feed: model.Feed{Title: "Cast", Episodes: []model.FeedEpisode{{
			GUID: "g1", Title: "Ep1", EnclosureURL: "http://feed.example/1.mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: 1,
		}}},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, feed.PodcastPID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %v (err %v)", eps, err)
	}
	ep := eps[0].PID

	podLibID, err := st.EnsurePodcastLibrary(ctx, "/pod")
	if err != nil {
		t.Fatalf("EnsurePodcastLibrary: %v", err)
	}

	chosen := coverPNG(t, 70)
	if err := st.SetEntityArt(ctx, model.ArtEpisode, ep, model.ArtRoleFront, chosen, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetEntityArt episode: %v", err)
	}
	// The download path re-attaches the feed's image on every fetch.
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: ep, LibraryID: podLibID,
		File: model.File{
			Path: []byte("/pod/1.mp3"), DisplayPath: "/pod/1.mp3", RelPath: []byte("1.mp3"),
			Kind: model.FileAudio, ContentHash: "c1", EssenceHash: "e1", ScanState: model.ScanIndexed,
		},
		Image: stamped(t, coverPNG(t, 71), model.SourceFeed, "", "http://feed.example/ep1.png"),
	}); err != nil {
		t.Fatalf("AttachEpisodeFile: %v", err)
	}
	db := roConn(t, dbPath)
	if got := scalarQueryStr(t, db,
		`SELECT am.source_hash FROM art_map am JOIN playable_item pi ON pi.id = am.entity_id
		 WHERE am.entity_type='episode' AND am.role='front' AND pi.pid = ?`, string(ep)); got != art.Hash(chosen) {
		t.Errorf("a download replaced a locked episode cover")
	}
}

// TestFieldProvenanceOverlaysArt covers the four overlay states: a cover with no lock
// row, a lock row with no cover, both together, and a cover inherited from the album.
func TestFieldProvenanceOverlaysArt(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	artRowOf := func(pid model.PID) (model.FieldProvenance, bool) {
		t.Helper()
		rows, err := st.FieldProvenance(ctx, pid)
		if err != nil {
			t.Fatalf("FieldProvenance %s: %v", pid, err)
		}
		for i, r := range rows {
			if r.Field != "art" {
				continue
			}
			// The rows stay field-ordered, which the insert has to preserve.
			if i > 0 && rows[i-1].Field > "art" {
				t.Errorf("art row is out of sort position: %v then art", rows[i-1].Field)
			}
			return r, true
		}
		return model.FieldProvenance{}, false
	}

	// A cover and no lock row at all: an art row appears anyway.
	covered := putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "One", "A",
		stamped(t, coverPNG(t, 40), model.SourceSidecar, "", ""))
	row, ok := artRowOf(covered)
	if !ok || row.Source != model.SourceSidecar || row.Locked || row.Value != "" {
		t.Errorf("covered item art row = %+v (found %v), want an unlocked sidecar row with no value", row, ok)
	}

	// A lock row and no cover: the row survives on its own, which is what locking art
	// to stop the scanner filling it looks like.
	bare := putCoveredTrack(t, st, lib.ID, "/lib/b.flac", "ess-b", "Two", "B", nil)
	if err := st.LockField(ctx, bare, "art"); err != nil {
		t.Fatalf("LockField: %v", err)
	}
	row, ok = artRowOf(bare)
	if !ok || !row.Locked {
		t.Errorf("locked-but-uncovered art row = %+v (found %v), want a locked row", row, ok)
	}

	// Both: art_map supplies the attribution, the row supplies the lock. The two lock
	// writers disagree about source, which is exactly what the overlay fixes.
	if err := st.LockField(ctx, covered, "art"); err != nil {
		t.Fatalf("LockField covered: %v", err)
	}
	if got := scalarQueryStr(t, db, `SELECT fp.source FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE pi.pid = ? AND fp.field = 'art'`, string(covered)); got != "tag" {
		t.Fatalf("lock row source = %q, want the tag LockField writes (the overlay's reason to exist)", got)
	}
	row, ok = artRowOf(covered)
	if !ok || row.Source != model.SourceSidecar || !row.Locked {
		t.Errorf("covered+locked art row = %+v (found %v), want a locked sidecar row", row, ok)
	}

	// An item whose only cover is the album's gets no art row: inherited art is not
	// the item's own field.
	albumPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = 'B'"))
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t, 41), model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("SetEntityArt album: %v", err)
	}
	if err := st.UnlockField(ctx, bare, "art"); err != nil {
		t.Fatalf("UnlockField: %v", err)
	}
	if row, ok := artRowOf(bare); ok {
		t.Errorf("inherited album art produced an item art row: %+v", row)
	}
}

// TestArtSourceQueryField selects items by where their own cover came from, and reads
// empty for an item whose only cover is inherited.
func TestArtSourceQueryField(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	putCoveredTrack(t, st, lib.ID, "/lib/a.flac", "ess-a", "Tagged", "A",
		stamped(t, coverPNG(t, 50), model.SourceTag, "", ""))
	putCoveredTrack(t, st, lib.ID, "/lib/b.flac", "ess-b", "Sidecar", "A",
		stamped(t, coverPNG(t, 51), model.SourceSidecar, "", ""))
	inherited := putCoveredTrack(t, st, lib.ID, "/lib/c.flac", "ess-c", "Inherited", "C", nil)

	db := roConn(t, dbPath)
	albumC := model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title = 'C'"))
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumC, model.ArtRoleFront, coverPNG(t, 52), model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}

	titles := func(source string) []string {
		t.Helper()
		items, err := st.QueryItems(ctx, query.New(query.EntityItems).
			Where("art_source", query.OpIs, source).Build(), "")
		if err != nil {
			t.Fatalf("query art_source %q: %v", source, err)
		}
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.Title
		}
		return out
	}
	if got := titles("sidecar"); len(got) != 1 || got[0] != "Sidecar" {
		t.Errorf("art_source is sidecar = %v, want [Sidecar]", got)
	}
	if got := titles("tag"); len(got) != 1 || got[0] != "Tagged" {
		t.Errorf("art_source is tag = %v, want [Tagged]", got)
	}
	// The inherited cover reads empty: the field answers for the item's own art only,
	// matching has_art.
	got := titles("")
	if len(got) != 1 || got[0] != "Inherited" {
		t.Errorf("art_source is '' = %v, want [Inherited] (pid %s)", got, inherited)
	}
}

func scalarInt64(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}
