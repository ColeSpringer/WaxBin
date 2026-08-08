package podcast_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playback"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/store/sqlite"
)

// unfetchFixture subscribes to a two-episode feed on a local server and returns
// the service plus both episodes, newest first.
func unfetchFixture(t *testing.T) (*podcast.Service, *sqlite.Store, []*model.Episode) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, feedXML(srv.URL, 2))
	})
	audio := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audiobyte"))
	}
	mux.HandleFunc("/1.mp3", audio)
	mux.HandleFunc("/2.mp3", audio)
	mux.HandleFunc("/1.srt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/srt")
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:04,000\nbody\n")
	})
	mux.HandleFunc("/art.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG(t))
	})

	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: dir},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod, err := svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	return svc, st, eps
}

func TestUnfetchReclaimsAndPreservesPlayState(t *testing.T) {
	ctx := context.Background()
	svc, st, eps := unfetchFixture(t)
	ep := eps[0]

	dl, err := svc.Download(ctx, ep.PID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	user, err := st.DefaultUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pb := playback.New(st)
	if err := pb.MarkPlayed(ctx, user.PID, ep.PID, true); err != nil {
		t.Fatalf("mark played: %v", err)
	}

	res, err := svc.Unfetch(ctx, ep.PID)
	if err != nil {
		t.Fatalf("Unfetch: %v", err)
	}
	if !res.Unfetched {
		t.Error("Unfetch of a downloaded episode reported Unfetched=false")
	}
	if res.ReclaimedBytes != int64(len("audiobyte")) {
		t.Errorf("reclaimed %d bytes, want %d", res.ReclaimedBytes, len("audiobyte"))
	}
	if _, err := os.Stat(dl.Path); !os.IsNotExist(err) {
		t.Errorf("file still on disk at %s (stat err %v)", dl.Path, err)
	}

	d, err := svc.Episode(ctx, ep.PID)
	if err != nil {
		t.Fatalf("episode detail: %v", err)
	}
	if d.Episode.Downloaded || d.Episode.State != model.StateRemote {
		t.Errorf("episode after unfetch = %+v, want remote and undownloaded", d.Episode)
	}
	ps, err := pb.State(ctx, user.PID, ep.PID)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Played || !ps.Finished {
		t.Errorf("play state did not survive the unfetch: %+v", ps)
	}
}

// TestUnfetchTwiceIsANoOp pins the benign race: a retention pass that already
// reclaimed the file must not turn the caller's action into an error.
func TestUnfetchTwiceIsANoOp(t *testing.T) {
	ctx := context.Background()
	svc, _, eps := unfetchFixture(t)
	ep := eps[0]

	if _, err := svc.Download(ctx, ep.PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	if _, err := svc.Unfetch(ctx, ep.PID); err != nil {
		t.Fatalf("first Unfetch: %v", err)
	}
	res, err := svc.Unfetch(ctx, ep.PID)
	if err != nil {
		t.Fatalf("second Unfetch returned an error: %v", err)
	}
	if res.Unfetched || res.ReclaimedBytes != 0 {
		t.Errorf("second Unfetch = %+v, want a no-op reclaiming nothing", res)
	}

	// An episode that was never downloaded is the same no-op.
	res, err = svc.Unfetch(ctx, eps[1].PID)
	if err != nil {
		t.Fatalf("Unfetch of a never-downloaded episode: %v", err)
	}
	if res.Unfetched {
		t.Errorf("never-downloaded episode reported Unfetched=true: %+v", res)
	}
}

// TestUnfetchIgnoresThePin is the deliberate asymmetry: a pin exempts an episode
// from retention, not from an explicit unfetch, and survives it.
func TestUnfetchIgnoresThePin(t *testing.T) {
	ctx := context.Background()
	svc, _, eps := unfetchFixture(t)
	ep := eps[1] // the older one, which retention would otherwise drop

	if _, err := svc.Download(ctx, ep.PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	if _, err := svc.Download(ctx, eps[0].PID); err != nil {
		t.Fatalf("download newest: %v", err)
	}
	// Re-upserting the episode under the same guid is how a pin is set; the
	// identity key resolves to the row the feed sync already created.
	if _, err := svc.AddEpisode(ctx, ep.PodcastPID, model.FeedEpisode{
		GUID: "ep-0001", Title: ep.Title, EnclosureURL: ep.EnclosureURL,
	}, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Retention leaves the pinned episode alone.
	if err := svc.SetRetention(ctx, eps[0].PodcastPID, 1); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	if _, err := svc.ApplyRetention(ctx, eps[0].PodcastPID); err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	d, err := svc.Episode(ctx, ep.PID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Episode.Downloaded {
		t.Fatal("retention dropped the pinned episode; the fixture no longer tests the pin")
	}

	res, err := svc.Unfetch(ctx, ep.PID)
	if err != nil {
		t.Fatalf("Unfetch of a pinned episode: %v", err)
	}
	if !res.Unfetched {
		t.Error("Unfetch refused a pinned episode")
	}
	after, err := svc.Episode(ctx, ep.PID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Episode.Pinned {
		t.Error("Unfetch cleared the pin; it should survive so a re-download stays protected")
	}
	if after.Episode.State != model.StateRemote {
		t.Errorf("pinned episode state = %s, want remote", after.Episode.State)
	}
}

// TestNilLeaserRunsInline pins the standalone contract: a Service built with no
// Leaser (which is every test in this package, and any embedder that does not inject
// one) runs its filesystem verbs directly rather than failing for want of a lease.
func TestNilLeaserRunsInline(t *testing.T) {
	ctx := context.Background()
	svc, _, eps := unfetchFixture(t)
	if _, err := svc.Download(ctx, eps[0].PID); err != nil {
		t.Fatalf("download with a nil leaser: %v", err)
	}
	if _, err := svc.Unfetch(ctx, eps[0].PID); err != nil {
		t.Fatalf("unfetch with a nil leaser: %v", err)
	}
	if _, err := svc.ApplyRetentionAll(ctx); err != nil {
		t.Fatalf("retention with a nil leaser: %v", err)
	}
	if err := svc.Remove(ctx, eps[0].PodcastPID); err != nil {
		t.Fatalf("remove with a nil leaser: %v", err)
	}
}
