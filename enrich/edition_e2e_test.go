package enrich_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
)

// The edition tier's own release ids, so a test can tell which pressing it resolved.
const (
	edGBMBID = "e0000000-0000-4000-8000-00000000000a"
	edUSMBID = "e0000000-0000-4000-8000-00000000000b"
	edJPMBID = "e0000000-0000-4000-8000-00000000000c"
)

// albumMarker returns an album's enrichment marker provider and matched flag.
func albumMarker(t *testing.T, dbPath string) (provider string, matched int) {
	t.Helper()
	db := roDB(t, dbPath)
	row := db.QueryRow(`SELECT ee.provider, ee.matched FROM entity_enrichment ee
		JOIN album al ON al.id = ee.entity_id
		WHERE ee.entity_type = 'album' AND al.title = 'Wish You Were Here'`)
	if err := row.Scan(&provider, &matched); err != nil {
		t.Fatalf("no album marker: %v", err)
	}
	return provider, matched
}

// TestEnrichMatchesReleaseFromMediaAndCountry is the deferred entry closed end to end:
// no barcode, no catalog number, resolved from the medium and country its tags carry.
func TestEnrichMatchesReleaseFromMediaAndCountry(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(3,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
		browseDoc(edJPMBID, []string{`12" Vinyl`}, "GB", ""),
	)}
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 1 {
		t.Fatalf("result = %+v, want 1 album searched and matched", res)
	}
	if got := albumMBID(t, dbPath); got != edGBMBID {
		t.Errorf("album mbid = %q, want %s (the GB CD)", got, edGBMBID)
	}
	// No identifier, so neither search tier spends a request.
	if len(mb.releaseQueries) != 0 {
		t.Errorf("release queries = %v, want none (nothing to search by)", mb.releaseQueries)
	}
	if len(mb.browseOffsets) != 1 {
		t.Errorf("browse offsets = %v, want exactly one page", mb.browseOffsets)
	}
	// The provider marker keeps an edition match findable and undoable.
	if provider, matched := albumMarker(t, dbPath); provider != "musicbrainz:edition" || matched != 1 {
		t.Errorf("marker = (%q, %d), want (musicbrainz:edition, 1)", provider, matched)
	}
}

// TestEnrichRefusesAnAmbiguousGroup: two GB CDs describe the album equally well, so the
// uniqueness gate writes nothing.
func TestEnrichRefusesAnAmbiguousGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "GB", ""),
	)}
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("ambiguous group wrote mbid %q, want empty", got)
	}
	// Still marked so the album is not re-browsed, but under the spine's provider: the
	// edition id marks the WRITES that must stay findable, and a refusal wrote nothing.
	if provider, matched := albumMarker(t, dbPath); provider != "musicbrainz" || matched != 0 {
		t.Errorf("marker = (%q, %d), want (musicbrainz, 0)", provider, matched)
	}
}

// TestEnrichSkipsTheBrowseWhenAnIdentifierDecides pins tier ordering: a resolving
// barcode short-circuits, so the whole-group browse is never requested.
func TestEnrichSkipsTheBrowseWhenAnIdentifierDecides(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		Barcode: relBarcode, Media: "CD", Country: "GB",
	})

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	mb.browsePages = []string{browsePage(1, browseDoc(edGBMBID, []string{"CD"}, "GB", ""))}
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != relOneMBID {
		t.Errorf("album mbid = %q, want %s (the barcode's release)", got, relOneMBID)
	}
	if len(mb.browseOffsets) != 0 {
		t.Errorf("browse offsets = %v, want none (the barcode decided first)", mb.browseOffsets)
	}
	// An identifier match keeps the spine's provider, not the edition one.
	if provider, _ := albumMarker(t, dbPath); provider != "musicbrainz" {
		t.Errorf("marker provider = %q, want musicbrainz", provider)
	}
}

// TestEnrichSkipsTheBrowseWithNothingInterpretable: the queue gate fires on a non-empty
// column, so a codec-only album reaches the phase and must cost no browse.
func TestEnrichSkipsTheBrowseWithNothingInterpretable(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "FLAC", "")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(1, browseDoc(edGBMBID, []string{"Digital Media"}, "GB", ""))}
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 0 {
		t.Fatalf("result = %+v, want 1 searched and 0 matched", res)
	}
	if len(mb.browseOffsets) != 0 {
		t.Errorf("browse offsets = %v, want none (MEDIA=FLAC is not evidence)", mb.browseOffsets)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("album mbid = %q, want empty", got)
	}
}

