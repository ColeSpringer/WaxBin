package podcast

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
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/store/sqlite"
)

// episodeReadStore counts the store's two single-episode reads.
type episodeReadStore struct {
	Store
	details, metas int
}

func (s *episodeReadStore) EpisodeByPID(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error) {
	s.details++
	return s.Store.EpisodeByPID(ctx, pid)
}

func (s *episodeReadStore) EpisodeMeta(ctx context.Context, pid model.PID) (*model.Episode, error) {
	s.metas++
	return s.Store.EpisodeMeta(ctx, pid)
}

// TestEpisodeOperationsReadTheEpisodeRowAlone: a download, an import, a transcript fetch
// and an unfetch each need the episode's row and nothing of its detail (chapters, persons,
// soundbites), so each takes the one-statement read. The batch read answers in request
// order.
func TestEpisodeOperationsReadTheEpisodeRowAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/srt")
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:04,000\nwords\n")
	}))
	defer srv.Close()
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: "Chan", Episodes: []model.FeedEpisode{
			{Title: "One", GUID: "youtube:video:v1", EnclosureURL: "yt://v1",
				TranscriptURL: srv.URL + "/1.srt", TranscriptType: "application/srt"},
			{Title: "Two", GUID: "youtube:video:v2", EnclosureURL: "yt://v2"},
		}},
		Payload: []byte("audio")}
	store := &episodeReadStore{Store: st}
	svc := New(store, meta.NewReader(), Config{Dir: t.TempDir(), Providers: []source.Provider{yt}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v)", len(eps), err)
	}
	one, two := eps[0], eps[1]
	if one.Title != "One" {
		one, two = two, one
	}

	if _, err := svc.Download(ctx, one.PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	src := filepath.Join(t.TempDir(), "two.mp3")
	if err := os.WriteFile(src, []byte("audio two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEpisodeFile(ctx, two.PID, src, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := svc.FetchTranscript(ctx, one.PID); err != nil {
		t.Fatalf("fetch transcript: %v", err)
	}
	if _, err := svc.Unfetch(ctx, one.PID); err != nil {
		t.Fatalf("unfetch: %v", err)
	}
	if store.details != 0 || store.metas != 4 {
		t.Errorf("detail reads %d, row reads %d; want none and one per operation", store.details, store.metas)
	}

	got, err := svc.EpisodesByPIDs(ctx, []model.PID{two.PID, one.PID})
	if err != nil || len(got) != 2 || got[0].PID != two.PID || got[1].PID != one.PID {
		t.Fatalf("EpisodesByPIDs = %v (err %v), want [Two One]", got, err)
	}
	if !got[1].HasTranscript || got[0].HasTranscript {
		t.Errorf("transcript flags = %t, %t; want only One's", got[0].HasTranscript, got[1].HasTranscript)
	}
}
