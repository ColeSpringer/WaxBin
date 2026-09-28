package enrich_test

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// The group-art backfill: the phase that re-asks about a release group whose identity is
// settled but whose front, or whose back, disc, booklet, and background slots, are
// empty. The release-group pass asks about art only while it walks the identity, so
// without this phase those slots are never filled after the first run.

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

// TestGroupArtAuxFillsSettledFront: a first run fills the front through the backfill,
// which answers the front's half alone. A later run with a provider serving the
// auxiliary roles still reaches the group, fills the empty roles, and leaves the front
// exactly as it was.
func TestGroupArtAuxFillsSettledFront(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	front := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		Ret: &enrich.Candidate{Cover: artImg(t, "front-hash")}}
	first, err := auxService(t, st, front).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.GroupArtEnriched != 1 || first.GroupArtMatched != 1 {
		t.Errorf("run 1 group art = %d enriched/%d matched, want the front backfilled", first.GroupArtEnriched, first.GroupArtMatched)
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
	if second.GroupArtEnriched != 1 || second.GroupArtMatched != 1 {
		t.Errorf("run 2 aux = %d enriched/%d matched, want 1/1", second.GroupArtEnriched, second.GroupArtMatched)
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
		"SELECT provider FROM entity_enrichment WHERE entity_type='group_art'"); p != "fanart" {
		t.Errorf("aux marker provider = %q, want fanart", p)
	}
}

// TestGroupArtFetchesAStockFrontAlone: on a stock install the built-in Cover Art Archive
// serves the group's front alone, so the backfill asks for the front and records that
// half and no other, leaving the auxiliary roles for a provider serving them to reach
// later. A second run asks nothing.
func TestGroupArtFetchesAStockFrontAlone(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	mb := newMBMock(t)
	caa, fetches := newCAAMock(t, pngBytes(t))
	svc := newService(st, mb.server.URL, caa.URL)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.GroupArtEnriched != 1 || res.GroupArtMatched != 1 || res.AuxArtFetched != 0 {
		t.Errorf("group art = %+v, want the front backfilled and nothing else", res)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='group_art'"); n != 0 {
		t.Errorf("auxiliary markers = %d, want 0 (nothing asked about those roles)", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='group_front' AND matched=1"); n != 1 {
		t.Errorf("matched front markers = %d, want 1", n)
	}
	asked := *fetches
	if again, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil || again.GroupArtEnriched != 0 || *fetches != asked {
		t.Errorf("second run = %+v (%v) with %d new fetches, want nothing asked", again, err, *fetches-asked)
	}
}

// TestGroupArtAuxMarkerStopsRepeat: a group no provider could serve is asked once, by the
// backfill, and never again on a later run. Force is the way back in.
func TestGroupArtAuxMarkerStopsRepeat(t *testing.T) {
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
	if first.GroupArtEnriched != 1 || first.GroupArtMatched != 0 {
		t.Fatalf("run 1 aux = %d enriched/%d matched, want 1/0", first.GroupArtEnriched, first.GroupArtMatched)
	}
	if calls != 1 {
		t.Fatalf("run 1 provider calls = %d, want 1 (the backfill's)", calls)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='group_art' AND matched=0"); n != 1 {
		t.Errorf("no-match marker rows = %d, want 1", n)
	}

	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.GroupArtEnriched != 0 {
		t.Errorf("run 2 aux entities = %d, want 0 (the marker holds)", second.GroupArtEnriched)
	}
	if calls != 1 {
		t.Errorf("provider calls after run 2 = %d, want the markers to have prevented more", calls)
	}

	forced, err := svc.Run(ctx, enrich.RunOptions{Force: true}, nil)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if forced.GroupArtEnriched != 1 || calls != 2 {
		t.Errorf("forced run aux = %d entities, %d calls, want 1 and 2", forced.GroupArtEnriched, calls)
	}
}

// TestGroupArtAuxStopsAtAFullSet: once one provider has supplied every auxiliary role
// there is nothing left to gather, so the providers behind it are not consulted at all.
// Without that stop their images are downloaded in full and then dropped.
func TestGroupArtAuxStopsAtAFullSet(t *testing.T) {
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

// TestGroupArtAuxHeartbeatDenominator: the ratio's numerator counts every phase a run
// executes, so its denominator has to as well. The group here is tagged with its
// release-group id up front, which is what makes the aux phase's own queue non-empty
// before the run starts and the count exact.
func TestGroupArtAuxHeartbeatDenominator(t *testing.T) {
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
	if res.ArtistsEnriched != 1 || res.ReleaseGroupsEnriched != 1 || res.GroupArtEnriched != 1 {
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

// TestGroupArtAuxQueuesAnUnmatchedGroupByTitle: the release-group twin of the
// name-keyed artist walk. The queue used to require release_group.mbid, so a group
// MusicBrainz never matched was never asked about its empty auxiliary slots. It walks by
// title now, and the request carries the title and primary-artist name with an empty
// MBID.
func TestGroupArtAuxQueuesAnUnmatchedGroupByTitle(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Basement Tape", "The Local Band", "Demo")

	var reqs []enrich.Request
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetReleaseGroup {
				return nil, nil
			}
			reqs = append(reqs, req)
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBack: artImg(t, "demo-back"),
			}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbMockNoArtist(t).URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{aux},
	}, nil)

	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ReleaseGroupsMatched != 0 {
		t.Fatalf("MusicBrainz matched %d groups; the fixture is supposed to miss", res.ReleaseGroupsMatched)
	}
	if res.GroupArtEnriched != 1 || res.GroupArtMatched != 1 {
		t.Fatalf("backfill = %d walked / %d matched, want 1 and 1 for an unmatched group",
			res.GroupArtEnriched, res.GroupArtMatched)
	}
	if len(reqs) != 1 {
		t.Fatalf("provider asked %d times, want once", len(reqs))
	}
	if reqs[0].Title != "Demo" || reqs[0].Artist != "The Local Band" || reqs[0].MBID != "" {
		t.Errorf("request = title %q / artist %q / mbid %q, want the title and artist alone",
			reqs[0].Title, reqs[0].Artist, reqs[0].MBID)
	}
	if reqs[0].Want != enrich.CapAuxArt {
		t.Errorf("request Want = %v, want CapAuxArt", reqs[0].Want)
	}
	if h := scalarStr(t, roDB(t, dbPath), `SELECT COALESCE((SELECT source_hash FROM art_map
		WHERE entity_type='release_group' AND role='back'), '')`); h != "demo-back" {
		t.Errorf("back hash = %q, want the title-keyed fill", h)
	}

	again, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.GroupArtEnriched != 0 {
		t.Errorf("second run walked %d groups; the marker should have held", again.GroupArtEnriched)
	}
}

// rgAuxHash reads one auxiliary role's stored hash at the release-group rung.
func rgAuxHash(t *testing.T, dbPath string, role model.ArtRole) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath),
		`SELECT COALESCE((SELECT source_hash FROM art_map WHERE entity_type='release_group' AND role=?), '')`, string(role))
}

