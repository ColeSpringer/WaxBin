package waxbin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// enrichMBMock answers the MusicBrainz endpoints for "Artist One" / "Album One"
// only, so a scope leak (looking up the other artist) surfaces as a no-match
// marker rather than a hang. The CAA/lyrics/genre providers are disabled by the
// callers, keeping the pass to MusicBrainz alone.
func enrichMBMock(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := strings.ToLower(r.URL.Query().Get("query"))
		switch {
		case r.URL.Path == "/artist" && strings.Contains(q, "artist one"):
			_, _ = w.Write([]byte(`{"artists":[{"id":"a1-mbid","name":"Artist One","sort-name":"Artist One","score":100}]}`))
		case r.URL.Path == "/artist" && q != "":
			_, _ = w.Write([]byte(`{"artists":[]}`))
		case r.URL.Path == "/artist/a1-mbid":
			_, _ = w.Write([]byte(`{"id":"a1-mbid","name":"Artist One","sort-name":"Artist One"}`))
		case r.URL.Path == "/release-group" && strings.Contains(q, "album one"):
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg1-mbid","title":"Album One","primary-type":"Album","score":100,
				"artist-credit":[{"artist":{"id":"a1-mbid","name":"Artist One"}}]}]}`))
		case r.URL.Path == "/release-group" && q != "":
			_, _ = w.Write([]byte(`{"release-groups":[]}`))
		case r.URL.Path == "/release-group/rg1-mbid":
			_, _ = w.Write([]byte(`{"id":"rg1-mbid","title":"Album One","primary-type":"Album","secondary-types":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// enrichTestConfig points enrichment at the mock with the optional providers
// off, so a run touches nothing but the MusicBrainz identity spine.
func enrichTestConfig(mbURL string) config.EnrichConfig {
	off := false
	return config.EnrichConfig{
		Contact:            "test@example.com",
		MusicBrainzBaseURL: mbURL,
		CoverArt:           &off,
		Lyrics:             &off,
		CommunityGenres:    &off,
	}
}

// TestEnrichScopedFacade drives EnrichOptions scoping through the facade: the
// mutual-exclusivity and resolution errors surface before any job starts, and a
// scoped run walks only the scoped item's targets.
func TestEnrichScopedFacade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Artist One", Album: "Album One", Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Artist Two", Album: "Album Two", Audio: testaudio.AudioWithSeed(2)}))

	mb := enrichMBMock(t)
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:     db,
		Roots:      []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment: enrichTestConfig(mb.URL),
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "One")

	// Both scopes at once, half an entity scope, an unknown item, and an
	// unsupported entity kind are all refused before a job starts.
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{ItemPID: pid, EntityType: read.EntityArtist, EntityPID: "x"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("item+entity scope err = %v, want CodeInvalid", err)
	}
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{EntityType: read.EntityArtist}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("half entity scope err = %v, want CodeInvalid", err)
	}
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{ItemPID: "01J0NONEXISTENT0000000000"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown item scope err = %v, want CodeNotFound", err)
	}
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{EntityType: read.EntityGenre, EntityPID: "x"}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("genre entity scope err = %v, want CodeUnsupported", err)
	}
	if jobs, err := lib.Jobs(ctx, 10); err == nil {
		for _, j := range jobs {
			if j.Kind == "enrich" {
				t.Fatalf("a refused scope still started job %+v", j)
			}
		}
	}

	// A scoped run touches only the scoped item's targets: one artist and one
	// release group get markers, the other track's entities stay unqueried.
	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{ItemPID: pid})
	if err != nil {
		t.Fatalf("scoped Enrich: %v", err)
	}
	if res.Result.ArtistsEnriched != 1 || res.Result.ReleaseGroupsEnriched != 1 {
		t.Fatalf("scoped result = %+v, want 1 artist + 1 release group", res.Result)
	}
	cov, err := lib.EnrichmentCoverage(ctx)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if cov.Artists != 1 || cov.ReleaseGroups != 1 {
		t.Fatalf("coverage = %+v, want exactly the scoped artist and release group marked", cov)
	}
}

