package enrich_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// The per-run breaker: a provider that fails three times in a row drops out of the pass,
// and a port phase left with no live provider ends its sweep rather than paging through
// every remaining target for a certain failure.

// seedTracks persists n tracks by n different artists, the population the lyrics phase
// walks one target at a time.
func seedTracks(t *testing.T, st *sqlite.Store, libID int64, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		k := strconv.Itoa(i)
		seedTrack(t, st, libID, "/lib/"+k+".mp3", "ess-"+k, "Song "+k, "Artist "+k, "Album "+k)
	}
}

// scriptedLyrics answers each call with the next script entry, true for lyrics and false
// for an error, repeating the last entry once the script runs out.
func scriptedLyrics(script []bool, calls *int) *enrich.Mock {
	return &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			i := *calls
			*calls++
			if i >= len(script) {
				i = len(script) - 1
			}
			if !script[i] {
				return nil, errors.New("lyrics service is down")
			}
			return &enrich.Candidate{Lyrics: &model.Lyrics{Unsynced: "la la"}}, nil
		}}
}

func lyricsService(st enrich.Store, p enrich.Provider) *enrich.Service {
	return enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, RetryMissesAfter: retryWindow,
		Providers: []enrich.Provider{p},
	}, nil)
}

// TestProviderTripsAfterConsecutiveFailures: a provider failing every call is asked three
// times, then left out; the lyrics phase has no other provider, so it stalls and the rest
// of the tracks are neither asked, counted, nor marked. The retry sweep behind it has
// nobody to ask either, so an expired miss it would have re-asked is left as it was and
// the phase is reported once.
func TestProviderTripsAfterConsecutiveFailures(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/old.mp3", "ess-old", "Old Song", "Old Artist", "Old Album")
	if _, err := lyricsService(st, &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics}).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("seeding run: %v", err)
	}
	backdateMisses(t, dbPath, retryAge)
	seedTracks(t, st, lib.ID, 5)

	calls := 0
	res, err := lyricsService(st, scriptedLyrics([]bool{false}, &calls)).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 3 {
		t.Errorf("provider calls = %d, want 3", calls)
	}
	if res.LyricsEnriched != 3 || res.Deferred != 3 || res.Retried != 0 {
		t.Errorf("walked %d / deferred %d / retried %d, want 3, 3 and 0", res.LyricsEnriched, res.Deferred, res.Retried)
	}
	if len(res.Stalled) != 1 || res.Stalled[0] != model.EnrichPhaseLyrics {
		t.Errorf("stalled = %v, want [lyrics] once", res.Stalled)
	}
	if owed, settled := owedMarkers(t, dbPath, "lyrics"), settledMarkers(t, dbPath, "lyrics"); owed != 3 || settled != 1 {
		t.Errorf("lyrics markers = %d owed / %d settled, want the three failures owed and the expired miss nobody re-asked", owed, settled)
	}
}

