package enrich_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
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

// seedTrackWith persists one track at path, with whatever else the caller put on tr.
// Artist, AlbumArtist, Album and TrackNo are the caller's to set; everything else about
// the file and the item follows from path, essence and title, which is the part every
// seeder in this package shares.
func seedTrackWith(t *testing.T, st *sqlite.Store, libID int64, path, essence, title string, tr model.Track) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
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
		Track: tr,
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// seedTrack persists one track (creating its artist/release-group/album entities).
func seedTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, artist, album string) model.PID {
	t.Helper()
	return seedTrackWith(t, st, libID, path, essence, title,
		model.Track{Artist: artist, AlbumArtist: artist, Album: album, TrackNo: 1})
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

// owedMarkers counts one marker type's rows recording a lookup as owed, the state a
// failed lookup leaves (owed = 1) until a later pass settles it.
func owedMarkers(t *testing.T, dbPath, typ string) int {
	t.Helper()
	return scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = ? AND owed = 1", typ)
}

// settledMarkers counts one marker type's rows recording an answer, a match or a miss.
func settledMarkers(t *testing.T, dbPath, typ string) int {
	t.Helper()
	return scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = ? AND owed = 0", typ)
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
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtReleaseGroup, PID: rgPID}, model.ArtRoleFront, 0)
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

// TestEnrichDisabledWithoutContact: with neither route to a runnable phase, the pass
// still refuses outright rather than walking queues nothing can serve.
func TestEnrichDisabledWithoutContact(t *testing.T) {
	st, _, _ := openStore(t)
	svc := enrich.New(st, enrich.Config{}, nil) // no contact, no providers
	if svc.Enabled() {
		t.Fatal("service should be disabled without a contact or a provider")
	}
	_, err := svc.Run(context.Background(), enrich.RunOptions{}, nil)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("Run without contact err = %v, want CodeUnsupported", err)
	}
}

