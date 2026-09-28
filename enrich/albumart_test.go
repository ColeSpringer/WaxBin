package enrich_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// albumArtService is the stock shape for this rung: a contact, cover art on, and no
// injected provider beyond the recorder a test passes in. The Cover Art Archive answers
// the release rung, so the front half runs on an install like this one.
func albumArtService(st enrich.Store, mbURL, caaURL string, providers ...enrich.Provider) *enrich.Service {
	return enrich.New(st, enrich.Config{
		Contact: "test@example.com", FetchCoverArt: true,
		MinRequestInterval: time.Millisecond,
		MusicBrainzBaseURL: mbURL, CoverArtBaseURL: caaURL,
		Providers: providers,
	}, nil)
}

// releaseAsks records the release-rung requests a provider was given and answers
// nothing, so the built-in archive still decides what lands.
func releaseAsks(seen *[]enrich.Request, caps enrich.Capability) *enrich.Mock {
	return &enrich.Mock{ProviderName: "recorder", Caps: caps,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRelease {
				*seen = append(*seen, req)
			}
			return nil, nil
		}}
}

func albumArtHash(t *testing.T, dbPath, role string) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath),
		`SELECT COALESCE((SELECT source_hash FROM art_map WHERE entity_type='album' AND role=?), '')`, role)
}

// TestAlbumArtBackfillFillsAPicardTaggedAlbum is the gap this rung closes. An album whose
// release id came off the tags is never queued for the release match, so before the
// backfill it showed the release group's cover, one edition standing in for all of them.
// It also pins the request shape WaxDeck asked for: both printed identifiers ride the
// release-rung art request beside the mbid.
func TestAlbumArtBackfillFillsAPicardTaggedAlbum(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode, CatalogNumber: "SHVL 804",
	})

	var asks []enrich.Request
	caa, hits := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	svc := albumArtService(st, mb.server.URL, caa, releaseAsks(&asks, enrich.CapCover))
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 1 || res.AlbumArtMatched != 1 || res.ArtFetched != 1 {
		t.Fatalf("result = %+v, want one album walked, matched and filled", res)
	}
	if *hits != 1 {
		t.Errorf("release cover fetches = %d, want 1", *hits)
	}
	if len(asks) != 1 {
		t.Fatalf("release asks = %d, want 1", len(asks))
	}
	got := asks[0]
	if got.MBID != edGBMBID || got.Barcode != relBarcode || got.CatalogNumber != "SHVL 804" {
		t.Errorf("request identity = %q/%q/%q, want the album's release id, barcode and catalog number",
			got.MBID, got.Barcode, got.CatalogNumber)
	}
	if got.Want != enrich.CapCover {
		t.Errorf("request Want = %v, want CapCover for an album with no front", got.Want)
	}
	db := roDB(t, dbPath)
	if p := scalarStr(t, db, `SELECT am.provider FROM art_map am JOIN album al ON al.id = am.entity_id
		WHERE am.entity_type='album' AND am.role='front'`); p != "coverartarchive" {
		t.Errorf("album front provider = %q, want coverartarchive", p)
	}

	// The marker stops the repeat, so a second run spends nothing.
	res, err = svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.AlbumArtEnriched != 0 || *hits != 1 {
		t.Errorf("second run walked %d albums and fetched %d covers, want 0 and the first fetch alone",
			res.AlbumArtEnriched, *hits)
	}
}

// TestAlbumArtSkipsATitleOnlyAlbum: the releases of one group share a title, so an album
// with no identifier can only be answered with the wrong edition's picture. It is left
// out of the walk entirely rather than asked and marked, which would record noise.
func TestAlbumArtSkipsATitleOnlyAlbum(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asks []enrich.Request
	caa, hits := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa, releaseAsks(&asks, enrich.CapCover)).
		Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 0 || len(asks) != 0 || *hits != 0 {
		t.Errorf("result = %+v, asks %d, fetches %d: a title-only album must not be asked about",
			res, len(asks), *hits)
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art'"); n != 0 {
		t.Errorf("album art markers = %d, want none", n)
	}
}