// TestOneFailureDoesNotTrip: the count is of consecutive failures, so a provider that
// fails on some targets and answers others stays in the pass, and three failures spread
// across the run do not add up to a trip.
func TestOneFailureDoesNotTrip(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 6)

	calls := 0
	res, err := lyricsService(st, scriptedLyrics([]bool{false, true, false, true, false, true}, &calls)).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 6 || res.Deferred != 3 || len(res.Stalled) != 0 {
		t.Errorf("calls %d / deferred %d / stalled %v, want 6, 3 and none", calls, res.Deferred, res.Stalled)
	}
	if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM lyrics`); n != 3 {
		t.Errorf("lyrics rows = %d, want the three answers", n)
	}
}

// TestATrippedProviderStartsCleanNextRun: the breaker is per run, so the next pass asks
// the provider about every target again.
func TestATrippedProviderStartsCleanNextRun(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTracks(t, st, lib.ID, 5)

	calls := 0
	failing := lyricsService(st, scriptedLyrics([]bool{false}, &calls))
	if _, err := failing.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	calls = 0
	res, err := lyricsService(st, scriptedLyrics([]bool{true}, &calls)).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if calls != 5 || res.LyricsMatched != 5 {
		t.Errorf("run 2 calls %d / matched %d, want every track asked and answered", calls, res.LyricsMatched)
	}
}

// identifiedAlbums seeds n albums under one release group, each carrying the barcode the
// album-art walk needs to ask about it.
func identifiedAlbums(t *testing.T, st *sqlite.Store, libID int64, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		seedAlbumTrack(t, st, libID, "ess-"+strconv.Itoa(i), model.Track{
			Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
			Barcode: relBarcode,
		})
	}
}

// releaseArt serves one role at the release rung alone, answering every ask with it or
// failing every one.
func releaseArt(t *testing.T, name string, caps enrich.Capability, role model.ArtRole, down *bool) *enrich.Mock {
	return &enrich.Mock{ProviderName: name, Caps: caps, CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetRelease: caps},
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			if *down {
				return nil, errors.New(name + " is down")
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{role: artImg(t, name+"-"+string(role))}}, nil
		}}
}

// TestATrippedProviderLeavesAMissBehindALiveOne: once the cover provider drops out, the
// albums after it settle on the live auxiliary provider's back cover, and the front it
// could not be asked about is recorded as a miss rather than a durable match, so the
// retry window asks about it again.
func TestATrippedProviderLeavesAMissBehindALiveOne(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	identifiedAlbums(t, st, lib.ID, 5)

	coversDown, fanartDown := true, false
	covers := releaseArt(t, "covers", enrich.CapCover, model.ArtRoleFront, &coversDown)
	fanart := releaseArt(t, "fanart", enrich.CapAuxArt, model.ArtRoleBack, &fanartDown)
	svc := enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, RetryMissesAfter: retryWindow,
		Providers: []enrich.Provider{covers, fanart},
	}, nil)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.AlbumArtEnriched != 5 || res.Deferred != 3 || len(res.Stalled) != 0 {
		t.Fatalf("run 1 = %d walked / %d deferred / stalled %v, want 5, 3 and none",
			res.AlbumArtEnriched, res.Deferred, res.Stalled)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM art_map WHERE entity_type = 'album' AND role = 'back'`); n != 5 {
		t.Errorf("album backs = %d, want the live provider's on all five", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment
		WHERE entity_type = 'album_art' AND owed = 0 AND matched = 0 AND provider = 'fanart'`); n != 2 {
		t.Errorf("miss markers naming fanart = %d, want the two albums after the trip", n)
	}
	if owed, settled := owedMarkers(t, dbPath, "album_art"), settledMarkers(t, dbPath, "album_art"); owed != 3 || settled != 2 {
		t.Errorf("album art markers = %d owed / %d settled, want the three failures owed beside the two misses", owed, settled)
	}

	coversDown = false
	backdateMisses(t, dbPath, retryAge)
	res, err = svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.AlbumArtEnriched != 5 || res.Retried != 2 {
		t.Fatalf("run 2 = %d walked / %d retried, want the three deferred fresh and the two misses retried",
			res.AlbumArtEnriched, res.Retried)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM art_map WHERE entity_type = 'album' AND role = 'front'`); n != 5 {
		t.Errorf("album fronts = %d, want every front asked for again", n)
	}
}

