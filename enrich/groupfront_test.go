package enrich_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// otherPNG is a second image, distinct from pngBytes, for the tests that replace a cover
// and need the hash to change.
func otherPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 6, 6))
	for x := 0; x < 6; x++ {
		for y := 0; y < 6; y++ {
			img.Set(x, y, color.RGBA{R: 10, G: uint8(x * 20), B: uint8(y * 20), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// caaMock serves the archive's real shape, which the group record is read off: a front
// request redirects into a per-release item, and the item serves the bytes with a
// validator. coverRelease is which release the group's front comes from, and both it and
// the etag are settable between runs, since a test plays the archive changing its mind.
type caaMock struct {
	mu           sync.Mutex
	art          []byte
	coverRelease string
	etag         string
	// group and release count front requests, images the downloads that carried bytes.
	group, release, images int
}

func (m *caaMock) hits() (group, release, images int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.group, m.release, m.images
}

func (m *caaMock) set(coverRelease, etag string, art []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coverRelease, m.etag = coverRelease, etag
	if art != nil {
		m.art = art
	}
}

// newRedirectingCAAMock builds the mock: /release-group/<g>/front and /release/<r>/front
// both 307 into /download/mbid-<id>/mbid-<id>-1.jpg, which serves the bytes with an ETag
// and answers 304 to a matching If-None-Match.
func newRedirectingCAAMock(t *testing.T, art []byte, coverRelease string) (string, *caaMock) {
	t.Helper()
	m := &caaMock{art: art, coverRelease: coverRelease, etag: `"v1"`}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/release-group/") && strings.HasSuffix(p, "/front"):
			m.group++
			http.Redirect(w, r, "/download/mbid-"+m.coverRelease+"/mbid-"+m.coverRelease+"-1.jpg", http.StatusTemporaryRedirect)
		case strings.HasPrefix(p, "/release/") && strings.HasSuffix(p, "/front"):
			m.release++
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/release/"), "/front")
			http.Redirect(w, r, "/download/mbid-"+id+"/mbid-"+id+"-1.jpg", http.StatusTemporaryRedirect)
		case strings.HasPrefix(p, "/download/"):
			w.Header().Set("ETag", m.etag)
			if r.Header.Get("If-None-Match") == m.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			m.images++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(m.art)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL, m
}

// seedWYWH seeds one track of Wish You Were Here under the given release id, which is
// what puts its album on the art queue with a group above it.
func seedWYWH(t *testing.T, st *sqlite.Store, libID int64, essence, releaseID string) {
	t.Helper()
	seedAlbumTrack(t, st, libID, essence, model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: releaseID, Barcode: relBarcode,
	})
}

// TestAlbumArtReusesTheGroupCoverForItsOwnRelease is the DEFERRED entry's case: the
// group's front is this pressing's, so the album takes the row rather than the picture.
func TestAlbumArtReusesTheGroupCoverForItsOwnRelease(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	g, r, images := hits.hits()
	if g != 1 || r != 0 || images != 1 {
		t.Fatalf("archive hits = group %d, release %d, images %d; want 1, 0, 1", g, r, images)
	}
	if res.ArtFetched != 1 || res.ArtReused != 1 || res.AlbumArtMatched != 1 {
		t.Fatalf("result = %+v, want one fetched group cover reused once at the album rung", res)
	}
	db := roDB(t, dbPath)
	want := scalarStr(t, db, `SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='front'`)
	if got := albumArtHash(t, dbPath, "front"); got != want {
		t.Errorf("album front = %q, want the group's %q", got, want)
	}
	if got := scalarStr(t, db,
		`SELECT source||'/'||provider FROM art_map WHERE entity_type='album' AND role='front'`); got != "enrichment/coverartarchive" {
		t.Errorf("album front provenance = %q, want enrichment/coverartarchive", got)
	}
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art' AND matched=1"); n != 1 {
		t.Errorf("matched album_art markers = %d, want 1", n)
	}

	res, err = svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.AlbumArtEnriched != 0 {
		t.Errorf("second run walked %d albums, want 0 (the marker holds)", res.AlbumArtEnriched)
	}
}

// TestAlbumArtFetchesTheReleaseFrontOfAnotherPressing: the group's bytes are some other
// edition's, so the album downloads its own.
func TestAlbumArtFetchesTheReleaseFrontOfAnotherPressing(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), relTwoMBID)
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, r, _ := hits.hits(); r != 1 {
		t.Errorf("release front requests = %d, want 1", r)
	}
	if res.ArtReused != 0 {
		t.Errorf("result = %+v, want nothing reused", res)
	}
	if got := albumArtHash(t, dbPath, "front"); got == "" {
		t.Error("album front is empty, want the release's own download")
	}
}

// TestAlbumArtDoesNotReuseAMovedGroupPick: the archive's pick moves to a newly ripped
// edition, but the group still holds the old bytes, so the new album must fetch its own
// rather than take a cover of a different pressing.
func TestAlbumArtDoesNotReuseAMovedGroupPick(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", relTwoMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), relTwoMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	seedWYWH(t, st, lib.ID, "ess-b", edGBMBID)
	hits.set(edGBMBID, `"v1"`, nil)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if _, r, _ := hits.hits(); r != 1 {
		t.Errorf("release front requests = %d, want 1: the record still names the old pick", r)
	}
	if res.ArtReused != 0 {
		t.Errorf("result = %+v, want nothing reused", res)
	}
}