// TestAlbumArtBlanksANonUUIDReleaseMBID: a scan stores MUSICBRAINZ_ALBUMID verbatim, so
// the column can hold something no provider will accept. The printed identifiers carry
// the ask instead of the whole request failing on a bad id.
func TestAlbumArtBlanksANonUUIDReleaseMBID(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: "not-a-uuid", Barcode: relBarcode,
	})
	if got := scalarStr(t, roDB(t, dbPath), "SELECT COALESCE(mbid,'') FROM album"); got != "not-a-uuid" {
		t.Fatalf("album mbid = %q; the fixture needs the scan to have stored the bad id", got)
	}

	var asks []enrich.Request
	caa, _ := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	if _, err := albumArtService(st, mb.server.URL, caa, releaseAsks(&asks, enrich.CapCover)).
		Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(asks) != 1 {
		t.Fatalf("release asks = %d, want 1 (the barcode still identifies it)", len(asks))
	}
	if asks[0].MBID != "" || asks[0].Barcode != relBarcode {
		t.Errorf("request = mbid %q / barcode %q, want the bad id dropped and the barcode kept",
			asks[0].MBID, asks[0].Barcode)
	}
}

// TestAlbumArtAsksUnderCapAuxArtBesideASettledFront: a member track's embedded cover
// answers the album's front, so the ask is redirected to the empty auxiliary slots rather
// than cancelled, and it goes to the providers that claim those roles.
func TestAlbumArtAsksUnderCapAuxArtBesideASettledFront(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrackWithCover(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	}, pngBytes(t))

	var asks []enrich.Request
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			asks = append(asks, req)
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, "late-front"),
				model.ArtRoleBack:  artImg(t, "back-hash"),
			}}, nil
		}}
	caa, hits := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa, aux).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(asks) != 1 || asks[0].Want != enrich.CapAuxArt {
		t.Fatalf("release asks = %+v, want one under CapAuxArt", asks)
	}
	if asks[0].MBID != edGBMBID || asks[0].Barcode != relBarcode {
		t.Errorf("aux ask = %q/%q, want the album's identifiers", asks[0].MBID, asks[0].Barcode)
	}
	if *hits != 0 || res.ArtFetched != 0 {
		t.Errorf("cover fetches = %d / ArtFetched %d, want none (the track's cover answers)", *hits, res.ArtFetched)
	}
	if res.AuxArtFetched != 1 {
		t.Errorf("aux images = %d, want 1 (the offered front is dropped)", res.AuxArtFetched)
	}
	if got := albumArtHash(t, dbPath, "back"); got != "back-hash" {
		t.Errorf("album back = %q, want the offered one", got)
	}
	if got := albumArtHash(t, dbPath, "front"); got != "" {
		t.Errorf("album front = %q, want none (the front stays the track's)", got)
	}
}

// TestAlbumArtAsksTheAuxProviderBehindTheCoverWinner: gatherArt stops at the first
// provider to supply a front, which is right at the rungs that have a backfill phase
// behind them. This rung IS the backfill and its marker is durable, so an auxiliary
// provider ordered after the cover winner would never be asked, once, ever.
func TestAlbumArtAsksTheAuxProviderBehindTheCoverWinner(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})

	cover := &enrich.Mock{ProviderName: "covers", Caps: enrich.CapCover,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			return &enrich.Candidate{Cover: artImg(t, "front-hash")}, nil
		}}
	var auxAsks []enrich.Request
	aux := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			auxAsks = append(auxAsks, req)
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleBack: artImg(t, "back-hash"),
			}}, nil
		}}
	mb := newRelMock(t, "[]")
	// Ordered cover-first, which is what makes the aux provider unreachable behind the
	// front winner.
	res, err := albumArtService(st, mb.server.URL, "", cover, aux).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(auxAsks) != 1 || auxAsks[0].Want != enrich.CapAuxArt {
		t.Fatalf("aux asks = %+v, want one under CapAuxArt behind the cover winner", auxAsks)
	}
	if res.ArtFetched != 1 || res.AuxArtFetched != 1 {
		t.Fatalf("art fetched = %d front / %d aux, want 1 and 1", res.ArtFetched, res.AuxArtFetched)
	}
	if got := albumArtHash(t, dbPath, "front"); got != "front-hash" {
		t.Errorf("album front = %q, want the cover provider's", got)
	}
	if got := albumArtHash(t, dbPath, "back"); got != "back-hash" {
		t.Errorf("album back = %q, want the aux provider's", got)
	}
}