// TestEnrichPagesAWholeGroup: the tier reads release-count from page one and keeps going
// until every release is accounted for.
func TestEnrichPagesAWholeGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "JP")

	// 120 releases: 100 US CDs on page one, 19 more plus the one Japanese CD on page two.
	first := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		first = append(first, browseDoc(fillerMBID(i), []string{"CD"}, "US", ""))
	}
	second := make([]string, 0, 20)
	for i := 100; i < 119; i++ {
		second = append(second, browseDoc(fillerMBID(i), []string{"CD"}, "US", ""))
	}
	second = append(second, browseDoc(edJPMBID, []string{"CD"}, "JP", ""))
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(120, first...), browsePage(120, second...)}

	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != edJPMBID {
		t.Errorf("album mbid = %q, want %s (the only JP pressing)", got, edJPMBID)
	}
	if want := []string{"0", "100"}; !strings.EqualFold(strings.Join(mb.browseOffsets, ","), strings.Join(want, ",")) {
		t.Errorf("browse offsets = %v, want %v", mb.browseOffsets, want)
	}
}

// TestEnrichRefusesAnOverCapGroup: too many editions for media and country to be unique
// among refuses whatever the pages say, so the tier stops after page one and caches that
// (a stable fact), leaving two albums in one group costing a single request.
func TestEnrichRefusesAnOverCapGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "JP")
	seedAlbumEdition(t, st, lib.ID, "ess-b", "CD", "US")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(1000, browseDoc(edJPMBID, []string{"CD"}, "JP", ""))}
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 2 || res.AlbumsMatched != 0 {
		t.Fatalf("result = %+v, want 2 searched and 0 matched", res)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("an over-cap group matched %q, want empty", got)
	}
	if len(mb.browseOffsets) != 1 {
		t.Fatalf("browse offsets = %v, want exactly one (page one, then cached)", mb.browseOffsets)
	}
	if n := cachedEditionGroups(t, dbPath); n != 1 {
		t.Errorf("cached edition groups = %d, want 1 (over cap is a stable fact)", n)
	}
}

// TestEnrichDoesNotCacheAShortRead: fewer releases than release-count is transient, not a
// fact about the group, so the second album re-fetches rather than inherit a partial set.
func TestEnrichDoesNotCacheAShortRead(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "JP")
	seedAlbumEdition(t, st, lib.ID, "ess-b", "CD", "US")

	// Claims three releases, serves one, and page two comes back empty.
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{
		browsePage(3, browseDoc(edJPMBID, []string{"CD"}, "JP", "")),
		browsePage(3),
	}
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("a short read matched %q, want empty (uniqueness over a partial group is not uniqueness)", got)
	}
	if n := cachedEditionGroups(t, dbPath); n != 0 {
		t.Errorf("cached edition groups = %d, want 0 (a short read is transient)", n)
	}
	// One attempt for the group, not one per album: never caching the read would
	// otherwise make every album under a permanently inconsistent group re-page it.
	if want := []string{"0", "100"}; strings.Join(mb.browseOffsets, ",") != strings.Join(want, ",") {
		t.Errorf("browse offsets = %v, want %v (one attempt per group per run)", mb.browseOffsets, want)
	}
}

// TestEnrichServesASecondAlbumFromTheGroupCache: the projection is cached per group, so a
// group's second album costs no request.
func TestEnrichServesASecondAlbumFromTheGroupCache(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")
	seedAlbumEdition(t, st, lib.ID, "ess-b", "CD", "JP")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(3,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
		browseDoc(edJPMBID, []string{"CD"}, "JP", ""),
	)}
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 2 || res.AlbumsMatched != 2 {
		t.Fatalf("result = %+v, want 2 searched and 2 matched", res)
	}
	if len(mb.browseOffsets) != 1 {
		t.Errorf("browse offsets = %v, want one (the second album reads the cache)", mb.browseOffsets)
	}
	db := roDB(t, dbPath)
	for _, want := range []string{edGBMBID, edJPMBID} {
		if n := scalarInt(t, db, "SELECT COUNT(*) FROM album WHERE mbid = ?", want); n != 1 {
			t.Errorf("no album resolved to %s", want)
		}
	}
}

