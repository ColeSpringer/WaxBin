package podcast_test

import (
	"bytes"
	"context"
	"errors"
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playback"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeAudio is nine bytes an enclosure gate takes for audio: an MPEG frame header, then
// payload.
var fakeAudio = []byte("\xff\xfb\x90\x64audio")

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
	t.Parallel()
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
		_, _ = w.Write(fakeAudio)
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
	if !d1.Episode.HasTranscript {
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
	if err := pb.MarkPlayed(ctx, user.PID, ep1.PID, true, nil); err != nil {
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
	t.Parallel()
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
		_, _ = w.Write(fakeAudio)
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

// TestDownloadSendsTheLoginOnlyToTheFeedsOrigin: a private feed's login goes with an
// enclosure on the feed's own host and never to an enclosure another host serves.
func TestDownloadSendsTheLoginOnlyToTheFeedsOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var cdnSawLogin atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			cdnSawLogin.Store(true)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(fakeAudio)
	}))
	t.Cleanup(cdn.Close)
	var base string
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/feed.xml" {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Private</title>
<item><title>Here</title><guid>here</guid><enclosure url="%s/1.mp3" type="audio/mpeg"/></item>
<item><title>There</title><guid>there</guid><enclosure url="%s/2.mp3" type="audio/mpeg"/></item>
</channel></rss>`, base, cdn.URL)
	}))
	t.Cleanup(feed.Close)
	base = feed.URL
	svc, _, _ := newTestService(t)
	pod, err := svc.Add(ctx, feed.URL+"/feed.xml", podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	for _, ep := range eps {
		if _, err := svc.Download(ctx, ep.PID); err != nil {
			t.Errorf("download %s: %v", ep.Title, err)
		}
	}
	if cdnSawLogin.Load() {
		t.Error("the feed's login went to another host's enclosure")
	}
}

// boundCipher is a test SecretCipher that seals a value by binding it to its key.
type boundCipher struct{}

func (boundCipher) Seal(aad, plaintext []byte) ([]byte, error) {
	return append(append(append([]byte{}, aad...), 0), plaintext...), nil
}

func (boundCipher) Open(aad, ciphertext []byte) ([]byte, error) {
	head := append(append([]byte{}, aad...), 0)
	if !bytes.HasPrefix(ciphertext, head) {
		return nil, errors.New("sealed under another key")
	}
	return ciphertext[len(head):], nil
}

// oneEpisodeServer serves a feed with one episode whose enclosure it also serves,
// counting every request and recording the login each carried ("user:pass", or "" for
// none).
func oneEpisodeServer(t *testing.T) (srv *httptest.Server, hits *atomic.Int64, lastLogin *atomic.Value) {
	t.Helper()
	hits, lastLogin = new(atomic.Int64), new(atomic.Value)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		login := ""
		if u, p, ok := r.BasicAuth(); ok {
			login = u + ":" + p
		}
		lastLogin.Store(login)
		if r.URL.Path != "/feed.xml" {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel><title>Private</title>
<item><title>One</title><guid>one</guid><enclosure url="http://%[1]s/1.mp3" type="audio/mpeg"/>
<itunes:image href="http://%[1]s/ep.png"/><podcast:transcript url="http://%[1]s/1.srt" type="application/srt"/></item>
</channel></rss>`, r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv, hits, lastLogin
}

