package netsafe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// hop is one request a test server saw.
type hop struct {
	server, path string
	login        bool // carried the test login
	referer      string
}

// hops records the requests test servers see, in order.
type hops struct {
	mu   sync.Mutex
	seen []hop
}

func (h *hops) record(server string, r *http.Request) {
	u, p, ok := r.BasicAuth()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, hop{server: server, path: r.URL.Path, login: ok && u == "alice" && p == "pw", referer: r.Header.Get("Referer")})
}

func (h *hops) list() []hop {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen)
}

func loginFor(origin string) *Credentials {
	return &Credentials{Origin: origin, User: "alice", Pass: "pw"}
}

// routed sends c's dial to each address in routes to the local server it names, so a
// test can speak to example.com on the default ports, and trusts the certificate of
// secure, which httptest issues for example.com.
func routed(c *Client, routes map[string]string, secure *httptest.Server) {
	tr := c.http.Transport.(*http.Transport)
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := routes[addr]
		if !ok {
			return nil, fmt.Errorf("no route to %s", addr)
		}
		return d.DialContext(ctx, network, to)
	}
	if secure != nil {
		roots := x509.NewCertPool()
		roots.AddCert(secure.Certificate())
		tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
}

// feedCDNChain fetches an enclosure on the feed's host that redirects to a CDN, which
// redirects back to the feed's host, with a login scoped to the feed, and returns the
// hops the two servers saw.
func feedCDNChain(t *testing.T) []hop {
	t.Helper()
	rec := &hops{}
	var feedURL string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("cdn", r)
		http.Redirect(w, r, feedURL+"/final.mp3", http.StatusFound)
	}))
	t.Cleanup(cdn.Close)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("feed", r)
		if r.URL.Path == "/ep.mp3" {
			http.Redirect(w, r, cdn.URL+"/ep.mp3?sig=s", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("audio"))
	}))
	t.Cleanup(feed.Close)
	feedURL = feed.URL
	req := Request{URL: feed.URL + "/ep.mp3?token=t", Credentials: loginFor(feed.URL + "/feed.xml")}
	if _, err := New(Policy{}).Do(context.Background(), req); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return rec.list()
}

// TestCredentialsFollowOnlyTheFeedOrigin: a login scoped to the feed goes on the first
// request and on the hop back to the feed's host, and never to the CDN between them,
// though both servers share an address and Go's own rule would copy it there.
func TestCredentialsFollowOnlyTheFeedOrigin(t *testing.T) {
	t.Parallel()
	got := feedCDNChain(t)
	want := []hop{{server: "feed", path: "/ep.mp3", login: true}, {server: "cdn", path: "/ep.mp3"}, {server: "feed", path: "/final.mp3", login: true}}
	if len(got) != len(want) {
		t.Fatalf("hops = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].server != want[i].server || got[i].path != want[i].path || got[i].login != want[i].login {
			t.Errorf("hop %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestNoRefererAcrossHops: no hop of a redirect chain tells its target the URL it came
// from, whose query can carry a token.
func TestNoRefererAcrossHops(t *testing.T) {
	t.Parallel()
	for i, h := range feedCDNChain(t) {
		if h.referer != "" {
			t.Errorf("hop %d to %s sent Referer %q", i, h.server, h.referer)
		}
	}
}

// TestCredentialsFollowAnHTTPSUpgrade: an http feed that moves to https on the same host
// at the default ports keeps its login, and the answer says the move was permanent.
func TestCredentialsFollowAnHTTPSUpgrade(t *testing.T) {
	t.Parallel()
	rec := &hops{}
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("https", r)
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer secure.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("http", r)
		http.Redirect(w, r, "https://example.com/feed.xml", http.StatusMovedPermanently)
	}))
	defer plain.Close()
	c := New(Policy{})
	routed(c, map[string]string{"example.com:80": plain.Listener.Addr().String(), "example.com:443": secure.Listener.Addr().String()}, secure)
	resp, err := c.Do(context.Background(), Request{URL: "http://example.com/feed.xml", Credentials: loginFor("http://example.com/feed.xml")})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rec.list(); len(got) != 2 || !got[0].login || !got[1].login {
		t.Errorf("hops = %+v, want the login on both", got)
	}
	if !resp.Permanent || resp.FinalURL != "https://example.com/feed.xml" {
		t.Errorf("answer = permanent %v at %s, want a permanent move to https", resp.Permanent, resp.FinalURL)
	}
}

// TestCredentialsDropOnADowngradeToHTTP: an https origin's login never goes to the same
// host over plain http.
func TestCredentialsDropOnADowngradeToHTTP(t *testing.T) {
	t.Parallel()
	rec := &hops{}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("http", r)
		_, _ = w.Write([]byte("audio"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("https", r)
		http.Redirect(w, r, "http://example.com/ep.mp3", http.StatusFound)
	}))
	defer secure.Close()
	c := New(Policy{})
	routed(c, map[string]string{"example.com:80": plain.Listener.Addr().String(), "example.com:443": secure.Listener.Addr().String()}, secure)
	if _, err := c.Do(context.Background(), Request{URL: "https://example.com/ep.mp3", Credentials: loginFor("https://example.com/feed.xml")}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rec.list(); len(got) != 2 || !got[0].login || got[1].login {
		t.Errorf("hops = %+v, want the login on https only", got)
	}
}

// TestCredentialsDropOnPortChange: another port on the same host is another origin.
func TestCredentialsDropOnPortChange(t *testing.T) {
	t.Parallel()
	rec := &hops{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("8080", r)
		_, _ = w.Write([]byte("audio"))
	}))
	defer other.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("80", r)
		http.Redirect(w, r, "http://example.com:8080/ep.mp3", http.StatusFound)
	}))
	defer feed.Close()
	c := New(Policy{})
	routed(c, map[string]string{"example.com:80": feed.Listener.Addr().String(), "example.com:8080": other.Listener.Addr().String()}, nil)
	if _, err := c.Do(context.Background(), Request{URL: "http://example.com/ep.mp3", Credentials: loginFor("http://example.com/feed.xml")}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rec.list(); len(got) != 2 || !got[0].login || got[1].login {
		t.Errorf("hops = %+v, want the login on port 80 only", got)
	}
}