// TestGroupArtAuxFailureLeavesTheGroupQueued: with one provider failing and another answering,
// the answer lands and the group stays queued for the one that failed. The next pass asks
// both again; the failed provider fills its slot and the healthy one's repeat is dropped
// at apply, since the slot it answered is no longer empty.
func TestGroupArtAuxFailureLeavesTheGroupQueued(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	down := true
	fanart := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetReleaseGroup {
				return nil, nil
			}
			if down {
				return nil, errors.New("fanart is down")
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{model.ArtRoleDisc: artImg(t, "disc-hash")}}, nil
		}}
	backs := 0
	audiodb := &enrich.Mock{ProviderName: "theaudiodb", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetReleaseGroup {
				return nil, nil
			}
			backs++
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBack: artImg(t, "back-"+strconv.Itoa(backs)),
			}}, nil
		}}
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{fanart, audiodb}}, nil)

	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.GroupArtEnriched != 1 || first.Deferred != 1 {
		t.Fatalf("run 1 = %d walked / %d deferred, want 1 and 1", first.GroupArtEnriched, first.Deferred)
	}
	if got := rgAuxHash(t, dbPath, model.ArtRoleBack); got != "back-1" {
		t.Errorf("run 1 back = %q, want the healthy provider's answer applied", got)
	}
	if owedMarkers(t, dbPath, "group_art") != 1 || settledMarkers(t, dbPath, "group_art") != 0 {
		t.Fatal("the group's aux art lookup is not owed while a provider owes an answer")
	}

	down = false
	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.GroupArtEnriched != 1 || second.Deferred != 0 {
		t.Fatalf("run 2 = %d walked / %d deferred, want 1 and 0", second.GroupArtEnriched, second.Deferred)
	}
	if got := rgAuxHash(t, dbPath, model.ArtRoleDisc); got != "disc-hash" {
		t.Errorf("run 2 disc = %q, want the recovered provider's answer", got)
	}
	if got := rgAuxHash(t, dbPath, model.ArtRoleBack); got != "back-1" {
		t.Errorf("run 2 back = %q, want back-1 kept: the repeat meets a filled slot", got)
	}
	db := roDB(t, dbPath)
	if m := scalarInt(t, db, `SELECT matched FROM entity_enrichment WHERE entity_type = 'group_art'`); m != 1 {
		t.Errorf("aux art marker matched = %d, want 1", m)
	}
	if p := scalarStr(t, db, `SELECT provider FROM entity_enrichment WHERE entity_type = 'group_art'`); p != "fanart" {
		t.Errorf("aux art marker provider = %q, want fanart", p)
	}
}