// TestAlbumArtRetriesAnExpiredMiss ties the retry window to the new marker: an album the
// archive had no cover for last month is asked again this month, and asked with the
// cache bypass a forced run uses.
func TestAlbumArtRetriesAnExpiredMiss(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})

	var asks []enrich.Request
	answer := false
	p := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			asks = append(asks, req)
			if !answer {
				return nil, nil
			}
			return &enrich.Candidate{Cover: artImg(t, "late-cover")}, nil
		}}
	mb := newRelMock(t, "[]")
	svc := enrich.New(st, enrich.Config{
		Contact: "test@example.com", RetryMissesAfter: retryWindow,
		MinRequestInterval: time.Millisecond, MusicBrainzBaseURL: mb.server.URL,
		Providers: []enrich.Provider{p},
	}, nil)
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
	// The group's front, which the same provider missed at the group rung, expires too.
	if res.Retried != 2 || res.AlbumArtEnriched != 1 || res.GroupArtEnriched != 1 || res.ArtFetched != 1 {
		t.Fatalf("result = %+v, want the album's cover landing on its retry beside the group's", res)
	}
	if len(asks) != 2 || !asks[1].Force {
		t.Fatalf("second asks = %+v, want a forced re-ask", asks)
	}
	if got := albumArtHash(t, dbPath, "front"); got != "late-cover" {
		t.Errorf("album front = %q, want the cover the re-ask found", got)
	}
}

// TestAlbumArtScopedToOneAlbum: --entity album:<pid> resolves to the album itself as well
// as to its release group, so the art rung is reachable by name from the CLI.
func TestAlbumArtScopedToOneAlbum(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})
	seedAlbumTrack(t, st, lib.ID, "ess-b", model.Track{
		Artist: "Genesis", AlbumArtist: "Genesis", Album: "Foxtrot", TrackNo: 1,
		MBReleaseID: edUSMBID, Barcode: relOtherCode,
	})

	pid := model.PID(scalarStr(t, roDB(t, dbPath), "SELECT pid FROM album WHERE title='Wish You Were Here'"))
	scope, err := st.EnrichScopeForEntity(ctx, read.EntityAlbum, pid)
	if err != nil {
		t.Fatalf("EnrichScopeForEntity: %v", err)
	}

	var asks []enrich.Request
	caa, _ := newReleaseCAAMock(t, pngBytes(t))
	mb := newRelMock(t, "[]")
	res, err := albumArtService(st, mb.server.URL, caa, releaseAsks(&asks, enrich.CapCover)).
		Run(ctx, enrich.RunOptions{Scope: scope}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 1 {
		t.Fatalf("result = %+v, want the one scoped album", res)
	}
	if len(asks) != 1 || asks[0].MBID != edGBMBID {
		t.Fatalf("release asks = %+v, want only the scoped album's", asks)
	}
}

