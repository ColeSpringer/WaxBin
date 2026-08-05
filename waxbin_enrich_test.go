package waxbin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
