package enrich_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// --- mocks for the port providers --------------------------------------------

// mbMockGenres serves the MusicBrainz endpoints for one Pink Floyd album, with the
// release-group genres controlled by the caller (empty for a "genre gap" test). The
// artist search returns no hit so only the release-group path is exercised.
func mbMockGenres(t *testing.T, rgGenres string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hasQuery := r.URL.Query().Get("query") != ""
		switch {
		case r.URL.Path == "/artist" && hasQuery:
			io(w, `{"artists":[]}`)
		case r.URL.Path == "/release-group" && hasQuery:
			io(w, `{"release-groups":[{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}]}]}`)
		case r.URL.Path == "/release-group/wywh-mbid":
			io(w, `{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","secondary-types":[],
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}],
				"genres":`+rgGenres+`}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// lbMock serves the ListenBrainz release-group metadata endpoint, returning the given
// tags JSON array for mbid and 404 elsewhere.
func lbMock(t *testing.T, mbid, tagsJSON string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/1/metadata/release_group") {
			w.Header().Set("Content-Type", "application/json")
			io(w, `{"`+mbid+`":{"tag":{"release_group":`+tagsJSON+`}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// lrclibMock serves the LRCLIB /api/get endpoint with fixed synced/plain lyrics.
func lrclibMock(t *testing.T, synced, plain string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/get") {
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(map[string]any{
				"instrumental": false, "syncedLyrics": synced, "plainLyrics": plain,
			})
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// deadURL returns the URL of an immediately-closed server, so a provider pointed at it
// fails fast (connection refused) instead of reaching the real network.
func deadURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := s.URL
	s.Close()
	return u
}

func genreProvenanceProvider(t *testing.T, dbPath string, item model.PID) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath), `SELECT COALESCE(fp.provider,'')
		FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.field = 'genre'`, string(item))
}

// --- tests -------------------------------------------------------------------

// TestInjectedProviderFillsGenreGap: MusicBrainz resolves the release group but
// returns no genres; an injected provider fills the gap, and its name is recorded as
// the genre field's provenance provider.
func TestInjectedProviderFillsGenreGap(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[]`) // no MB genres
	mock := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapGenres,
		Ret: &enrich.Candidate{Genres: []string{"Shoegaze"}}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{mock},
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	if g := scalarStr(t, db, `SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(item)); g != "Shoegaze" {
		t.Errorf("track.genre = %q, want Shoegaze (injected provider gap-fill)", g)
	}
	if prov := genreProvenanceProvider(t, dbPath, item); prov != "discogs" {
		t.Errorf("genre provenance provider = %q, want discogs", prov)
	}
	assertDerivedConsistent(t, st)
}

// TestInjectedProviderWinsGenreConflict: with MusicBrainz genres, a ListenBrainz
// genre, and an injected genre all present, the injected provider's genre is the
// display-primary one (injected outranks the built-ins) and its name is the recorded
// provider, while the union still includes every provider's genres.
func TestInjectedProviderWinsGenreConflict(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[{"name":"Progressive Rock","count":5}]`)
	lb := lbMock(t, "wywh-mbid", `[{"tag":"art rock","count":9,"genre_mbid":"g1"}]`)
	mock := &enrich.Mock{ProviderName: "lastfm", Caps: enrich.CapGenres,
		Ret: &enrich.Candidate{Genres: []string{"Dream Pop"}}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCommunityGenres: true,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: lb.URL,
		Providers: []enrich.Provider{mock},
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	g := scalarStr(t, db, `SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(item))
	if !strings.HasPrefix(g, "Dream Pop") {
		t.Errorf("track.genre = %q, want to start with Dream Pop (injected wins the primary slot)", g)
	}
	// The union carries all three sources' genres.
	for _, want := range []string{"Dream Pop", "Progressive Rock", "art rock"} {
		n := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN genre gn ON gn.id=ig.genre_id
			JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=? AND gn.name=?`, string(item), want)
		if n != 1 {
			t.Errorf("genre %q not attached (union incomplete)", want)
		}
	}
	if prov := genreProvenanceProvider(t, dbPath, item); prov != "lastfm" {
		t.Errorf("genre provenance provider = %q, want lastfm (display-primary provider)", prov)
	}
	assertDerivedConsistent(t, st)
}

// TestListenBrainzGenres: the built-in ListenBrainz provider supplies genres from an
// httptest server, recorded with its provider name, when nothing else has any.
func TestListenBrainzGenres(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[]`)
	lb := lbMock(t, "wywh-mbid", `[{"tag":"art rock","count":3,"genre_mbid":"g1"},{"tag":"psychedelic","count":1,"genre_mbid":"g2"}]`)
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCommunityGenres: true,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: lb.URL,
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(item)); n != 2 {
		t.Errorf("item genres = %d, want 2 (both ListenBrainz tags)", n)
	}
	if prov := genreProvenanceProvider(t, dbPath, item); prov != "listenbrainz" {
		t.Errorf("genre provenance provider = %q, want listenbrainz", prov)
	}
}

