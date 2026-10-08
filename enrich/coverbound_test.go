package enrich_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/enrich"
)

// bigPNG is an opaque, textured picture over art.MaxSourceDim on its long side. It is a
// strip rather than a square so that bounding it, which the provider does inside its
// 15 second budget, stays quick under the race detector.
func bigPNG(t *testing.T) []byte {
	t.Helper()
	w, h := art.MaxSourceDim+200, 300
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(521288629)
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			img.Set(x, y, color.RGBA{uint8(x*255/w) + uint8(seed&7), uint8(y*255/h) + uint8((seed>>8)&7), 40, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// caaServing serves picture at the given paths, 404s everything else, and counts every
// request by path.
func caaServing(t *testing.T, picture []byte, paths ...string) (*httptest.Server, func(path string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	served := map[string]bool{}
	for _, p := range paths {
		served[p] = true
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if !served[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(picture)
	}))
	t.Cleanup(s.Close)
	return s, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}
}

const groupFrontQ = `SELECT am.%s FROM art_map am JOIN release_group rg ON rg.id = am.entity_id
	WHERE am.entity_type = 'release_group' AND am.role = 'front'`

// TestCoverArtAsksForTheTwelveHundredRung: the archive serves each cover at 250, 500 and
// 1200 pixels beside the original, and the catalog asks for the 1200 one.
func TestCoverArtAsksForTheTwelveHundredRung(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	caa, hits := caaServing(t, pngBytes(t), "/release-group/wywh-mbid/front-1200")
	if _, err := newService(st, newMBMock(t).server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := caa.URL + "/release-group/wywh-mbid/front-1200"
	if got := scalarStr(t, roDB(t, dbPath), fmt.Sprintf(groupFrontQ, "source_url")); got != want {
		t.Errorf("fetched cover source_url = %q, want %q", got, want)
	}
	if n := hits("/release-group/wywh-mbid/front"); n != 0 {
		t.Errorf("the original was requested %d times, want none when the 1200 rung answers", n)
	}
}

// TestCoverArtFallsBackToTheOriginal: a cover the archive holds no 1200 thumbnail for is
// fetched as the original, which the catalog then bounds itself, recording the stored
// bytes' hash.
func TestCoverArtFallsBackToTheOriginal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	caa, hits := caaServing(t, bigPNG(t), "/release-group/wywh-mbid/front")
	if _, err := newService(st, newMBMock(t).server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := hits("/release-group/wywh-mbid/front-1200"); n != 1 {
		t.Errorf("the 1200 rung was asked %d times, want once before the original", n)
	}
	db := roDB(t, dbPath)
	want := caa.URL + "/release-group/wywh-mbid/front"
	if got := scalarStr(t, db, fmt.Sprintf(groupFrontQ, "source_url")); got != want {
		t.Errorf("fetched cover source_url = %q, want the original's %q", got, want)
	}
	var w int
	var format, hash string
	var data []byte
	if err := db.QueryRow(`SELECT s.width, s.format, s.hash, s.data FROM art_source s
		JOIN art_map am ON am.source_hash = s.hash WHERE am.entity_type = 'release_group'`).
		Scan(&w, &format, &hash, &data); err != nil {
		t.Fatalf("read the stored cover: %v", err)
	}
	if w != art.MaxSourceDim || hash != art.Hash(data) {
		t.Errorf("stored cover is %s, %d wide, hash matches its bytes %t; want %d wide with a matching hash",
			format, w, hash == art.Hash(data), art.MaxSourceDim)
	}
}

// TestCoverArtMissingAtBothRungsIsNoMatch: a group with no cover answers 404 at the 1200
// rung and again for the original, and the target settles as a clean miss.
func TestCoverArtMissingAtBothRungsIsNoMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	caa, hits := caaServing(t, nil)
	if _, err := newService(st, newMBMock(t).server.URL, caa.URL).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a, b := hits("/release-group/wywh-mbid/front-1200"), hits("/release-group/wywh-mbid/front"); a != 1 || b != 1 {
		t.Errorf("requests = %d at the 1200 rung and %d for the original, want one each", a, b)
	}
	if n := scalarInt(t, roDB(t, dbPath), "SELECT COUNT(*) FROM art_map WHERE entity_type = 'release_group'"); n != 0 {
		t.Errorf("a group with no cover holds %d front rows, want 0", n)
	}
}
