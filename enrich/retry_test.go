package enrich_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
)

// retryWindow is the window every test here configures, and retryAge the age it
// backdates a marker to, comfortably past it.
const (
	retryWindow = 30 * 24 * time.Hour
	retryAge    = 40 * 24 * time.Hour
)

// rwDB opens a second read-write connection, so a test can age the enrichment markers
// the store deliberately has no API to backdate (roDB beside it is read-only). The busy
// timeout matters: the store owns a write connection on the same file, and a backdate
// landing mid-commit would otherwise fail outright instead of waiting the moment out.
func rwDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open rw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// backdateMisses ages every no-match marker so the retry sweep has something to select,
// never a sleep: the coarse Windows clock makes two stamps taken in one run equal.
func backdateMisses(t *testing.T, dbPath string, age time.Duration) {
	t.Helper()
	if _, err := rwDB(t, dbPath).Exec("UPDATE entity_enrichment SET enriched_at = ? WHERE matched = 0",
		time.Now().Add(-age).UnixNano()); err != nil {
		t.Fatalf("backdate misses: %v", err)
	}
}

// backdateEveryMarker ages the matched markers too, which is how a test shows a match
// is durable rather than merely younger than the cutoff.
func backdateEveryMarker(t *testing.T, dbPath string, age time.Duration) {
	t.Helper()
	if _, err := rwDB(t, dbPath).Exec("UPDATE entity_enrichment SET enriched_at = ?",
		time.Now().Add(-age).UnixNano()); err != nil {
		t.Fatalf("backdate markers: %v", err)
	}
}

// retryMBMock serves the identity ladder for one artist and one release group, with the
// artist search answering nothing until answerArtist is set. That is the shape the
// window exists for: MusicBrainz gained the artist between two runs.
type retryMBMock struct {
	server       *httptest.Server
	answerArtist bool
	artistAsks   int
}