// TestAlbumArtAuxHalfNeedsAReleaseRungProvider: a fan-art service keyed on release groups
// declares the release rung empty, so an identified album whose front is settled is not
// walked for its auxiliary slots on that provider's account, where it would be asked,
// answered nil, and marked a miss every retry window. An album with no front is still
// walked, since the archive declares the release rung.
func TestAlbumArtAuxHalfNeedsAReleaseRungProvider(t *testing.T) {
	ctx := context.Background()
	var releaseAsks int
	fanart := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapAuxArt | enrich.CapArtistArt,
		CapsAt: map[enrich.TargetType]enrich.Capability{
			enrich.TargetReleaseGroup: enrich.CapAuxArt,
			enrich.TargetArtist:       enrich.CapArtistArt | enrich.CapAuxArt,
		},
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRelease {
				releaseAsks++
			}
			return nil, nil
		}}
	track := model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	}

	st, dbPath, lib := openStore(t)
	seedAlbumTrackWithCover(t, st, lib.ID, "ess-a", track, pngBytes(t))
	caa, hits := newReleaseCAAMock(t, pngBytes(t))
	res, err := albumArtService(st, newRelMock(t, "[]").server.URL, caa, fanart).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 0 || releaseAsks != 0 || *hits != 0 {
		t.Fatalf("walked %d albums, %d release asks, %d fetches; want none for an album whose front is settled",
			res.AlbumArtEnriched, releaseAsks, *hits)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art'"); n != 0 {
		t.Errorf("album art markers = %d, want none", n)
	}

	st2, _, lib2 := openStore(t)
	seedAlbumTrack(t, st2, lib2.ID, "ess-a", track)
	caa2, hits2 := newReleaseCAAMock(t, pngBytes(t))
	res2, err := albumArtService(st2, newRelMock(t, "[]").server.URL, caa2, fanart).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run without the embedded cover: %v", err)
	}
	if res2.AlbumArtEnriched != 1 || *hits2 != 1 {
		t.Errorf("walked %d albums with %d fetches, want the front half to walk it once", res2.AlbumArtEnriched, *hits2)
	}
	if releaseAsks != 0 {
		t.Errorf("fanart asked %d times at the release rung, want never", releaseAsks)
	}
}

// TestAlbumArtFrontHalfNeedsAReleaseRungProvider: a contact-less install whose only cover
// provider serves release groups has nobody to ask about an album's own front, so an
// identified album without one is neither walked nor marked. That provider does serve
// the group-art backfill.
func TestAlbumArtFrontHalfNeedsAReleaseRungProvider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})
	var releaseAsks int
	groupCovers := &enrich.Mock{ProviderName: "groupcovers", Caps: enrich.CapCover,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetReleaseGroup: enrich.CapCover},
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRelease {
				releaseAsks++
			}
			return nil, nil
		}}
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics}
	svc := enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{groupCovers, lyrics},
	}, nil)
	if got, want := svc.Phases(), []model.EnrichPhase{model.EnrichPhaseGroupArt, model.EnrichPhaseLyrics}; !slices.Equal(got, want) {
		t.Errorf("phases = %v, want %v", got, want)
	}
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 0 || releaseAsks != 0 {
		t.Errorf("walked %d albums with %d release asks, want none", res.AlbumArtEnriched, releaseAsks)
	}
	if res.LyricsEnriched != 1 {
		t.Errorf("lyrics walked %d tracks, want 1", res.LyricsEnriched)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='album_art'"); n != 0 {
		t.Errorf("album art markers = %d, want none", n)
	}
}