// TestServeProxiedScopedEnrich round-trips the EnrichParams scope fields over
// the socket: a bad scope keeps its error class (resolved synchronously, before
// a job starts), and a good item scope runs as a server-side job whose result
// reflects only the scoped targets.
func TestServeProxiedScopedEnrich(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Artist One", Album: "Album One", Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Artist Two", Album: "Album Two", Audio: testaudio.AudioWithSeed(2)}))

	mb := enrichMBMock(t)
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:     db,
		Roots:      []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		IPCSocket:  sock,
		Enrichment: enrichTestConfig(mb.URL),
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		_ = lib.Close()
		t.Fatalf("scan: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	pid := itemPIDByTitle(t, ctx, lib, "One")
	c := dialWhenReady(t, sock)

	// Scope errors keep their class across the wire and never start a job.
	if _, err := c.RunEnrich(ctx, proxy.EnrichParams{ItemPID: "01J0NONEXISTENT0000000000"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("proxied unknown item scope err = %v, want CodeNotFound", err)
	}
	if _, err := c.RunEnrich(ctx, proxy.EnrichParams{ItemPID: string(pid), EntityType: "artist", EntityPID: "x"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("proxied item+entity scope err = %v, want CodeInvalid", err)
	}

	// A good item scope runs server-side; the tailed result covers only the
	// scoped targets.
	jobPID, err := c.RunEnrich(ctx, proxy.EnrichParams{ItemPID: string(pid)})
	if err != nil {
		t.Fatalf("proxied scoped enrich: %v", err)
	}
	job := waitForJobDone(t, ctx, lib, jobPID)
	var r enrich.Result
	if err := json.Unmarshal([]byte(job.Result), &r); err != nil {
		t.Fatalf("decode job result %q: %v", job.Result, err)
	}
	if r.ArtistsEnriched != 1 || r.ReleaseGroupsEnriched != 1 {
		t.Fatalf("proxied scoped result = %+v, want 1 artist + 1 release group", r)
	}
}

// TestScannedBookEnriches covers the book arm of enrichment, which
// BooksNeedingEnrichment gates on a non-empty book.mbid. Until the scanner copied the
// release id off a file's tags, that gate was never satisfied on a scanned library.
func TestScannedBookEnriches(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	// A .m4b holding valid MP3 bytes classifies as a book. The release id rides in a
	// TXXX frame, the way Picard writes it into an ID3 tag.
	const releaseMBID = "b1f4ec1e-0c8a-4b0e-9c22-2c1d0d5e7a33"
	writeFile(t, filepath.Join(root, "hobbit.m4b"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "The Hobbit", Artist: "J.R.R. Tolkien", AlbumArtist: "J.R.R. Tolkien",
		Album: "The Hobbit",
		TXXX:  []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: releaseMBID}},
	}))

	// The barcode is a real ISBN-13, which ApplyBookEnrichment's format check requires
	// before it lands in isbn.
	var releaseHits int
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/release/"+releaseMBID {
			releaseHits++
			_, _ = w.Write([]byte(`{"id":"` + releaseMBID + `","title":"The Hobbit",
				"asin":"B002V0QUOC","barcode":"9780261102217",
				"label-info":[{"label":{"name":"HarperCollins"}}]}`))
			return
		}
		// Everything else answers empty, so a stray lookup cannot fill these by accident.
		_, _ = w.Write([]byte(`{"artists":[],"release-groups":[]}`))
	}))
	t.Cleanup(mb.Close)

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:     db,
		Roots:      []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment: enrichTestConfig(mb.URL),
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	pid := itemPIDByTitle(t, ctx, lib, "The Hobbit")
	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.BooksEnriched != 1 || res.Result.BooksMatched != 1 {
		t.Fatalf("enrich result = %+v, want 1 book enriched and matched; the book arm is "+
			"reachable only when scan writes book.mbid", res.Result)
	}
	if releaseHits != 1 {
		t.Errorf("MusicBrainz release lookups = %d, want 1", releaseHits)
	}

	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if d.ASIN != "B002V0QUOC" || d.ISBN != "9780261102217" || d.Publisher != "HarperCollins" {
		t.Errorf("book after enrichment: asin=%q isbn=%q publisher=%q, want the release's values",
			d.ASIN, d.ISBN, d.Publisher)
	}
}