// groupArchive is a Cover Art Archive standing in for the release-group rung alone: it
// serves the group's front, answers 404 while the cover is missing, and 500 while down.
type groupArchive struct {
	mu    sync.Mutex
	art   []byte
	state string // "up", "missing", or "down"
	asks  int
}

func newGroupArchive(t *testing.T, art []byte, state string) (string, *groupArchive) {
	t.Helper()
	a := &groupArchive{art: art, state: state}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.URL.Path != "/release-group/wywh-mbid/front" {
			http.NotFound(w, r)
			return
		}
		a.asks++
		switch a.state {
		case "missing":
			http.NotFound(w, r)
		case "down":
			http.Error(w, "unavailable", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(a.art)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL, a
}

func (a *groupArchive) set(state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
}

func (a *groupArchive) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asks
}

// stockRun runs one pass on a stock install: a contact and the built-in archive, with
// the cover art toggle and the provider list hook as given.
func stockRun(t *testing.T, st enrich.Store, mbURL, caaURL string, coverArt bool, list func([]enrich.Provider) []enrich.Provider) *enrich.Result {
	t.Helper()
	res, err := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCoverArt: coverArt,
		MusicBrainzBaseURL: mbURL, CoverArtBaseURL: caaURL, ListenBrainzBaseURL: deadURL(t),
		ProviderList: list,
	}, nil).Run(context.Background(), enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestGroupArtFillsAFrontSettledWithoutCoverArt is the deferred entry's first case: a
// group whose identity settled while cover art was off has no front, and nothing asked
// again. With the archive on, the backfill asks about it on the next run.
func TestGroupArtFillsAFrontSettledWithoutCoverArt(t *testing.T) {
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	mbURL := mbMockGenres(t, `[]`).URL
	caaURL, arch := newGroupArchive(t, pngBytes(t), "up")

	first := stockRun(t, st, mbURL, caaURL, false, nil)
	if first.ReleaseGroupsMatched != 1 || rgFrontHash(t, dbPath) != "" {
		t.Fatalf("run 1 = %+v with front %q, want the group settled with no front", first, rgFrontHash(t, dbPath))
	}
	if n := owedMarkers(t, dbPath, "group_art") + settledMarkers(t, dbPath, "group_art"); n != 0 {
		t.Fatalf("run 1 group_art markers = %d, want none (the phase could not run)", n)
	}

	second := stockRun(t, st, mbURL, caaURL, true, nil)
	if second.GroupArtEnriched != 1 || second.GroupArtMatched != 1 || second.ArtFetched != 1 {
		t.Errorf("run 2 = %+v, want the group's front backfilled", second)
	}
	if rgFrontHash(t, dbPath) == "" {
		t.Error("run 2 left the group without a front")
	}
	if p := scalarStr(t, roDB(t, dbPath), `SELECT provider FROM art_map
		WHERE entity_type='release_group' AND role='front'`); p != "coverartarchive" {
		t.Errorf("front provider = %q, want coverartarchive", p)
	}
	if n := arch.count(); n != 1 {
		t.Errorf("archive asks = %d, want 1", n)
	}
}

// TestGroupArtReachesAGroupTheHookLeftTheArchiveOutOf is the entry's third case: a pass
// whose provider list left the archive out settled the group with no front, and a later
// pass with the archive in the list backfills it.
func TestGroupArtReachesAGroupTheHookLeftTheArchiveOutOf(t *testing.T) {
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	mbURL := mbMockGenres(t, `[]`).URL
	caaURL, arch := newGroupArchive(t, pngBytes(t), "up")

	first := stockRun(t, st, mbURL, caaURL, true, func(fixed []enrich.Provider) []enrich.Provider {
		return byNames(fixed, "musicbrainz")
	})
	if first.ReleaseGroupsMatched != 1 || rgFrontHash(t, dbPath) != "" || arch.count() != 0 {
		t.Fatalf("run 1 = %+v, want the group settled without asking the archive", first)
	}
	second := stockRun(t, st, mbURL, caaURL, true, nil)
	if second.GroupArtMatched != 1 || rgFrontHash(t, dbPath) == "" {
		t.Errorf("run 2 = %+v, want the front backfilled once the archive is in the pass", second)
	}
}

// TestGroupArtAsksAFreshGroupsFront: the group-art backfill is the one asker for a vacant
// group front. The release-group pass lands the id and leaves the front to it, so a fresh
// group costs the archive one request in its first pass, and an outage leaves the
// backfill's lookup owed while the identity settles.
func TestGroupArtAsksAFreshGroupsFront(t *testing.T) {
	for _, c := range []struct {
		state                 string
		owedGroupArt, settled int
	}{{"missing", 0, 1}, {"down", 1, 0}} {
		t.Run(c.state, func(t *testing.T) {
			st, dbPath, lib := openStore(t)
			seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
			caaURL, arch := newGroupArchive(t, pngBytes(t), c.state)
			res := stockRun(t, st, mbMockGenres(t, `[]`).URL, caaURL, true, nil)
			if n := arch.count(); n != 1 {
				t.Errorf("archive asks = %d, want 1", n)
			}
			if res.ReleaseGroupsMatched != 1 || res.GroupArtEnriched != 1 {
				t.Errorf("result = %+v, want the identity matched and the backfill asking the front", res)
			}
			if owedMarkers(t, dbPath, "release_group") != 0 || settledMarkers(t, dbPath, "release_group") != 1 {
				t.Error("the identity did not settle; it asks nothing about a vacant front")
			}
			if o, s := owedMarkers(t, dbPath, "group_front"), settledMarkers(t, dbPath, "group_front"); o != c.owedGroupArt || s != c.settled {
				t.Errorf("group_front markers owed %d, settled %d; want %d and %d", o, s, c.owedGroupArt, c.settled)
			}
		})
	}
}

// TestGroupArtAsksACoverlessGroupOnce: a group the archive has no cover for is asked once,
// and the miss holds until the retry window rather than being asked again next pass.
func TestGroupArtAsksACoverlessGroupOnce(t *testing.T) {
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	mbURL := mbMockGenres(t, `[]`).URL
	caaURL, arch := newGroupArchive(t, pngBytes(t), "missing")
	stockRun(t, st, mbURL, caaURL, true, nil)
	second := stockRun(t, st, mbURL, caaURL, true, nil)
	if n := arch.count(); n != 1 {
		t.Errorf("archive asks over two passes = %d, want 1", n)
	}
	if second.GroupArtEnriched != 0 {
		t.Errorf("run 2 walked %d groups for art, want 0", second.GroupArtEnriched)
	}
}

// TestGroupArtAsksTheFrontOfAGroupMusicBrainzMissed: the release-group pass asks for a
// front only when MusicBrainz matched the group, so a miss stamped this run does not
// leave the front to it. A title-keyed provider serving the group rung is asked for the
// front of an unmatched group in the same run.
func TestGroupArtAsksTheFrontOfAGroupMusicBrainzMissed(t *testing.T) {
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Basement Tape", "The Local Band", "Demo")
	fronts := 0
	p := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover | enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetReleaseGroup || !req.Wants(enrich.CapCover) {
				return nil, nil
			}
			fronts++
			return &enrich.Candidate{Cover: artImg(t, "demo-front")}, nil
		}}
	res, err := auxService(t, st, p).Run(context.Background(), enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReleaseGroupsEnriched != 1 || res.ReleaseGroupsMatched != 0 {
		t.Fatalf("result = %+v, want the group walked and missed", res)
	}
	if fronts != 1 || rgFrontHash(t, dbPath) != "demo-front" || res.GroupArtMatched != 1 {
		t.Errorf("front asks = %d, front = %q, group art matched = %d; want the backfill to ask and fill it",
			fronts, rgFrontHash(t, dbPath), res.GroupArtMatched)
	}
}