// cachedEditionGroups counts the projected whole-group cache entries.
func cachedEditionGroups(t *testing.T, dbPath string) int {
	t.Helper()
	return scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM enrichment_cache WHERE cache_key LIKE 'mb:rg-editions:%'")
}

// TestEnrichTakesTheMatchedReleasesOwnCover: the album rung of the art chain had no
// producer until now, so this is the first art of the edition actually held.
func TestEnrichTakesTheMatchedReleasesOwnCover(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	art := pngBytes(t)
	caa, hits := newReleaseCAAMock(t, art)
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	svc := enrich.New(st, enrich.Config{
		Contact:            "test@example.com",
		MatchReleases:      true,
		FetchCoverArt:      true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL,
		CoverArtBaseURL:    caa,
	}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != edGBMBID {
		t.Fatalf("album mbid = %q, want %s", got, edGBMBID)
	}
	if *hits != 1 {
		t.Errorf("release cover fetches = %d, want 1", *hits)
	}
	// It lands on the album's own art_map row, the rung ResolveArt consults first.
	db := roDB(t, dbPath)
	n := scalarInt(t, db, `SELECT COUNT(*) FROM art_map am JOIN album al ON al.id = am.entity_id
		WHERE am.entity_type = 'album' AND am.role = 'front' AND al.title = 'Wish You Were Here'`)
	if n != 1 {
		t.Errorf("album front-cover map rows = %d, want 1", n)
	}
}

// newReleaseCAAMock serves one release's front cover and counts the fetches. Only the
// /release rung, so a release-group fetch would 404 and show zero hits.
func newReleaseCAAMock(t *testing.T, art []byte) (string, *int) {
	t.Helper()
	hits := new(int)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release/"+edGBMBID+"/front" {
			*hits++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(art)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL, hits
}

// fillerMBID is a distinct UUID for one of a big group's filler releases.
func fillerMBID(i int) string {
	return "f0000000-0000-4000-8000-0000000" + strconv.Itoa(100000+i)
}

// TestEnrichLeavesAnAlbumQueuedAfterAShortRead: not caching the partial group is only
// half the retry. Writing a no-match marker would keep the album out of the queue on
// every later run, so a transient read must write nothing at all.
func TestEnrichLeavesAnAlbumQueuedAfterAShortRead(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "JP")

	// Claims three releases, serves one, then an empty page two.
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{
		browsePage(3, browseDoc(edJPMBID, []string{"CD"}, "JP", "")),
		browsePage(3),
	}
	svc := newRelService(st, mb.server.URL)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album'"); n != 0 {
		t.Fatalf("album markers = %d, want 0 so the album stays queued", n)
	}

	// The next unforced run picks it up and resolves it.
	mb.browsePages = []string{browsePage(2,
		browseDoc(edJPMBID, []string{"CD"}, "JP", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 1 {
		t.Fatalf("second run = %+v, want the album re-queued and matched", res)
	}
	if got := albumMBID(t, dbPath); got != edJPMBID {
		t.Errorf("album mbid = %q, want %s", got, edJPMBID)
	}
}

// TestEnrichRefusesABrowseWithNoUsableCount: a page carrying releases but no
// release-count cannot be judged complete, and taking the count as zero would satisfy
// every completeness test and hand the matcher whatever fraction the page held.
func TestEnrichRefusesABrowseWithNoUsableCount(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "JP")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{`{"releases":[` +
		browseDoc(edJPMBID, []string{"CD"}, "JP", "") + `,` +
		browseDoc(edUSMBID, []string{"CD"}, "US", "") + `]}`}
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("a countless browse matched %q, want empty", got)
	}
	if n := cachedEditionGroups(t, dbPath); n != 0 {
		t.Errorf("cached edition groups = %d, want 0", n)
	}
}

// TestEnrichCountsReleaseCovers: a background job serializes Result alone, so a run that
// fetched two hundred album covers must not report no art work at all.
func TestEnrichCountsReleaseCovers(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	caa, _ := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	res, err := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MatchReleases: true, FetchCoverArt: true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL, CoverArtBaseURL: caa,
	}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ArtFetched != 1 {
		t.Errorf("ArtFetched = %d, want 1", res.ArtFetched)
	}
	if got := albumMBID(t, dbPath); got != edGBMBID {
		t.Errorf("album mbid = %q, want %s", got, edGBMBID)
	}
}