// TestEnrichmentTagWriteBackSurvivesRescan is the reason the write-back exists. The
// scanner rebuilds a book's identifier columns from tags, so a value living only in
// the catalog is cleared by the next content-changed rescan and the enrichment marker
// then stops `enrich` refilling it. Writing it to the file puts it where the scanner
// reads.
//
// The pid assertion is the other half: ASIN and ISBN feed identity.BookKey, so writing
// them re-keys the book unless the pass re-anchors its stored key.
func TestEnrichmentTagWriteBackSurvivesRescan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const releaseMBID = "b1f4ec1e-0c8a-4b0e-9c22-2c1d0d5e7a33"
	path := filepath.Join(root, "hobbit.m4b")
	spec := testaudio.MP3Spec{
		Title: "The Hobbit", Artist: "J.R.R. Tolkien", AlbumArtist: "J.R.R. Tolkien",
		Album: "The Hobbit",
		TXXX:  []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: releaseMBID}},
	}
	writeFile(t, path, testaudio.BuildMP3FromSpec(spec))

	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/release/"+releaseMBID {
			_, _ = w.Write([]byte(`{"id":"` + releaseMBID + `","title":"The Hobbit",
				"asin":"B002V0QUOC","barcode":"9780261102217",
				"label-info":[{"label":{"name":"HarperCollins"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"artists":[],"release-groups":[]}`))
	}))
	t.Cleanup(mb.Close)

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment:          enrichTestConfig(mb.URL),
		WriteEnrichmentTags: true,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "The Hobbit")

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TagsWritten != 1 {
		t.Fatalf("enrich wrote %d files (failed %d, unrepresented %d), want 1",
			res.Result.TagsWritten, res.Result.TagsFailed, res.Result.TagsUnrepresented)
	}

	// The values are on disk: a fresh read of the file sees them.
	fm, err := meta.NewReader().Read(ctx, path)
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	if fm.Tags.ASIN != "B002V0QUOC" || fm.Tags.ISBN != "9780261102217" || fm.Tags.Publisher != "HarperCollins" {
		t.Fatalf("file tags after write-back: asin=%q isbn=%q publisher=%q",
			fm.Tags.ASIN, fm.Tags.ISBN, fm.Tags.Publisher)
	}

	// Retag the file the way a user would, forcing the full rescan path that used to
	// clear these columns.
	spec.Genre = "Fantasy"
	writeFile(t, path, mergeTags(t, path, spec))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("Book after rescan (the pid did not survive the re-anchor): %v", err)
	}
	if d.ASIN != "B002V0QUOC" || d.ISBN != "9780261102217" || d.Publisher != "HarperCollins" {
		t.Errorf("after rescan: asin=%q isbn=%q publisher=%q, want the enriched values to survive",
			d.ASIN, d.ISBN, d.Publisher)
	}
}

// TestMatchedReleaseStaysOffDisk is the guard on the one thing the album release
// match deliberately does not do. identity.AlbumKey returns "mbid:"+lower(mbid) when
// a file carries one, so writing a matched release id to disk would re-key the album
// on the next scan and abandon the row's pid, curation, and stars. The value lives in
// the catalog only; enrichedTagSelect does not carry it.
func TestMatchedReleaseStaysOffDisk(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const (
		rgMBID  = "b0000000-0000-4000-8000-000000000002"
		relMBID = "c0000000-0000-4000-8000-000000000003"
		barcode = "0075992739429"
	)
	path := filepath.Join(root, "song.mp3")
	writeFile(t, path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Shine On", Artist: "Artist One", AlbumArtist: "Artist One", Album: "Album One",
		TXXX: []testaudio.TXXXFrame{
			{Desc: "MusicBrainz Release Group Id", Value: rgMBID},
			{Desc: "BARCODE", Value: barcode},
		},
	}))

	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/release-group/"+rgMBID:
			_, _ = w.Write([]byte(`{"id":"` + rgMBID + `","title":"Album One","primary-type":"Album"}`))
		case r.URL.Path == "/release" && r.URL.Query().Get("query") != "":
			_, _ = w.Write([]byte(`{"releases":[{"id":"` + relMBID + `","title":"Album One","score":100,
				"barcode":"` + barcode + `","release-group":{"id":"` + rgMBID + `"}}]}`))
		default:
			_, _ = w.Write([]byte(`{"artists":[],"release-groups":[],"releases":[]}`))
		}
	}))
	t.Cleanup(mb.Close)

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment:          enrichTestConfig(mb.URL),
		WriteEnrichmentTags: true,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Shine On")
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{}); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	// The catalog took the release id.
	item, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.AlbumMBID != relMBID {
		t.Fatalf("item AlbumMBID = %q, want the matched release %s", item.AlbumMBID, relMBID)
	}

	// The file did not: it still carries only the release-group id it was tagged with.
	fm, err := meta.NewReader().Read(ctx, path)
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	if fm.Tags.MBReleaseID != "" {
		t.Errorf("file gained a release id %q; writing one re-keys the album on the next scan",
			fm.Tags.MBReleaseID)
	}
	if fm.Tags.MBReleaseGroupID != rgMBID {
		t.Errorf("file release-group id = %q, want the tagged %s", fm.Tags.MBReleaseGroupID, rgMBID)
	}
}