// TestLRCLIBLyrics: the built-in LRCLIB provider fills lyrics for a track that has
// none, from an httptest server, records the per-recording marker, and stamps the
// provider as the lyrics source. A second run is a no-op (the marker is respected).
func TestLRCLIBLyrics(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[]`)
	lrc := lrclibMock(t, "[00:10.00]shine on\n[00:12.50]you crazy diamond", "shine on\nyou crazy diamond")
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: lrc.URL,
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsMatched != 1 || res.LyricsEnriched != 1 {
		t.Fatalf("lyrics enriched=%d matched=%d, want 1/1", res.LyricsEnriched, res.LyricsMatched)
	}
	ly, err := st.LyricsByItem(ctx, item)
	if err != nil {
		t.Fatalf("LyricsByItem: %v", err)
	}
	if ly.Source != "lrclib" {
		t.Errorf("lyrics source = %q, want lrclib", ly.Source)
	}
	if len(ly.Synced) != 2 || ly.Synced[0].TimeMS != 10000 || ly.Synced[1].TimeMS != 12500 {
		t.Errorf("synced lines = %+v, want two at 10000/12500 ms", ly.Synced)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment ee JOIN playable_item pi ON pi.id=ee.entity_id
		WHERE ee.entity_type='lyrics' AND ee.matched=1 AND pi.pid=?`, string(item)); n != 1 {
		t.Errorf("lyrics marker rows = %d, want 1 matched", n)
	}
	// Coverage ignores the lyrics marker (it counts only the three entity types).
	cov, err := st.EnrichmentCoverage(ctx)
	if err != nil {
		t.Fatalf("EnrichmentCoverage: %v", err)
	}
	if cov.Artists != 1 || cov.ReleaseGroups != 1 || cov.Books != 0 {
		t.Errorf("coverage = %+v, want 1 artist, 1 rg, 0 books (lyrics not counted)", cov)
	}

	// Second run does not re-query the track (the marker is respected).
	res2, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.LyricsEnriched != 0 {
		t.Errorf("second run looked up %d lyrics, want 0 (marker respected)", res2.LyricsEnriched)
	}
}

// TestOptionalProviderErrorDoesNotAbort: an injected provider that errors is
// best-effort, so the run completes and the MusicBrainz genres still land.
func TestOptionalProviderErrorDoesNotAbort(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[{"name":"Progressive Rock","count":5}]`)
	boom := &enrich.Mock{ProviderName: "flaky", Caps: enrich.CapGenres, Err: errors.New("provider exploded")}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{boom},
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("a failing optional provider must not abort the run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 {
		t.Fatalf("release groups matched = %d, want 1", res.ReleaseGroupsMatched)
	}
	db := roDB(t, dbPath)
	if g := scalarStr(t, db, `SELECT t.genre FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(item)); g != "Progressive Rock" {
		t.Errorf("track.genre = %q, want Progressive Rock (MB genre survives the provider error)", g)
	}
}