// TestAlbumArtDoesNotReuseWhenTheGroupFrontChangedUnderTheRecord: the record is about
// bytes, so a group whose front was replaced no longer answers for them.
func TestAlbumArtDoesNotReuseWhenTheGroupFrontChangedUnderTheRecord(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	db := roDB(t, dbPath)
	rgPID := model.PID(scalarStr(t, db, "SELECT pid FROM release_group WHERE title='Wish You Were Here'"))
	alPID := model.PID(scalarStr(t, db, "SELECT pid FROM album WHERE title='Wish You Were Here'"))
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, otherPNG(t), "",
		model.Attribution{Source: model.SourceEnrichment, Provider: "coverartarchive"}, model.LockOf(false), false); err != nil {
		t.Fatalf("replace the group front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, alPID, model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear the album front: %v", err)
	}

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if _, r, _ := hits.hits(); r != 1 {
		t.Errorf("release front requests = %d, want 1: the hashes disagree", r)
	}
	if res.ArtReused != 0 {
		t.Errorf("result = %+v, want nothing reused", res)
	}
}

// TestAlbumArtDoesNotReuseAHandSetGroupCover: a cover the user chose carries no record,
// so the album asks the archive for its own rather than copying it.
func TestAlbumArtDoesNotReuseAHandSetGroupCover(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	rgPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM release_group WHERE title='Wish You Were Here'"))
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, otherPNG(t), "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("hand-set the group front: %v", err)
	}

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	g, r, _ := hits.hits()
	if g != 0 || r != 1 {
		t.Errorf("archive hits = group %d, release %d; want 0 and 1", g, r)
	}
	if res.ArtReused != 0 {
		t.Errorf("result = %+v, want nothing reused", res)
	}
}

// TestAlbumArtReuseIgnoresForce: the record is a fact about the bytes the catalog holds,
// not a cached answer, so a forced walk of the album-art phase reads it too.
func TestAlbumArtReuseIgnoresForce(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	alPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM album WHERE title='Wish You Were Here'"))
	if err := st.SetEntityArt(ctx, model.ArtAlbum, alPID, model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear the album front: %v", err)
	}

	res, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseAlbumArt}}, nil)
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if _, r, _ := hits.hits(); r != 0 {
		t.Errorf("release front requests = %d, want 0: force does not bypass the record", r)
	}
	if res.ArtReused != 1 {
		t.Errorf("result = %+v, want the front reused again", res)
	}
	if albumArtHash(t, dbPath, "front") == "" {
		t.Error("album front is empty after the forced reuse")
	}
}

// TestAlbumArtRecordsOncePerGroupFetch: one record per group serves every album under it,
// so the pressing the archive picked reuses and the other one downloads.
func TestAlbumArtRecordsOncePerGroupFetch(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)
	seedWYWH(t, st, lib.ID, "ess-b", edUSMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	g, r, _ := hits.hits()
	if g != 1 || r != 1 {
		t.Errorf("archive hits = group %d, release %d; want 1 and 1", g, r)
	}
	if res.ArtReused != 1 {
		t.Errorf("result = %+v, want exactly one reuse", res)
	}
}

// TestForcedGroupFetchIsConditionalWhenTheCatalogHoldsTheBytes: a forced walk of the
// release-group phase re-fetches every group front, and the recorded validator turns
// that into a request with no download for a cover the archive has not changed.
func TestForcedGroupFetchIsConditionalWhenTheCatalogHoldsTheBytes(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, _, images := hits.hits(); images != 1 {
		t.Fatalf("first run downloaded %d images, want 1", images)
	}
	before := scalarStr(t, roDB(t, dbPath),
		`SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='front'`)

	res, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseReleaseGroup}}, nil)
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if _, _, images := hits.hits(); images != 1 {
		t.Errorf("images downloaded = %d, want the first one alone (the archive answered 304)", images)
	}
	if res.ArtFetched != 0 || res.ReleaseGroupsEnriched != 1 {
		t.Errorf("result = %+v, want the group re-walked with nothing fetched", res)
	}
	if got := scalarStr(t, roDB(t, dbPath),
		`SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='front'`); got != before {
		t.Errorf("group front = %q, want it left at %q", got, before)
	}
}

// TestForcedGroupFetchDownloadsWhenTheArchiveChanged: the validator is the archive's, so
// a cover it has replaced still downloads, and the record follows the new bytes.
func TestForcedGroupFetchDownloadsWhenTheArchiveChanged(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	before := scalarStr(t, roDB(t, dbPath),
		`SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='front'`)

	hits.set(edGBMBID, `"v2"`, otherPNG(t))
	if _, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseReleaseGroup}}, nil); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if _, _, images := hits.hits(); images != 2 {
		t.Errorf("images downloaded = %d, want 2", images)
	}
	if got := scalarStr(t, roDB(t, dbPath),
		`SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role='front'`); got == before {
		t.Error("the group front did not follow the archive's new bytes")
	}
	if got := scalarStr(t, roDB(t, dbPath),
		`SELECT CAST(payload AS TEXT) FROM enrichment_cache WHERE cache_key LIKE 'caa:rg-front:%'`); !strings.Contains(got, `v2`) {
		t.Errorf("group record = %s, want the new validator", got)
	}
}

// TestForcedGroupFetchIsUnconditionalForAHandSetFront: a cover the user chose carries no
// record, so an unlocked slot is fetched plainly and replaced, which is what art set
// --no-lock documents.
func TestForcedGroupFetchIsUnconditionalForAHandSetFront(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedWYWH(t, st, lib.ID, "ess-a", edGBMBID)

	caa, hits := newRedirectingCAAMock(t, pngBytes(t), edGBMBID)
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	rgPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM release_group WHERE title='Wish You Were Here'"))
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, model.ArtRoleFront, otherPNG(t), "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("hand-set the group front: %v", err)
	}

	if _, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseReleaseGroup}}, nil); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if _, _, images := hits.hits(); images != 2 {
		t.Errorf("images downloaded = %d, want 2 (no validator was sent)", images)
	}
}
