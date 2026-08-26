package enrich_test

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// The auxiliary-art backfill: the phase that re-asks about a release group whose front
// cover is settled but whose back, disc, booklet, and background slots are empty. Both
// art-fetching passes pre-guard on the front, so without this phase those slots are
// never filled after the first run.

// seedTaggedTrack persists one track whose file already names its release group, so a
// test can start from a group that carries an mbid without running a pass to fill it.
func seedTaggedTrack(t *testing.T, st *sqlite.Store, libID int64, path, essence, rgMBID string) {
	t.Helper()
	_, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Shine On",
			SortKey: model.SortKey("Shine On"), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{
			Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here",
			TrackNo: 1, MBReleaseGroupID: rgMBID,
		},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
}

// auxService builds a service with the given providers wired to the release-group mock.
func auxService(t *testing.T, st enrich.Store, providers ...enrich.Provider) *enrich.Service {
	t.Helper()
	return enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, ListenBrainzBaseURL: deadURL(t),
		Providers: providers,
	}, nil)
}

// TestAuxBackfillFillsSettledFront is the deferred gap closed: a first run settles the
// front and marks the group, which puts it out of reach of both art-fetching passes
// forever. A later run with an aux-capable provider fills the empty roles and leaves
// the decided front exactly as it was.
func TestAuxBackfillFillsSettledFront(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	front := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		Ret: &enrich.Candidate{Cover: artImg(t, "front-hash")}}
	first, err := auxService(t, st, front).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.AuxArtEnriched != 0 {
		t.Errorf("run 1 aux entities = %d, want 0 (no provider advertises CapAuxArt)", first.AuxArtEnriched)
	}
	if h := rgFrontHash(t, dbPath); h != "front-hash" {
		t.Fatalf("run 1 front hash = %q, want front-hash", h)
	}

	// The same provider, now advertising the backfill capability and offering a front
	// it must not get to write.
	var wants []enrich.Capability
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			wants = append(wants, req.Want)
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, "late-front"),
				model.ArtRoleBack:  artImg(t, "back-hash"),
				model.ArtRoleDisc:  artImg(t, "disc-hash"),
			}}, nil
		}}
	second, err := auxService(t, st, aux).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.ReleaseGroupsEnriched != 0 {
		t.Errorf("run 2 release groups = %d, want 0 (the front pass is done with this group)", second.ReleaseGroupsEnriched)
	}
	if second.AuxArtEnriched != 1 || second.AuxArtMatched != 1 {
		t.Errorf("run 2 aux = %d enriched/%d matched, want 1/1", second.AuxArtEnriched, second.AuxArtMatched)
	}
	if second.AuxArtFetched != 2 {
		t.Errorf("run 2 aux images = %d, want 2 (the offered front is dropped)", second.AuxArtFetched)
	}
	for _, w := range wants {
		if w != enrich.CapAuxArt {
			t.Errorf("provider asked with Want %v, want CapAuxArt only", w)
		}
	}

	db := roDB(t, dbPath)
	if h := rgFrontHash(t, dbPath); h != "front-hash" {
		t.Errorf("front hash = %q, want the settled front-hash untouched", h)
	}
	for _, role := range []string{"back", "disc"} {
		var hash, source, provider string
		err := db.QueryRow(`SELECT source_hash, source, provider FROM art_map
			WHERE entity_type='release_group' AND role=?`, role).Scan(&hash, &source, &provider)
		if err != nil {
			t.Fatalf("read %s row: %v", role, err)
		}
		if hash != role+"-hash" || source != string(model.SourceEnrichment) || provider != "fanart" {
			t.Errorf("%s slot = %q %q/%q, want %s-hash enrichment/fanart", role, hash, source, provider, role)
		}
	}
	if p := scalarStr(t, db,
		"SELECT provider FROM entity_enrichment WHERE entity_type='aux_art'"); p != "fanart" {
		t.Errorf("aux marker provider = %q, want fanart", p)
	}
}

// TestAuxBackfillSkippedWithoutCapableProvider: the built-in Cover Art Archive serves
// the front alone, so a stock install runs the phase not at all. No queue query, no
// marker, nothing for a user who injected no provider to pay for.
func TestAuxBackfillSkippedWithoutCapableProvider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, _ := newCAAMock(t, pngBytes(t))
	res, err := newService(st, mb.server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AuxArtEnriched != 0 || res.AuxArtMatched != 0 {
		t.Errorf("aux = %d enriched/%d matched, want 0/0", res.AuxArtEnriched, res.AuxArtMatched)
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art'"); n != 0 {
		t.Errorf("aux marker rows = %d, want 0 (a phase that never ran marks nothing)", n)
	}
}

