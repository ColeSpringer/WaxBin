package enrich_test

import (
	"context"
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