// TestAlbumArtOwesNothingForAuxiliaryRolesNobodyServes: with no provider serving the
// auxiliary roles at the release rung, the album's full set is its front. An injected
// cover provider failing ahead of the archive, which then supplies the front, leaves
// nothing a later pass could fill, so the album settles instead of being owed a lookup no
// queue would ever select again.
func TestAlbumArtOwesNothingForAuxiliaryRolesNobodyServes(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})
	art := pngBytes(t)
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release/"+edGBMBID+"/front" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(art)
	}))
	t.Cleanup(caa.Close)
	failing := &enrich.Mock{ProviderName: "covers", Caps: enrich.CapCover,
		CapsAt: map[enrich.TargetType]enrich.Capability{enrich.TargetRelease: enrich.CapCover},
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			return nil, errors.New("covers is down")
		}}
	res, err := albumArtService(st, newRelMock(t, "[]").server.URL, caa.URL, failing).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AlbumArtEnriched != 1 || res.ArtFetched != 1 || res.Deferred != 0 {
		t.Fatalf("run = %d walked / %d fetched / %d deferred, want the archive's front and nothing owed",
			res.AlbumArtEnriched, res.ArtFetched, res.Deferred)
	}
	if owed, settled := owedMarkers(t, dbPath, "album_art"), settledMarkers(t, dbPath, "album_art"); owed != 0 || settled != 1 {
		t.Errorf("album art markers = %d owed / %d settled, want one settled match", owed, settled)
	}
}

// TestAlbumArtFailureLeavesTheAlbumQueued: an archive error on the release cover is not a
// miss, so the album is asked again on the next pass rather than waiting a retry window
// for a cover the archive has.
func TestAlbumArtFailureLeavesTheAlbumQueued(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
		Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
		MBReleaseID: edGBMBID, Barcode: relBarcode,
	})
	art := pngBytes(t)
	fetches := 0
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release/"+edGBMBID+"/front" {
			http.NotFound(w, r)
			return
		}
		fetches++
		if fetches == 1 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(art)
	}))
	t.Cleanup(caa.Close)
	svc := albumArtService(st, newRelMock(t, "[]").server.URL, caa.URL)

	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.AlbumArtEnriched != 1 || first.Deferred != 1 {
		t.Fatalf("run 1 = %d walked / %d deferred, want 1 and 1", first.AlbumArtEnriched, first.Deferred)
	}
	if owedMarkers(t, dbPath, "album_art") != 1 || settledMarkers(t, dbPath, "album_art") != 0 {
		t.Fatal("the album's art lookup is not owed after the archive failed")
	}

	second, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if second.AlbumArtEnriched != 1 || second.Retried != 0 || second.ArtFetched != 1 || fetches != 2 {
		t.Fatalf("run 2 = %+v after %d fetches, want the album asked fresh and filled", second, fetches)
	}
	if got := albumArtHash(t, dbPath, "front"); got == "" {
		t.Error("album front is empty after the archive recovered")
	}
}

// TestArchiveAnswersOnlyForAMissingOrOversizedCover: a cover the archive does not have and
// one too large to store are answers about that release, so the album settles as a miss.
// Anything else can come from a service that is not answering, a blanket block or a
// maintenance page as much as a darkened item, so it leaves the lookup owed, which costs
// a darkened item one more request and keeps an outage from settling a night's albums.
func TestArchiveAnswersOnlyForAMissingOrOversizedCover(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		owed  bool
		serve http.HandlerFunc
	}{
		{"no cover", false, http.NotFound},
		{"an oversized original", false, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, 24<<20+1))
		}},
		{"a refused image", true, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}},
		{"a page that is not an image", true, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		}},
		{"a failing service", true, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, dbPath, lib := openStore(t)
			seedAlbumTrack(t, st, lib.ID, "ess-a", model.Track{
				Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wish You Were Here", TrackNo: 1,
				MBReleaseID: edGBMBID, Barcode: relBarcode,
			})
			caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/release/"+edGBMBID+"/front" {
					tc.serve(w, r)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(caa.Close)
			res, err := albumArtService(st, newRelMock(t, "[]").server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			owed, settled := owedMarkers(t, dbPath, "album_art"), settledMarkers(t, dbPath, "album_art")
			if tc.owed && (res.Deferred != 1 || owed != 1) {
				t.Errorf("deferred %d with %d owed / %d settled, want the lookup owed", res.Deferred, owed, settled)
			}
			if !tc.owed && (res.Deferred != 0 || settled != 1) {
				t.Errorf("deferred %d with %d owed / %d settled, want the album settled as a miss", res.Deferred, owed, settled)
			}
		})
	}
}
