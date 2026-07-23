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
	sock := filepath.Join(t.TempDir(), "wax.sock")
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