// mergeTags rebuilds a fixture file's tags from spec while preserving the identifier
// tags the write-back just wrote, which is what a real retag in a tag editor does.
func mergeTags(t *testing.T, path string, spec testaudio.MP3Spec) []byte {
	t.Helper()
	fm, err := meta.NewReader().Read(context.Background(), path)
	if err != nil {
		t.Fatalf("read for merge: %v", err)
	}
	spec.Label = fm.Tags.Publisher
	spec.TXXX = append(spec.TXXX,
		testaudio.TXXXFrame{Desc: "ASIN", Value: fm.Tags.ASIN},
		testaudio.TXXXFrame{Desc: "ISBN", Value: fm.Tags.ISBN})
	return testaudio.BuildMP3FromSpec(spec)
}

// TestEnrichWithoutContactRunsInjectedProvider: an embedder that ships its own provider
// gets a working pass with no MusicBrainz contact configured, which is the contact-less
// half of retiring a downstream by-name sweep. The identity phases stay off, so nothing
// reaches MusicBrainz, and the artist is reached by name.
func TestEnrichWithoutContactRunsInjectedProvider(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "The Local Band", Album: "Demo", Audio: testaudio.AudioWithSeed(1)}))

	var asked []enrich.Request
	art := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			asked = append(asked, req)
			if req.Type != enrich.TargetArtist {
				return nil, nil
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: {Data: coverPNG(t), Format: "png", Width: 4, Height: 4},
			}}, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment:          config.EnrichConfig{}, // no contact
		EnrichmentProviders: []enrich.Provider{art},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("contact-less Enrich: %v", err)
	}
	if res.Result.ArtistsEnriched != 0 || res.Result.ReleaseGroupsEnriched != 0 {
		t.Errorf("identity phases walked %+v, want none without a contact", res.Result)
	}
	if res.Result.ArtistArtEnriched != 1 || res.Result.ArtistArtMatched != 1 {
		t.Fatalf("artist-art backfill = %d walked / %d matched, want 1 and 1",
			res.Result.ArtistArtEnriched, res.Result.ArtistArtMatched)
	}
	if len(asked) != 1 || asked[0].Artist != "The Local Band" || asked[0].MBID != "" {
		t.Fatalf("provider asked %+v, want one request keyed on the name alone", asked)
	}
}

// TestEnrichmentWritesTheNewFillsToDisk: the write-back reaches every field the fields
// walks fill and the scanner reads back. A track's BPM and ISRC land on its own file, an
// album's LABEL fans across every member (it lives on the album row, so it rides its own
// path), and a book's DATE and narrator land on the book's file.
func TestEnrichmentWritesTheNewFillsToDisk(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	pathA := filepath.Join(root, "a.mp3")
	pathB := filepath.Join(root, "b.mp3")
	writeFile(t, pathA, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, pathB, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(2)}))

	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			switch req.Type {
			case enrich.TargetRecording:
				return &enrich.Candidate{Fields: map[string]string{
					"bpm": "120", "isrc": "GBAYA7500098",
				}}, nil
			case enrich.TargetRelease:
				return &enrich.Candidate{Fields: map[string]string{"label": "Harvest"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{fields},
		WriteEnrichmentTags: true,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TagsFailed != 0 {
		t.Fatalf("write-back failed on %d files", res.Result.TagsFailed)
	}
	// Two member files, each carrying its own enriched fields and its album's label, so
	// each is opened once and counted once. Fanning the label separately would rewrite
	// both files a second time and report four writes for two files.
	if res.Result.TagsWritten != 2 {
		t.Fatalf("wrote %d files, want the 2 members written once each", res.Result.TagsWritten)
	}

	r := meta.NewReader()
	for _, p := range []string{pathA, pathB} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("re-read %s: %v", p, err)
		}
		if fm.Tags.BPM != 120 {
			t.Errorf("%s BPM = %d, want 120", filepath.Base(p), fm.Tags.BPM)
		}
		if fm.Tags.ISRC != "GBAYA7500098" {
			t.Errorf("%s ISRC = %q, want the enriched identifier", filepath.Base(p), fm.Tags.ISRC)
		}
		// The album label reached every member, not just the first.
		if fm.Tags.Label != "Harvest" {
			t.Errorf("%s LABEL = %q, want the album's label fanned out", filepath.Base(p), fm.Tags.Label)
		}
	}
}