func newRetryMBMock(t *testing.T) *retryMBMock {
	t.Helper()
	m := &retryMBMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hasQuery := r.URL.Query().Get("query") != ""
		switch {
		case r.URL.Path == "/artist" && hasQuery:
			m.artistAsks++
			if !m.answerArtist {
				io(w, `{"artists":[]}`)
				return
			}
			io(w, `{"artists":[{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd","score":100}]}`)
		case r.URL.Path == "/artist/pf-mbid":
			io(w, `{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd"}`)
		case r.URL.Path == "/release-group" && hasQuery:
			io(w, `{"release-groups":[{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}]}]}`)
		case r.URL.Path == "/release-group/wywh-mbid":
			io(w, `{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","secondary-types":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func retryMBService(st enrich.Store, mbURL string, window time.Duration) *enrich.Service {
	return enrich.New(st, enrich.Config{
		Contact:            "test@example.com",
		RetryMissesAfter:   window,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbURL,
	}, nil)
}

// retryArtService is the port-phase fixture: no contact at all, so the only phase that
// runs is the artist-art backfill the injected provider gates.
func retryArtService(st enrich.Store, p enrich.Provider, window time.Duration) *enrich.Service {
	return enrich.New(st, enrich.Config{
		RetryMissesAfter:   window,
		MinRequestInterval: time.Millisecond,
		Providers:          []enrich.Provider{p},
	}, nil)
}

// TestRetrySweepReAsksAnExpiredIdentityMiss is the ask end to end: an artist
// MusicBrainz had nothing for is asked again once its marker is older than the window,
// and asked with the cache bypassed, since the cached miss is exactly what earned the
// marker.
func TestRetrySweepReAsksAnExpiredIdentityMiss(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	svc := retryMBService(st, mb.server.URL, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if mb.artistAsks != 1 {
		t.Fatalf("artist searches = %d, want 1", mb.artistAsks)
	}

	// A second run before the window elapses walks nothing at all.
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.ArtistsEnriched != 0 || res.Retried != 0 || mb.artistAsks != 1 {
		t.Fatalf("a fresh marker was re-asked: %+v, searches %d", res, mb.artistAsks)
	}

	backdateMisses(t, dbPath, retryAge)
	mb.answerArtist = true
	res, err = svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("third Run: %v", err)
	}
	if res.Retried != 1 || res.ArtistsEnriched != 1 || res.ArtistsMatched != 1 {
		t.Fatalf("result = %+v, want one retried artist that matched", res)
	}
	if mb.artistAsks != 2 {
		t.Errorf("artist searches = %d, want 2 (the retry bypasses the cached miss)", mb.artistAsks)
	}
	if got := scalarStr(t, roDB(t, dbPath), "SELECT COALESCE(mbid,'') FROM artist WHERE name='Pink Floyd'"); got != "pf-mbid" {
		t.Errorf("artist mbid = %q, want pf-mbid", got)
	}
}

// TestRetrySweepForcesTheProviderRequest: a provider that caches its own misses has to
// be told, or the re-ask returns the answer that earned the marker.
func TestRetrySweepForcesTheProviderRequest(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asks []enrich.Request
	answer := false
	p := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks = append(asks, req)
			if !answer {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, "late-portrait"),
			}}, nil
		}}
	svc := retryArtService(st, p, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(asks) != 1 || asks[0].Force {
		t.Fatalf("first asks = %+v, want one unforced request", asks)
	}

	backdateMisses(t, dbPath, retryAge)
	answer = true
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Retried != 1 || res.ArtistArtEnriched != 1 || res.ArtFetched != 1 {
		t.Fatalf("result = %+v, want one retried artist whose front landed", res)
	}
	if len(asks) != 2 || !asks[1].Force {
		t.Fatalf("second asks = %+v, want a forced re-ask", asks)
	}
	if got := scalarStr(t, roDB(t, dbPath),
		`SELECT source_hash FROM art_map WHERE entity_type='artist' AND role='front'`); got != "late-portrait" {
		t.Errorf("artist front = %q, want late-portrait", got)
	}
}

// TestRetrySweepLeavesAMatchedMarkerAlone: an aux backfill that answered with a back
// cover and nothing else still has three empty slots, so only the marker keeps it out
// of the queue. That marker is durable: a provider gained later needs a forced run.
func TestRetrySweepLeavesAMatchedMarkerAlone(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asks int
	p := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks++
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBack: artImg(t, "back-hash"),
			}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		RetryMissesAfter: retryWindow, MinRequestInterval: time.Millisecond,
		Providers: []enrich.Provider{p},
	}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if asks != 1 {
		t.Fatalf("aux asks = %d, want 1", asks)
	}

	backdateEveryMarker(t, dbPath, retryAge)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Retried != 0 || res.AuxArtEnriched != 0 || asks != 1 {
		t.Errorf("result = %+v, asks %d: a matched marker must stay durable", res, asks)
	}

	// A forced run is the documented way past it.
	if _, err := svc.Run(ctx, enrich.RunOptions{Force: true}, nil); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if asks != 2 {
		t.Errorf("aux asks after --force = %d, want 2", asks)
	}
}

// TestRetryWindowOfZeroNeverReAsks: the config key's off switch.
func TestRetryWindowOfZeroNeverReAsks(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asks int
	p := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			asks++
			return nil, nil
		}}
	svc := retryArtService(st, p, 0)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	backdateMisses(t, dbPath, retryAge)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Retried != 0 || asks != 1 {
		t.Errorf("result = %+v, asks %d: a zero window expires nothing", res, asks)
	}
}

// TestFreshTargetsAreWalkedBeforeRetries is why the retries are a second sweep rather
// than a wider predicate: on a capped nightly run the budget has to reach the files
// nobody has looked at yet, even when an older miss sorts ahead of them by id.
func TestFreshTargetsAreWalkedBeforeRetries(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asks []enrich.Request
	p := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks = append(asks, req)
			return nil, nil
		}}
	svc := retryArtService(st, p, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// The newcomer takes a higher artist rowid than the artist already marked, so a
	// single keyset walk over both sweeps would reach the old miss first.
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Echoes", "Genesis", "Foxtrot")
	backdateMisses(t, dbPath, retryAge)
	asks = nil

	res, err := svc.Run(ctx, enrich.RunOptions{Limit: 1}, nil)
	if err != nil {
		t.Fatalf("capped Run: %v", err)
	}
	if res.Retried != 0 || res.ArtistArtEnriched != 1 {
		t.Fatalf("result = %+v, want the one fresh artist and no retry", res)
	}
	if len(asks) != 1 || asks[0].Artist != "Genesis" {
		t.Fatalf("asks = %+v, want only the newcomer", asks)
	}

	// Uncapped, the next run picks up the retry the cap deferred. The newcomer is not
	// due again: walking it stamped a marker of its own.
	asks = nil
	res, err = svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("uncapped Run: %v", err)
	}
	if res.Retried != 1 || res.ArtistArtEnriched != 1 {
		t.Fatalf("result = %+v, want the deferred retry and nothing else", res)
	}
	if len(asks) != 1 || asks[0].Artist != "Pink Floyd" {
		t.Fatalf("asks = %+v, want only the expired marker", asks)
	}
}

// TestFreshTargetsWinTheBudgetAcrossPhasesToo is the half a single-phase test cannot
// see: the sweeps are walked outside the phase loop, so a capped run reaches the new
// files of EVERY phase before it re-asks about anything. With the sweeps nested inside a
// phase, the artist-art retry below would take the budget and the newly scanned track's
// lyrics would go unlooked-at night after night.
func TestFreshTargetsWinTheBudgetAcrossPhasesToo(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	p := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt | enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) { return nil, nil }}
	svc := retryArtService(st, p, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// A newcomer under the SAME artist, so the only fresh target it adds belongs to the
	// lyrics phase, which runs after the artist-art one.
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Echoes", "Pink Floyd", "Wish You Were Here")
	backdateMisses(t, dbPath, retryAge)

	res, err := svc.Run(ctx, enrich.RunOptions{Limit: 1}, nil)
	if err != nil {
		t.Fatalf("capped Run: %v", err)
	}
	if res.Retried != 0 || res.LyricsEnriched != 1 || res.ArtistArtEnriched != 0 {
		t.Fatalf("result = %+v, want the new track's lyrics and no re-ask", res)
	}
}

// TestRetriedAlbumReBrowsesAGroupASiblingCached: the per-run browse memo records that a
// group was attempted, and the fresh sweep can attempt one off the stored edition set
// without a request. A retried album under that group must still get a real re-browse,
// or the re-ask reads the same stale answer that earned its marker.
func TestRetriedAlbumReBrowsesAGroupASiblingCached(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumEdition(t, st, lib.ID, "ess-b", "CD", "JP")

	// Two CDs and no JP pressing: the JP album's country predicate affirms nothing, so
	// the medium alone leaves two candidates and the album takes a no-match marker.
	mb := newRelMock(t, "[]")
	mb.browsePages = []string{browsePage(2,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
	)}
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", MatchReleases: true, RetryMissesAfter: retryWindow,
		MinRequestInterval: time.Millisecond, MusicBrainzBaseURL: mb.server.URL,
	}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(mb.browseOffsets) != 1 {
		t.Fatalf("browse offsets = %v, want one page", mb.browseOffsets)
	}

	// A sibling under the same group, and a group that has since gained the JP pressing.
	seedAlbumEdition(t, st, lib.ID, "ess-a", "CD", "GB")
	backdateMisses(t, dbPath, retryAge)
	mb.browsePages = []string{browsePage(3,
		browseDoc(edGBMBID, []string{"CD"}, "GB", ""),
		browseDoc(edUSMBID, []string{"CD"}, "US", ""),
		browseDoc(edJPMBID, []string{"CD"}, "JP", ""),
	)}

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Retried != 1 || res.AlbumsSearched != 2 || res.AlbumsMatched != 2 {
		t.Fatalf("result = %+v, want the fresh sibling and the retried album both matched", res)
	}
	if len(mb.browseOffsets) != 2 {
		t.Errorf("browse offsets = %v, want a second page for the retried album", mb.browseOffsets)
	}
	db := roDB(t, dbPath)
	for essence, want := range map[string]string{"ess-a": edGBMBID, "ess-b": edJPMBID} {
		got := scalarStr(t, db, `SELECT COALESCE(al.mbid,'') FROM album al
			JOIN track t ON t.album_id = al.id JOIN item_file f ON f.item_id = t.item_id
			JOIN file fi ON fi.id = f.file_id WHERE fi.essence_hash = ?`, essence)
		if got != want {
			t.Errorf("%s album mbid = %q, want %s", essence, got, want)
		}
	}
}

// TestRetryOnlyRunReportsAFullHeartbeat: the denominator is asked for under SweepDue, so
// a run made entirely of retries reports a real ratio rather than jumping to one on its
// first target.
func TestRetryOnlyRunReportsAFullHeartbeat(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Echoes", "Genesis", "Foxtrot")

	p := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) { return nil, nil }}
	svc := retryArtService(st, p, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	backdateMisses(t, dbPath, retryAge)

	var seen []float64
	res, err := svc.Run(ctx, enrich.RunOptions{}, func(progress float64, _ string) error {
		seen = append(seen, progress)
		return nil
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Retried != 2 {
		t.Fatalf("result = %+v, want two retried artists", res)
	}
	if len(seen) < 2 || seen[0] != 0.5 || seen[1] != 1 {
		t.Errorf("heartbeat progress = %v, want 0.5 then 1 (the count covers both sweeps)", seen)
	}
}
