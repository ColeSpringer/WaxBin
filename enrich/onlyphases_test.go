package enrich_test

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// countingLyrics is a lyrics provider that finds nothing and counts its asks.
func countingLyrics(asks *int) *enrich.Mock {
	return &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			*asks++
			return nil, nil
		}}
}

// TestPhasesWalksTheNamedPhasesAlone: a run given a phase list walks those phases and no
// other, forced or not, and its reach holds only their targets.
func TestPhasesWalksTheNamedPhasesAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Echoes", "Genesis", "Foxtrot")

	mb := newRetryMBMock(t)
	var asks int
	svc := forceService(st, mb.server.URL, retryWindow, countingLyrics(&asks))
	only := []model.EnrichPhase{model.EnrichPhaseLyrics}
	var seen []float64
	res, err := svc.Run(ctx, enrich.RunOptions{Phases: only}, func(p float64, _ string) error {
		seen = append(seen, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsEnriched != 2 || res.ArtistsEnriched != 0 || res.ReleaseGroupsEnriched != 0 || asks != 2 {
		t.Fatalf("result = %+v with %d lyrics asks, want the two lyrics targets alone", res, asks)
	}
	if mb.artistAsks != 0 {
		t.Errorf("artist searches = %d, want none", mb.artistAsks)
	}
	if r := res.Reach; len(r.LyricsItemIDs) != 2 || len(r.ArtistIDs)+len(r.ReleaseGroupIDs)+len(r.AlbumIDs)+len(r.BookItemIDs)+len(r.FieldsItemIDs) != 0 {
		t.Errorf("reach = %+v, want the two lyrics targets alone", r)
	}
	if len(seen) < 2 || seen[0] != 0.5 {
		t.Errorf("heartbeat = %v, want the first target to be half the lyrics-only total", seen)
	}

	res, err = svc.Run(ctx, enrich.RunOptions{Force: true, Phases: only}, nil)
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if res.LyricsEnriched != 2 || res.ArtistsEnriched != 0 || asks != 4 || mb.artistAsks != 0 {
		t.Errorf("forced result = %+v with %d lyrics asks and %d artist searches, want the lyrics re-asked alone",
			res, asks, mb.artistAsks)
	}
}

// TestPhasesNarrowsAScope: a scope and a phase list intersect, and a listed phase the
// scope gives nothing to walk is an empty walk rather than a refusal.
func TestPhasesNarrowsAScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, lib := openStore(t)
	pid := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	var asks int
	svc := forceService(st, mb.server.URL, retryWindow, countingLyrics(&asks))
	scope, err := st.EnrichScopeForItem(ctx, pid)
	if err != nil {
		t.Fatalf("EnrichScopeForItem: %v", err)
	}
	res, err := svc.Run(ctx, enrich.RunOptions{Scope: scope, Phases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil)
	if err != nil {
		t.Fatalf("scoped Run: %v", err)
	}
	if res.LyricsEnriched != 1 || res.ArtistsEnriched != 0 || asks != 1 || mb.artistAsks != 0 {
		t.Errorf("scoped result = %+v with %d lyrics asks, want the item's lyrics alone", res, asks)
	}

	artistOnly := &model.EnrichScope{ArtistIDs: scope.ArtistIDs}
	res, err = svc.Run(ctx, enrich.RunOptions{Scope: artistOnly, Phases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil)
	if err != nil {
		t.Fatalf("a scope with nothing for the listed phase: %v", err)
	}
	if res.LyricsEnriched != 0 || res.ArtistsEnriched != 0 || asks != 1 {
		t.Errorf("result = %+v with %d lyrics asks, want nothing walked", res, asks)
	}
}

// TestPhasesRefusals: an unknown phase and a forced phase outside the list are usage
// errors, and a listed phase the install does not build is refused naming its gate, all
// before anything is walked.
func TestPhasesRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newRetryMBMock(t)
	var asks int
	svc := forceService(st, mb.server.URL, retryWindow, countingLyrics(&asks))
	for name, opts := range map[string]enrich.RunOptions{
		"unknown phase": {Phases: []model.EnrichPhase{"nope"}},
		"forced phase outside the list": {Phases: []model.EnrichPhase{model.EnrichPhaseLyrics},
			ForcePhases: []model.EnrichPhase{model.EnrichPhaseArtist}},
	} {
		if _, err := svc.Run(ctx, opts, nil); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: err = %v, want CodeInvalid", name, err)
		}
	}

	noLyrics := retryMBService(st, mb.server.URL, retryWindow)
	_, err := noLyrics.Run(ctx, enrich.RunOptions{Phases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil)
	if !waxerr.Is(err, waxerr.CodeUnsupported) || !strings.Contains(err.Error(), "lyrics provider") {
		t.Errorf("an unbuilt listed phase = %v, want CodeUnsupported naming the lyrics provider", err)
	}
	if asks != 0 || mb.artistAsks != 0 {
		t.Errorf("asked %d lyrics and %d artist lookups, want none: every refusal precedes the walk", asks, mb.artistAsks)
	}

	res, err := svc.Run(ctx, enrich.RunOptions{Phases: []model.EnrichPhase{model.EnrichPhaseLyrics},
		ForcePhases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil)
	if err != nil || res.LyricsEnriched != 1 || res.ArtistsEnriched != 0 {
		t.Errorf("forcing a listed phase = %+v (err %v), want the lyrics walked alone", res, err)
	}
}

// TestCheckPhaseOptions: the one validator the engine, the facade and the CLI share
// refuses each bad combination with the same words, CodeInvalid, and passes a good one.
func TestCheckPhaseOptions(t *testing.T) {
	t.Parallel()
	artist, lyrics := []model.EnrichPhase{model.EnrichPhaseArtist}, []model.EnrichPhase{model.EnrichPhaseLyrics}
	for name, c := range map[string]struct {
		force, scoped  bool
		phases, forced []model.EnrichPhase
		want           string
	}{
		"unknown listed":      {phases: []model.EnrichPhase{"aux-art"}, want: `unknown enrichment phase "aux-art" (want one of artist|release-group|`},
		"unknown forced":      {forced: []model.EnrichPhase{"nope"}, want: `unknown enrichment phase "nope"`},
		"forced beside force": {force: true, forced: artist, want: "exclusive"},
		"forced in a scope":   {scoped: true, forced: artist, want: "cannot combine"},
		"forced outside list": {phases: lyrics, forced: artist, want: "not among"},
	} {
		err := enrich.CheckPhaseOptions(c.force, c.scoped, c.phases, c.forced)
		if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want CodeInvalid containing %q", name, err, c.want)
		}
	}
	if err := enrich.CheckPhaseOptions(false, true, lyrics, nil); err != nil {
		t.Errorf("a scoped phase list: %v", err)
	}
	if err := enrich.CheckPhaseOptions(false, false, append(artist, model.EnrichPhaseLyrics), artist); err != nil {
		t.Errorf("a forced phase within the list: %v", err)
	}
}
