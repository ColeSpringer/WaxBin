package enrich_test

import (
	"context"
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
	if res.Retried != 1 || res.AlbumArtEnriched != 1 || res.ArtFetched != 1 {
		t.Fatalf("result = %+v, want one retried album whose cover landed", res)
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
