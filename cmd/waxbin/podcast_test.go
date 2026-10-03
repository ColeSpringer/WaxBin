package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

const oneEpisodeFeed = `<?xml version="1.0"?><rss version="2.0"><channel><title>Show</title>
<item><title>One</title><guid>ep-1</guid><enclosure url="http://example.invalid/1.mp3" type="audio/mpeg" length="1"/></item>
</channel></rss>`

// TestPodcastSyncAllCountsTheFailedFeeds: a batch sync keeps going past a dead feed,
// and names and counts it beside the feeds that synced.
func TestPodcastSyncAllCountsTheFailedFeeds(t *testing.T) {
	t.Parallel()
	serve := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(oneEpisodeFeed))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	live, dead := serve(), serve()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	var deadPID string
	for _, srv := range []*httptest.Server{live, dead} {
		out, err := runCLIJSON(t, db, root, "podcast", "add", srv.URL)
		if err != nil {
			t.Fatalf("podcast add: %v", err)
		}
		var env struct {
			Data struct {
				PID string `json:"pid"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("podcast add printed %q: %v", out, err)
		}
		deadPID = env.Data.PID
	}
	dead.Close()

	out, err := runCLIJSON(t, db, root, "podcast", "sync")
	if err != nil {
		t.Fatalf("podcast sync: %v", err)
	}
	var env struct {
		Data struct {
			Feeds    int `json:"feeds"`
			Failed   int `json:"failed"`
			Failures []struct {
				PodcastPID string `json:"podcastPid"`
				Err        string `json:"error"`
			} `json:"failures"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("podcast sync printed %q: %v", out, err)
	}
	got := env.Data
	if got.Feeds != 1 || got.Failed != 1 || len(got.Failures) != 1 ||
		got.Failures[0].PodcastPID != deadPID || got.Failures[0].Err == "" {
		t.Fatalf("sync = %+v, want one feed synced and the dead one %s failed with its reason", got, deadPID)
	}
}