// TestEnrichmentWritesBookFieldsToDisk: every book field the walk fills reaches the file,
// and a forced rescan reads them back onto the same item. edition feeds the identity key,
// so the write has to re-anchor the book or the rescan would resolve a fresh one.
func TestEnrichmentWritesBookFieldsToDisk(t *testing.T) {
	const blurb = "Bilbo Baggins is a hobbit who enjoys a comfortable life."
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	path := filepath.Join(root, "hobbit.m4b")
	writeFile(t, path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "The Hobbit", Artist: "J.R.R. Tolkien", AlbumArtist: "J.R.R. Tolkien",
		Album: "The Hobbit",
	}))

	books := &enrich.Mock{ProviderName: "audnexus", Caps: enrich.CapBookMeta,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetBook {
				return nil, nil
			}
			return &enrich.Candidate{
				Publisher: "HarperCollins",
				Fields: map[string]string{
					"year": "1937-09-21", "narrator": "Rob Inglis",
					"subtitle": "There and Back Again", "edition": "75th Anniversary Edition",
					"description": blurb,
				},
			}, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{books},
		WriteEnrichmentTags: true,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "The Hobbit")

	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, path)
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	if fm.Tags.Year != 1937 {
		t.Errorf("file year = %d, want the folded 1937", fm.Tags.Year)
	}
	if fm.Tags.Publisher != "HarperCollins" {
		t.Errorf("file publisher = %q, want the enriched value", fm.Tags.Publisher)
	}
	if len(fm.Tags.Narrators) == 0 || fm.Tags.Narrators[0] != "Rob Inglis" {
		t.Errorf("file narrators = %v, want the enriched narrator", fm.Tags.Narrators)
	}
	if fm.Tags.Subtitle != "There and Back Again" {
		t.Errorf("file subtitle = %q, want the enriched value", fm.Tags.Subtitle)
	}
	if fm.Tags.Edition != "75th Anniversary Edition" {
		t.Errorf("file edition = %q, want the enriched value", fm.Tags.Edition)
	}
	if fm.Tags.Description != blurb {
		t.Errorf("file description = %q, want the enriched value", fm.Tags.Description)
	}

	// Forced, which is the scan that recomputes identity from tags and rebuilds the
	// unlocked columns from them.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book pid did not survive scan --force (the written edition moved the key): %v", err)
	}
	if d.Subtitle != "There and Back Again" || d.Edition != "75th Anniversary Edition" || d.Description != blurb {
		t.Errorf("after the rescan subtitle/edition/description = %q/%q/%q, want the values read back from the file",
			d.Subtitle, d.Edition, d.Description)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("books after the rescan = %d (err %v), want the one re-anchored", len(items), err)
	}
}

