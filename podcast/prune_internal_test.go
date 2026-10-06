package podcast

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/store/sqlite"
)

// TestDownloadSurvivesAPruneOfItsShowFolder: a download prepares outside the podcast
// lease, where an unfetch's prune can take the show folder; the space check reads the
// download folder, and the fetch makes the show folder again.
func TestDownloadSurvivesAPruneOfItsShowFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: "Chan", Episodes: []model.FeedEpisode{
			{Title: "One", GUID: "youtube:video:v1", EnclosureURL: "yt://v1"},
		}},
		Payload: []byte("audio")}
	dir := t.TempDir()
	svc := New(st, meta.NewReader(), Config{Dir: dir, ReserveBytes: 1, Providers: []source.Provider{yt}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.freeSpace = func(path string) (uint64, error) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
		return diskfree.Available(path)
	}
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v)", len(eps), err)
	}
	if _, err := svc.Download(ctx, eps[0].PID); err != nil {
		t.Fatalf("download: %v", err)
	}
}