// TestEnrichRunsInjectedPhasesWithoutContact: the contact is what MusicBrainz and the
// key-free built-ins demand, and it says nothing about a provider that brings its own
// credentials. Without one the identity phases are skipped and the injected provider's
// phase walks anyway, which is what makes a contact-less install worth running at all.
// The FetchLyrics default is on regardless of the contact, so this also pins that no
// built-in is registered: an unpaced walk of every track against LRCLIB under the
// default User-Agent is exactly what the guard exists to prevent.
func TestEnrichRunsInjectedPhasesWithoutContact(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Basement Tape", "The Local Band", "Demo")

	var reqs []enrich.Request
	art := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			reqs = append(reqs, req)
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, "local-front"),
			}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		// No contact. The three built-in toggles are on the way the facade defaults
		// them, so nothing but the guard keeps LRCLIB and the rest out.
		FetchCoverArt: true, FetchLyrics: true, FetchCommunityGenres: true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: deadURL(t), ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: deadURL(t),
		Providers: []enrich.Provider{art},
	}, nil)
	if !svc.Enabled() {
		t.Fatal("a registered CapArtistArt provider should enable the pass without a contact")
	}

	var beats int
	res, err := svc.Run(ctx, enrich.RunOptions{}, func(float64, string) error { beats++; return nil })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ArtistsEnriched != 0 || res.ReleaseGroupsEnriched != 0 || res.BooksEnriched != 0 {
		t.Errorf("identity phases walked %d artists / %d groups / %d books, want none without a contact",
			res.ArtistsEnriched, res.ReleaseGroupsEnriched, res.BooksEnriched)
	}
	if res.LyricsEnriched != 0 {
		t.Errorf("lyrics walked %d tracks; no built-in should be registered without a contact",
			res.LyricsEnriched)
	}
	if res.ArtistArtEnriched != 1 || res.ArtistArtMatched != 1 {
		t.Fatalf("artist-art backfill = %d walked / %d matched, want 1 and 1",
			res.ArtistArtEnriched, res.ArtistArtMatched)
	}
	// The one ask is the artist backfill's, by name: nothing reached MusicBrainz to
	// give the artist an id.
	if len(reqs) != 1 || reqs[0].Type != enrich.TargetArtist ||
		reqs[0].Artist != "The Local Band" || reqs[0].MBID != "" {
		t.Fatalf("provider asked %+v, want one artist request keyed on the name alone", reqs)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "local-front" {
		t.Errorf("artist front hash = %q, want the name-keyed fill", h)
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

// TestEnrichScopedRun drives a scoped pass end to end: only the scoped item's
// artist and release group are looked up and marked (the rest of the catalog is
// untouched), and the scope implies force, so a second scoped run re-enriches
// its targets instead of skipping them by their markers.
func TestEnrichScopedRun(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	scoped := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Other Song", "Other Artist", "Other Album")

	scope, err := st.EnrichScopeForItem(ctx, scoped)
	if err != nil {
		t.Fatalf("EnrichScopeForItem: %v", err)
	}

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{Scope: scope}, nil)
	if err != nil {
		t.Fatalf("scoped Run: %v", err)
	}
	if res.ArtistsEnriched != 1 || res.ReleaseGroupsEnriched != 1 {
		t.Fatalf("scoped result = %+v, want exactly 1 artist + 1 release group", res)
	}

	db := roDB(t, dbPath)
	// The scoped targets are enriched and marked with full provenance.
	if mbid := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM artist WHERE name='Pink Floyd'"); mbid != "pf-mbid" {
		t.Errorf("scoped artist mbid = %q, want pf-mbid", mbid)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment ee JOIN artist a ON a.id=ee.entity_id
		WHERE ee.entity_type='artist' AND a.name='Pink Floyd'`); n != 1 {
		t.Errorf("scoped artist marker rows = %d, want 1", n)
	}
	// The out-of-scope artist and release group were never looked up: no mbid, no
	// marker (the mock would have answered for them too, so absence proves the
	// scope pruned the walk, not the provider).
	if mbid := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM artist WHERE name='Other Artist'"); mbid != "" {
		t.Errorf("out-of-scope artist gained mbid %q", mbid)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment ee JOIN artist a ON a.id=ee.entity_id
		WHERE ee.entity_type='artist' AND a.name='Other Artist'`); n != 0 {
		t.Errorf("out-of-scope artist has %d marker rows, want 0", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment ee JOIN release_group rg ON rg.id=ee.entity_id
		WHERE ee.entity_type='release_group' AND rg.title='Other Album'`); n != 0 {
		t.Errorf("out-of-scope release group has %d marker rows, want 0", n)
	}

	// A second scoped run without Force still re-enriches its targets (scope
	// implies force); an unscoped unforced run would have found nothing to do.
	res2, err := svc.Run(ctx, enrich.RunOptions{Scope: scope}, nil)
	if err != nil {
		t.Fatalf("second scoped Run: %v", err)
	}
	if res2.ArtistsEnriched != 1 || res2.ReleaseGroupsEnriched != 1 {
		t.Fatalf("second scoped result = %+v, want the targets re-enriched (scope implies force)", res2)
	}
}

// TestEnrichScopedRunSkipsEmptyPhases verifies a scope with targets for only one
// phase runs just that phase: an artist-only scope must not walk release groups,
// books, or lyrics.
func TestEnrichScopedRunSkipsEmptyPhases(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	db := roDB(t, dbPath)
	var artistID int64
	if err := db.QueryRow("SELECT id FROM artist WHERE name='Pink Floyd'").Scan(&artistID); err != nil {
		t.Fatalf("resolve artist: %v", err)
	}

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{Scope: &model.EnrichScope{ArtistIDs: []int64{artistID}}}, nil)
	if err != nil {
		t.Fatalf("artist-only scoped Run: %v", err)
	}
	if res.ArtistsEnriched != 1 || res.ReleaseGroupsEnriched != 0 || res.LyricsEnriched != 0 {
		t.Fatalf("artist-only scoped result = %+v, want 1 artist and nothing else", res)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group'"); n != 0 {
		t.Errorf("release-group markers = %d, want 0 (phase must be skipped)", n)
	}
}

// TestCoverArtProvenanceRecordsProviderAndURL: a fetched release-group cover is stored
// as enrichment art naming the provider and the archive request URL, not the redirect
// target the client actually read the bytes from.
func TestCoverArtProvenanceRecordsProviderAndURL(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	if _, err := newService(st, mb.server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	db := roDB(t, dbPath)
	const q = `SELECT am.%s FROM art_map am JOIN release_group rg ON rg.id = am.entity_id
		WHERE am.entity_type = 'release_group' AND am.role = 'front'`
	if got := scalarStr(t, db, fmt.Sprintf(q, "source")); got != string(model.SourceEnrichment) {
		t.Errorf("fetched cover source = %q, want enrichment", got)
	}
	if got := scalarStr(t, db, fmt.Sprintf(q, "provider")); got != "coverartarchive" {
		t.Errorf("fetched cover provider = %q, want coverartarchive", got)
	}
	want := caa.URL + "/release-group/wywh-mbid/front"
	if got := scalarStr(t, db, fmt.Sprintf(q, "source_url")); got != want {
		t.Errorf("fetched cover source_url = %q, want the stable request URL %q", got, want)
	}
}

// TestLockedCoverIsNotFetched: the store refuses to replace a locked cover, so the pass
// must not spend a rate-limited Cover Art Archive request discovering that on every
// forced run.
func TestLockedCoverIsNotFetched(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	rgPID := scalarStr(t, roDB(t, dbPath), "SELECT pid FROM release_group WHERE title='Wish You Were Here'")
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, model.PID(rgPID), model.ArtRoleFront, pngBytes(t), "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("SetEntityArt: %v", err)
	}

	mb := newMBMock(t)
	caa, caaHits := newCAAMock(t, pngBytes(t))
	if _, err := newService(st, mb.server.URL, caa.URL).Run(ctx, enrich.RunOptions{Force: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if *caaHits != 0 {
		t.Errorf("CAA fetched %d times for a locked cover, want 0", *caaHits)
	}
}

// flakyGroupFront serves the one group's front after failing the first fails fetches,
// counting every fetch, and 404s every other path.
func flakyGroupFront(t *testing.T, art []byte, fails int) (*httptest.Server, *int) {
	t.Helper()
	fetches := new(int)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group/wywh-mbid/front" {
			http.NotFound(w, r)
			return
		}
		*fetches++
		if *fetches <= fails {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(art)
	}))
	t.Cleanup(s.Close)
	return s, fetches
}

// TestGroupFrontFailureLeavesTheGroupQueued: MusicBrainz answered, so the identity lands,
// but the archive failed on the group's front, and no later phase ever asks about a group
// front. The group is owed the lookup, so the next pass re-walks it off the MusicBrainz
// cache, and the entity delta rides on what actually landed: one for the identity, one
// when the front arrives.
func TestGroupFrontFailureLeavesTheGroupQueued(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, fetches := flakyGroupFront(t, pngBytes(t), 1)
	svc := newService(st, mb.server.URL, caa.URL)
	db := roDB(t, dbPath)
	scanned := scalarInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='release_group'")
	groupDeltas := func() int {
		return scalarInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='release_group'") - scanned
	}

	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.ReleaseGroupsMatched != 1 || first.Deferred != 1 {
		t.Fatalf("run 1 = %d matched / %d deferred, want 1 and 1", first.ReleaseGroupsMatched, first.Deferred)
	}
	if m := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM release_group"); m != "wywh-mbid" {
		t.Errorf("group mbid = %q, want the identity landed despite the art failure", m)
	}
	if g := scalarStr(t, db, `SELECT t.genre FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(item)); g == "" {
		t.Error("genres did not land beside the failed front")
	}
	if n := settledMarkers(t, dbPath, "release_group"); n != 0 || owedMarkers(t, dbPath, "release_group") != 1 {
		t.Fatalf("group markers = %d settled, want the lookup owed while the front is", n)
	}
	if n := groupDeltas(); n != 1 {
		t.Fatalf("group deltas after run 1 = %d, want 1 for the mbid and type", n)
	}
	requests := mb.requests

	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ReleaseGroupsEnriched != 1 || second.Deferred != 0 || second.ArtFetched != 1 || mb.requests != requests {
		t.Fatalf("run 2 = %+v with %d new MusicBrainz requests, want the front fetched off a cached re-walk", second, mb.requests-requests)
	}
	if *fetches != 2 {
		t.Errorf("front fetches = %d, want 2", *fetches)
	}
	if n := settledMarkers(t, dbPath, "release_group"); n != 1 {
		t.Errorf("group markers = %d, want the durable match once the front landed", n)
	}
	if n := groupDeltas(); n != 2 {
		t.Errorf("group deltas after the front landed = %d, want 2", n)
	}
}

// TestAGroupFrontFailingTwiceSettlesTheGroup: the pass after a front failure asks the
// archive once more, and that ask settles the group whatever it gets, so a front the
// archive keeps failing on costs one more request rather than one per pass. The re-walk
// changed nothing, so it sends no delta.
func TestAGroupFrontFailingTwiceSettlesTheGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, fetches := flakyGroupFront(t, pngBytes(t), 2)
	svc := newService(st, mb.server.URL, caa.URL)
	db := roDB(t, dbPath)
	scanned := scalarInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='release_group'")
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ReleaseGroupsEnriched != 1 || second.Deferred != 0 || *fetches != 2 {
		t.Fatalf("run 2 = %d walked / %d deferred after %d fetches, want the second failure to settle the group",
			second.ReleaseGroupsEnriched, second.Deferred, *fetches)
	}
	if owed, settled := owedMarkers(t, dbPath, "release_group"), settledMarkers(t, dbPath, "release_group"); owed != 0 || settled != 1 {
		t.Errorf("group markers = %d owed / %d settled, want the match settled", owed, settled)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM change_log WHERE entity_type='release_group'") - scanned; n != 1 {
		t.Errorf("group deltas = %d, want only the identity's, since the re-walk changed nothing", n)
	}
	third, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if third.ReleaseGroupsEnriched != 0 || *fetches != 2 {
		t.Errorf("run 3 walked %d groups after %d fetches, want the settled group left alone", third.ReleaseGroupsEnriched, *fetches)
	}
}

