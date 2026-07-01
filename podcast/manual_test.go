package podcast_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/store/sqlite"
)

// failAttachStore delegates to a real store but forces AttachEpisodeFile to fail, to
// exercise the ingest rollback path.
type failAttachStore struct {
	podcast.Store
}

func (failAttachStore) AttachEpisodeFile(context.Context, model.AttachEpisodeFileInput) (model.PID, error) {
	return "", errors.New("simulated catalog failure")
}

// TestImportEpisodeFileRestoresMovedFileOnCatalogFailure verifies that when the
// catalog write fails after a moved (not copied) acquired file has landed, the file
// is restored to its source rather than deleted; it is the user's only copy.
func TestImportEpisodeFileRestoresMovedFileOnCatalogFailure(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	real, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	dir := t.TempDir()
	svc := podcast.New(failAttachStore{real}, meta.NewReader(), podcast.Config{Dir: dir},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	show, _, err := real.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: "manual:s", FeedURL: "manual:s", SourceType: model.SourceManual, Title: "Show",
	})
	if err != nil {
		t.Fatalf("UpsertShow: %v", err)
	}
	ep, err := real.UpsertEpisode(ctx, model.UpsertEpisodeInput{
		PodcastPID: show, Pinned: true,
		Episode: model.FeedEpisode{Title: "Clip", GUID: "c1"},
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "clip.mp3")
	if err := os.WriteFile(src, []byte("the user's only copy"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// Move (keepOriginal=false): the source is consumed into the podcast dir, then the
	// (simulated) catalog write fails.
	if _, err := svc.ImportEpisodeFile(ctx, ep.EpisodePID, src, false); err == nil {
		t.Fatal("expected the simulated catalog failure")
	}
	// The file must not be lost: it is restored to its source path.
	if data, serr := os.ReadFile(src); serr != nil {
		t.Fatalf("moved source file was lost after a catalog failure: %v", serr)
	} else if string(data) != "the user's only copy" {
		t.Fatalf("restored file content = %q", string(data))
	}
}

func newTestService(t *testing.T, providers ...source.Provider) (*podcast.Service, *sqlite.Store, string) {
	t.Helper()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := t.TempDir()
	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: dir, Providers: providers},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, st, dir
}

// TestYouTubeProviderDispatch verifies an injected mock youtube provider handles
// AddSource, Sync, and Download through the shared podcast engine.
func TestYouTubeProviderDispatch(t *testing.T) {
	ctx := context.Background()
	yt := &source.Mock{
		Type:        model.SourceYouTube,
		IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: "My Channel", Episodes: []model.FeedEpisode{
			{Title: "Vid One", GUID: "youtube:video:v1", EnclosureURL: "yt://v1", EnclosureType: "audio/mpeg"},
		}},
		Payload: []byte("fake-audio-bytes"),
	}
	svc, _, _ := newTestService(t, yt)

	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource youtube: %v", err)
	}
	if pod.SourceType != model.SourceYouTube || pod.EpisodeCount != 1 {
		t.Fatalf("youtube show = %+v, want source youtube / 1 episode", pod)
	}

	eps, _ := svc.Episodes(ctx, pod.PID, 0)
	if len(eps) != 1 {
		t.Fatalf("episodes = %d, want 1", len(eps))
	}
	// Download dispatches to the youtube provider's Fetch (writes the mock payload).
	dl, err := svc.Download(ctx, eps[0].PID)
	if err != nil {
		t.Fatalf("Download via youtube provider: %v", err)
	}
	if dl.Bytes != int64(len(yt.Payload)) {
		t.Fatalf("downloaded %d bytes, want %d", dl.Bytes, len(yt.Payload))
	}
	if data, _ := os.ReadFile(dl.Path); string(data) != string(yt.Payload) {
		t.Fatalf("downloaded file content = %q, want the mock payload", string(data))
	}
}

// TestManualShowAndEpisode verifies a manual show is created, accepts curated
// episodes, and never syncs because there is no feed to enumerate.
func TestManualShowAndEpisode(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)

	pod, err := svc.AddManual(ctx, "Curated", podcast.ManualOptions{Author: "Me"})
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	if pod.SourceType != model.SourceManual {
		t.Fatalf("manual show source = %q", pod.SourceType)
	}

	res, err := svc.AddEpisode(ctx, pod.PID, model.FeedEpisode{
		Title: "One-shot", EnclosureURL: "http://x/one.mp3",
	}, true)
	if err != nil || !res.Created {
		t.Fatalf("AddEpisode: %+v err=%v", res, err)
	}
	ep, err := svc.Episode(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if !ep.Episode.Pinned {
		t.Fatal("manual episode should be pinned")
	}

	// Syncing a manual show does nothing: there is no provider fetch and no new feed
	// episode to add.
	sync, err := svc.Sync(ctx, pod.PID)
	if err != nil {
		t.Fatalf("Sync manual show: %v", err)
	}
	if sync.EpisodesAdded != 0 {
		t.Fatalf("manual Sync added %d episodes, want 0", sync.EpisodesAdded)
	}
}

// TestSyncYouTubeAbsentProvider verifies a youtube show cannot sync when the build
// has no youtube provider registered.
func TestSyncYouTubeAbsentProvider(t *testing.T) {
	ctx := context.Background()
	// Register the youtube provider only long enough to subscribe, then a fresh
	// service without it stands in for a default CLI build.
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: "Chan"}}
	svc, st, _ := newTestService(t, yt)
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// A service without the youtube provider, like the default CLI build, cannot sync
	// a youtube show.
	svc2 := podcast.New(st, meta.NewReader(), podcast.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc2.Sync(ctx, pod.PID); err == nil {
		t.Fatal("syncing a youtube show without its provider should error")
	}
}
