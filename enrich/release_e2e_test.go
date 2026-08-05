package enrich_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// The album release match runs against real UUIDs, since the phase refuses to
// interpolate anything else into a Lucene query.
const (
	relArtistMBID = "a0000000-0000-4000-8000-000000000001"
	relRGMBID     = "b0000000-0000-4000-8000-000000000002"
	relOneMBID    = "c0000000-0000-4000-8000-000000000003"
	relTwoMBID    = "d0000000-0000-4000-8000-000000000004"

	// A valid EAN-13 and its UPC-A spelling, plus a second, different release's code.
	relBarcode    = "0075992739429"
	relBarcodeUPC = "075992739429"
	relOtherCode  = "5099902154251"
)

// relMock serves the MusicBrainz endpoints an album release match needs: the artist
// and release-group ladder that fills release_group.mbid, plus the release search.
// releases is the JSON array body the /release search answers with, so a test picks
// what came back; releaseQueries records every query string it was asked.
type relMock struct {
	server         *httptest.Server
	releases       string
	releaseQueries []string
	requests       int
}

func newRelMock(t *testing.T, releases string) *relMock {
	t.Helper()
	m := &relMock{releases: releases}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests++
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		switch {
		case r.URL.Path == "/artist" && q != "":
			io(w, `{"artists":[{"id":"`+relArtistMBID+`","name":"Pink Floyd","sort-name":"Pink Floyd","score":100}]}`)
		case r.URL.Path == "/artist/"+relArtistMBID:
			io(w, `{"id":"`+relArtistMBID+`","name":"Pink Floyd","sort-name":"Pink Floyd"}`)
		case r.URL.Path == "/release-group" && q != "":
			io(w, `{"release-groups":[{"id":"`+relRGMBID+`","title":"Wish You Were Here","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"`+relArtistMBID+`","name":"Pink Floyd"}}]}]}`)
		case r.URL.Path == "/release-group/"+relRGMBID:
			io(w, `{"id":"`+relRGMBID+`","title":"Wish You Were Here","primary-type":"Album","secondary-types":[]}`)
		case r.URL.Path == "/release" && q != "":
			m.releaseQueries = append(m.releaseQueries, q)
			io(w, `{"releases":`+m.releases+`}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

// releaseDoc builds one search document in the seeded release group.
func releaseDoc(id, barcode, catNo string) string {
	label := ""
	if catNo != "" {
		label = `,"label-info":[{"catalog-number":"` + catNo + `","label":{"name":"Harvest"}}]`
	}
	return `{"id":"` + id + `","title":"Wish You Were Here","score":100,"barcode":"` + barcode + `",
		"release-group":{"id":"` + relRGMBID + `"}` + label + `}`
}

// seedAlbumWithIdentifiers persists one track whose album carries the given release
// identifiers, which is the population the phase serves: tagged from Discogs or by a
// ripper that read a barcode, but with no MUSICBRAINZ_ALBUMID. rgTag is stored on the
// release group verbatim, so a test can put a malformed id there the way a tag can.
func seedAlbumWithIdentifiers(t *testing.T, st *sqlite.Store, libID int64, essence, barcode, catNo, rgTag string) {
	t.Helper()
	path := "/lib/" + essence + ".mp3"
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Shine On",
			SortKey: model.SortKey("Shine On"), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{
			Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
			Barcode: barcode, CatalogNumber: catNo, MBReleaseGroupID: rgTag,
		},
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
}

func newRelService(st enrich.Store, mbURL string) *enrich.Service {
	return enrich.New(st, enrich.Config{
		Contact:            "test@example.com",
		MatchReleases:      true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbURL,
	}, nil)
}

func albumMBID(t *testing.T, dbPath string) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath), "SELECT COALESCE(mbid,'') FROM album WHERE title='Wish You Were Here'")
}

// TestEnrichMatchesReleaseFromBarcode is the phase end to end. It also pins the
// ordering the phase list depends on: the album query requires a non-empty
// release_group.mbid, and nothing tagged one here, so the match can only happen
// because the release-group phase ran first in the same pass.
func TestEnrichMatchesReleaseFromBarcode(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcode, "", "")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 1 {
		t.Fatalf("result = %+v, want 1 album searched and matched", res)
	}
	if got := albumMBID(t, dbPath); got != relOneMBID {
		t.Errorf("album mbid = %q, want %s", got, relOneMBID)
	}

	// The search was constrained to the group and asked for both stored spellings, so
	// a release MusicBrainz holds under the UPC form is still retrieved.
	if len(mb.releaseQueries) != 1 {
		t.Fatalf("release queries = %v, want exactly one", mb.releaseQueries)
	}
	q := mb.releaseQueries[0]
	for _, want := range []string{"rgid:" + relRGMBID, relBarcode, relBarcodeUPC} {
		if !strings.Contains(q, want) {
			t.Errorf("release query %q is missing %q", q, want)
		}
	}
}

// TestEnrichMatchesAReleaseStoredUnderTheOtherGTINForm is the case the two spellings
// exist for: the album is tagged with a 12-digit UPC and MusicBrainz holds the same
// number as a zero-padded EAN-13. Comparing folds them together, but only if the
// query retrieved the document in the first place.
func TestEnrichMatchesAReleaseStoredUnderTheOtherGTINForm(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcodeUPC, "", "")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != relOneMBID {
		t.Errorf("album mbid = %q, want %s", got, relOneMBID)
	}
	if len(mb.releaseQueries) != 1 || !strings.Contains(mb.releaseQueries[0], relBarcode) {
		t.Errorf("release queries = %v, want one naming the EAN-13 spelling", mb.releaseQueries)
	}
}

// TestEnrichMatchesReleaseFromCatalogNumber uses a catalog number carrying a space
// and a slash, which is the case escapeLucene exists for, and checks that the
// barcode tier costs no request when the album has no barcode.
func TestEnrichMatchesReleaseFromCatalogNumber(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", "", "SHVL 804/A", "")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, "", "SHVL-804/A")+"]")
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsMatched != 1 {
		t.Fatalf("result = %+v, want 1 album matched", res)
	}
	if got := albumMBID(t, dbPath); got != relOneMBID {
		t.Errorf("album mbid = %q, want %s", got, relOneMBID)
	}
	if len(mb.releaseQueries) != 1 || !strings.Contains(mb.releaseQueries[0], "catno") {
		t.Fatalf("release queries = %v, want one catno query and no barcode query", mb.releaseQueries)
	}
}

// TestEnrichMatchesAPopularReleaseGroup is the regression the search endpoint exists
// to prevent: a group with far more releases than a browse page holds still resolves,
// because the query is pointed at the identifier rather than paged through the group.
func TestEnrichMatchesAPopularReleaseGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcode, "", "")

	// 150 releases in the group; only the one carrying the barcode comes back, which
	// is exactly what a browse capped at 100 per page could not guarantee.
	docs := make([]string, 0, 25)
	docs = append(docs, releaseDoc(relOneMBID, relBarcode, ""))
	for i := 10; i < 34; i++ {
		docs = append(docs, releaseDoc("e0000000-0000-4000-8000-0000000000"+strconv.Itoa(i), relOtherCode, ""))
	}
	mb := newRelMock(t, "["+strings.Join(docs, ",")+"]")
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := albumMBID(t, dbPath); got != relOneMBID {
		t.Errorf("album mbid = %q, want %s (the one release carrying the barcode)", got, relOneMBID)
	}
}

// TestEnrichRecordsAlbumNoMatch pins the marker: an album nothing could identify is
// not re-searched on the next unforced run.
func TestEnrichRecordsAlbumNoMatch(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcode, "", "")

	// Two releases share the barcode, so the uniqueness gate refuses to pick one.
	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+","+releaseDoc(relTwoMBID, relBarcode, "")+"]")
	svc := newRelService(st, mb.server.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 1 || res.AlbumsMatched != 0 {
		t.Fatalf("result = %+v, want 1 searched, 0 matched (ambiguous)", res)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("ambiguous match wrote mbid %q, want empty", got)
	}

	before := mb.requests
	res2, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.AlbumsSearched != 0 {
		t.Errorf("second run searched %d albums, want 0 (the no-match marker)", res2.AlbumsSearched)
	}
	if mb.requests != before {
		t.Errorf("second run made %d more requests, want 0", mb.requests-before)
	}
}

// TestEnrichDoesNotMarkAnUnsearchableAlbum: a release-group mbid that is not a UUID
// cannot be interpolated into a query, so the album is skipped without a request and
// WITHOUT a marker. A marker there would mean repairing the id never re-queues the
// album short of --force.
func TestEnrichDoesNotMarkAnUnsearchableAlbum(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	// A scan stores identifiers verbatim, so a tag can put anything in that column.
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcode, "", "not-a-uuid")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	if _, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(mb.releaseQueries) != 0 {
		t.Errorf("release queries = %v, want none (nothing safe to search with)", mb.releaseQueries)
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album'"); n != 0 {
		t.Errorf("album markers = %d, want 0 so a repaired id re-queues", n)
	}
}

// TestEnrichLeavesUnidentifiableAlbumEmpty checks the queue gate: an album with no
// barcode and no catalog number is never queued, so it costs no request at all.
func TestEnrichLeavesUnidentifiableAlbumEmpty(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", "", "", "")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	res, err := newRelService(st, mb.server.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 0 {
		t.Errorf("albums searched = %d, want 0 (no identifier to search by)", res.AlbumsSearched)
	}
	if len(mb.releaseQueries) != 0 {
		t.Errorf("release queries = %v, want none", mb.releaseQueries)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("album mbid = %q, want empty", got)
	}
}

// TestEnrichSkipsTheReleaseMatchWhenDisabled pins the toggle: with MatchReleases off
// the phase does not run, so no album is searched and none carries a marker.
func TestEnrichSkipsTheReleaseMatchWhenDisabled(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumWithIdentifiers(t, st, lib.ID, "ess-a", relBarcode, "", "")

	mb := newRelMock(t, "["+releaseDoc(relOneMBID, relBarcode, "")+"]")
	svc := enrich.New(st, enrich.Config{
		Contact:            "test@example.com",
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL,
	}, nil)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumsSearched != 0 || len(mb.releaseQueries) != 0 {
		t.Errorf("disabled toggle still searched: result %+v, queries %v", res, mb.releaseQueries)
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album'"); n != 0 {
		t.Errorf("album markers = %d, want 0", n)
	}
	if got := albumMBID(t, dbPath); got != "" {
		t.Errorf("album mbid = %q, want empty", got)
	}
}
