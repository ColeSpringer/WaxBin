package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// TestPodcastAddSaysItKeptTheStoredAddress: adding a subscribed show again says what the
// add did not take: an address of its own, which --replace-address moves the feed to, or
// another login at the stored address (the same login is a sync and says nothing). A
// move with no login says the stored login was dropped.
func TestPodcastAddSaysItKeptTheStoredAddress(t *testing.T) {
	t.Parallel()
	serve := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><rss version="2.0" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Members</title><podcast:guid>members</podcast:guid><item><title>One</title><guid>one</guid></item></channel></rss>`)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	srv, other := serve(), serve()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	add := func(args ...string) string {
		t.Helper()
		out, err := runCLIText(t, db, root, append([]string{"podcast", "add"}, args...)...)
		if err != nil {
			t.Fatalf("podcast add %v: %v", args, err)
		}
		return out
	}
	notes := func(out string) string {
		_, rest, _ := strings.Cut(out, "\n")
		return rest
	}
	if out := add(srv.URL+"/feed.xml?token=a", "--user", "a", "--pass", "pa"); notes(out) != "" {
		t.Fatalf("a first add printed %q, want no note", out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{srv.URL + "/feed.xml?token=b", "--user", "b", "--pass", "pb"},
			"Kept the stored address " + srv.URL + "; pass --replace-address to move the feed and use this login\n"},
		{[]string{srv.URL + "/feed.xml?token=a", "--user", "a", "--pass", "new"},
			"The show was already subscribed, so this login was not stored; pass --replace-address to store it, or use podcast auth\n"},
		{[]string{srv.URL + "/feed.xml?token=a", "--user", "a", "--pass", "pa"}, ""},
		{[]string{srv.URL + "/feed.xml?token=a", "--replace-address"}, ""},
		{[]string{other.URL + "/feed.xml?token=c", "--replace-address"},
			"Dropped the stored login, which was for the old address; pass --user and --pass with --replace-address to log in at " + other.URL + "\n"},
	} {
		if out := add(c.args...); notes(out) != c.want {
			t.Errorf("podcast add %v printed %q, want the note %q", c.args, out, c.want)
		}
	}
	out, err := runCLIJSON(t, db, root, "podcast", "list")
	if err != nil {
		t.Fatalf("podcast list: %v", err)
	}
	var env struct {
		Data []struct {
			FeedURL  string `json:"feedUrl"`
			AuthUser string `json:"authUser"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("podcast list printed %q: %v", out, err)
	}
	if len(env.Data) != 1 || env.Data[0].FeedURL != other.URL+"/feed.xml?token=c" || env.Data[0].AuthUser != "" {
		t.Fatalf("podcast list = %+v, want the show moved to the other host with no login", env.Data)
	}
}

// TestPodcastChaptersFetchesAndLists: podcast chapters --fetch stores an episode's
// declared chapters with no download, and podcast chapters lists them.
func TestPodcastChaptersFetchesAndLists(t *testing.T) {
	t.Parallel()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/c.json" {
			w.Header().Set("Content-Type", "application/json+chapters")
			_, _ = io.WriteString(w, `{"version":"1.2.0","chapters":[{"startTime":0,"title":"Open"},{"startTime":30,"title":"Close"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Show</title><item><title>One</title><guid>one</guid><podcast:chapters url="%s/c.json"/></item></channel></rss>`, base)
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	run := func(v any, args ...string) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if err := json.Unmarshal([]byte(out), v); err != nil {
			t.Fatalf("%v printed %q: %v", args, out, err)
		}
	}
	var added struct {
		Data struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	run(&added, "podcast", "add", srv.URL+"/feed.xml")
	var shown struct {
		Data struct {
			Episodes []struct {
				PID string `json:"pid"`
			} `json:"episodes"`
		} `json:"data"`
	}
	run(&shown, "podcast", "show", added.Data.PID)
	if len(shown.Data.Episodes) != 1 {
		t.Fatalf("show listed %d episodes, want 1", len(shown.Data.Episodes))
	}
	ep := shown.Data.Episodes[0].PID
	var fetched struct {
		Data struct {
			Fetched bool `json:"fetched"`
		} `json:"data"`
	}
	run(&fetched, "podcast", "chapters", ep, "--fetch")
	var listed struct {
		Data []struct {
			Title   string `json:"title"`
			StartMS int64  `json:"startMs"`
		} `json:"data"`
	}
	run(&listed, "podcast", "chapters", ep)
	if !fetched.Data.Fetched || len(listed.Data) != 2 || listed.Data[1].Title != "Close" || listed.Data[1].StartMS != 30000 {
		t.Fatalf("fetched %v, listed %+v, want Open and Close at 30 s", fetched.Data.Fetched, listed.Data)
	}
	if out, err := runCLIText(t, db, root, "podcast", "chapters", ep); err != nil || !strings.Contains(out, "Close") {
		t.Fatalf("podcast chapters printed %q (err %v), want the chapter list", out, err)
	}
}

// TestRootListingsHintWhenOnlyThePodcastLibraryExists: a catalog whose one library is the
// internal podcast library, made by the open, still says no library root is registered.
func TestRootListingsHintWhenOnlyThePodcastLibraryExists(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	t.Setenv("WAXBIN_PODCAST_DIR", t.TempDir())
	db := filepath.Join(t.TempDir(), "catalog.db")
	run := func(args ...string) string {
		t.Helper()
		cmd := newRootCmd(&globals{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", db}, args...))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout.String()
	}
	if out := run("init"); !strings.Contains(out, "No library roots registered") {
		t.Errorf("init printed %q, want the no-roots hint", out)
	}
	if out := run("library", "list"); !strings.Contains(out, "(no library roots registered)") {
		t.Errorf("library list printed %q, want the no-roots hint", out)
	}
	if out := run("doctor"); !strings.Contains(out, "libraries:      0\n") {
		t.Errorf("doctor printed %q, want no library roots counted", out)
	}
}
