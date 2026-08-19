package podcast_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/store/sqlite"
)

// testPNGBytes encodes a PNG of the given size, so a user cover is distinguishable
// from the feed's by content hash.
func testPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// coverFixture serves a one-episode feed plus its channel image, counting requests to
// the image path. source.Mock cannot drive any of this: fetchImage goes through the
// netsafe client podcast.New builds internally, which no mock hook sees.
type coverFixture struct {
	svc    *podcast.Service
	store  *sqlite.Store
	feed   string
	mu     sync.Mutex
	hits   int
	status int
}

func newCoverFixture(t *testing.T) *coverFixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &coverFixture{store: st, status: http.StatusOK}
	png := tinyPNG(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, fmt.Sprintf(`<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>Cover Cast</title>
    <itunes:image href="%s/art.png"/>
    <item>
      <title>Only Episode</title>
      <guid>ep-1</guid>
      <enclosure url="%s/1.mp3" length="9" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`, srv.URL, srv.URL))
	})
	mux.HandleFunc("/art.png", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		status := f.status
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})

	f.feed = srv.URL + "/feed.xml"
	f.svc = podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *coverFixture) imageHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *coverFixture) setStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

func (f *coverFixture) subscribe(t *testing.T) model.PID {
	t.Helper()
	pod, err := f.svc.Add(context.Background(), f.feed, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return pod.PID
}

func (f *coverFixture) coverHash(t *testing.T, pid model.PID) string {
	t.Helper()
	blob, err := f.store.ResolveArt(context.Background(),
		model.EntityRef{Type: model.ArtPodcast, PID: pid}, model.ArtRoleFront, 0)
	if err != nil {
		return ""
	}
	return blob.SourceHash
}

// The deferred scenario: a locked cover must cost no fetch at all, and unlocking must
// let the feed refill it even though the feed's image URL never changed. That last half
// is what the old compare got wrong, since podcast.image_url had already advanced to
// the feed's URL while the lock skipped the attach.
func TestSyncSkipsFetchWhileCoverLocked(t *testing.T) {
	ctx := context.Background()
	f := newCoverFixture(t)
	pid := f.subscribe(t)
	if f.imageHits() != 1 {
		t.Fatalf("subscribe made %d image requests, want 1", f.imageHits())
	}

	// A user cover, locked. Nothing about the feed changes from here on.
	user := testPNGBytes(t, 8, 8)
	if err := f.store.SetEntityArt(ctx, model.ArtPodcast, pid, model.ArtRoleFront, user, true, true); err != nil {
		t.Fatalf("set user cover: %v", err)
	}
	locked := f.coverHash(t, pid)

	for i := 0; i < 2; i++ {
		if _, err := f.svc.Sync(ctx, pid); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
	}
	if got := f.imageHits(); got != 1 {
		t.Errorf("two syncs under a lock made %d image requests total, want the 1 from subscribe", got)
	}
	if f.coverHash(t, pid) != locked {
		t.Errorf("locked cover changed under sync")
	}

	// Unlocking releases it, and the very next sync refills from the feed.
	if err := f.store.SetArtLock(ctx, model.ArtPodcast, pid, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := f.svc.Sync(ctx, pid); err != nil {
		t.Fatalf("sync after unlock: %v", err)
	}
	if got := f.imageHits(); got != 2 {
		t.Fatalf("sync after unlock made %d image requests total, want 2", got)
	}
	if h := f.coverHash(t, pid); h == locked || h == "" {
		t.Errorf("cover after unlock = %q, want the feed's image back", h)
	}
}

// A cleared cover leaves no art_map row, so the compare reads empty and the next sync
// refills it. This is the hole in the rejected design (reinterpreting podcast.image_url
// as "where the cover came from"): nothing resets that column on a clear, so it would
// compare A against A forever and the show would stay coverless.
func TestSyncRefillsClearedCover(t *testing.T) {
	ctx := context.Background()
	f := newCoverFixture(t)
	pid := f.subscribe(t)
	fromFeed := f.coverHash(t, pid)
	if fromFeed == "" {
		t.Fatal("subscribe attached no cover")
	}

	// Cleared without a lock, so nothing but the compare decides what happens next.
	if err := f.store.SetEntityArt(ctx, model.ArtPodcast, pid, model.ArtRoleFront, nil, false, true); err != nil {
		t.Fatalf("clear cover: %v", err)
	}
	if f.coverHash(t, pid) != "" {
		t.Fatal("cover survived the clear")
	}

	if _, err := f.svc.Sync(ctx, pid); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.imageHits(); got != 2 {
		t.Errorf("sync after a clear made %d image requests total, want 2", got)
	}
	if f.coverHash(t, pid) != fromFeed {
		t.Errorf("cover after sync = %q, want the feed's %q back", f.coverHash(t, pid), fromFeed)
	}
}

// A failed fetch attaches nothing, so source_url stays as it was and the next sync
// tries again rather than deciding the cover is current.
func TestSyncRetriesAfterFailedCoverFetch(t *testing.T) {
	ctx := context.Background()
	f := newCoverFixture(t)
	f.setStatus(http.StatusInternalServerError)
	pid := f.subscribe(t)
	if f.imageHits() != 1 {
		t.Fatalf("subscribe made %d image requests, want 1", f.imageHits())
	}
	if f.coverHash(t, pid) != "" {
		t.Fatal("a 500 attached a cover")
	}

	f.setStatus(http.StatusOK)
	if _, err := f.svc.Sync(ctx, pid); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := f.imageHits(); got != 2 {
		t.Errorf("sync after a failed fetch made %d image requests total, want 2 (it gave up)", got)
	}
	if f.coverHash(t, pid) == "" {
		t.Error("retry attached no cover")
	}

	// And once it lands, the compare goes quiet again.
	if _, err := f.svc.Sync(ctx, pid); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if got := f.imageHits(); got != 2 {
		t.Errorf("sync with the cover current made %d image requests total, want 2", got)
	}
}

// Re-adding a subscribed feed is a sync, and OPML import routes every entry through
// Add. A show whose cover the user locked must not pay for a full image download that
// the store then discards.
func TestReAddSkipsFetchWhileCoverLocked(t *testing.T) {
	ctx := context.Background()
	f := newCoverFixture(t)
	pid := f.subscribe(t)
	if f.imageHits() != 1 {
		t.Fatalf("subscribe made %d image requests, want 1", f.imageHits())
	}

	user := testPNGBytes(t, 8, 8)
	if err := f.store.SetEntityArt(ctx, model.ArtPodcast, pid, model.ArtRoleFront, user, true, true); err != nil {
		t.Fatalf("set user cover: %v", err)
	}
	locked := f.coverHash(t, pid)

	if _, err := f.svc.Add(ctx, f.feed, podcast.AddOptions{}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := f.imageHits(); got != 1 {
		t.Errorf("re-add under a lock made %d image requests total, want the 1 from subscribe", got)
	}
	if f.coverHash(t, pid) != locked {
		t.Error("re-add replaced the locked cover")
	}
}

// The same read also means a re-add whose cover is already current costs no download.
func TestReAddSkipsFetchWhenCoverCurrent(t *testing.T) {
	ctx := context.Background()
	f := newCoverFixture(t)
	f.subscribe(t)
	if _, err := f.svc.Add(ctx, f.feed, podcast.AddOptions{}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := f.imageHits(); got != 1 {
		t.Errorf("re-add with the cover current made %d image requests total, want 1", got)
	}
}
