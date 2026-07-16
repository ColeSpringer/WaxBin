package enrich_test

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// --- test fixtures: a MusicBrainz + Cover Art Archive mock ------------------

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 40), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

type mbMock struct {
	server   *httptest.Server
	requests int
}

// newMBMock serves the MusicBrainz endpoints enrichment uses for one album by
// "Pink Floyd". It counts requests so tests can assert caching.
func newMBMock(t *testing.T) *mbMock {
	t.Helper()
	m := &mbMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests++
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		hasQuery := r.URL.Query().Get("query") != ""
		switch {
		case path == "/artist" && hasQuery:
			io(w, `{"artists":[{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd","score":100}]}`)
		case path == "/artist/pf-mbid":
			io(w, `{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd",
				"aliases":[{"name":"The Pink Floyd Sound"}],
				"relations":[{"type":"member of band","direction":"forward","artist":{"id":"gilmour-mbid","name":"David Gilmour"}}],
				"genres":[{"name":"Progressive Rock","count":4}]}`)
		case path == "/release-group" && hasQuery:
			io(w, `{"release-groups":[{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}]}]}`)
		case path == "/release-group/wywh-mbid":
			io(w, `{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","secondary-types":[],
				"genres":[{"name":"Progressive Rock","count":5},{"name":"Rock","count":3}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func io(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(body)) }

func newCAAMock(t *testing.T, art []byte) (*httptest.Server, *int) {
	t.Helper()
	hits := new(int)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release-group/wywh-mbid/front" {
			*hits++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(art)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s, hits
}

// --- store seeding ----------------------------------------------------------

func openStore(t *testing.T) (*sqlite.Store, string, *model.Library) {
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

// seedTrack persists one track (creating its artist/release-group/album entities).
func seedTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, artist, album string) model.PID {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: artist, AlbumArtist: artist, Album: album, TrackNo: 1},
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// roDB opens a read-only connection for assertion queries against the live catalog.
func roDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func scalarStr(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func scalarInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// newService builds an enrichment service wired to the mock endpoints with pacing
// effectively disabled (a tiny non-zero interval). Community genres are on (as in the
// default build) but its ListenBrainz base URL, and the (here-disabled) LRCLIB base
// URL, point at the CAA mock, which 404s their paths, so the pass never reaches the
// real network for a genre/lyrics lookup.
func newService(st enrich.Store, mbURL, caaURL string) *enrich.Service {
	return enrich.New(st, enrich.Config{
		Contact:              "test@example.com",
		FetchCoverArt:        true,
		FetchCommunityGenres: true,
		MinRequestInterval:   time.Millisecond,
		MusicBrainzBaseURL:   mbURL,
		CoverArtBaseURL:      caaURL,
		ListenBrainzBaseURL:  caaURL,
		LRCLibBaseURL:        caaURL,
	}, nil)
}

// --- tests ------------------------------------------------------------------

func TestEnrichHappyPath(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	item2 := seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Have a Cigar", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	art := pngBytes(t)
	caa, caaHits := newCAAMock(t, art)

	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 {
		t.Fatalf("release groups matched = %d, want 1", res.ReleaseGroupsMatched)
	}
	if res.ArtistsMatched != 1 {
		t.Fatalf("artists matched = %d, want 1 (Pink Floyd)", res.ArtistsMatched)
	}
	if res.ArtFetched != 1 {
		t.Fatalf("art fetched = %d, want 1", res.ArtFetched)
	}
	if *caaHits != 1 {
		t.Fatalf("CAA hits = %d, want 1", *caaHits)
	}

	db := roDB(t, dbPath)

	// Release group: MBID + type populated.
	if mbid := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM release_group WHERE title='Wish You Were Here'"); mbid != "wywh-mbid" {
		t.Errorf("release_group mbid = %q, want wywh-mbid", mbid)
	}
	if typ := scalarStr(t, db, "SELECT COALESCE(type,'') FROM release_group WHERE title='Wish You Were Here'"); typ != "album" {
		t.Errorf("release_group type = %q, want album", typ)
	}

	// Artist: MBID + alias populated.
	if mbid := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM artist WHERE name='Pink Floyd'"); mbid != "pf-mbid" {
		t.Errorf("artist mbid = %q, want pf-mbid", mbid)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM artist_alias al JOIN artist a ON a.id=al.artist_id WHERE a.name='Pink Floyd' AND al.name='The Pink Floyd Sound'"); n != 1 {
		t.Errorf("expected the Pink Floyd Sound alias, found %d", n)
	}

	// Genres populated on both items (they had none).
	genreCount := scalarInt(t, db, `SELECT COUNT(DISTINCT g.name) FROM genre g
		JOIN item_genre ig ON ig.genre_id=g.id`)
	if genreCount != 2 {
		t.Errorf("distinct genres attached = %d, want 2 (Progressive Rock, Rock)", genreCount)
	}
	item2Genres := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig
		JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(item2))
	if item2Genres != 2 {
		t.Errorf("item2 genres = %d, want 2", item2Genres)
	}
	// Enrichment provenance recorded for the genre field.
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM field_provenance WHERE field='genre' AND source='enrichment'"); n != 2 {
		t.Errorf("genre enrichment provenance rows = %d, want 2", n)
	}
	// Denormalized track.genre set too, so the item display and `--genre` filter
	// (which read t.genre, not item_genre) also see the enrichment genres.
	if g := scalarStr(t, db, `SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(item2)); g == "" {
		t.Errorf("denormalized track.genre not set for enriched item")
	}

	// Cover art resolves at the release-group level.
	rgPID := model.PID(scalarStr(t, db, "SELECT pid FROM release_group WHERE title='Wish You Were Here'"))
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtReleaseGroup, PID: rgPID}, 0)
	if err != nil {
		t.Fatalf("ResolveArt(release_group): %v", err)
	}
	if !bytes.Equal(blob.Bytes, art) {
		t.Errorf("resolved art (%d bytes) does not match the fetched cover (%d bytes)", len(blob.Bytes), len(art))
	}

	// Response cache populated (offline re-use).
	if payload, ok, err := st.EnrichmentCacheGet(ctx, "mb:rg:wywh-mbid"); err != nil || !ok || len(payload) == 0 {
		t.Errorf("release-group response not cached (ok=%v err=%v len=%d)", ok, err, len(payload))
	}

	// Coverage reflects the run.
	cov, err := st.EnrichmentCoverage(ctx)
	if err != nil {
		t.Fatalf("EnrichmentCoverage: %v", err)
	}
	if cov.ReleaseGroups != 1 || cov.Artists != 1 || cov.Matched < 2 {
		t.Errorf("coverage = %+v, want 1 rg, 1 artist, >=2 matched", cov)
	}

	// Derived state stays consistent (genre rollups were maintained).
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("VerifyDerived: %v", err)
	}
	if !rep.Consistent() {
		t.Errorf("derived data inconsistent after enrichment: %+v", rep)
	}

	// A second, non-forced run is a no-op: everything is already marked.
	res2, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if n := res2Total(res2); n != 0 {
		t.Errorf("second run enriched %d entities, want 0 (all marked)", n)
	}
}