// TestAnAlbumOnlyAnOutProviderCouldFillIsLeftAlone: albums that already hold a front need
// only their auxiliary roles, which only the auxiliary provider serves. Once it drops
// out, those albums have nobody left to ask, so the walk passes them by without a marker,
// a count or a heartbeat, as a stall would, while the phase itself keeps its live cover
// provider. The next pass finds them fresh.
func TestAnAlbumOnlyAnOutProviderCouldFillIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	for i := 1; i <= 5; i++ {
		seedAlbumTrackWithCover(t, st, lib.ID, "ess-"+strconv.Itoa(i), model.Track{
			Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
			Barcode: relBarcode,
		}, pngBytes(t))
	}
	coversDown, fanartDown := false, true
	covers := releaseArt(t, "covers", enrich.CapCover, model.ArtRoleFront, &coversDown)
	fanart := releaseArt(t, "fanart", enrich.CapAuxArt, model.ArtRoleBack, &fanartDown)
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{covers, fanart}}, nil)
	var beats []string
	res, err := svc.Run(ctx, enrich.RunOptions{}, func(_ float64, msg string) error {
		beats = append(beats, msg)
		return nil
	})
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.AlbumArtEnriched != 3 || res.Deferred != 3 || len(res.Stalled) != 0 {
		t.Fatalf("run 1 = %d walked / %d deferred / stalled %v, want the three asked, all owed, and no stall",
			res.AlbumArtEnriched, res.Deferred, res.Stalled)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'album_art'"); n != 3 {
		t.Errorf("album art markers = %d, want only the three asked", n)
	}
	walked := 0
	for _, b := range beats {
		if strings.Contains(b, "album art") {
			walked++
		}
	}
	if walked != 3 {
		t.Errorf("album art heartbeats = %d, want one per album asked", walked)
	}

	fanartDown = false
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM art_map WHERE entity_type = 'album' AND role = 'back'`); n != 5 {
		t.Errorf("album backs after run 2 = %d, want all five", n)
	}
}

// TestAStalledPhaseLeavesMarkersAlone: a stall ends the retry sweep too, so the expired
// misses nobody asked about keep the marker they had, stamp included, exactly as a
// --limit cutoff leaves them.
func TestAStalledPhaseLeavesMarkersAlone(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 5)

	calls := 0
	if _, err := lyricsService(st, &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics}).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	backdateMisses(t, dbPath, retryAge)
	stamp := scalarInt(t, roDB(t, dbPath), `SELECT MAX(enriched_at) FROM entity_enrichment WHERE entity_type = 'lyrics'`)

	res, err := lyricsService(st, scriptedLyrics([]bool{false}, &calls)).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if calls != 3 || res.Retried != 3 || res.Deferred != 3 {
		t.Errorf("calls %d / retried %d / deferred %d, want 3 each", calls, res.Retried, res.Deferred)
	}
	if len(res.Stalled) != 1 || res.Stalled[0] != model.EnrichPhaseLyrics {
		t.Errorf("stalled = %v, want [lyrics]", res.Stalled)
	}
	db := roDB(t, dbPath)
	if owed := owedMarkers(t, dbPath, "lyrics"); owed != 3 {
		t.Errorf("owed lyrics markers = %d, want the three the retry sweep asked", owed)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'lyrics' AND matched = 0 AND enriched_at = ?`, stamp); n != 2 {
		t.Errorf("misses still carrying the old stamp = %d, want the two the stall never reached", n)
	}
}

