package enrich_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// forceService is the fixture a phase-scoped force needs: a contact for the identity
// phases and an injected provider for a backfill, so a run has two phases to tell apart.
func forceService(st enrich.Store, mbURL string, window time.Duration, providers ...enrich.Provider) *enrich.Service {
	return enrich.New(st, enrich.Config{
		Contact:            "test@example.com",
		RetryMissesAfter:   window,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbURL,
		Providers:          providers,
	}, nil)
}

// TestForcePhaseReAsksTheNamedPhaseAlone is the DEFERRED entry's case: a provider
// registered after the markers settled is asked about them, and the identity phase
// nobody named is left alone.
func TestForcePhaseReAsksTheNamedPhaseAlone(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	mb.answerArtist = true
	var asks []enrich.Request
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asks = append(asks, req)
			return nil, nil
		}}
	svc := forceService(st, mb.server.URL, retryWindow, art)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if mb.artistAsks != 1 || len(asks) != 1 {
		t.Fatalf("first run: artist searches %d, art asks %d, want 1 and 1", mb.artistAsks, len(asks))
	}

	res, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtistArt}}, nil)
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if res.ArtistArtEnriched != 1 || res.Retried != 0 {
		t.Errorf("result = %+v, want one enriched artist-art target and no retries", res)
	}
	if len(asks) != 2 || !asks[1].Force {
		t.Fatalf("art asks = %+v, want a second, forced request", asks)
	}
	if mb.artistAsks != 1 {
		t.Errorf("artist searches = %d, want 1: the identity phase was not forced", mb.artistAsks)
	}
}

// TestForcePhaseLeavesTheOtherPhasesOnTheirOwnSweeps: the unnamed phases still walk
// fresh and retry, and the named one is walked once rather than once per sweep.
func TestForcePhaseLeavesTheOtherPhasesOnTheirOwnSweeps(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	var asks int
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			asks++
			return nil, nil
		}}
	svc := forceService(st, mb.server.URL, retryWindow, art)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if asks != 1 {
		t.Fatalf("art asks = %d, want 1", asks)
	}

	backdateMisses(t, dbPath, retryAge)
	mb.answerArtist = true
	res, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtistArt}}, nil)
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if res.Retried != 1 || res.ArtistsMatched != 1 {
		t.Errorf("result = %+v, want the identity retry to have walked", res)
	}
	if asks != 2 {
		t.Errorf("art asks = %d, want 2: the retry sweep skips a forced phase", asks)
	}
}

// TestForcePhaseRefusesBadCombinations: an unknown name, and the two combinations that
// already force everything, are refused before anything is walked.
func TestForcePhaseRefusesBadCombinations(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	svc := forceService(st, mb.server.URL, retryWindow)
	cases := map[string]enrich.RunOptions{
		"unknown phase": {ForcePhases: []model.EnrichPhase{"nope"}},
		"with force":    {Force: true, ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtist}},
		"with a scope":  {Scope: &model.EnrichScope{ArtistIDs: []int64{1}}, ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtist}},
	}
	for name, opts := range cases {
		res, err := svc.Run(ctx, opts, nil)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: err = %v, want CodeInvalid", name, err)
		}
		if res != nil && (res.ArtistsEnriched != 0 || res.ReleaseGroupsEnriched != 0) {
			t.Errorf("%s: result = %+v, want nothing walked", name, res)
		}
	}
	if mb.artistAsks != 0 {
		t.Errorf("artist searches = %d, want none", mb.artistAsks)
	}
}

// TestForcePhaseRefusesAPhaseTheInstallDoesNotRun: forcing a phase this install skips
// would walk nothing and report a complete run, so it is refused instead, naming the
// gate.
func TestForcePhaseRefusesAPhaseTheInstallDoesNotRun(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	noLyrics := retryMBService(st, mb.server.URL, retryWindow)
	_, err := noLyrics.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("forced lyrics err = %v, want CodeUnsupported", err)
	}
	if !strings.Contains(err.Error(), "lyrics provider") {
		t.Errorf("err = %v, want the lyrics provider named", err)
	}
	if mb.artistAsks != 0 {
		t.Errorf("artist searches = %d, want none: the refusal precedes the walk", mb.artistAsks)
	}

	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) { return nil, nil }}
	noContact := retryArtService(st, art, retryWindow)
	_, err = noContact.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtist}}, nil)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("forced artist err = %v, want CodeUnsupported", err)
	}
	if !strings.Contains(err.Error(), "MusicBrainz contact") {
		t.Errorf("err = %v, want the contact named", err)
	}
}

// TestForcePhaseHeartbeatReportsAFullRun: the denominator counts a forced phase under
// SweepAll, so a run made entirely of re-asks reports a real ratio.
func TestForcePhaseHeartbeatReportsAFullRun(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Echoes", "Genesis", "Foxtrot")

	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) { return nil, nil }}
	svc := retryArtService(st, art, retryWindow)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	var seen []float64
	res, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtistArt}},
		func(progress float64, _ string) error {
			seen = append(seen, progress)
			return nil
		})
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if res.ArtistArtEnriched != 2 {
		t.Fatalf("result = %+v, want both artists re-asked", res)
	}
	if len(seen) < 2 || seen[0] != 0.5 || seen[1] != 1 {
		t.Errorf("heartbeat progress = %v, want 0.5 then 1", seen)
	}
}
