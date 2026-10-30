package podcast_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
)

const guidFeed = `<?xml version="1.0"?><rss version="2.0" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Members</title><podcast:guid>members-guid</podcast:guid>
<item><title>One</title><guid>one</guid></item></channel></rss>`

// guidShow serves one show, carrying a podcast:guid, at /feed.xml under any token, the
// way a private feed hands each subscriber an address of its own: each token has an ETag
// of its own and an enclosure address carrying it. When login is set it answers 401 to a
// request without that login. The path, token, login and If-None-Match of the last
// request are kept.
type guidShow struct {
	srv                              *httptest.Server
	login                            string
	mu                               sync.Mutex
	path, token, sent, ifNoneMatched string
}

func newGUIDShow(t *testing.T, login string) *guidShow {
	t.Helper()
	gs := &guidShow{login: login}
	gs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		sent := ""
		if u, p, ok := r.BasicAuth(); ok {
			sent = u + ":" + p
		}
		gs.mu.Lock()
		gs.path, gs.token, gs.ifNoneMatched, gs.sent = r.URL.Path, token, r.Header.Get("If-None-Match"), sent
		gs.mu.Unlock()
		if gs.login != "" && sent != gs.login {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/ep1.mp3" {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v-`+token+`"`)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><rss version="2.0" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Members</title><podcast:guid>members-guid</podcast:guid>
<item><title>One</title><guid>one</guid><enclosure url="`+gs.srv.URL+`/ep1.mp3?token=`+token+`" type="audio/mpeg"/></item>
</channel></rss>`)
	}))
	t.Cleanup(gs.srv.Close)
	return gs
}

func (gs *guidShow) url(token string) string { return gs.srv.URL + "/feed.xml?token=" + token }

// last reports the path, token, login and If-None-Match of the latest request.
func (gs *guidShow) last() (path, token, login, ifNoneMatch string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return gs.path, gs.token, gs.sent, gs.ifNoneMatched
}

// episode returns the show's one episode.
func episode(t *testing.T, svc *podcast.Service, show model.PID) *model.Episode {
	t.Helper()
	eps, err := svc.Episodes(context.Background(), show, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}
	return eps[0]
}