// TestGroupArtHeartbeatStaysShortOfDoneUntilTheEnd: the denominator is counted before the
// pass and a pass can do more than it counted, since an id the release-group pass lands
// re-queues a group the backfill had marked from a request without one. The heartbeat
// stays short of 1 until the last beat says the pass is done.
func TestGroupArtHeartbeatStaysShortOfDoneUntilTheEnd(t *testing.T) {
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	caaURL, _ := newGroupArchive(t, pngBytes(t), "up")
	// The miss an earlier pass left from a request without an id.
	var rgID int64
	var rgPID string
	if err := roDB(t, dbPath).QueryRow("SELECT id, pid FROM release_group").Scan(&rgID, &rgPID); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyReleaseGroupArtBackfill(context.Background(), model.ReleaseGroupArtBackfill{
		ReleaseGroupID: rgID, PID: model.PID(rgPID)}); err != nil {
		t.Fatal(err)
	}
	cfg := enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCoverArt: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, CoverArtBaseURL: caaURL, ListenBrainzBaseURL: deadURL(t),
	}
	var beats []float64
	res, err := enrich.New(st, cfg, nil).Run(context.Background(), enrich.RunOptions{}, func(p float64, _ string) error {
		beats = append(beats, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.GroupArtMatched != 1 || rgFrontHash(t, dbPath) == "" {
		t.Fatalf("result = %+v, want the landed id to re-queue the group and fill its front", res)
	}
	if len(beats) < 2 || beats[len(beats)-1] != 1 {
		t.Fatalf("heartbeats = %v, want several ending at 1", beats)
	}
	for i, b := range beats[:len(beats)-1] {
		if b >= 1 || (i > 0 && b < beats[i-1]) {
			t.Errorf("heartbeats = %v, want each before the last below 1 and none going back", beats)
			break
		}
	}
}

// TestGroupArtRetriesAFrontMissedBesideAnAuxMatch: a front the archive had no picture for
// stays a miss even though a provider filled an auxiliary role in the same walk, so the
// retry window asks about the front again, and only the front.
func TestGroupArtRetriesAFrontMissedBesideAnAuxMatch(t *testing.T) {
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	caaURL, arch := newGroupArchive(t, pngBytes(t), "missing")
	auxAsks := 0
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			auxAsks++
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: artImg(t, "back-hash")}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCoverArt: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, CoverArtBaseURL: caaURL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{aux}, RetryMissesAfter: retryWindow,
	}, nil)
	run := func(n int) *enrich.Result {
		t.Helper()
		res, err := svc.Run(context.Background(), enrich.RunOptions{}, nil)
		if err != nil {
			t.Fatalf("run %d: %v", n, err)
		}
		return res
	}
	if first := run(1); first.GroupArtMatched != 1 || arch.count() != 1 || auxAsks != 1 {
		t.Fatalf("run 1 = %+v with %d archive and %d aux asks, want one of each and the back filled", first, arch.count(), auxAsks)
	}
	db := roDB(t, dbPath)
	if m := scalarInt(t, db, "SELECT matched FROM entity_enrichment WHERE entity_type='group_front'"); m != 0 {
		t.Fatalf("front marker matched = %d, want the miss recorded beside the auxiliary match", m)
	}
	run(2)
	if arch.count() != 1 || auxAsks != 1 {
		t.Fatalf("run 2 asked the archive %d and the aux provider %d times, want nothing new", arch.count(), auxAsks)
	}
	backdateMisses(t, dbPath, retryAge)
	arch.set("up")
	if third := run(3); third.ArtFetched != 1 || arch.count() != 2 || auxAsks != 1 || rgFrontHash(t, dbPath) == "" {
		t.Errorf("run 3 = %+v with %d archive and %d aux asks, want the front re-asked alone and filled", third, arch.count(), auxAsks)
	}
}

