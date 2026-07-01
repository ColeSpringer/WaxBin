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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playback"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// feedXML builds a feed served from base, with `n` episodes (n=1 keeps only the
// newer Episode Two, simulating a truncated feed).
func feedXML(base string, n int) string {
	ep1 := fmt.Sprintf(`
    <item>
      <title>Episode One</title>
      <guid>ep-0001</guid>
      <description>The first episode.</description>
      <pubDate>Tue, 02 Jan 2024 15:04:05 +0000</pubDate>
      <itunes:duration>300</itunes:duration>
      <enclosure url="%s/1.mp3" length="9" type="audio/mpeg"/>
      <podcast:transcript url="%s/1.srt" type="application/srt"/>
    </item>`, base, base)
	ep2 := fmt.Sprintf(`
    <item>
      <title>Episode Two</title>
      <guid>ep-0002</guid>
      <pubDate>Wed, 03 Jan 2024 00:00:00 +0000</pubDate>
      <itunes:duration>600</itunes:duration>
      <enclosure url="%s/2.mp3" length="9" type="audio/mpeg"/>
    </item>`, base)
	items := ep2
	if n >= 2 {
		items = ep1 + ep2
	}
	return fmt.Sprintf(`<?xml version="1.0"?>
<rss version="2.0"
     xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
     xmlns:podcast="https://podcastindex.org/namespace/1.0">
  <channel>
    <title>Test Cast</title>
    <itunes:author>Jane</itunes:author>
    <itunes:image href="%s/art.png"/>
    <podcast:guid>test-cast-guid</podcast:guid>%s
  </channel>
</rss>`, base, items)
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestPodcastEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var mu sync.Mutex
	truncated := false
	png := tinyPNG(t)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := 2
		if truncated {
			n = 1
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, feedXML(srv.URL, n))
	})
	audio := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audiobyte"))
	}
	mux.HandleFunc("/1.mp3", audio)
	mux.HandleFunc("/2.mp3", audio)
	mux.HandleFunc("/1.srt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/srt")
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:04,000\nthe magic transcriptword appears here\n")
	})
	mux.HandleFunc("/art.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})

	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Subscribe.
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pod.EpisodeCount != 2 {
		t.Fatalf("episode count = %d, want 2", pod.EpisodeCount)
	}

	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %d", len(eps))
	}
	// Newest first: Episode Two (Jan 3), then Episode One (Jan 2).
	ep2, ep1 := eps[0], eps[1]
	if ep1.Title != "Episode One" || ep2.Title != "Episode Two" {
		t.Fatalf("ordering: %q, %q", eps[0].Title, eps[1].Title)
	}
	if ep1.State != model.StateRemote || ep1.Downloaded {
		t.Fatalf("ep1 should be remote/undownloaded: %s downloaded=%v", ep1.State, ep1.Downloaded)
	}

	// Download both episodes.
	if _, err := svc.Download(ctx, ep1.PID); err != nil {
		t.Fatalf("download ep1: %v", err)
	}
	dl2, err := svc.Download(ctx, ep2.PID)
	if err != nil {
		t.Fatalf("download ep2: %v", err)
	}
	if _, err := os.Stat(dl2.Path); err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}

	d1, err := svc.Episode(ctx, ep1.PID)
	if err != nil {
		t.Fatalf("episode detail: %v", err)
	}
	if !d1.Episode.Downloaded || d1.Episode.State != model.StatePresent {
		t.Fatalf("ep1 should be present/downloaded after download: %+v", d1.Episode)
	}
	if !d1.HasTranscript {
		t.Fatal("ep1 should have a stored transcript")
	}

	// Transcript search finds the episode by a body-only word.
	sr, err := st.Search(ctx, "transcriptword", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search transcript: %v", err)
	}
	if !hasEpisode(sr.Episodes, ep1.PID) {
		t.Fatalf("transcript search missed ep1: %+v", sr.Episodes)
	}
	// Metadata search finds episodes by title.
	sr2, _ := st.Search(ctx, "Episode", read.SearchOptions{})
	if len(sr2.Episodes) == 0 {
		t.Fatal("metadata search found no episodes")
	}

	// Mark the older episode played, then let retention drop it; the play state must
	// survive (it is keyed on the item, which stays).
	user, err := st.DefaultUser(ctx)
	if err != nil {
		t.Fatalf("default user: %v", err)
	}
	pb := playback.New(st)
	if err := pb.MarkPlayed(ctx, user.PID, ep1.PID, true); err != nil {
		t.Fatalf("mark played: %v", err)
	}

	if err := svc.SetRetention(ctx, pod.PID, 1); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	ret, err := svc.ApplyRetention(ctx, pod.PID)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	if ret.Removed != 1 {
		t.Fatalf("retention removed = %d, want 1", ret.Removed)
	}

	// ep1 (older) is dropped back to remote, its file gone, but play state preserved.
	d1after, _ := svc.Episode(ctx, ep1.PID)
	if d1after.Episode.Downloaded || d1after.Episode.State != model.StateRemote {
		t.Fatalf("ep1 should be remote after retention: %+v", d1after.Episode)
	}
	if d1after.Episode.DisplayPath != "" {
		if _, err := os.Stat(d1after.Episode.DisplayPath); err == nil {
			t.Fatal("ep1 file should be removed from disk")
		}
	}
	ps, err := pb.State(ctx, user.PID, ep1.PID)
	if err != nil {
		t.Fatalf("play state: %v", err)
	}
	if !ps.Played {
		t.Fatal("play state must survive retention")
	}

	// Truncation safety: switch to a feed listing only Episode Two and re-sync.
	// Episode One must NOT be deleted.
	mu.Lock()
	truncated = true
	mu.Unlock()
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("sync truncated: %v", err)
	}
	epsAfter, _ := svc.Episodes(ctx, pod.PID, 0)
	if len(epsAfter) != 2 {
		t.Fatalf("truncated feed deleted an episode: have %d, want 2", len(epsAfter))
	}
}

func hasEpisode(hits []read.SearchHit, pid model.PID) bool {
	for _, h := range hits {
		if h.PID == pid {
			return true
		}
	}
	return false
}

func TestUnsubscribeRemovesEpisodesAndFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, feedXML(srv.URL, 2))
	})
	audio := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audiobyte"))
	}
	mux.HandleFunc("/1.mp3", audio)
	mux.HandleFunc("/2.mp3", audio)

	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	eps, _ := svc.Episodes(ctx, pod.PID, 0)
	dl, err := svc.Download(ctx, eps[0].PID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	// A private feed's stored password must not outlive the subscription.
	if err := svc.SetAuth(ctx, pod.PID, "user", "hunter2"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	secretKey := "podcast.auth." + string(pod.PID)
	if _, err := st.GetSecret(ctx, secretKey); err != nil {
		t.Fatalf("secret should exist after SetAuth: %v", err)
	}

	if err := svc.Remove(ctx, pod.PID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := svc.Get(ctx, pod.PID); err == nil {
		t.Fatal("podcast should be gone after remove")
	}
	if _, err := os.Stat(dl.Path); err == nil {
		t.Fatal("downloaded file should be removed on unsubscribe")
	}
	if _, err := st.GetSecret(ctx, secretKey); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("auth secret should be deleted on unsubscribe, got %v", err)
	}
}