// TestOriginRule: the origin a login is scoped to is a URL's scheme, its host lowercased
// as given and its port, the http form admitting its https upgrade at the default ports.
func TestOriginRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		origin, hop string
		admits      bool
	}{
		{"http://feeds.example/feed.xml", "http://feeds.example/ep.mp3", true},
		{"http://feeds.example/feed.xml", "http://Feeds.Example:80/ep.mp3", true},
		{"http://feeds.example/feed.xml", "https://feeds.example/ep.mp3", true},
		{"https://feeds.example/feed.xml", "https://feeds.example:443/ep.mp3", true},
		{"https://feeds.example/feed.xml", "http://feeds.example/ep.mp3", false},
		{"http://feeds.example:8080/feed.xml", "https://feeds.example/ep.mp3", false},
		{"http://feeds.example/feed.xml", "https://feeds.example:8443/ep.mp3", false},
		{"http://feeds.example/feed.xml", "http://cdn.feeds.example/ep.mp3", false},
		{"http://xn--bcher-kva.example/feed.xml", "http://bücher.example/ep.mp3", false},
		{"http://[2001:db8::1]/feed.xml", "http://[2001:db8::1]:80/ep.mp3", true},
		{"manual:01ABCDEF", "http://feeds.example/ep.mp3", false},
		{"", "http://feeds.example/ep.mp3", false},
		{"http://feeds.example/feed.xml", "ftp://feeds.example/ep.mp3", false},
		{"http://feeds.example/feed.xml", "http://feeds.example:bad/ep.mp3", false},
	} {
		if got := WithinOrigin(tc.origin, tc.hop); got != tc.admits {
			t.Errorf("%s to %s = %v, want %v", tc.origin, tc.hop, got, tc.admits)
		}
	}
}

// TestResponseSaysWhetherTheChainWasPermanent: an answer reached only through 301s and
// 308s says so; one temporary hop, or no redirect at all, does not.
func TestResponseSaysWhetherTheChainWasPermanent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusMovedPermanently)
		case "/b":
			http.Redirect(w, r, "/c", http.StatusFound)
		case "/x":
			http.Redirect(w, r, "/y", http.StatusMovedPermanently)
		case "/y":
			http.Redirect(w, r, "/z", http.StatusPermanentRedirect)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	c := New(Policy{})
	for path, want := range map[string]bool{"/a": false, "/x": true, "/z": false} {
		resp, err := c.Do(context.Background(), Request{URL: srv.URL + path})
		if err != nil || resp.Permanent != want {
			t.Errorf("%s: permanent = %v (err %v), want %v", path, resp != nil && resp.Permanent, err, want)
		}
	}
}