// TestEnrichmentWriteBackRetriesOnTheNextPass: a pass whose writes fail (a read-only
// library) leaves the values catalog-only, and the next pass has nothing new to fill,
// since every item is already marked. The failed files still have to be written then,
// which the write-back's own drift rows arrange: one file through the item path with
// its bpm and the album label folded in, the other through the label fan-out alone.
func TestEnrichmentWriteBackRetriesOnTheNextPass(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only bit, so the writes would succeed")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	pathA := filepath.Join(root, "a.mp3")
	pathB := filepath.Join(root, "b.mp3")
	writeFile(t, pathA, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, pathB, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(2)}))

	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			switch req.Type {
			case enrich.TargetRecording:
				if req.Title == "One" {
					return &enrich.Candidate{Fields: map[string]string{"bpm": "120"}}, nil
				}
			case enrich.TargetRelease:
				return &enrich.Candidate{Fields: map[string]string{"label": "Harvest"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{fields},
		WriteEnrichmentTags: true,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// The write is a rewrite into the directory, so removing the directory's write bit
	// is what makes it fail. Windows ignores that bit on a directory, so there an open
	// handle on each target blocks the replacing rename instead.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	var handles []*os.File
	if runtime.GOOS == "windows" {
		for _, p := range []string{pathA, pathB} {
			h, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			handles = append(handles, h)
		}
	}
	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TagsFailed != 2 || res.Result.TagsWritten != 0 {
		t.Fatalf("read-only pass wrote %d and failed %d, want 0 written and both files failed",
			res.Result.TagsWritten, res.Result.TagsFailed)
	}
	drift, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginEnrichment, Code: model.DiagTagWriteUnsynced})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(drift) != 2 {
		t.Fatalf("drift rows = %+v, want one per failed file", drift)
	}

	// Writable again. Nothing is left to fill, so this pass has only the retries.
	for _, h := range handles {
		_ = h.Close()
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich again: %v", err)
	}
	if res.Result.TagsWritten != 2 || res.Result.TagsFailed != 0 {
		t.Fatalf("retry pass wrote %d and failed %d, want both files written", res.Result.TagsWritten, res.Result.TagsFailed)
	}
	r := meta.NewReader()
	fa, err := r.Read(ctx, pathA)
	if err != nil {
		t.Fatalf("re-read a: %v", err)
	}
	if fa.Tags.BPM != 120 || fa.Tags.Label != "Harvest" {
		t.Errorf("a.mp3 BPM/LABEL = %d/%q, want 120/Harvest from the retried item write", fa.Tags.BPM, fa.Tags.Label)
	}
	fb, err := r.Read(ctx, pathB)
	if err != nil {
		t.Fatalf("re-read b: %v", err)
	}
	if fb.Tags.Label != "Harvest" {
		t.Errorf("b.mp3 LABEL = %q, want Harvest from the retried label fan-out", fb.Tags.Label)
	}
	drift, err = lib.FileDiagnostics(ctx, model.DiagnosticFilter{Origin: model.OriginEnrichment})
	if err != nil {
		t.Fatalf("diagnostics after retry: %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("diagnostics after retry = %+v, want the drift cleared by the landed writes", drift)
	}
}

// TestEnrichmentWriteTagsCatchesUpAfterAPassWithoutIt: write-tags mirrors what
// enrichment filled, not only what the pass now finishing filled. A library enriched
// with it off has nothing new to fill when it is turned on, and the pass still writes
// every value that never reached its file, once.
func TestEnrichmentWriteTagsCatchesUpAfterAPassWithoutIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	pathA := filepath.Join(root, "a.mp3")
	pathB := filepath.Join(root, "b.mp3")
	writeFile(t, pathA, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, pathB, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(2)}))
	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			switch req.Type {
			case enrich.TargetRecording:
				return &enrich.Candidate{Fields: map[string]string{"bpm": "120"}}, nil
			case enrich.TargetRelease:
				return &enrich.Candidate{Fields: map[string]string{"label": "Harvest"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{fields},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Result.TrackFieldsMatched != 2 || res.Result.TagsWritten != 0 {
		t.Fatalf("pass without write-tags: %d matched, %d written, want 2 matched and nothing written",
			res.Result.TrackFieldsMatched, res.Result.TagsWritten)
	}
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("enrich --write-tags: %v", err)
	}
	if res.Result.TrackFieldsEnriched != 0 {
		t.Fatalf("second pass looked up %d tracks, want none: every item is marked", res.Result.TrackFieldsEnriched)
	}
	if res.Result.TagsWritten != 2 || res.Result.TagsFailed != 0 {
		t.Fatalf("second pass wrote %d files (failed %d), want the 2 left catalog-only by the first",
			res.Result.TagsWritten, res.Result.TagsFailed)
	}
	r := meta.NewReader()
	for _, p := range []string{pathA, pathB} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("re-read %s: %v", p, err)
		}
		if fm.Tags.BPM != 120 || fm.Tags.Label != "Harvest" {
			t.Errorf("%s BPM/LABEL = %d/%q, want 120/Harvest written by the catch-up pass", filepath.Base(p), fm.Tags.BPM, fm.Tags.Label)
		}
	}
	// Settled: a third pass has nothing to write.
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("third enrich: %v", err)
	}
	if res.Result.TagsWritten+res.Result.TagsFailed+res.Result.TagsUnrepresented != 0 {
		t.Errorf("third pass wrote %d, failed %d, unrepresented %d, want nothing left",
			res.Result.TagsWritten, res.Result.TagsFailed, res.Result.TagsUnrepresented)
	}
}