// TestGroupRefreshLeavesTheAuxProvidersToTheBackfill: a forced release-group walk refreshes
// a held front from the cover providers alone. The auxiliary roles are the group-art
// backfill's to ask, so a provider serving only those is not consulted on the way.
func TestGroupRefreshLeavesTheAuxProvidersToTheBackfill(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	rgPID := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM release_group"))
	for _, role := range append([]model.ArtRole{model.ArtRoleFront}, model.AuxArtRoles()...) {
		if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rgPID, role, pngBytes(t), "png",
			model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
			t.Fatalf("set %s: %v", role, err)
		}
	}
	auxAsks := 0
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			auxAsks++
			return nil, nil
		}}
	caa, fetches := newCAAMock(t, pngBytes(t))
	res, err := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCoverArt: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, CoverArtBaseURL: caa.URL, ListenBrainzBaseURL: deadURL(t),
		Providers: []enrich.Provider{aux},
	}, nil).Run(ctx, enrich.RunOptions{Force: true}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ReleaseGroupsMatched != 1 || *fetches != 1 {
		t.Fatalf("result = %+v with %d archive fetches, want the held front refreshed once", res, *fetches)
	}
	if auxAsks != 0 {
		t.Errorf("auxiliary provider asked %d times, want 0", auxAsks)
	}
}
