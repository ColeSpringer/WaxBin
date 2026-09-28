package enrich_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// TestArtistArtFillsFrontAndAux is the gap: an injected provider advertising both
// capabilities now reaches the artist rung and fills its front and its auxiliary roles,
// through the same store helpers the release group uses.
func TestArtistArtFillsFrontAndAux(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var artistReqs int
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
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
	if _, err := artistArtService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if artistReqs == 0 {
		t.Fatal("the provider was never asked about the artist")
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
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	artistPID := model.PID(scalarStr(t, roDB(t, dbPath),
		"SELECT pid FROM artist WHERE name = 'Pink Floyd'"))
	if _, err := st.SetArtLock(ctx, model.ArtArtist, artistPID, model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock the artist art: %v", err)
	}

	var asked bool
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetArtist {
				asked = true
			}
			return nil, nil
		}}
	if _, err := artistArtService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
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

// TestArtistArtRiderFailureLeavesTheArtistQueued: the artist identity rung follows the
// release-group one. A failed art rider leaves the artist's identity in place and its
// marker off, so the next pass asks the provider again.
func TestArtistArtRiderFailureLeavesTheArtistQueued(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	down := true
	covers := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			if down {
				return nil, errors.New("fanart is down")
			}
			return &enrich.Candidate{Cover: artImg(t, "artist-front")}, nil
		}}
	svc := artistArtService(t, st, covers)
	db := roDB(t, dbPath)
	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.ArtistsMatched != 1 || first.Deferred != 1 {
		t.Fatalf("run 1 = %d matched / %d deferred, want 1 and 1", first.ArtistsMatched, first.Deferred)
	}
	if m := scalarStr(t, db, "SELECT COALESCE(mbid,'') FROM artist WHERE name='Pink Floyd'"); m != "pf-mbid" {
		t.Errorf("artist mbid = %q, want the identity landed", m)
	}
	if owedMarkers(t, dbPath, "artist") != 1 || settledMarkers(t, dbPath, "artist") != 0 {
		t.Fatal("the artist's lookup is not owed while the rider owes an answer")
	}

	down = false
	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ArtistsEnriched != 1 || second.Deferred != 0 {
		t.Fatalf("run 2 = %d walked / %d deferred, want the artist re-walked and settled", second.ArtistsEnriched, second.Deferred)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("artist front = %q, want the rider's answer", h)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist' AND matched=1"); n != 1 {
		t.Errorf("matched artist markers = %d, want 1", n)
	}
}

// TestAnArtistWithAFrontIsNotDeferredForItsAuxRider: the identity rungs defer only for
// the front. An artist already holding one is asked about its auxiliary roles on the way
// past, and a failure there leaves the identity settled; artist auxiliary art belongs to
// the artist-art backfill, which asks again under its own marker.
func TestAnArtistWithAFrontIsNotDeferredForItsAuxRider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	pid := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM artist WHERE name='Pink Floyd'"))
	if err := st.SetEntityArt(ctx, model.ArtArtist, pid, model.ArtRoleFront, pngBytes(t), "",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set the artist's front: %v", err)
	}
	backgrounds := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetArtist: enrich.CapAuxArt},
		Err:    errors.New("fanart key expired")}
	if _, err := artistArtService(t, st, backgrounds).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if settledMarkers(t, dbPath, "artist") != 1 || owedMarkers(t, dbPath, "artist") != 0 {
		t.Errorf("artist identity = %d settled / %d owed, want settled", settledMarkers(t, dbPath, "artist"), owedMarkers(t, dbPath, "artist"))
	}
}