// TestEnrichmentScopedWriteTagsStaysScoped: a scoped run writes what is owed within its
// scope and nothing beyond it, so an item's "enrich now" cannot turn into a rewrite of
// every catalog-only value in the library. The rest stays owed for a full run.
func TestEnrichmentScopedWriteTagsStaysScoped(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	pathA := filepath.Join(root, "a.mp3")
	pathB := filepath.Join(root, "b.mp3")
	// Different albums, same artist: an artist reaches nothing the write-back writes.
	writeFile(t, pathA, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, pathB, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Meddle",
		Audio: testaudio.AudioWithSeed(2)}))
	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRecording {
				return &enrich.Candidate{Fields: map[string]string{"bpm": "120"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{fields},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{}); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{ItemPID: itemPIDByTitle(t, ctx, lib, "One"), WriteTags: true})
	if err != nil {
		t.Fatalf("scoped enrich: %v", err)
	}
	if res.Result.TagsWritten != 1 || res.Result.TagsFailed != 0 {
		t.Fatalf("scoped pass wrote %d files (failed %d), want the scoped item's file alone",
			res.Result.TagsWritten, res.Result.TagsFailed)
	}
	r := meta.NewReader()
	fa, err := r.Read(ctx, pathA)
	if err != nil {
		t.Fatalf("re-read a: %v", err)
	}
	fb, err := r.Read(ctx, pathB)
	if err != nil {
		t.Fatalf("re-read b: %v", err)
	}
	if fa.Tags.BPM != 120 || fb.Tags.BPM != 0 {
		t.Fatalf("BPM a/b = %d/%d, want the scoped file written and the other left alone", fa.Tags.BPM, fb.Tags.BPM)
	}
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("full enrich: %v", err)
	}
	if res.Result.TagsWritten != 1 {
		t.Fatalf("full pass wrote %d files, want the one still owed", res.Result.TagsWritten)
	}
	if fb, err = r.Read(ctx, pathB); err != nil || fb.Tags.BPM != 120 {
		t.Errorf("b.mp3 BPM = %d (err %v), want 120 from the full pass", fb.Tags.BPM, err)
	}
}

// TestEnrichmentLimitedWriteTagsWritesWhatItLookedUp: --limit caps a run's work, and
// the write-back is part of it. A limited run writes what is owed on the entities it
// looked up and nothing beyond them, so a pacing run cannot turn into a rewrite of
// every catalog-only value in the library. The rest waits for an unlimited run.
func TestEnrichmentLimitedWriteTagsWritesWhatItLookedUp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	pathA := filepath.Join(root, "a.mp3")
	pathB := filepath.Join(root, "b.mp3")
	writeFile(t, pathA, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Animals",
		Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, pathB, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Meddle",
		Audio: testaudio.AudioWithSeed(2)}))
	fields := &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type == enrich.TargetRecording {
				return &enrich.Candidate{Fields: map[string]string{"bpm": "120"}}, nil
			}
			return nil, nil
		}}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:              db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{fields},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Both filled, neither written.
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{}); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	// Forced so the limited run looks something up at all; the cap stops it after one.
	res, err := lib.Enrich(ctx, waxbin.EnrichOptions{Force: true, Limit: 1, WriteTags: true})
	if err != nil {
		t.Fatalf("limited enrich: %v", err)
	}
	if res.Result.TrackFieldsEnriched != 1 {
		t.Fatalf("limited run looked up %d tracks, want 1", res.Result.TrackFieldsEnriched)
	}
	if res.Result.TagsWritten != 1 || res.Result.TagsFailed != 0 {
		t.Fatalf("limited run wrote %d files (failed %d), want only the one it looked up",
			res.Result.TagsWritten, res.Result.TagsFailed)
	}
	r := meta.NewReader()
	withBPM := 0
	for _, p := range []string{pathA, pathB} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("re-read %s: %v", p, err)
		}
		if fm.Tags.BPM == 120 {
			withBPM++
		}
	}
	if withBPM != 1 {
		t.Fatalf("%d files carry the bpm after the limited run, want exactly 1", withBPM)
	}
	// An unlimited run writes what the limited one left owed.
	res, err = lib.Enrich(ctx, waxbin.EnrichOptions{WriteTags: true})
	if err != nil {
		t.Fatalf("unlimited enrich: %v", err)
	}
	if res.Result.TagsWritten != 1 {
		t.Fatalf("unlimited run wrote %d files, want the one still owed", res.Result.TagsWritten)
	}
}