func res2Total(r *enrich.Result) int {
	return r.ArtistsEnriched + r.ReleaseGroupsEnriched + r.BooksEnriched
}

func TestEnrichRespectsGenreLock(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	locked := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Have a Cigar", "Pink Floyd", "Wish You Were Here")

	// Lock the genre on the first item; enrichment must not populate it.
	if err := st.LockField(ctx, locked, "genre"); err != nil {
		t.Fatalf("LockField: %v", err)
	}

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	svc := newService(st, mb.server.URL, caa.URL)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	db := roDB(t, dbPath)
	lockedGenres := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig
		JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(locked))
	if lockedGenres != 0 {
		t.Errorf("locked item got %d genres, want 0 (genre lock ignored)", lockedGenres)
	}
	total := scalarInt(t, db, "SELECT COUNT(*) FROM item_genre")
	if total == 0 {
		t.Errorf("the unlocked item should still have gained genres")
	}
}

// caaStatus serves a fixed status code at the front-cover path (for the
// definitive-vs-transient distinction).
func caaStatus(t *testing.T, code int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestEnrichCoverArt404IsNotFatal(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa := caaStatus(t, http.StatusNotFound) // no cover for this release group
	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("a 404 cover must not fail the run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 || res.ArtFetched != 0 {
		t.Fatalf("res = %+v, want 1 matched, 0 art", res)
	}
	// The release group is still enriched (type/mbid), just without a cover.
	db := roDB(t, dbPath)
	if typ := scalarStr(t, db, "SELECT COALESCE(type,'') FROM release_group WHERE title='Wish You Were Here'"); typ != "album" {
		t.Errorf("release_group type = %q, want album despite no cover", typ)
	}
}

func TestEnrichCoverArtTransientErrorIsBestEffort(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa := caaStatus(t, http.StatusInternalServerError) // transient
	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	// Cover art is best-effort: a transient CAA error is logged and skipped, never
	// aborting the run (only MusicBrainz, the core, aborts).
	if err != nil {
		t.Fatalf("a transient cover-art error must not abort the run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 || res.ArtFetched != 0 {
		t.Fatalf("res = %+v, want 1 matched, 0 art", res)
	}
	// The release group is still enriched (type/mbid) despite the cover failure.
	db := roDB(t, dbPath)
	if typ := scalarStr(t, db, "SELECT COALESCE(type,'') FROM release_group WHERE title='Wish You Were Here'"); typ != "album" {
		t.Errorf("release_group type = %q, want album despite the cover failure", typ)
	}
}

// TestEnrichReleaseGroupSearchRejectsWrongArtist verifies the release-group text
// search will not adopt a title-matching hit credited to a different artist (the
// "Greatest Hits" MBID-theft guard).
func TestEnrichReleaseGroupSearchRejectsWrongArtist(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Track", "Pink Floyd", "Greatest Hits")

	// The MB mock returns a title match credited to a DIFFERENT artist.
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/artist" && r.URL.Query().Get("query") != "":
			io(w, `{"artists":[]}`)
		case r.URL.Path == "/release-group" && r.URL.Query().Get("query") != "":
			io(w, `{"release-groups":[{"id":"other-mbid","title":"Greatest Hits","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"queen-mbid","name":"Queen"}}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mb.Close()
	caa := caaStatus(t, http.StatusNotFound)
	svc := newService(st, mb.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReleaseGroupsMatched != 0 {
		t.Fatalf("matched %d release groups, want 0 (wrong-artist hit must be rejected)", res.ReleaseGroupsMatched)
	}
	db := roDB(t, dbPath)
	if mbid := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM release_group WHERE title='Greatest Hits'"); mbid != "" {
		t.Fatalf("release_group adopted a wrong-artist mbid %q", mbid)
	}
}

func TestEnrichTrailingSlashBaseURL(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	// Trailing slashes on the base URLs must not produce a double slash that 404s.
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", FetchCoverArt: true, MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.server.URL + "/", CoverArtBaseURL: caa.URL + "/",
	}, nil)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run with trailing-slash base URLs: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 {
		t.Fatalf("release groups matched = %d, want 1 (trailing slash broke the path)", res.ReleaseGroupsMatched)
	}
}

// TestEnrichDoesNotCachePoisonedResponse verifies a 2xx-but-garbage body is not
// cached: it would otherwise wedge every non-forced resume (re-read, re-fail).
func TestEnrichDoesNotCachePoisonedResponse(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	// The permissive MIME allow-list accepts octet-stream, so a non-JSON body passes
	// the MIME check and reaches the parser.
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("this is not json"))
	}))
	defer mb.Close()
	caa := caaStatus(t, http.StatusNotFound)
	svc := newService(st, mb.URL, caa.URL)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err == nil {
		t.Fatal("a garbage MB response should surface as a parse error")
	}
	if _, ok, err := st.EnrichmentCacheGet(ctx, "mb:artist-search:pink floyd"); err != nil || ok {
		t.Fatalf("a garbage response must not be cached (ok=%v err=%v)", ok, err)
	}
}

func TestEnrichDisabledWithoutContact(t *testing.T) {
	st, _, _ := openStore(t)
	svc := enrich.New(st, enrich.Config{}, nil) // no contact
	if svc.Enabled() {
		t.Fatal("service should be disabled without a contact")
	}
	_, err := svc.Run(context.Background(), enrich.RunOptions{}, nil)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("Run without contact err = %v, want CodeUnsupported", err)
	}
}

func TestEnrichOfflineDegradesGracefully(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	// Point at a server that is immediately closed, so requests fail (connection
	// refused) rather than hang. Enrichment must return an error, not panic.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: deadURL, CoverArtBaseURL: deadURL,
	}, nil)
	_, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err == nil {
		t.Fatal("offline Run should return an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "enrich") && !waxerr.Is(err, waxerr.CodeIO) {
		// The error should surface as an I/O failure from the provider fetch.
		t.Logf("offline error: %v", err)
	}
}