// TestTripIsPerProvider: a tripped auxiliary provider takes only its own slots out of
// the pass, so the cover provider keeps filling fronts and the phase does not stall.
func TestTripIsPerProvider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	identifiedAlbums(t, st, lib.ID, 5)

	coversDown, fanartDown := false, true
	covers := releaseArt(t, "covers", enrich.CapCover, model.ArtRoleFront, &coversDown)
	fanart := releaseArt(t, "fanart", enrich.CapAuxArt, model.ArtRoleBack, &fanartDown)
	res, err := enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{covers, fanart},
	}, nil).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 5 || res.Deferred != 3 || len(res.Stalled) != 0 {
		t.Fatalf("walked %d / deferred %d / stalled %v, want 5, 3 and none", res.AlbumArtEnriched, res.Deferred, res.Stalled)
	}
	if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM art_map WHERE entity_type = 'album' AND role = 'front'`); n != 5 {
		t.Errorf("album fronts = %d, want the cover provider's on all five", n)
	}
}

// warnings records the messages of every record at Warn or above.
type warnings struct{ msgs []string }

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.msgs = append(w.msgs, r.Message)
	}
	return nil
}
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }

// TestACanceledRunDoesNotBlameTheProvider: a call cut short by the run's own
// cancellation says nothing about the provider, so it is neither reported as a provider
// failure nor counted toward a trip.
func TestACanceledRunDoesNotBlameTheProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _, lib := openStore(t)
	seedTracks(t, st, lib.ID, 1)
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(ctx context.Context, _ enrich.Request) (*enrich.Candidate, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	var logs warnings
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{lyrics}}, slog.New(&logs))
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err == nil {
		t.Fatal("a canceled run reported success")
	}
	if len(logs.msgs) != 0 {
		t.Errorf("warnings on a canceled run = %q, want none blaming the provider", logs.msgs)
	}
}

// TestDeferredLookupsNeverHoldUpNewTargets: a lookup a provider keeps failing on is
// asked after the pass's new targets, so three of them tripping the provider cannot
// keep a new track from ever being asked, which is what happened while a failed lookup
// went back to the fresh queue at the head of every pass. That ask settles them.
func TestDeferredLookupsNeverHoldUpNewTargets(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 6)

	refused := map[string]bool{"Song 1": true, "Song 3": true, "Song 5": true}
	var asked []string
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asked = append(asked, req.Title)
			if refused[req.Title] {
				return nil, errors.New("lyrics service failed on this track")
			}
			return &enrich.Candidate{Lyrics: &model.Lyrics{Unsynced: "la la"}}, nil
		}}
	svc := lyricsService(st, lyrics)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	seedTrack(t, st, lib.ID, "/lib/7.mp3", "ess-7", "Song 7", "Artist 7", "Album 7")
	asked = nil
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(asked) == 0 || asked[0] != "Song 7" {
		t.Fatalf("run 2 asked %v, want the new track first", asked)
	}
	if res.LyricsMatched != 1 || res.Deferred != 0 || owedMarkers(t, dbPath, "lyrics") != 0 {
		t.Errorf("run 2 = %d matched / %d deferred / %d owed, want the new track answered and the three settled",
			res.LyricsMatched, res.Deferred, owedMarkers(t, dbPath, "lyrics"))
	}
	if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM lyrics l JOIN playable_item pi ON pi.id = l.item_id
		WHERE pi.title = 'Song 7'`); n != 1 {
		t.Errorf("lyrics rows for the new track = %d, want 1", n)
	}
}

// TestASecondFailureSettlesAnOwedLookup: a lookup a provider failed on is asked again on
// a later pass, and that ask settles it even when it fails again, so a target the
// provider always fails on costs one more request and then waits out the retry window
// like any other miss.
func TestASecondFailureSettlesAnOwedLookup(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 1)

	calls := 0
	svc := lyricsService(st, scriptedLyrics([]bool{false}, &calls))
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if owedMarkers(t, dbPath, "lyrics") != 1 {
		t.Fatal("the failed lookup is not owed")
	}
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if calls != 2 || res.Deferred != 0 || owedMarkers(t, dbPath, "lyrics") != 0 || settledMarkers(t, dbPath, "lyrics") != 1 {
		t.Fatalf("after run 2: %d calls, %d deferred, %d owed, %d settled; want the lookup asked again and settled as a miss",
			calls, res.Deferred, owedMarkers(t, dbPath, "lyrics"), settledMarkers(t, dbPath, "lyrics"))
	}
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if calls != 2 {
		t.Errorf("run 3 asked the settled miss again: %d calls, want 2", calls)
	}
}