// manyGroupsMB answers a release-group search for any title with one group of that title
// by "Band", and the lookup of that group, so a test can walk several groups. Artist
// searches match nothing.
func manyGroupsMB(t *testing.T) *mbMock {
	t.Helper()
	m := &mbMock{}
	titleOf := map[string]string{}
	title := regexp.MustCompile(`releasegroup:"([^"]*)"`)
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests++
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		credit := `"artist-credit":[{"artist":{"id":"band-mbid","name":"Band"}}]`
		switch {
		case r.URL.Path == "/artist" && q != "":
			io(w, `{"artists":[]}`)
		case r.URL.Path == "/release-group" && q != "":
			mt := title.FindStringSubmatch(q)
			if mt == nil {
				io(w, `{"release-groups":[]}`)
				return
			}
			id := "rg-" + strings.ReplaceAll(strings.ToLower(mt[1]), " ", "-")
			titleOf[id] = mt[1]
			io(w, `{"release-groups":[{"id":"`+id+`","title":"`+mt[1]+`","primary-type":"Album","score":100,`+credit+`}]}`)
		case strings.HasPrefix(r.URL.Path, "/release-group/") && titleOf[strings.TrimPrefix(r.URL.Path, "/release-group/")] != "":
			id := strings.TrimPrefix(r.URL.Path, "/release-group/")
			io(w, `{"id":"`+id+`","title":"`+titleOf[id]+`","primary-type":"Album","secondary-types":[],`+credit+`,"genres":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

// TestATrippedArchiveDefersTheGroupsAfterIt: the identity rungs never stall, since the
// spine has to land, but a matched identity marker is durable and nothing later asks
// about a group front. So a group walked while the archive is out of the pass is
// deferred like the ones it failed on, and the next pass re-walks all of them off the
// MusicBrainz cache and fetches their fronts.
func TestATrippedArchiveDefersTheGroupsAfterIt(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Band", "Album "+k)
	}
	mb := manyGroupsMB(t)
	down := 0
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/release-group/") {
			down++
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(dead.Close)

	res, err := newService(st, mb.server.URL, dead.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if down != 3 {
		t.Errorf("archive fetches = %d, want 3 before it dropped out", down)
	}
	if res.ReleaseGroupsMatched != 5 || res.Deferred != 5 || len(res.Stalled) != 0 {
		t.Fatalf("run 1 = %d matched / %d deferred / stalled %v, want 5, 5 and none",
			res.ReleaseGroupsMatched, res.Deferred, res.Stalled)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM release_group WHERE mbid LIKE 'rg-%'"); n != 5 {
		t.Errorf("groups with an mbid = %d, want 5", n)
	}
	if owedMarkers(t, dbPath, "release_group") != 5 || settledMarkers(t, dbPath, "release_group") != 0 {
		t.Errorf("group markers = %d owed / %d settled, want all five owed",
			owedMarkers(t, dbPath, "release_group"), settledMarkers(t, dbPath, "release_group"))
	}

	requests := mb.requests
	healthy, fetched := 0, pngBytes(t)
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/release-group/") {
			healthy++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fetched)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(archive.Close)
	res, err = newService(st, mb.server.URL, archive.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if healthy != 5 || res.ArtFetched != 5 || mb.requests != requests {
		t.Fatalf("run 2 fetched %d fronts (%d counted) with %d new MusicBrainz requests, want 5, 5 and none",
			healthy, res.ArtFetched, mb.requests-requests)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='release_group' AND matched=1"); n != 5 {
		t.Errorf("matched group markers = %d, want 5", n)
	}
}

// TestAGroupLeftUnaskedStaysOwed: the pass after an archive outage asks the owed groups
// again while the archive is still failing. The first three fail again, which settles
// them and trips the archive; the two walked after that were never asked, so they stay
// owed, and the pass after fetches their fronts.
func TestAGroupLeftUnaskedStaysOwed(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Band", "Album "+k)
	}
	mb := manyGroupsMB(t)
	healthy, fronts, art := false, 0, pngBytes(t)
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/release-group/") {
			http.NotFound(w, r)
			return
		}
		if !healthy {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		fronts++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(art)
	}))
	t.Cleanup(archive.Close)
	svc := newService(st, mb.server.URL, archive.URL)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if owed := owedMarkers(t, dbPath, "release_group"); owed != 5 {
		t.Fatalf("owed groups after the outage = %d, want 5", owed)
	}

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if owed, settled := owedMarkers(t, dbPath, "release_group"), settledMarkers(t, dbPath, "release_group"); owed != 2 || settled != 3 || res.Deferred != 2 {
		t.Fatalf("run 2 = %d owed / %d settled / %d deferred, want the three asked settled and the two unasked still owed",
			owed, settled, res.Deferred)
	}

	healthy = true
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if fronts != 2 || owedMarkers(t, dbPath, "release_group") != 0 {
		t.Errorf("run 3 fetched %d fronts with %d still owed, want the two unasked groups' fronts", fronts, owedMarkers(t, dbPath, "release_group"))
	}
}

// TestAGenreRiderLeftUnaskedStaysOwed is the genre twin of TestAGroupLeftUnaskedStaysOwed:
// MusicBrainz has no genres for these groups, so the injected genre provider is the only
// source, and the groups it was never asked about stay owed until it answers.
func TestAGenreRiderLeftUnaskedStaysOwed(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Band", "Album "+k)
	}
	mb := manyGroupsMB(t)
	healthy := false
	genres := &enrich.Mock{ProviderName: "tags", Caps: enrich.CapGenres,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			if !healthy {
				return nil, errors.New("tags is down")
			}
			return &enrich.Candidate{Genres: []string{"Rock"}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MinRequestInterval: time.Millisecond, MusicBrainzBaseURL: mb.server.URL,
		Providers: []enrich.Provider{genres},
	}, nil)
	for run := 1; run <= 2; run++ {
		if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if owed, settled := owedMarkers(t, dbPath, "release_group"), settledMarkers(t, dbPath, "release_group"); owed != 2 || settled != 3 {
		t.Fatalf("after run 2: %d owed / %d settled, want the three asked twice settled and the two unasked still owed", owed, settled)
	}
	healthy = true
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM track WHERE genre = 'Rock'"); n != 2 || owedMarkers(t, dbPath, "release_group") != 0 {
		t.Errorf("run 3 filled %d tracks with %d groups still owed, want the two unasked groups' genres", n, owedMarkers(t, dbPath, "release_group"))
	}
}

// TestAGroupWhoseTracksHaveGenresIsNotOwedThem: the genre fill reaches only tracks with no
// genre, so when every track of a group already carries one, a failed genre provider
// leaves nothing a later pass could add, and the group settles.
func TestAGroupWhoseTracksHaveGenresIsNotOwedThem(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrackWith(t, st, lib.ID, "/lib/1.mp3", "ess-1", "Song 1",
		model.Track{Artist: "Band", AlbumArtist: "Band", Album: "Album 1", TrackNo: 1, Genre: "Jazz", Genres: []string{"Jazz"}})
	mb := manyGroupsMB(t)
	genres := &enrich.Mock{ProviderName: "tags", Caps: enrich.CapGenres,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			return nil, errors.New("tags is down")
		}}
	res, err := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MinRequestInterval: time.Millisecond, MusicBrainzBaseURL: mb.server.URL,
		Providers: []enrich.Provider{genres},
	}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 || res.Deferred != 0 {
		t.Fatalf("run = %d matched / %d deferred, want the group settled", res.ReleaseGroupsMatched, res.Deferred)
	}
	if owed, settled := owedMarkers(t, dbPath, "release_group"), settledMarkers(t, dbPath, "release_group"); owed != 0 || settled != 1 {
		t.Errorf("group markers = %d owed / %d settled, want one settled match", owed, settled)
	}
}

// TestAnArtistFrontLeftUnaskedStaysOwed is the artist twin of
// TestAGroupLeftUnaskedStaysOwed, with an injected cover provider serving the artist rung.
func TestAnArtistFrontLeftUnaskedStaysOwed(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Artist "+k, "Album "+k)
	}
	mb := manyNamesMB(t)
	healthy := false
	portraits := &enrich.Mock{ProviderName: "portraits", Caps: enrich.CapCover,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetArtist: enrich.CapCover},
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if !healthy {
				return nil, errors.New("portraits is down")
			}
			return &enrich.Candidate{Cover: artImg(t, "portrait-"+req.Artist)}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MinRequestInterval: time.Millisecond, MusicBrainzBaseURL: mb.server.URL,
		Providers: []enrich.Provider{portraits},
	}, nil)
	for run := 1; run <= 2; run++ {
		if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if owed, settled := owedMarkers(t, dbPath, "artist"), settledMarkers(t, dbPath, "artist"); owed != 2 || settled != 3 {
		t.Fatalf("after run 2: %d owed / %d settled, want the three asked twice settled and the two unasked still owed", owed, settled)
	}
	healthy = true
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM art_map WHERE entity_type = 'artist' AND role = 'front'"); n != 2 ||
		owedMarkers(t, dbPath, "artist") != 0 {
		t.Errorf("run 3 attached %d artist fronts with %d artists still owed, want the two unasked artists' fronts", n, owedMarkers(t, dbPath, "artist"))
	}
}

// manyNamesMB answers an artist search for any name with one artist of that name, and a
// release-group search for any title with one group of that title credited to the
// searched artist, with the lookups behind both, so a test can walk several of each.
func manyNamesMB(t *testing.T) *mbMock {
	t.Helper()
	m := &mbMock{}
	nameOf, creditOf := map[string]string{}, map[string]string{}
	quoted := func(q, field string) string {
		mt := regexp.MustCompile(field + `:"([^"]*)"`).FindStringSubmatch(q)
		if mt == nil {
			return ""
		}
		return mt[1]
	}
	slug := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), " ", "-") }
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests++
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.URL.Path == "/artist" && q != "":
			name := quoted(q, "artist")
			nameOf["ar-"+slug(name)] = name
			io(w, `{"artists":[{"id":"ar-`+slug(name)+`","name":"`+name+`","sort-name":"`+name+`","score":100}]}`)
		case strings.HasPrefix(r.URL.Path, "/artist/") && nameOf[id] != "":
			io(w, `{"id":"`+id+`","name":"`+nameOf[id]+`","sort-name":"`+nameOf[id]+`"}`)
		case r.URL.Path == "/release-group" && q != "":
			title, artist := quoted(q, "releasegroup"), quoted(q, "artist")
			gid := "rg-" + slug(title)
			nameOf[gid], creditOf[gid] = title, artist
			io(w, `{"release-groups":[{"id":"`+gid+`","title":"`+title+`","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"ar-`+slug(artist)+`","name":"`+artist+`"}}]}]}`)
		case strings.HasPrefix(r.URL.Path, "/release-group/") && nameOf[id] != "":
			io(w, `{"id":"`+id+`","title":"`+nameOf[id]+`","primary-type":"Album","secondary-types":[],
				"artist-credit":[{"artist":{"id":"ar-`+slug(creditOf[id])+`","name":"`+creditOf[id]+`"}}],"genres":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

// TestACappedRunReachesLaterPhasesPastOwedIdentities: an identity rung never stalls, so a
// rider that never recovers (a revoked key) leaves every artist it touches owed. Those
// are re-walked after every phase's new targets, so a nightly cap still reaches the
// release groups instead of spending itself on the same artists every night.
func TestACappedRunReachesLaterPhasesPastOwedIdentities(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	for i := 1; i <= 6; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Artist "+k, "Album "+k)
	}
	revoked := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetArtist: enrich.CapCover},
		Err:    errors.New("401: api key revoked")}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: manyNamesMB(t).server.URL,
		Providers:          []enrich.Provider{revoked},
	}, nil)

	first, err := svc.Run(ctx, enrich.RunOptions{Limit: 4}, nil)
	if err != nil {
		t.Fatalf("night 1: %v", err)
	}
	if first.ArtistsEnriched != 4 || first.Deferred != 4 {
		t.Fatalf("night 1 = %d artists / %d deferred, want the cap spent on four owed artists", first.ArtistsEnriched, first.Deferred)
	}
	second, err := svc.Run(ctx, enrich.RunOptions{Limit: 4}, nil)
	if err != nil {
		t.Fatalf("night 2: %v", err)
	}
	if second.ReleaseGroupsEnriched == 0 {
		t.Fatalf("night 2 = %d artists / %d groups, want the cap to reach the release groups", second.ArtistsEnriched, second.ReleaseGroupsEnriched)
	}
}

// TestAnAuxFailureDoesNotDeferTheGroup: the group rung's art rider is owed only its
// front, the slot nothing later asks about. An auxiliary provider failing there, or
// dropping out of the pass after three failures, leaves every group settled, and the
// auxiliary backfill, which asks that provider about the same groups under its own
// marker, is what carries the failure.
func TestAnAuxFailureDoesNotDeferTheGroup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Band", "Album "+k)
	}
	fanart := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetReleaseGroup: enrich.CapAuxArt},
		Err:    errors.New("fanart key expired")}
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", FetchCoverArt: true, MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: manyGroupsMB(t).server.URL, CoverArtBaseURL: caaStatus(t, http.StatusNotFound).URL,
		Providers: []enrich.Provider{fanart},
	}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if settledMarkers(t, dbPath, "release_group") != 5 || owedMarkers(t, dbPath, "release_group") != 0 {
		t.Errorf("group identities = %d settled / %d owed, want all five settled: their fronts were answered",
			settledMarkers(t, dbPath, "release_group"), owedMarkers(t, dbPath, "release_group"))
	}
}

// TestAGroupHoldingAFrontIsNotOwedForItsRider: a group that already holds a front asks the
// archive only to refresh it, so a failed or skipped refresh leaves the group settled. A
// forced run during an archive outage therefore owes nothing for the groups it walks,
// rather than leaving the whole catalog to be walked again.
func TestAGroupHoldingAFrontIsNotOwedForItsRider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, lib.ID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Band", "Album "+k)
	}
	down := false
	art := pngBytes(t)
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/release-group/") {
			http.NotFound(w, r)
			return
		}
		if down {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(art)
	}))
	t.Cleanup(caa.Close)
	svc := newService(st, manyGroupsMB(t).server.URL, caa.URL)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND role='front'"); n != 5 {
		t.Fatalf("group fronts after run 1 = %d, want 5", n)
	}

	down = true
	res, err := svc.Run(ctx, enrich.RunOptions{Force: true}, nil)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if res.Deferred != 0 || settledMarkers(t, dbPath, "release_group") != 5 || owedMarkers(t, dbPath, "release_group") != 0 {
		t.Errorf("forced run = %d deferred, %d settled / %d owed groups; want every group settled, since each holds a front",
			res.Deferred, settledMarkers(t, dbPath, "release_group"), owedMarkers(t, dbPath, "release_group"))
	}
}
