package enrich_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
)

// The artist rung of the art chain. enrichArtist resolved an identity and applied
// aliases and relations, and that was the whole pass, so an injected fanart.tv-shaped
// provider had no way to deliver an artist thumb or a scenic background even though
// model.ArtArtist is a first-class art level.

// mbMockArtist serves an artist search and lookup that match Pink Floyd, plus the
// release-group endpoints mbMockGenres serves, so a full run reaches the artist rung
// with a match instead of writing a no-match marker.
func mbMockArtist(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hasQuery := r.URL.Query().Get("query") != ""
		switch {
		case r.URL.Path == "/artist" && hasQuery:
			io(w, `{"artists":[{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd","score":100}]}`)
		case r.URL.Path == "/artist/pf-mbid":
			io(w, `{"id":"pf-mbid","name":"Pink Floyd","sort-name":"Pink Floyd","aliases":[],"relations":[]}`)
		case r.URL.Path == "/release-group" && hasQuery:
			io(w, `{"release-groups":[{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}]}]}`)
		case r.URL.Path == "/release-group/wywh-mbid":
			io(w, `{"id":"wywh-mbid","title":"Wish You Were Here","primary-type":"Album","secondary-types":[],
				"artist-credit":[{"artist":{"id":"pf-mbid","name":"Pink Floyd"}}],"genres":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func artistArtService(t *testing.T, st enrich.Store, providers ...enrich.Provider) *enrich.Service {
	t.Helper()
	return enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbMockArtist(t).URL, ListenBrainzBaseURL: deadURL(t),
		Providers: providers,
	}, nil)
}

// artistArtHash reads one role's stored hash at the artist rung.
func artistArtHash(t *testing.T, dbPath, role string) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath),
		`SELECT COALESCE((SELECT source_hash FROM art_map WHERE entity_type='artist' AND role=?), '')`, role)
}

// TestArtistArtFillsFrontAndAux is the gap: an injected provider serving the artist rung
// reaches it and fills its front and its auxiliary roles in one pass, through the
// artist-art backfill and the same store helpers the release group uses.
func TestArtistArtFillsFrontAndAux(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var artistReqs int
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			artistReqs++
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront:      artImg(t, "artist-front"),
				model.ArtRoleBackground: artImg(t, "artist-bg"),
			}}, nil
		}}
	res, err := artistArtService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if artistReqs != 1 || res.ArtistArtEnriched != 1 || res.ArtistArtMatched != 1 {
		t.Fatalf("asked %d times, backfill %d walked / %d matched, want one ask filling both",
			artistReqs, res.ArtistArtEnriched, res.ArtistArtMatched)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("artist front hash = %q, want artist-front", h)
	}
	if h := artistArtHash(t, dbPath, "background"); h != "artist-bg" {
		t.Errorf("artist background hash = %q, want artist-bg", h)
	}
}

// TestArtistArtSkippedWhenLocked: the whole-entity art lock cancels both asks, so a
// forced re-run does not re-download one picture per locked artist.
func TestArtistArtSkippedWhenLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	artistPID := model.PID(scalarStr(t, roDB(t, dbPath),
		"SELECT pid FROM artist WHERE name = 'Pink Floyd'"))
	if _, err := st.SetArtLock(ctx, model.ArtArtist, artistPID, model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock the artist art: %v", err)
	}

	var asked bool
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetArtist {
				asked = true
			}
			return nil, nil
		}}
	for _, opts := range []enrich.RunOptions{{}, {Force: true}} {
		if _, err := artistArtService(t, st, mock).Run(ctx, opts, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if asked {
		t.Error("a locked artist art still spent a provider request")
	}
	if h := artistArtHash(t, dbPath, "front"); h != "" {
		t.Errorf("artist front hash = %q, want nothing written under the lock", h)
	}
}

// TestArtistArtStockRunSpendsNothing: no built-in provider answers at this rung, so a
// stock install makes no extra request and the artist stays without a picture.
func TestArtistArtStockRunSpendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	res, err := artistArtService(t, st).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ArtistsMatched == 0 {
		t.Fatal("the artist did not match, so the rung was never reached")
	}
	if h := artistArtHash(t, dbPath, "front"); h != "" {
		t.Errorf("artist front hash = %q, want nothing from a stock run", h)
	}
}

// TestAFailedArtistArtAskLeavesTheIdentitySettled: the artist identity has no art rider,
// so an artist-art service that is down costs the identity nothing. The backfill owes its
// halves, and the next pass asks again and fills the front.
func TestAFailedArtistArtAskLeavesTheIdentitySettled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	down := true
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			if down {
				return nil, errors.New("fanart is down")
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{model.ArtRoleFront: artImg(t, "artist-front")}}, nil
		}}
	svc := artistArtService(t, st, art)
	db := roDB(t, dbPath)
	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.ArtistsMatched != 1 || first.ArtistArtEnriched != 1 || first.Deferred != 1 {
		t.Fatalf("run 1 = %d matched / %d backfilled / %d deferred, want 1, 1 and 1",
			first.ArtistsMatched, first.ArtistArtEnriched, first.Deferred)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist' AND matched=1 AND owed=0"); n != 1 {
		t.Errorf("settled matched artist identities = %d, want 1", n)
	}
	if owedMarkers(t, dbPath, "artist_front") != 1 {
		t.Fatal("the artist's front half is not owed after the provider failed")
	}

	down = false
	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ArtistsEnriched != 0 || second.ArtistArtEnriched != 1 || second.Deferred != 0 {
		t.Fatalf("run 2 = %d identities / %d backfilled / %d deferred, want the backfill alone re-asked",
			second.ArtistsEnriched, second.ArtistArtEnriched, second.Deferred)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("artist front = %q, want the recovered provider's", h)
	}
}

// TestArtistRungIgnoresCoverProviders: the artist rung consults CapArtistArt alone, so a
// cover provider that declares no rungs is never asked about an artist. The identity
// settles and the backfill writes no marker for a phase that never ran.
func TestArtistRungIgnoresCoverProviders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	artistAsks := 0
	covers := &enrich.Mock{ProviderName: "covers", Caps: enrich.CapCover | enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetArtist {
				artistAsks++
			}
			return &enrich.Candidate{Cover: artImg(t, "a-cover")}, nil
		}}
	res, err := artistArtService(t, st, covers).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if artistAsks != 0 {
		t.Errorf("the cover provider was asked about the artist %d times, want never", artistAsks)
	}
	if res.ArtistsMatched != 1 || settledMarkers(t, dbPath, "artist") != 1 || owedMarkers(t, dbPath, "artist") != 0 {
		t.Errorf("artist identity = %d matched, %d settled, %d owed; want it settled",
			res.ArtistsMatched, settledMarkers(t, dbPath, "artist"), owedMarkers(t, dbPath, "artist"))
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type IN ('artist_art','artist_front')"); n != 0 {
		t.Errorf("artist art markers = %d, want none", n)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "" {
		t.Errorf("artist front hash = %q, want nothing from a cover provider", h)
	}
}

// TestAnArtistIsAskedOncePerPass is the deferred double ask: a provider serving both
// capabilities at every rung, with no picture for this artist, hears about it once per
// pass rather than once from the identity walk and again from the backfill.
func TestAnArtistIsAskedOncePerPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	artistAsks := 0
	both := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetArtist {
				artistAsks++
			}
			return nil, nil
		}}
	res, err := artistArtService(t, st, both).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ArtistsMatched != 1 || res.ArtistArtEnriched != 1 {
		t.Fatalf("result = %+v, want the identity matched and the backfill walking the artist", res)
	}
	if artistAsks != 1 {
		t.Errorf("artist asks in one pass = %d, want 1", artistAsks)
	}
}

// TestArtistArtRetriesAFrontMissedBesideAnAuxMatch: a front no provider had stays a miss
// even though an auxiliary role landed in the same walk, so the retry window asks about
// the front again while the auxiliary half's match stands untouched.
func TestArtistArtRetriesAFrontMissedBesideAnAuxMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	hasFront, asks := false, 0
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks++
			roles := map[model.ArtRole]*model.ArtImage{model.ArtRoleBackground: artImg(t, "artist-bg")}
			if hasFront {
				roles[model.ArtRoleFront] = artImg(t, "artist-front")
			}
			return &enrich.Candidate{Art: roles}, nil
		}}
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, RetryMissesAfter: retryWindow,
		Providers: []enrich.Provider{art}}, nil)
	run := func(n int) *enrich.Result {
		t.Helper()
		res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
		if err != nil {
			t.Fatalf("run %d: %v", n, err)
		}
		return res
	}
	if first := run(1); first.ArtistArtMatched != 1 || asks != 1 || artistArtHash(t, dbPath, "background") != "artist-bg" {
		t.Fatalf("run 1 = %+v with %d asks, want the background filled", first, asks)
	}
	db := roDB(t, dbPath)
	if m := scalarInt(t, db, "SELECT matched FROM entity_enrichment WHERE entity_type='artist_front'"); m != 0 {
		t.Fatalf("front marker matched = %d, want the miss recorded beside the auxiliary match", m)
	}
	auxStamp := scalarInt(t, db, "SELECT enriched_at FROM entity_enrichment WHERE entity_type='artist_art'")
	run(2)
	if asks != 1 {
		t.Fatalf("run 2 asked %d times in all, want nothing new inside the window", asks)
	}
	backdateMisses(t, dbPath, retryAge)
	hasFront = true
	if third := run(3); third.ArtFetched != 1 || asks != 2 || artistArtHash(t, dbPath, "front") != "artist-front" {
		t.Errorf("run 3 = %+v with %d asks, want the front re-asked and filled", third, asks)
	}
	if got := scalarInt(t, db, "SELECT enriched_at FROM entity_enrichment WHERE entity_type='artist_art'"); got != auxStamp {
		t.Error("the retry re-settled the auxiliary half, which it had no reason to ask about")
	}
}

// TestArtistArtAuxFillsSettledFront: a pass whose provider serves only fronts asks
// nothing about the background, so a provider serving backgrounds that joins later
// reaches the artist on its first pass, with no forced run and no window to wait out,
// and the front stays as it was.
func TestArtistArtAuxFillsSettledFront(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	portraits := &enrich.Mock{ProviderName: "portraits", Caps: enrich.CapArtistFront,
		Ret: &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{model.ArtRoleFront: artImg(t, "artist-front")}}}
	cfg := enrich.Config{MinRequestInterval: time.Millisecond, RetryMissesAfter: retryWindow,
		Providers: []enrich.Provider{portraits}}
	if _, err := enrich.New(st, cfg, nil).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Fatalf("run 1 front = %q, want artist-front", h)
	}

	bgAsks := 0
	backgrounds := &enrich.Mock{ProviderName: "backgrounds", Caps: enrich.CapArtistAuxArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			bgAsks++
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront:      artImg(t, "late-front"),
				model.ArtRoleBackground: artImg(t, "artist-bg"),
			}}, nil
		}}
	cfg.Providers = []enrich.Provider{portraits, backgrounds}
	res, err := enrich.New(st, cfg, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if bgAsks != 1 || res.ArtistArtMatched != 1 || artistArtHash(t, dbPath, "background") != "artist-bg" {
		t.Errorf("run 2 = %+v with %d asks, want the background filled on the first pass that serves it", res, bgAsks)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("front = %q, want the settled artist-front untouched", h)
	}
}

// TestArtistArtStopsOnceABackgroundLands: background is the one auxiliary role an artist
// carries, so the gather stops asking once a front and a background are held, and a
// release role a provider offers for an artist is not stored at the artist rung.
func TestArtistArtStopsOnceABackgroundLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	first := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		Ret: &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
			model.ArtRoleFront:      artImg(t, "artist-front"),
			model.ArtRoleBackground: artImg(t, "artist-bg"),
			model.ArtRoleBack:       artImg(t, "artist-back"),
		}}}
	laterAsks := 0
	later := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			laterAsks++
			return nil, nil
		}}
	res, err := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond,
		Providers: []enrich.Provider{first, later}}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ArtistArtMatched != 1 || laterAsks != 0 {
		t.Errorf("result = %+v with %d asks of the second provider, want the first to settle both halves", res, laterAsks)
	}
	if h := artistArtHash(t, dbPath, "back"); h != "" {
		t.Errorf("artist back = %q, want no release role stored at the artist rung", h)
	}
	if h := artistArtHash(t, dbPath, "background"); h != "artist-bg" {
		t.Errorf("artist background = %q, want artist-bg", h)
	}
}

// TestAFrontOnlyArtistProviderLeavesTheBackgroundUnasked is the nuisance closed: a
// provider serving artist fronts alone is asked about fronts alone, so an artist it has
// no background for leaves no background miss to re-ask every retry window.
func TestAFrontOnlyArtistProviderLeavesTheBackgroundUnasked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Demo", "The Local Band", "Basement")
	asks := 0
	deezer := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistFront,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks++
			if req.Want != enrich.CapArtistFront {
				t.Errorf("request Want = %v, want CapArtistFront", req.Want)
			}
			if req.Artist != "Pink Floyd" {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{model.ArtRoleFront: artImg(t, "pf-front")}}, nil
		}}
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, RetryMissesAfter: retryWindow,
		Providers: []enrich.Provider{deezer}}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if asks != 2 || artistArtHash(t, dbPath, "front") != "pf-front" {
		t.Fatalf("run 1: %d asks, front %q; want both artists asked once and Pink Floyd's front filled", asks, artistArtHash(t, dbPath, "front"))
	}
	backdateMisses(t, dbPath, retryAge)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if asks != 3 {
		t.Errorf("asks after the window = %d, want the one front miss re-asked and nothing else", asks)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'artist_art'"); n != 0 {
		t.Errorf("background markers = %d, want none for a half no provider serves", n)
	}
}

// TestABackgroundOnlyArtistProviderLeavesTheFrontUnasked is the mirror: a provider serving
// backgrounds alone is asked for them alone and never answers for a front.
func TestABackgroundOnlyArtistProviderLeavesTheFrontUnasked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	backgrounds := &enrich.Mock{ProviderName: "backgrounds", Caps: enrich.CapArtistAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Want != enrich.CapArtistAuxArt {
				t.Errorf("request Want = %v, want CapArtistAuxArt", req.Want)
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront:      artImg(t, "stray-front"),
				model.ArtRoleBackground: artImg(t, "pf-bg"),
			}}, nil
		}}
	if _, err := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond,
		Providers: []enrich.Provider{backgrounds}}, nil).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if artistArtHash(t, dbPath, "background") != "pf-bg" || artistArtHash(t, dbPath, "front") != "" {
		t.Errorf("background %q, front %q; want the background filled and the front left alone",
			artistArtHash(t, dbPath, "background"), artistArtHash(t, dbPath, "front"))
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'artist_front'"); n != 0 {
		t.Errorf("front markers = %d, want none for a half no provider serves", n)
	}
}

// TestRunWarnsOfReleaseArtOfferedForArtists: a provider offering CapCover or CapAuxArt at
// the artist rung, whether it declared that rung or declares no rungs at all, the way one
// written before the artist capabilities did, is no longer asked about artists. The
// service says so once per provider rather than going quiet, and leaves a provider that
// scopes its release art to the release rungs alone.
func TestRunWarnsOfReleaseArtOfferedForArtists(t *testing.T) {
	t.Parallel()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	old := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
		CapsAt: map[enrich.TargetType]enrich.Capability{
			enrich.TargetReleaseGroup: enrich.CapCover | enrich.CapAuxArt,
			enrich.TargetArtist:       enrich.CapAuxArt,
		}}
	undeclared := &enrich.Mock{ProviderName: "covers", Caps: enrich.CapCover}
	current := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetArtist: enrich.CapArtistArt}}
	scoped := &enrich.Mock{ProviderName: "archive", Caps: enrich.CapCover,
		CapsAt: map[enrich.TargetType]enrich.Capability{
			enrich.TargetReleaseGroup: enrich.CapCover,
			enrich.TargetRelease:      enrich.CapCover,
		}}
	var logs warnings
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond,
		Providers: []enrich.Provider{old, undeclared, current, scoped}}, slog.New(&logs))
	for range 2 {
		if _, err := svc.Run(context.Background(), enrich.RunOptions{}, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	var warned []string
	for i, m := range logs.msgs {
		if strings.Contains(m, "artist rung") {
			warned = append(warned, logs.providers[i])
		}
	}
	if !slices.Equal(warned, []string{"fanart", "covers"}) {
		t.Errorf("warned about %v, want fanart and covers once each", warned)
	}
}