// TestAFailingClusterHoldsUpTheOwedSweepOnePassAtMost: three tracks the provider always
// fails on sit at the head of the owed sweep, ahead of a track whose lookup failed once.
// They trip the provider in the owed sweep, but their asks there settle them, so the
// next pass reaches the track behind them rather than tripping on them again until they
// age out.
func TestAFailingClusterHoldsUpTheOwedSweepOnePassAtMost(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 3)

	calls := map[string]int{}
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			calls[req.Title]++
			if req.Title != "Song 4" || calls[req.Title] == 1 {
				return nil, errors.New("lyrics service failed on this track")
			}
			return &enrich.Candidate{Lyrics: &model.Lyrics{Unsynced: "la la"}}, nil
		}}
	svc := lyricsService(st, lyrics)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	seedTrack(t, st, lib.ID, "/lib/4.mp3", "ess-4", "Song 4", "Artist 4", "Album 4")
	for run := 2; run <= 3; run++ {
		if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if calls["Song 4"] != 2 {
		t.Errorf("Song 4 asked %d times, want its one more ask on run 3", calls["Song 4"])
	}
	if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM lyrics l JOIN playable_item pi ON pi.id = l.item_id
		WHERE pi.title = 'Song 4'`); n != 1 {
		t.Errorf("lyrics rows for Song 4 = %d, want its answer on run 3", n)
	}
	if owed := owedMarkers(t, dbPath, "lyrics"); owed != 0 {
		t.Errorf("owed lyrics lookups after run 3 = %d, want every one settled", owed)
	}
}

// TestEveryWalkSettlesAnOwedLookupOnItsNextAsk: every port walk leaves a lookup its
// provider failed on owed, and the next pass's ask settles it, a second failure included.
func TestEveryWalkSettlesAnOwedLookupOnItsNextAsk(t *testing.T) {
	ctx := context.Background()
	down := func(name string, caps enrich.Capability, at enrich.TargetType) *enrich.Mock {
		return &enrich.Mock{ProviderName: name, Caps: caps, CapsAt: map[enrich.TargetType]enrich.Capability{at: caps},
			EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
				return nil, errors.New(name + " is down")
			}}
	}
	track := func(t *testing.T, st *sqlite.Store, libID int64) {
		seedTrack(t, st, libID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	}
	album := func(t *testing.T, st *sqlite.Store, libID int64) {
		seedAlbumTrack(t, st, libID, "ess-a", model.Track{
			Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1, Barcode: relBarcode,
		})
	}
	book := func(t *testing.T, st *sqlite.Store, libID int64) {
		seedBook(t, st, libID, "/lib/b.m4b", "ess-b", "Neuromancer", "William Gibson")
	}
	cases := []struct {
		name, marker string
		seed         func(t *testing.T, st *sqlite.Store, libID int64)
		provider     *enrich.Mock
	}{
		{"aux art", "aux_art", track, down("fanart", enrich.CapAuxArt, enrich.TargetReleaseGroup)},
		{"artist art", "artist_art", track, down("deezer", enrich.CapArtistArt, enrich.TargetArtist)},
		{"album art", "album_art", album, down("covers", enrich.CapCover, enrich.TargetRelease)},
		{"track fields", "fields", track, down("getsongbpm", enrich.CapFields, enrich.TargetRecording)},
		{"book fields", "fields", book, down("audible", enrich.CapBookMeta, enrich.TargetBook)},
		{"album fields", "fields_album", album, down("discogs", enrich.CapFields, enrich.TargetRelease)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, dbPath, lib := openStore(t)
			tc.seed(t, st, lib.ID)
			svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{tc.provider}}, nil)
			first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
			if err != nil {
				t.Fatalf("run 1: %v", err)
			}
			if first.Deferred != 1 || owedMarkers(t, dbPath, tc.marker) != 1 {
				t.Fatalf("run 1 = %d deferred / %d owed, want the failed lookup owed", first.Deferred, owedMarkers(t, dbPath, tc.marker))
			}
			second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
			if err != nil {
				t.Fatalf("run 2: %v", err)
			}
			if second.Deferred != 0 || owedMarkers(t, dbPath, tc.marker) != 0 || settledMarkers(t, dbPath, tc.marker) != 1 {
				t.Errorf("run 2 = %d deferred / %d owed / %d settled, want the second failure settled",
					second.Deferred, owedMarkers(t, dbPath, tc.marker), settledMarkers(t, dbPath, tc.marker))
			}
		})
	}
}

// TestAnOwedLookupNothingAsksSettlesAfterAWeek: an owed lookup whose slot was filled
// some other way is never selected again, so once it is a week old the pass settles it
// as it stood, a miss dated from when it became owed, rather than keep the owed sweep
// running for it forever.
func TestAnOwedLookupNothingAsksSettlesAfterAWeek(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTracks(t, st, lib.ID, 1)

	calls := 0
	svc := lyricsService(st, scriptedLyrics([]bool{false}, &calls))
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	pid := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM playable_item LIMIT 1"))
	if err := st.SetItemLyrics(ctx, pid, &model.Lyrics{Unsynced: "typed in"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("set lyrics: %v", err)
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour).UnixNano()
	if _, err := rwDB(t, dbPath).Exec("UPDATE entity_enrichment SET enriched_at = ? WHERE owed = 1", weekAgo); err != nil {
		t.Fatalf("backdate the owed lookup: %v", err)
	}
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if calls != 1 || owedMarkers(t, dbPath, "lyrics") != 0 || settledMarkers(t, dbPath, "lyrics") != 1 {
		t.Fatalf("after run 2: %d calls, %d owed, %d settled; want the week-old lookup settled without another ask",
			calls, owedMarkers(t, dbPath, "lyrics"), settledMarkers(t, dbPath, "lyrics"))
	}
	if stamp := int64(scalarInt(t, roDB(t, dbPath), `SELECT enriched_at FROM entity_enrichment WHERE entity_type = 'lyrics'`)); stamp != weekAgo {
		t.Errorf("the miss is dated %d, want when it became owed (%d)", stamp, weekAgo)
	}
}

// TestALookupDeferredThisPassWaitsForTheNext: the owed sweep asks only what earlier
// passes left owed, so a track that fails on this pass's fresh sweep is not asked again
// minutes later in the same pass.
func TestALookupDeferredThisPassWaitsForTheNext(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTracks(t, st, lib.ID, 1)
	var asked []string
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asked = append(asked, req.Title)
			return nil, errors.New("lyrics service is down")
		}}
	svc := lyricsService(st, lyrics)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	seedTrack(t, st, lib.ID, "/lib/2.mp3", "ess-2", "Song 2", "Artist 2", "Album 2")
	asked = nil
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(asked) != 2 || asked[0] != "Song 2" || asked[1] != "Song 1" {
		t.Errorf("run 2 asked %v, want the new track then the one owed from run 1, each once", asked)
	}
}

// TestAForcedPhaseWalksAnOwedTargetOnce: a phase-scoped force walks every target in its
// first sweep, owed ones included, so the owed sweep behind it has nothing left for that
// phase.
func TestAForcedPhaseWalksAnOwedTargetOnce(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTracks(t, st, lib.ID, 1)
	calls := 0
	svc := lyricsService(st, scriptedLyrics([]bool{false}, &calls))
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	calls = 0
	if _, err := svc.Run(ctx, enrich.RunOptions{ForcePhases: []model.EnrichPhase{model.EnrichPhaseLyrics}}, nil); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if calls != 1 {
		t.Errorf("forced run asked %d times, want the owed track once", calls)
	}
}

// TestAStallNamesOnlyItsOwnProviders: the stall warning and heartbeat name the providers
// that left this phase with nobody to ask, not every provider that dropped out of the
// pass, so a lyrics stall never blames an artist-art service.
func TestAStallNamesOnlyItsOwnProviders(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTracks(t, st, lib.ID, 5)
	artistArt := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt, Err: errors.New("deezer is down")}
	lyrics := &enrich.Mock{ProviderName: "lrc", Caps: enrich.CapLyrics, Err: errors.New("lrc is down")}
	svc := enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{artistArt, lyrics},
	}, nil)
	var stalls []string
	res, err := svc.Run(ctx, enrich.RunOptions{}, func(_ float64, msg string) error {
		if strings.HasPrefix(msg, "stalled ") {
			stalls = append(stalls, msg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stalled) != 2 {
		t.Fatalf("stalled = %v, want artist-art and lyrics", res.Stalled)
	}
	want := []string{"stalled artist art: deezer out of this pass", "stalled lyrics: lrc out of this pass"}
	if len(stalls) != 2 || stalls[0] != want[0] || stalls[1] != want[1] {
		t.Errorf("stall heartbeats = %q, want %q", stalls, want)
	}
}
