package enrich_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
)

// The artist-art backfill: the phase that re-asks about an artist the identity pass has
// already marked. Artist art is fetched inside that pass, so an artist marked before an
// art provider was injected has no picture and no way to get one short of a --force run
// that re-searches MusicBrainz for every artist in the catalog.

// artistArtMarker reads the backfill marker's provider for the one artist, empty when no
// marker was written.
func artistArtMarker(t *testing.T, dbPath string) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath),
		`SELECT COALESCE((SELECT provider FROM entity_enrichment WHERE entity_type='artist_art'), '')`)
}

// TestArtistArtBackfillFillsAMarkedArtist is the deferred gap closed. The first run
// settles the artist's identity and marks it, which puts it out of the identity pass's
// queue for good; the second run, with an art provider injected, fills the front the
// first run had no source for.
func TestArtistArtBackfillFillsAMarkedArtist(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	first, err := artistArtService(t, st).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.ArtistsMatched == 0 {
		t.Fatal("the artist did not match, so it was never marked")
	}
	if first.ArtistArtEnriched != 0 {
		t.Errorf("run 1 artist-art entities = %d, want 0 (no provider advertises CapArtistArt)", first.ArtistArtEnriched)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "" {
		t.Fatalf("run 1 artist front hash = %q, want nothing from a stock run", h)
	}

	var wants []enrich.Capability
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			wants = append(wants, req.Want)
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront:      artImg(t, "artist-front"),
				model.ArtRoleBackground: artImg(t, "artist-bg"),
			}}, nil
		}}
	second, err := artistArtService(t, st, art).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ArtistsEnriched != 0 {
		t.Errorf("run 2 identity artists = %d, want 0: the marker still holds that queue shut", second.ArtistsEnriched)
	}
	if second.ArtistArtEnriched != 1 || second.ArtistArtMatched != 1 {
		t.Fatalf("run 2 backfill = %d walked / %d matched, want 1 and 1",
			second.ArtistArtEnriched, second.ArtistArtMatched)
	}
	for _, w := range wants {
		if w != enrich.CapArtistArt {
			t.Errorf("request Want = %v, want CapArtistArt so a provider can key its answer", w)
		}
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("artist front hash = %q, want the backfilled front", h)
	}
	if h := artistArtHash(t, dbPath, "background"); h != "artist-bg" {
		t.Errorf("artist background hash = %q, want the backfilled background", h)
	}

	// The marker stops it repeating.
	third, err := artistArtService(t, st, art).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if third.ArtistArtEnriched != 0 {
		t.Errorf("run 3 walked %d artists again; the marker should have held", third.ArtistArtEnriched)
	}
}

// TestArtistArtBackfillQueuesAnEmptyAuxSlot: an artist whose front is settled but whose
// auxiliary slots are empty is queued too, which is the half auxArtNeededPredicate's
// shape already covers at the release-group rung.
func TestArtistArtBackfillQueuesAnEmptyAuxSlot(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	front := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, "artist-front"),
			}}, nil
		}}
	if _, err := artistArtService(t, st, front).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Fatalf("run 1 artist front hash = %q, want the identity pass's front", h)
	}

	var offeredFront bool
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			offeredFront = true
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront:      artImg(t, "second-front"),
				model.ArtRoleBackground: artImg(t, "artist-bg"),
			}}, nil
		}}
	res, err := artistArtService(t, st, aux).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.ArtistArtEnriched != 1 {
		t.Fatalf("backfill walked %d artists, want the one with an empty aux slot", res.ArtistArtEnriched)
	}
	if !offeredFront {
		t.Fatal("the provider was never asked, so the aux vacancy did not queue the artist")
	}
	if h := artistArtHash(t, dbPath, "background"); h != "artist-bg" {
		t.Errorf("artist background hash = %q, want the backfilled background", h)
	}
	// The settled front is a decided slot, so a second offer must not take it.
	if h := artistArtHash(t, dbPath, "front"); h != "artist-front" {
		t.Errorf("artist front hash = %q, want the first run's front left alone", h)
	}
}

// TestArtistArtBackfillSkipsALockedArtist: the whole-entity art lock keeps the artist out
// of the queue, so a locked artist spends no request and takes no marker.
func TestArtistArtBackfillSkipsALockedArtist(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	if _, err := artistArtService(t, st).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("identity run: %v", err)
	}
	artistPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM artist WHERE name = 'Pink Floyd'"))
	if err := st.SetArtLock(ctx, model.ArtArtist, artistPID, model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock the artist art: %v", err)
	}

	var asked bool
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetArtist {
				asked = true
			}
			return nil, nil
		}}
	res, err := artistArtService(t, st, art).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asked || res.ArtistArtEnriched != 0 {
		t.Errorf("a locked artist was walked (%d) or asked (%v)", res.ArtistArtEnriched, asked)
	}
	if m := artistArtMarker(t, dbPath); m != "" {
		t.Errorf("marker provider = %q, want no marker on a locked artist", m)
	}
}

// TestArtistArtBackfillDropsARoleHeldEmptyByItsLock: the queue's vacancy test is
// approximate (it counts stored rows, not locks), so a role deliberately held empty reads
// as a vacancy and is dropped at apply instead. The marker is still written, so it costs
// one pass rather than a request every run.
func TestArtistArtBackfillDropsARoleHeldEmptyByItsLock(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	if _, err := artistArtService(t, st).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("identity run: %v", err)
	}
	artistPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM artist WHERE name = 'Pink Floyd'"))
	if err := st.SetArtLock(ctx, model.ArtArtist, artistPID, model.ArtRoleBackground, true); err != nil {
		t.Fatalf("lock the background role: %v", err)
	}

	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBackground: artImg(t, "artist-bg"),
			}}, nil
		}}
	if _, err := artistArtService(t, st, art).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h := artistArtHash(t, dbPath, "background"); h != "" {
		t.Errorf("background hash = %q, want the role's own lock to have held it empty", h)
	}
	if m := artistArtMarker(t, dbPath); m == "" {
		t.Error("no marker written, so the artist is re-asked every run")
	}
}

// TestArtistArtBackfillIsSilentWithoutTheCapability: a provider advertising only CapCover
// is the state every art provider written before this bit is in. It must not queue the
// phase at all, or a stock install (whose Cover Art Archive answers nothing for an artist)
// would stamp a permanent no-match on every artist, which is the bug this closes.
func TestArtistArtBackfillIsSilentWithoutTheCapability(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	if _, err := artistArtService(t, st).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("identity run: %v", err)
	}

	cover := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		Ret: &enrich.Candidate{Cover: artImg(t, "front-hash")}}
	res, err := artistArtService(t, st, cover).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ArtistArtEnriched != 0 {
		t.Errorf("backfill walked %d artists without a CapArtistArt provider", res.ArtistArtEnriched)
	}
	if m := artistArtMarker(t, dbPath); m != "" {
		t.Errorf("marker provider = %q, want no marker when the phase did not run", m)
	}
}