// TestASealedSecretWithoutACipherFailsTheDownloadAsTheCatalogs: a stored password the
// catalog cannot open fails a download and a sync before either reaches the network,
// as the catalog's failure rather than the feed's.
func TestASealedSecretWithoutACipherFailsTheDownloadAsTheCatalogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, hits, lastLogin := oneEpisodeServer(t)
	db := filepath.Join(t.TempDir(), "catalog.db")
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	sealing, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test", SecretCipher: boundCipher{}})
	if err != nil {
		t.Fatalf("open with a cipher: %v", err)
	}
	pod, err := podcast.New(sealing, meta.NewReader(), podcast.Config{Dir: t.TempDir()}, discard).
		Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := sealing.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open without a cipher: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir()}, discard)
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}
	hits.Store(0)

	if _, err := svc.Download(ctx, eps[0].PID); err == nil || source.IsProviderError(err) {
		t.Fatalf("download = %v, want the catalog's failure to read the login", err)
	}
	if _, err := svc.Sync(ctx, pod.PID); err == nil || source.IsProviderError(err) {
		t.Fatalf("sync = %v, want the catalog's failure to read the login", err)
	}
	if err := svc.FetchTranscript(ctx, eps[0].PID); err == nil || source.IsProviderError(err) {
		t.Fatalf("fetch transcript = %v, want the catalog's failure to read the login", err)
	}
	res, err := svc.SyncAll(ctx)
	if err != nil {
		t.Fatalf("sync all: %v", err)
	}
	if ferr := res.Failures[pod.PID]; ferr == nil || source.IsProviderError(ferr) {
		t.Fatalf("sync all failure for the show = %v, want the catalog's", ferr)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the feed's host was asked %d times with a login the catalog could not read", n)
	}

	// An import moves a file already here, so it goes on, and its image is asked for with
	// no login rather than half of one.
	src := filepath.Join(t.TempDir(), "one.mp3")
	if err := os.WriteFile(src, fakeAudio, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEpisodeFile(ctx, eps[0].PID, src, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if n, got := hits.Load(), lastLogin.Load(); n != 1 || got != "" {
		t.Fatalf("the import asked %d times, last with the login %q, want once with none", n, got)
	}
}

// TestAMissingPasswordFailsBeforeTheFeedIsAsked: a show whose password row is gone, as a
// restore of a backup trimmed of its secrets leaves it, fails its sync before the feed is
// asked rather than sending its user name alone, and says the password is missing.
func TestAMissingPasswordFailsBeforeTheFeedIsAsked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, hits, _ := oneEpisodeServer(t)
	svc, st, _ := newTestService(t)
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.DeleteSecret(ctx, model.PodcastSecretKey(pod.PID)); err != nil {
		t.Fatalf("drop the secret: %v", err)
	}
	hits.Store(0)
	_, err = svc.Sync(ctx, pod.PID)
	if err == nil || source.IsProviderError(err) || waxerr.ReasonOf(err) != podcast.ReasonPasswordMissing {
		t.Fatalf("sync = %v (reason %q), want the catalog's %q", err, waxerr.ReasonOf(err), podcast.ReasonPasswordMissing)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the feed was asked %d times with its password missing", n)
	}
}

// TestAPasswordWithoutAUserIsRefused: a login is a user name, with or without a
// password, so a password alone is refused before anything is asked or stored.
func TestAPasswordWithoutAUserIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, hits, _ := oneEpisodeServer(t)
	svc, _, _ := newTestService(t)
	if _, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{Pass: "token"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("add with a password alone = %v, want CodeInvalid", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the feed was asked %d times for a refused login", n)
	}
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := svc.SetAuth(ctx, pod.PID, "", "token"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("set auth with a password alone = %v, want CodeInvalid", err)
	}
}

// TestClearingThePasswordSendsTheUserAlone: setting a login again without a password
// leaves none of the old one behind.
func TestClearingThePasswordSendsTheUserAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, _, lastLogin := oneEpisodeServer(t)
	svc, _, _ := newTestService(t)
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := svc.SetAuth(ctx, pod.PID, "alice", ""); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := lastLogin.Load(); got != "alice:" {
		t.Fatalf("sync sent the login %q, want the user alone (alice:)", got)
	}
}

// TestImportWithoutAnImageReadsNoLogin: importing a file for an episode with no image to
// fetch makes no request, so it neither reads the show's login nor warns that it could
// not.
func TestImportWithoutAnImageReadsNoLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	sealing, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test", SecretCipher: boundCipher{}})
	if err != nil {
		t.Fatalf("open with a cipher: %v", err)
	}
	svc := podcast.New(sealing, meta.NewReader(), podcast.Config{Dir: t.TempDir()}, discard)
	show, err := svc.AddManual(ctx, "Private", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual: %v", err)
	}
	if err := svc.SetAuth(ctx, show.PID, "alice", "pw"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	ep, err := svc.AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Plain", GUID: "plain"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	if err := sealing.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open without a cipher: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	svc = podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir()}, slog.New(slog.NewTextHandler(logs, nil)))
	src := filepath.Join(t.TempDir(), "plain.mp3")
	if err := os.WriteFile(src, fakeAudio, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEpisodeFile(ctx, ep.EpisodePID, src, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "login") {
		t.Fatalf("the import logged %q, want nothing about a login it had no use for", out)
	}
}