// TestInjectedCoverProviderServesTheReleaseRung: the release cover goes through the
// provider list like every other cover, so an embedder's provider can answer it and
// still outrank the built-in Cover Art Archive.
func TestInjectedCoverProviderServesTheReleaseRung(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	art := pngBytes(t)
	var sawRelease bool
	injected := &enrich.Mock{
		ProviderName: "fanart", Caps: enrich.CapCover,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			sawRelease = true
			return &enrich.Candidate{Cover: &model.ArtImage{
				Data: art, Hash: "injected-hash", Format: "png", Width: 4, Height: 4,
			}}, nil
		},
	}
	caa, caaHits := newReleaseCAAMock(t, art)
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	_, err := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MatchReleases: true, FetchCoverArt: true,
		Providers:          []enrich.Provider{injected},
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL, CoverArtBaseURL: caa,
	}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawRelease {
		t.Error("the injected provider was never asked for the release cover")
	}
	if *caaHits != 0 {
		t.Errorf("the built-in CAA answered %d times despite a higher-priority provider", *caaHits)
	}
	if got := scalarStr(t, roDB(t, dbPath), `SELECT am.source_hash FROM art_map am
		JOIN album al ON al.id = am.entity_id
		WHERE am.entity_type='album' AND am.role='front' AND al.title='Wish You Were Here'`); got != "injected-hash" {
		t.Errorf("stored album cover = %q, want the injected provider's", got)
	}
}

// TestEnrichSurvivesAStaleReleaseGroupID: a merged-away or deleted group browses to 404.
// That is one album's bad tag, not a broken run, so it must not abort the pass the way an
// unreachable service does.
func TestEnrichSurvivesAStaleReleaseGroupID(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")

	// browsePages stays nil, so the mock 404s the browse the way MusicBrainz would.
	mb := newRelMock(t, "[]")
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run aborted on a 404 browse: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 0 {
		t.Fatalf("result = %+v, want 1 searched and 0 matched", res)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("album mbid = %q, want empty", got)
	}
	// A missing group is a stable fact, so the album is marked rather than left queued.
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album'"); n != 1 {
		t.Errorf("album markers = %d, want 1", n)
	}
}

// TestForcedRunBrowsesEachGroupOnce: force bypasses the per-group cache read, so without
// a per-run memo every album under one group would re-page the whole thing.
func TestForcedRunBrowsesEachGroupOnce(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")
	seedAlbumEdition(t, st, lib.ID, "ess-b", "CD", "JP")

	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(3,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
		browseDoc(edJPMBID, []string{"CD"}, "JP", ""),
	)}
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{Force: true}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsMatched != 2 {
		t.Fatalf("result = %+v, want both albums matched", res)
	}
	if len(mb.browseOffsets) != 1 {
		t.Errorf("browse offsets = %v, want one (a forced run refreshes a group once)", mb.browseOffsets)
	}
}

// TestReleaseCoverIsSkippedWhenTheAlbumAlreadyHasArt: the store fills only when empty, so
// asking a provider for a cover the album would keep spends a rate-limited request on a
// picture nothing stores. Most rips carry embedded art, so this is the common case.
func TestReleaseCoverIsSkippedWhenTheAlbumAlreadyHasArt(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrackWithCover(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		Media: "CD", Country: "GB",
	}, pngBytes(t))

	caa, hits := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	res, err := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MatchReleases: true, FetchCoverArt: true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL, CoverArtBaseURL: caa,
	}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != edGBMBID {
		t.Fatalf("album mbid = %q, want %s", got, edGBMBID)
	}
	if *hits != 0 {
		t.Errorf("release cover fetches = %d, want 0 (the track's embedded cover already answers)", *hits)
	}
	if res.ArtFetched != 0 {
		t.Errorf("ArtFetched = %d, want 0", res.ArtFetched)
	}
	// The track's own cover still answers for the album, through the derived rung.
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album'"); n != 0 {
		t.Errorf("album art_map rows = %d, want 0 (the cover stays derived)", n)
	}
}
