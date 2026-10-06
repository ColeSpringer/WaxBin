package podcast_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/store/sqlite"
)

// renamedShow serves a two-episode feed whose show title the test can change, and returns
// the service, its download folder, the show and its episodes, newest first.
func renamedShow(t *testing.T) (svc *podcast.Service, dir string, pod *model.Podcast, eps []*model.Episode, retitle func(string)) {
	t.Helper()
	ctx := context.Background()
	dir = t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var mu sync.Mutex
	title := "Test Cast"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.Replace(feedXML(srv.URL, 2), "<title>Test Cast</title>", "<title>"+title+"</title>", 1))
	})
	audio := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audiobyte"))
	}
	mux.HandleFunc("/1.mp3", audio)
	mux.HandleFunc("/2.mp3", audio)
	svc = podcast.New(st, meta.NewReader(), podcast.Config{Dir: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod, err = svc.Add(ctx, srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if eps, err = svc.Episodes(ctx, pod.PID, 0); err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	retitle = func(s string) {
		mu.Lock()
		title = s
		mu.Unlock()
		if _, err := svc.Sync(ctx, pod.PID); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	return svc, dir, pod, eps, retitle
}

func download(t *testing.T, svc *podcast.Service, ep *model.Episode) string {
	t.Helper()
	dl, err := svc.Download(context.Background(), ep.PID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	return dl.Path
}

// TestUnfetchOfTheLastEpisodePrunesTheShowFolder: the show folder, junk and all, goes
// with the last episode unfetched from it, and the download folder stays.
func TestUnfetchOfTheLastEpisodePrunesTheShowFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, dir, _, eps, _ := renamedShow(t)
	first := download(t, svc, eps[0])
	download(t, svc, eps[1])
	show := filepath.Dir(first)
	if err := os.WriteFile(filepath.Join(show, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Unfetch(ctx, eps[0].PID)
	if err != nil || res.DirsPruned != 0 {
		t.Fatalf("first unfetch = %+v (err %v), want the folder kept for the other episode", res, err)
	}
	res, err = svc.Unfetch(ctx, eps[1].PID)
	if err != nil || res.DirsPruned != 1 {
		t.Fatalf("last unfetch = %+v (err %v), want the show folder pruned", res, err)
	}
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Errorf("the show folder is still there (err %v)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the download folder itself was removed: %v", err)
	}
}

// TestRemovePrunesTheShowFolder: unsubscribing deletes the downloads and their folder.
func TestRemovePrunesTheShowFolder(t *testing.T) {
	t.Parallel()
	svc, _, pod, eps, _ := renamedShow(t)
	show := filepath.Dir(download(t, svc, eps[0]))
	if err := svc.Remove(context.Background(), pod.PID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Errorf("the show folder is still there (err %v)", err)
	}
}

// TestRetentionPrunesAFolderItEmpties: a show renamed between downloads keeps episodes in
// two folders, and retention removing the old folder's last episode removes the folder.
func TestRetentionPrunesAFolderItEmpties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, pod, eps, retitle := renamedShow(t)
	old := filepath.Dir(download(t, svc, eps[1]))
	retitle("Renamed Cast")
	kept := filepath.Dir(download(t, svc, eps[0]))
	if old == kept {
		t.Fatalf("both downloads landed in %s; the rename gave no new folder", old)
	}
	if err := svc.SetRetention(ctx, pod.PID, 1); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.ApplyRetention(ctx, pod.PID); err != nil || res.Removed != 1 {
		t.Fatalf("retention = %+v (err %v), want one removed", res, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the emptied folder is still there (err %v)", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the kept episode's folder went: %v", err)
	}
}