// TestLyricsFillWhenEmpty: a track that already has lyrics is never looked up, so an
// existing sidecar/embedded copy is preserved and no marker is written.
func TestLyricsFillWhenEmpty(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrackWithLyrics(t, st, lib.ID, &model.Lyrics{
		Source: "lrc", Synced: []model.SyncedLine{{TimeMS: 0, Text: "existing"}},
	})

	mb := mbMockGenres(t, `[]`)
	lrc := lrclibMock(t, "[00:05.00]replacement", "replacement")
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: lrc.URL,
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsEnriched != 0 {
		t.Errorf("looked up %d lyrics, want 0 (the track already has lyrics)", res.LyricsEnriched)
	}
	ly, err := st.LyricsByItem(ctx, item)
	if err != nil {
		t.Fatalf("LyricsByItem: %v", err)
	}
	if ly.Source != "lrc" || len(ly.Synced) != 1 || ly.Synced[0].Text != "existing" {
		t.Errorf("lyrics were overwritten: %+v, want the original sidecar copy", ly)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='lyrics'`); n != 0 {
		t.Errorf("lyrics markers = %d, want 0 (a track with lyrics is not looked up)", n)
	}
}

// TestInjectedGenreProviderRespectsLock: an injected genre provider must honor a genre
// lock the same as the built-in path: a locked item is never filled, while an unlocked
// sibling in the same release group is.
func TestInjectedGenreProviderRespectsLock(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	locked := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Have a Cigar", "Pink Floyd", "Wish You Were Here")
	if err := st.LockField(ctx, locked, "genre"); err != nil {
		t.Fatalf("LockField: %v", err)
	}

	mb := mbMockGenres(t, `[]`)
	mock := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapGenres,
		Ret: &enrich.Candidate{Genres: []string{"Shoegaze"}}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{mock},
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(locked)); n != 0 {
		t.Errorf("locked item got %d genres from the injected provider, want 0", n)
	}
	if total := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre`); total == 0 {
		t.Errorf("the unlocked sibling should still have gained the injected genre")
	}
}

// TestLyricsSkipsUntaggedTrack: a track with no artist is never looked up, so no
// negative lyrics marker is written; otherwise retagging it later would leave it
// permanently skipped.
func TestLyricsSkipsUntaggedTrack(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Mystery Track", "", "")

	lrc := lrclibMock(t, "[00:01.00]hi", "hi") // would match, but the track can't be keyed
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: lrc.URL,
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsEnriched != 0 {
		t.Errorf("looked up %d lyrics, want 0 (an untagged track is skipped)", res.LyricsEnriched)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='lyrics'`); n != 0 {
		t.Errorf("lyrics markers = %d, want 0 (an untagged track must not be marked un-enrichable)", n)
	}
}

// TestLRCLIBParsesEdgeTimestamps: an over-precise fraction (4 digits) and a long
// minute field (3 digits) both parse rather than dropping the line.
func TestLRCLIBParsesEdgeTimestamps(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	lrc := lrclibMock(t, "[00:10.0000]over\n[100:00.00]long", "over\nlong")
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: lrc.URL,
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ly, err := st.LyricsByItem(ctx, item)
	if err != nil {
		t.Fatalf("LyricsByItem: %v", err)
	}
	if len(ly.Synced) != 2 {
		t.Fatalf("synced lines = %d, want 2 (both edge timestamps parsed, none dropped)", len(ly.Synced))
	}
	if ly.Synced[0].TimeMS != 10000 {
		t.Errorf("4-digit-fraction line = %d ms, want 10000 (excess precision truncated)", ly.Synced[0].TimeMS)
	}
	if ly.Synced[1].TimeMS != 6000000 {
		t.Errorf("3-digit-minute line = %d ms, want 6000000", ly.Synced[1].TimeMS)
	}
}

// TestListenBrainzDropsFolksonomyTags: community tags with no genre_mbid (raw
// folksonomy like "seen live") are dropped, never written as genres.
func TestListenBrainzDropsFolksonomyTags(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[]`)
	lb := lbMock(t, "wywh-mbid", `[{"tag":"seen live","count":9},{"tag":"favorites","count":8}]`)
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCommunityGenres: true,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: lb.URL,
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(item)); n != 0 {
		t.Errorf("item genres = %d, want 0 (folksonomy tags must not become genres)", n)
	}
}