// TestAuxBackfillMarkerStopsRepeat: a group no provider could serve is asked once, not
// once per run. Force is the way back in.
func TestAuxBackfillMarkerStopsRepeat(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	calls := 0
	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			calls++
			return nil, nil
		}}
	svc := auxService(t, st, mock)

	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.AuxArtEnriched != 1 || first.AuxArtMatched != 0 {
		t.Fatalf("run 1 aux = %d enriched/%d matched, want 1/0", first.AuxArtEnriched, first.AuxArtMatched)
	}
	if calls != 1 {
		t.Fatalf("run 1 provider calls = %d, want 1", calls)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art' AND matched=0"); n != 1 {
		t.Errorf("no-match marker rows = %d, want 1", n)
	}

	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.AuxArtEnriched != 0 {
		t.Errorf("run 2 aux entities = %d, want 0 (the marker holds)", second.AuxArtEnriched)
	}
	if calls != 1 {
		t.Errorf("provider calls after run 2 = %d, want the marker to have prevented a second", calls)
	}

	forced, err := svc.Run(ctx, enrich.RunOptions{Force: true}, nil)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if forced.AuxArtEnriched != 1 || calls != 2 {
		t.Errorf("forced run aux = %d entities, %d calls, want 1 and 2", forced.AuxArtEnriched, calls)
	}
}

// TestAuxBackfillStopsAtAFullSet: once one provider has supplied every auxiliary role
// there is nothing left to gather, so the providers behind it are not consulted at all.
// Without that stop their images are downloaded in full and then dropped.
func TestAuxBackfillStopsAtAFullSet(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTaggedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "wywh-mbid")

	full := map[model.ArtRole]*model.ArtImage{}
	for _, role := range model.AuxArtRoles() {
		full[role] = artImg(t, string(role)+"-hash")
	}
	first := &enrich.Mock{ProviderName: "first", Caps: enrich.CapAuxArt,
		Ret: &enrich.Candidate{Art: full}}
	calls := 0
	second := &enrich.Mock{ProviderName: "second", Caps: enrich.CapAuxArt,
		EnrichFunc: func(ctx context.Context, req enrich.Request) (*enrich.Candidate, error) {
			calls++
			return nil, nil
		}}

	res, err := auxService(t, st, first, second).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AuxArtFetched != len(model.AuxArtRoles()) {
		t.Errorf("aux images = %d, want every role from the first provider", res.AuxArtFetched)
	}
	if calls != 0 {
		t.Errorf("second provider calls = %d, want 0 (the first left no vacancy)", calls)
	}
}

// TestAuxBackfillHeartbeatDenominator: the ratio's numerator counts every phase a run
// executes, so its denominator has to as well. The group here is tagged with its
// release-group id up front, which is what makes the aux phase's own queue non-empty
// before the run starts and the count exact.
func TestAuxBackfillHeartbeatDenominator(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTaggedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "wywh-mbid")

	mock := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		Ret: &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
			model.ArtRoleBack: artImg(t, "back-hash"),
		}}}
	var beats []float64
	res, err := auxService(t, st, mock).Run(ctx, enrich.RunOptions{}, func(p float64, msg string) error {
		beats = append(beats, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One artist, one release group, one aux backfill.
	if res.ArtistsEnriched != 1 || res.ReleaseGroupsEnriched != 1 || res.AuxArtEnriched != 1 {
		t.Fatalf("result = %+v, want one of each phase", res)
	}
	if len(beats) < 3 {
		t.Fatalf("heartbeats = %v, want one per entity", beats)
	}
	// A denominator missing the aux phase would read 2, making the first beat 0.5.
	if math.Abs(beats[0]-1.0/3.0) > 1e-9 {
		t.Errorf("first heartbeat = %v, want 1/3 (the aux phase counted in the denominator)", beats[0])
	}
	if beats[len(beats)-1] != 1 {
		t.Errorf("final heartbeat = %v, want 1", beats[len(beats)-1])
	}
}