// TestReAddKeepsTheStoredAddressAndLogin: a second subscriber adding the same show at an
// address and with a login of their own changes neither, nor the validators the stored
// address answered with.
func TestReAddKeepsTheStoredAddressAndLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gs := newGUIDShow(t, "")
	svc, _, _ := newTestService(t)
	first, err := svc.Add(ctx, gs.url("alice"), podcast.AddOptions{User: "alice", Pass: "pw1"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, again := range []struct {
		url  string
		opts podcast.AddOptions
	}{
		{gs.url("bob"), podcast.AddOptions{User: "bob", Pass: "pw2"}},
		{gs.url("alice"), podcast.AddOptions{User: "alice", Pass: "changed"}},
	} {
		got, err := svc.Add(ctx, again.url, again.opts)
		if err != nil {
			t.Fatalf("re-add at %s: %v", again.url, err)
		}
		if got.PID != first.PID || got.FeedURL != gs.url("alice") || got.AuthUser != "alice" || got.ETag != `"v-alice"` {
			t.Fatalf("re-added at %s: show %s at %s as %s with %s, want %s kept at %s as alice with \"v-alice\"",
				again.url, got.PID, got.FeedURL, got.AuthUser, got.ETag, first.PID, gs.url("alice"))
		}
		if ep := episode(t, svc, first.PID); ep.EnclosureURL != gs.srv.URL+"/ep1.mp3?token=alice" {
			t.Fatalf("re-added at %s: the episode's enclosure is %s, want alice's", again.url, ep.EnclosureURL)
		}
	}
	if _, err := svc.Sync(ctx, first.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, token, login, inm := gs.last(); token != "alice" || login != "alice:pw1" || inm != `"v-alice"` {
		t.Fatalf("sync asked token %q as %q with If-None-Match %s, want alice's address, login and validator", token, login, inm)
	}
	if _, err := svc.Download(ctx, episode(t, svc, first.PID).PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	if path, token, login, _ := gs.last(); path != "/ep1.mp3" || token != "alice" || login != "alice:pw1" {
		t.Fatalf("download asked %s with token %q as %q, want alice's enclosure and login", path, token, login)
	}
}

// TestReAddOfAPrivateShowUsesItsStoredLogin: adding a private show again at its own
// address with no login, as an OPML import does, asks as the show's own sync does and
// updates it.
func TestReAddOfAPrivateShowUsesItsStoredLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gs := newGUIDShow(t, "alice:pw1")
	svc, _, _ := newTestService(t)
	first, err := svc.Add(ctx, gs.url("alice"), podcast.AddOptions{User: "alice", Pass: "pw1"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	again, err := svc.Add(ctx, gs.url("alice"), podcast.AddOptions{})
	if err != nil {
		t.Fatalf("re-add with no login: %v", err)
	}
	if again.PID != first.PID || again.AuthUser != "alice" {
		t.Fatalf("re-added show = %s as %q, want %s as alice", again.PID, again.AuthUser, first.PID)
	}
}

// TestAMoveTakesTheLoginGivenOrNone: a feed the operator moves takes the login given with
// the move, and no login when none is given, since the old one was for the old address;
// asked again at its own address, the show keeps its login.
func TestAMoveTakesTheLoginGivenOrNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gs := newGUIDShow(t, "")
	svc, _, _ := newTestService(t)
	first, err := svc.Add(ctx, gs.url("a"), podcast.AddOptions{User: "alice", Pass: "secret"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, c := range []struct {
		url       string
		opts      podcast.AddOptions
		user      string
		dropped   bool
		lastLogin string
	}{
		{gs.url("a"), podcast.AddOptions{ReplaceAddress: true}, "alice", false, "alice:secret"},
		{gs.url("b"), podcast.AddOptions{ReplaceAddress: true, User: "bob", Pass: "pw"}, "bob", false, ""},
		{gs.url("c"), podcast.AddOptions{ReplaceAddress: true}, "", true, ""},
	} {
		got, err := svc.Add(ctx, c.url, c.opts)
		if err != nil {
			t.Fatalf("move to %s: %v", c.url, err)
		}
		if got.PID != first.PID || got.FeedURL != c.url || got.AuthUser != c.user || !got.Synced || got.LoginDropped != c.dropped {
			t.Fatalf("move to %s = %s at %s as %q (synced %v, dropped %v), want the show there as %q (dropped %v)",
				c.url, got.PID, got.FeedURL, got.AuthUser, got.Synced, got.LoginDropped, c.user, c.dropped)
		}
		if _, _, login, _ := gs.last(); c.lastLogin != "" && login != c.lastLogin {
			t.Fatalf("move to %s asked as %q, want %q", c.url, login, c.lastLogin)
		}
	}
	if _, err := svc.Sync(ctx, first.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, _, login, _ := gs.last(); login != "" {
		t.Fatalf("the sync after a move with no login sent %q, want none", login)
	}
}

// TestAMoveWithARefusedLoginLeavesTheShowWhereItWas: a move whose new login the catalog
// refuses moves nothing, so the old login never reaches the new address.
func TestAMoveWithARefusedLoginLeavesTheShowWhereItWas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gs, other := newGUIDShow(t, ""), newGUIDShow(t, "")
	svc, _, _ := newTestService(t)
	first, err := svc.Add(ctx, gs.url("a"), podcast.AddOptions{User: "alice", Pass: "secret"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// A plaintext catalog refuses a value that would read as sealed.
	if _, err := svc.Add(ctx, other.url("b"), podcast.AddOptions{ReplaceAddress: true, User: "bob", Pass: "wbenc:x"}); err == nil {
		t.Fatal("a move with a refused password succeeded")
	}
	got, err := svc.Get(ctx, first.PID)
	if err != nil || got.FeedURL != gs.url("a") || got.AuthUser != "alice" {
		t.Fatalf("show after the refused move = %+v (err %v), want it at %s as alice", got, err, gs.url("a"))
	}
}

// movingFeed serves guidFeed at /feed.xml until moved is set, then answers /feed.xml with
// status, sending it to to. With validators set it tags the feed with an ETag and answers
// a request carrying it with 304.
type movingFeed struct {
	srv        *httptest.Server
	moved      atomic.Bool
	status     int
	to         string
	validators bool
}

func newMovingFeed(t *testing.T, status int, to func(base string) string) *movingFeed {
	t.Helper()
	mf := &movingFeed{status: status}
	mf.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed.xml" && mf.moved.Load() {
			http.Redirect(w, r, mf.to, mf.status)
			return
		}
		if mf.validators {
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, guidFeed)
	}))
	t.Cleanup(mf.srv.Close)
	mf.to = to(mf.srv.URL)
	return mf
}

// syncedAddress subscribes to mf, moves it, syncs and returns the show's address after.
func syncedAddress(t *testing.T, mf *movingFeed) string {
	t.Helper()
	ctx := context.Background()
	svc, st, _ := newTestService(t)
	pod, err := svc.Add(ctx, mf.srv.URL+"/feed.xml", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	mf.moved.Store(true)
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := st.PodcastByPID(ctx, pod.PID)
	if err != nil {
		t.Fatalf("read the show: %v", err)
	}
	return got.FeedURL
}

// movedTo runs a show added at from through one sync whose source says it moved to to,
// with a login, and returns the show's address and login after.
func movedTo(t *testing.T, from, to string) (addr, user string) {
	t.Helper()
	ctx := context.Background()
	calls := 0
	rss := &source.Mock{Type: model.SourceRSS, EnumerateFunc: func(context.Context, source.Request) (*source.Enumeration, error) {
		calls++
		e := &source.Enumeration{IdentityKey: "pguid:members-guid",
			Feed: &model.Feed{Title: "Members", Episodes: []model.FeedEpisode{{Title: "One", GUID: "one"}}}}
		if calls > 1 {
			e.MovedTo = to
		}
		return e, nil
	}}
	svc, st, _ := newTestService(t, rss)
	pod, err := svc.Add(ctx, from, podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := st.PodcastByPID(ctx, pod.PID)
	if err != nil {
		t.Fatalf("read the show: %v", err)
	}
	return got.FeedURL, got.AuthUser
}

// TestSyncFollowsAPermanentRedirectOnTheFeedsHost: a feed whose own origin says it moved
// for good, to a path of its own or to its https form, is stored at its new address, its
// login with it.
func TestSyncFollowsAPermanentRedirectOnTheFeedsHost(t *testing.T) {
	t.Parallel()
	mf := newMovingFeed(t, http.StatusMovedPermanently, func(base string) string { return base + "/moved.xml" })
	if got := syncedAddress(t, mf); got != mf.srv.URL+"/moved.xml" {
		t.Fatalf("address after a 301 to a new path = %s, want %s/moved.xml", got, mf.srv.URL)
	}
	if addr, user := movedTo(t, "http://feeds.example/feed.xml", "https://feeds.example/feed.xml"); addr != "https://feeds.example/feed.xml" || user != "alice" {
		t.Fatalf("after a move to https the show is at %s as %q, want the https address and alice's login", addr, user)
	}
	// A new address that answers nothing changed still takes the show.
	unchanged := newMovingFeed(t, http.StatusMovedPermanently, func(base string) string { return base + "/moved.xml" })
	unchanged.validators = true
	if got := syncedAddress(t, unchanged); got != unchanged.srv.URL+"/moved.xml" {
		t.Fatalf("address after a 301 to an unchanged feed = %s, want %s/moved.xml", got, unchanged.srv.URL)
	}
}

// TestSyncIgnoresARedirectToAnotherHost: a move to another host, another port, from
// https to http or to an address past MaxURLBytes leaves the stored address alone.
func TestSyncIgnoresARedirectToAnotherHost(t *testing.T) {
	t.Parallel()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, guidFeed)
	}))
	t.Cleanup(other.Close)
	mf := newMovingFeed(t, http.StatusMovedPermanently, func(string) string { return other.URL + "/feed.xml" })
	if got := syncedAddress(t, mf); got != mf.srv.URL+"/feed.xml" {
		t.Fatalf("address after a 301 to another host = %s, want it kept at %s/feed.xml", got, mf.srv.URL)
	}
	for _, c := range []struct{ from, to string }{
		{"https://feeds.example/feed.xml", "https://cdn.example/feed.xml"},
		{"https://feeds.example/feed.xml", "https://feeds.example:8443/feed.xml"},
		{"https://feeds.example/feed.xml", "http://feeds.example/feed.xml"},
		{"https://feeds.example/feed.xml", "https://feeds.example/" + strings.Repeat("u", podcast.MaxURLBytes)},
	} {
		if addr, _ := movedTo(t, c.from, c.to); addr != c.from {
			t.Errorf("a move from %s to %s left the show at %s, want it kept", c.from, c.to, addr)
		}
	}
}

// TestSyncIgnoresATemporaryRedirect: a feed that only redirects for now keeps its address.
func TestSyncIgnoresATemporaryRedirect(t *testing.T) {
	t.Parallel()
	mf := newMovingFeed(t, http.StatusFound, func(base string) string { return base + "/elsewhere.xml" })
	if got := syncedAddress(t, mf); got != mf.srv.URL+"/feed.xml" {
		t.Fatalf("address after a 302 = %s, want it kept at %s/feed.xml", got, mf.srv.URL)
	}
}