// TestMusicBrainzGenresSurviveProviderCap: an injected provider flooding more genres
// than the cap never evicts an authoritative MusicBrainz genre; only the non-MB
// additions are capped.
func TestMusicBrainzGenresSurviveProviderCap(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := mbMockGenres(t, `[{"name":"Progressive Rock","count":5},{"name":"Psychedelic Rock","count":4}]`)
	flood := []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7"} // 7 > maxEnrichGenres (6)
	mock := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapGenres, Ret: &enrich.Candidate{Genres: flood}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{mock},
	}, nil)

	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	db := roDB(t, dbPath)
	for _, want := range []string{"Progressive Rock", "Psychedelic Rock"} {
		n := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN genre gn ON gn.id=ig.genre_id
			JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=? AND gn.name=?`, string(item), want)
		if n != 1 {
			t.Errorf("MB genre %q missing (evicted by the provider cap)", want)
		}
	}
	injected := scalarInt(t, db, `SELECT COUNT(*) FROM item_genre ig JOIN genre gn ON gn.id=ig.genre_id
		JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=? AND gn.name LIKE 'g_'`, string(item))
	if injected != 6 {
		t.Errorf("injected genres applied = %d, want 6 (cap on additions)", injected)
	}
}

// TestGatherCoverPassesIdentityHints: the cover request carries the release title and
// artist, so an injected cover provider that keys on text (not only the MBID) can match.
func TestGatherCoverPassesIdentityHints(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var gotTitle, gotArtist string
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			gotTitle, gotArtist = req.Title, req.Artist
			return &enrich.Candidate{Cover: &model.ArtImage{
				Data: pngBytes(t), Hash: "cover-hash", Format: "png", Width: 4, Height: 4,
			}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{mock},
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotTitle != "Wish You Were Here" || gotArtist != "Pink Floyd" {
		t.Errorf("cover request hints = (%q, %q), want (Wish You Were Here, Pink Floyd)", gotTitle, gotArtist)
	}
	if res.ArtFetched != 1 {
		t.Errorf("art fetched = %d, want 1 (injected cover applied)", res.ArtFetched)
	}
}

// TestLRCLIBRetriesWithoutDurationOnMiss: when the duration-keyed /api/get 404s (a
// duration drift), the provider retries by name and still finds the lyrics, so the
// track is not permanently marked lyric-less.
func TestLRCLIBRetriesWithoutDurationOnMiss(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	// 404 whenever a duration is supplied (simulating a drift beyond LRCLIB's tolerance);
	// return lyrics only for the name-only retry.
	lrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("duration") != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"instrumental": false, "syncedLyrics": "[00:01.00]hi", "plainLyrics": "hi"})
		_, _ = w.Write(b)
	}))
	t.Cleanup(lrc.Close)
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, ListenBrainzBaseURL: deadURL(t), LRCLibBaseURL: lrc.URL,
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsMatched != 1 {
		t.Fatalf("lyrics matched = %d, want 1 (name-only retry should find lyrics)", res.LyricsMatched)
	}
	if _, err := st.LyricsByItem(ctx, item); err != nil {
		t.Errorf("LyricsByItem after retry: %v, want the retried lyrics", err)
	}
}

// seedTrackWithLyrics persists one present track carrying the given lyrics.
func seedTrackWithLyrics(t *testing.T, st *sqlite.Store, libID int64, ly *model.Lyrics) model.PID {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/l.mp3"), DisplayPath: "/lib/l.mp3", RelPath: []byte("l.mp3"),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-lyr", EssenceHash: "ess-lyr", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Have Lyrics",
			SortKey: model.SortKey("Have Lyrics"), IdentityKey: "essence:ess-lyr",
		},
		Track:  model.Track{Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Meddle", TrackNo: 1},
		Lyrics: ly,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// assertDerivedConsistent fails if the catalog's derived data (rollups, FTS) drifted,
// so a provider-driven genre/entity write leaves db verify clean.
func assertDerivedConsistent(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil {
		t.Fatalf("VerifyDerived: %v", err)
	}
	if !rep.Consistent() {
		t.Errorf("derived data inconsistent after enrichment: %+v", rep)
	}
}
