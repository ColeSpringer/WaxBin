package netsafe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

func TestDoReadsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer srv.Close()

	c := New(Policy{})
	resp, err := c.Do(context.Background(), Request{URL: srv.URL, AcceptMIME: []string{"application/rss+xml"}})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(resp.Body) != "<rss/>" || resp.ETag != `"abc"` {
		t.Fatalf("body=%q etag=%q", resp.Body, resp.ETag)
	}
}

func TestMaxBytesEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	c := New(Policy{MaxBytes: 1024})
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for oversized body, got %v", err)
	}
}

func TestMIMEValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html/>"))
	}))
	defer srv.Close()

	c := New(Policy{})
	_, err := c.Do(context.Background(), Request{URL: srv.URL, AcceptMIME: []string{"audio/*"}})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for MIME mismatch, got %v", err)
	}
}

func TestRedirectLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	c := New(Policy{MaxRedirects: 3})
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err == nil {
		t.Fatal("want error on redirect loop")
	}
}

func TestConditionalGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	c := New(Policy{})
	resp, err := c.Do(context.Background(), Request{URL: srv.URL, IfNoneMatch: `"v1"`})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.NotModified {
		t.Fatal("want NotModified for matching ETag")
	}
}

func TestSSRFBlocksLoopback(t *testing.T) {
	// httptest binds to loopback, which the SSRF guard refuses when enabled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	c := New(Policy{BlockPrivateIPs: true})
	if _, err := c.Do(context.Background(), Request{URL: srv.URL}); err == nil {
		t.Fatal("want error connecting to loopback with SSRF guard on")
	}

	// With the guard off the same request succeeds.
	c2 := New(Policy{})
	if _, err := c2.Do(context.Background(), Request{URL: srv.URL}); err != nil {
		t.Fatalf("guard off should allow loopback: %v", err)
	}
}

// TestIsBlockedIPTable: the guard refuses every non-public range, judges an IPv6 address
// that embeds an IPv4 one by the IPv4 address too, and lets public addresses through.
func TestIsBlockedIPTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr    string
		blocked bool
		form    string
	}{
		{"127.0.0.1", true, "loopback"},
		{"10.1.2.3", true, "private"},
		{"172.16.0.1", true, "private"},
		{"192.168.1.1", true, "private"},
		{"169.254.169.254", true, "link-local, the cloud metadata address"},
		{"0.0.0.0", true, "unspecified"},
		{"0.1.2.3", true, "this network"},
		{"100.100.100.200", true, "CGNAT"},
		{"192.0.0.8", true, "IETF protocol assignments"},
		{"240.0.0.1", true, "reserved"},
		{"255.255.255.255", true, "broadcast"},
		{"198.18.0.1", true, "benchmarking"},
		{"198.19.255.255", true, "benchmarking"},
		{"192.0.2.1", true, "TEST-NET-1"},
		{"198.51.100.1", true, "TEST-NET-2"},
		{"203.0.113.1", true, "TEST-NET-3"},
		{"2001:2::1", true, "IPv6 benchmarking"},
		{"3fff::1", true, "IPv6 documentation"},
		{"100::1", true, "IPv6 discard"},
		{"224.0.0.1", true, "link-local multicast"},
		{"239.1.2.3", true, "admin-scoped multicast"},
		{"233.252.0.1", true, "multicast"},
		{"::1", true, "IPv6 loopback"},
		{"::", true, "IPv6 unspecified"},
		{"fd00::1", true, "ULA"},
		{"fe80::1%eth0", true, "IPv6 link-local with a zone"},
		{"fec0::1%eth0", true, "site-local with a zone"},
		{"2001:db8::1%eth0", true, "documentation with a zone"},
		{"64:ff9b:1::1%eth0", true, "local-use NAT64 with a zone"},
		{"100::1%eth0", true, "discard with a zone"},
		{"3fff::1%eth0", true, "documentation with a zone"},
		{"2001:2::1%eth0", true, "benchmarking with a zone"},
		{"fec0::1", true, "site-local"},
		{"2001:db8::1", true, "documentation"},
		{"ff02::1", true, "link-local multicast"},
		{"ff05::2", true, "site-local multicast"},
		{"ff0e::1", true, "global multicast"},
		{"::ffff:127.0.0.1", true, "IPv4-mapped loopback"},
		{"64:ff9b::7f00:1", true, "NAT64 of loopback"},
		{"64:ff9b::a9fe:a9fe", true, "NAT64 of the metadata address"},
		{"64:ff9b:1::808:808", true, "local-use NAT64, whatever it embeds"},
		{"2002:7f00:1::1", true, "6to4 of loopback"},
		{"2002:a00:1:1::1", true, "6to4 of a private address"},
		{"2001:0:7f00:1::1", true, "Teredo whose server is loopback"},
		{"2001:0:808:808::80ff:fffe", true, "Teredo whose client is loopback"},
		{"2600::5efe:a00:1", true, "ISATAP of a private address"},
		{"2600::200:5efe:7f00:1", true, "ISATAP, universal, of loopback"},
		{"::a00:1", true, "IPv4-compatible private"},
		{"::ffff:0:a00:1", true, "IPv4-translated private"},
		{"8.8.8.8", false, "public"},
		{"93.184.216.34", false, "public"},
		{"2606:4700:4700::1111", false, "public IPv6"},
		{"64:ff9b::808:808", false, "NAT64 of a public address"},
		{"2002:808:808::1", false, "6to4 of a public address"},
		{"2001:0:4136:e378:8000:63bf:f7f7:fbfb", false, "Teredo between public addresses"},
		{"2600::5efe:808:808", false, "ISATAP of a public address"},
		{"::ffff:8.8.8.8", false, "IPv4-mapped public"},
	} {
		if got := isBlockedIP(netip.MustParseAddr(tc.addr)); got != tc.blocked {
			t.Errorf("%s (%s): blocked = %v, want %v", tc.addr, tc.form, got, tc.blocked)
		}
	}
}

// TestUntypedAnswerPassesTheAllowList: an answer that declares no type is not refused by
// an allow-list; many feeds omit the header.
func TestUntypedAnswerPassesTheAllowList(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suppress Go's content sniffing so the response carries no Content-Type.
		w.Header()["Content-Type"] = []string{}
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer srv.Close()
	if _, err := New(Policy{}).Do(context.Background(), Request{URL: srv.URL, AcceptMIME: []string{"application/rss+xml"}}); err != nil {
		t.Fatalf("an untyped answer = %v, want it accepted", err)
	}
}

func TestSchemeRejected(t *testing.T) {
	c := New(Policy{})
	if _, err := c.Do(context.Background(), Request{URL: "file:///etc/passwd"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for file scheme, got %v", err)
	}
}

func TestSafeFilename(t *testing.T) {
	cases := []struct{ in, fallback, want string }{
		{"https://h/path/ep-12.mp3?token=secret", "fb", "ep-12.mp3"},
		{"https://h/", "fallback.mp3", "fallback.mp3"},
		{"../../etc/passwd", "fb", "passwd"},
		{"https://h/a/b/..", "fb", "fb"}, // ".." trims to empty -> fallback
		{`weird\name.mp3`, "fb", "name.mp3"},
		{`https://h/ep:1*2<x>.mp3`, "fb", "ep12x.mp3"}, // Windows-reserved chars dropped
	}
	for _, c := range cases {
		if got := SafeFilename(c.in, c.fallback); got != c.want {
			t.Errorf("SafeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
		// The result must be safe to create on any platform (no separators or
		// Windows-reserved characters).
		if strings.ContainsAny(SafeFilename(c.in, c.fallback), `/\:*?"<>|`) {
			t.Errorf("SafeFilename(%q) leaked an unsafe character", c.in)
		}
	}
}

// TestTooLargeTellsAnOversizedBody: a body over the cap is an answer about the resource
// itself, so it reads apart from a media type the caller refused and from a status, both
// of which can come from a service that is not answering, while every one keeps its
// class and its message.
func TestTooLargeTellsAnOversizedBody(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, 64))
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c := New(Policy{})

	_, err := c.Do(ctx, Request{URL: srv.URL + "/big", MaxBytes: 16})
	var tooLarge *TooLargeError
	if !TooLarge(err) || !waxerr.Is(err, waxerr.CodeInvalid) || !errors.As(err, &tooLarge) || tooLarge.Limit != 16 ||
		err.Error() != "netsafe.read: response exceeds 16-byte limit" {
		t.Errorf("oversized Do = %v (too large %v), want a CodeInvalid TooLarge with its message", err, TooLarge(err))
	}
	_, _, err = c.Stream(ctx, Request{URL: srv.URL + "/big?token=t"}, io.Discard, 16)
	if want := "netsafe.Stream: response from " + srv.URL + " exceeds 16-byte limit"; !TooLarge(err) || err.Error() != want {
		t.Errorf("oversized Stream = %v (too large %v), want a TooLarge reading %q", err, TooLarge(err), want)
	}
	_, err = c.Do(ctx, Request{URL: srv.URL + "/page", AcceptMIME: []string{"image/*"}})
	if TooLarge(err) || !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("wrong media type = %v (too large %v), want a CodeInvalid that is not TooLarge", err, TooLarge(err))
	}
	_, err = c.Do(ctx, Request{URL: srv.URL + "/gone?token=t"})
	var status *StatusError
	if TooLarge(err) || !waxerr.Is(err, waxerr.CodeIO) || !errors.As(err, &status) || status.Status != http.StatusForbidden ||
		err.Error() != "netsafe.Do: "+srv.URL+" returned HTTP 403" {
		t.Errorf("refused status = %v (too large %v), want a CodeIO status error naming the host", err, TooLarge(err))
	}
}

// TestFailuresCarryTheirReason: every way a fetch fails names its reason beside its class,
// with a typed cause that holds the reason's data.
func TestFailuresCarryTheirReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html/>"))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/stall":
			stall(w, r)
		default:
			_, _ = w.Write(make([]byte, 64))
		}
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	c := New(Policy{MaxRedirects: 3, Timeout: 500 * time.Millisecond})
	do := func(c *Client, r Request) error { _, err := c.Do(ctx, r); return err }
	_, _, streamErr := c.Stream(ctx, Request{URL: srv.URL + "/big"}, io.Discard, 16)

	var status *StatusError
	var media *MediaTypeError
	var blocked *BlockedAddressError
	var hops *RedirectLimitError
	for _, tc := range []struct {
		name   string
		err    error
		code   waxerr.Code
		reason waxerr.Reason
		cause  func(error) bool
	}{
		{"a 404", do(c, Request{URL: srv.URL + "/missing"}), waxerr.CodeNotFound, ReasonHTTPStatus,
			func(err error) bool { return errors.As(err, &status) && status.Status == http.StatusNotFound }},
		{"an oversized body", do(c, Request{URL: srv.URL + "/big", MaxBytes: 16}), waxerr.CodeInvalid, ReasonTooLarge, TooLarge},
		{"an oversized stream", streamErr, waxerr.CodeInvalid, ReasonTooLarge, TooLarge},
		{"a refused type", do(c, Request{URL: srv.URL + "/page", AcceptMIME: []string{"audio/*"}}), waxerr.CodeInvalid, ReasonMediaType,
			func(err error) bool { return errors.As(err, &media) && media.Got == "text/html" }},
		{"a redirect loop", do(c, Request{URL: srv.URL + "/loop"}), waxerr.CodeIO, ReasonRedirect,
			func(err error) bool { return errors.As(err, &hops) }},
		{"a redirect that would drop a body", do(c, Request{URL: srv.URL + "/moved", Body: []byte("x=1")}), waxerr.CodeIO, ReasonRedirect,
			func(err error) bool { return !errors.As(err, &hops) }},
		{"a body that stalls", do(c, Request{URL: srv.URL + "/stall"}), waxerr.CodeIO, ReasonTransport,
			func(error) bool { return true }},
		{"a guarded address", do(New(Policy{BlockPrivateIPs: true}), Request{URL: srv.URL}), waxerr.CodeIO, ReasonBlockedAddress,
			func(err error) bool { return errors.As(err, &blocked) && blocked.Addr == "127.0.0.1" }},
		{"nothing listening", do(c, Request{URL: closed.URL}), waxerr.CodeIO, ReasonTransport,
			func(error) bool { return true }},
	} {
		if !waxerr.Is(tc.err, tc.code) || waxerr.ReasonOf(tc.err) != tc.reason || !tc.cause(tc.err) {
			t.Errorf("%s = %v (%s %q), want %s %q with its cause", tc.name, tc.err, waxerr.CodeOf(tc.err), waxerr.ReasonOf(tc.err), tc.code, tc.reason)
		}
	}
}

// TestNetsafeErrorsNameTheHostOnly: a URL's login, path and query can carry a token, so no
// error says more of it than its scheme and host.
func TestNetsafeErrorsNameTheHostOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/big") {
			_, _ = w.Write(make([]byte, 64))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	secret := func(base, path string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("alice", "pa55word")
		u.Path = path + "/sekrit"
		u.RawQuery = "token=T0KEN"
		return u.String()
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := "https://cdn.example/sekrit/ep%zz.mp3?token=T0KEN"
		if r.URL.Path == "/relative" {
			loc = "/sekrit/ep%zz.mp3?token=T0KEN"
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}))
	defer broken.Close()
	open, guarded := New(Policy{}), New(Policy{BlockPrivateIPs: true})
	do := func(c *Client, raw string) error { _, err := c.Do(ctx, Request{URL: raw}); return err }
	_, _, oversized := open.Stream(ctx, Request{URL: secret(srv.URL, "/big")}, io.Discard, 16)
	for name, err := range map[string]error{
		"a refused dial":                          do(guarded, secret(srv.URL, "/feed")),
		"nothing listening":                       do(open, secret(closed.URL, "/feed")),
		"a 403":                                   do(open, secret(srv.URL, "/feed")),
		"an oversized body":                       oversized,
		"a bad scheme":                            do(open, "ftp://alice:pa55word@host/sekrit?token=T0KEN"),
		"no scheme":                               do(open, "alice:pa55word@host/sekrit?token=T0KEN"),
		"a url that will not parse":               do(open, "http://alice:pa55word@host:sekrit/x?token=T0KEN"),
		"a url nested after no scheme":            do(open, "alice:pa55word@host/sekrit?token=T0KEN&return=https://x"),
		"a redirect that will not parse":          do(open, broken.URL),
		"a relative redirect that will not parse": do(open, broken.URL+"/relative"),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		for _, leak := range []string{"alice", "pa55word", "sekrit", "T0KEN"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: %q names %q", name, err, leak)
			}
		}
	}
}

// TestOneHostPacesOnce: requests are paced per host, whatever login, port or scheme a
// URL names.
func TestOneHostPacesOnce(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"https://alice:pw@Feeds.Example:8443/a?x=1", "https://feeds.example/b", "http://feeds.example/c",
		"http://feeds.example:80/d"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := paceKey(u); got != "feeds.example" {
			t.Errorf("paceKey(%s) = %q, want feeds.example", raw, got)
		}
	}
}

// stall answers with the head of a body and then sends nothing more until the client
// goes away.
func stall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	_, _ = w.Write(make([]byte, 64))
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// TestErrorsNameTheHostThatAnswered: after a redirect, an error names the host that
// refused or overran, not the one first asked.
func TestErrorsNameTheHostThatAnswered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write(make([]byte, 64))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer cdn.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			http.Redirect(w, r, closed.URL+"/ep.mp3", http.StatusFound)
		default:
			http.Redirect(w, r, cdn.URL+r.URL.Path, http.StatusFound)
		}
	}))
	defer feed.Close()
	c := New(Policy{})
	_, refused := c.Do(ctx, Request{URL: feed.URL + "/ep.mp3"})
	_, _, overran := c.Stream(ctx, Request{URL: feed.URL + "/big"}, io.Discard, 16)
	_, unreached := c.Do(ctx, Request{URL: feed.URL + "/gone"})
	for name, tc := range map[string]struct {
		err  error
		host string
	}{
		"a refused status":   {refused, cdn.URL},
		"an oversized body":  {overran, cdn.URL},
		"an unreachable hop": {unreached, closed.URL},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.host) || strings.Contains(tc.err.Error(), feed.URL) {
			t.Errorf("%s = %v, want it to name %s and not %s", name, tc.err, tc.host, feed.URL)
		}
	}
}

// TestACancelMidBodyIsACancel: a caller that gives up while the body streams gets a
// cancel, not a transport failure.
func TestACancelMidBodyIsACancel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(stall))
	defer srv.Close()
	c := New(Policy{})
	for name, fetch := range map[string]func(context.Context) error{
		"Do": func(ctx context.Context) error { _, err := c.Do(ctx, Request{URL: srv.URL}); return err },
		"Stream": func(ctx context.Context) error {
			_, _, err := c.Stream(ctx, Request{URL: srv.URL}, io.Discard, 0)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err := fetch(ctx)
		cancel()
		if !waxerr.Is(err, waxerr.CodeCanceled) || waxerr.ReasonOf(err) != "" {
			t.Errorf("%s = %v (%s %q), want a cancel with no reason", name, err, waxerr.CodeOf(err), waxerr.ReasonOf(err))
		}
	}
}

// TestStreamOutlastsItsTimeout: a download that keeps arriving is not cut off at the
// policy's timeout, which bounds a stream's silences rather than its length.
func TestStreamOutlastsItsTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		for range 12 {
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer srv.Close()
	_, n, err := New(Policy{Timeout: time.Second}).Stream(context.Background(), Request{URL: srv.URL}, io.Discard, 0)
	if err != nil || n != 12*1024 {
		t.Fatalf("stream = %d bytes (err %v), want all 12 KiB", n, err)
	}
}

// TestStreamFailsWhenItGoesQuiet: a stream whose body stops, or whose answer never
// comes, fails within the timeout as a transport failure.
func TestStreamFailsWhenItGoesQuiet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/silent" {
			<-r.Context().Done()
			return
		}
		stall(w, r)
	}))
	defer srv.Close()
	c := New(Policy{Timeout: 500 * time.Millisecond})
	for _, path := range []string{"/stall", "/silent"} {
		start := time.Now()
		_, _, err := c.Stream(context.Background(), Request{URL: srv.URL + path}, io.Discard, 0)
		if !waxerr.Is(err, waxerr.CodeIO) || waxerr.ReasonOf(err) != ReasonTransport || time.Since(start) > 10*time.Second {
			t.Errorf("%s = %v (%s %q) after %s, want a transport failure near the timeout", path, err, waxerr.CodeOf(err),
				waxerr.ReasonOf(err), time.Since(start))
		}
	}
}

// eofWithData returns its data and an error from the same Read, as a body cut off
// mid-stream can.
type eofWithData struct{ done bool }

func (e *eofWithData) Read(p []byte) (int, error) {
	if e.done {
		return 0, io.EOF
	}
	e.done = true
	return copy(p, "<html>"), io.ErrUnexpectedEOF
}

type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) {
	return 0, waxerr.New(waxerr.CodeInvalid, "sniff", "not media").WithReason(ReasonMediaType)
}

// TestCopyBlamesTheWriterThatRefused: when one step both reads to a cut-off and writes
// to a writer that refuses, the writer's refusal is what comes back; a read failure
// alone is the transport's.
func TestCopyBlamesTheWriterThatRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := copyBody(ctx, "op", refusingWriter{}, &eofWithData{}, 1024, nil); waxerr.ReasonOf(err) != ReasonMediaType {
		t.Errorf("both failing = %v (%q), want the writer's media_type", err, waxerr.ReasonOf(err))
	}
	if _, err := copyBody(ctx, "op", io.Discard, &eofWithData{}, 1024, nil); waxerr.ReasonOf(err) != ReasonTransport {
		t.Errorf("a cut-off read = %v (%q), want transport", err, waxerr.ReasonOf(err))
	}
}

// TestAStatusFromOutsideTheLoginsOriginSaysSo: a refusal from a host the login was not
// sent to says where the login goes, and one from the login's own origin does not.
func TestAStatusFromOutsideTheLoginsOriginSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved.xml" {
			http.Redirect(w, r, other.URL+"/feed.xml", http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer feed.Close()
	c := New(Policy{})
	var status *StatusError
	_, err := c.Do(ctx, Request{URL: feed.URL + "/moved.xml", Credentials: loginFor(feed.URL + "/moved.xml")})
	if !errors.As(err, &status) || status.LoginOrigin != feed.URL || !strings.Contains(err.Error(), "login") {
		t.Errorf("a 401 off the login's origin = %v (login origin %q), want it to say the login goes to %s", err, status.LoginOrigin, feed.URL)
	}
	status = nil
	_, err = c.Do(ctx, Request{URL: feed.URL + "/feed.xml", Credentials: loginFor(feed.URL + "/feed.xml")})
	if !errors.As(err, &status) || status.LoginOrigin != "" {
		t.Errorf("a 401 at the login's origin = %v, want no word on where the login goes", err)
	}
}

// TestAContentTypeListIsJudgedByEveryType: a header that lists several types is refused
// when any of them is, and judged by its first against the allow-list.
func TestAContentTypeListIsJudgedByEveryType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", r.URL.Query().Get("t"))
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	c := New(Policy{})
	for ctype, refused := range map[string]bool{
		"audio/x-mpegurl,audio/mpeg":  true,
		"audio/mpeg, audio/x-mpegurl": true,
		"text/html, audio/mpeg":       true,
		"audio/mpeg, charset=utf-8":   false,
		"audio/mpeg; charset=utf-8":   false,
	} {
		_, err := c.Do(ctx, Request{URL: srv.URL + "/?t=" + url.QueryEscape(ctype), AcceptMIME: []string{"audio/*"},
			RefuseMIME: []string{"audio/x-mpegurl"}})
		if got := waxerr.ReasonOf(err) == ReasonMediaType; got != refused {
			t.Errorf("%q = %v, want refused %v", ctype, err, refused)
		}
	}
}

// TestStreamDoesNotCountASlowWriteAsSilence: time a download spends writing to its own
// destination is not time the source was quiet.
func TestStreamDoesNotCountASlowWriteAsSilence(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		for range 4 {
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	dst := &slowWriter{pause: 600 * time.Millisecond}
	_, n, err := New(Policy{Timeout: 300 * time.Millisecond}).Stream(context.Background(), Request{URL: srv.URL}, dst, 0)
	if err != nil || n != 4096 {
		t.Fatalf("stream = %d bytes (err %v), want all 4 KiB past a slow write", n, err)
	}
}

// slowWriter takes pause over its first write, as a busy disk can.
type slowWriter struct {
	pause time.Duration
	done  bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.done {
		w.done = true
		time.Sleep(w.pause)
	}
	return len(p), nil
}

// TestStreamDoesNotCountThePaceAsSilence: waiting out the host's pace is not time the
// source was quiet.
func TestStreamDoesNotCountThePaceAsSilence(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(make([]byte, 64))
	}))
	defer srv.Close()
	c := New(Policy{Timeout: 300 * time.Millisecond, MinHostInterval: 800 * time.Millisecond})
	for i := range 2 {
		if _, _, err := c.Stream(context.Background(), Request{URL: srv.URL}, io.Discard, 0); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
}

// TestAQuietRedirectHopIsNamed: a stream whose redirect target never answers names that
// target, not the host first asked.
func TestAQuietRedirectHopIsNamed(t *testing.T) {
	t.Parallel()
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer silent.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, silent.URL+"/ep.mp3", http.StatusFound)
	}))
	defer feed.Close()
	_, _, err := New(Policy{Timeout: 300 * time.Millisecond}).Stream(context.Background(), Request{URL: feed.URL + "/ep.mp3"}, io.Discard, 0)
	if err == nil || !strings.Contains(err.Error(), silent.URL) || strings.Contains(err.Error(), feed.URL) {
		t.Fatalf("stream = %v, want it to name %s", err, silent.URL)
	}
}

// TestUnansweredShowsNoURL: whatever Go puts in a failure's text, a URL in it is cut to
// its scheme and host, and a failure with no host to name still reads cleanly.
func TestUnansweredShowsNoURL(t *testing.T) {
	t.Parallel()
	inner := errors.New(`proxy refused "https://alice:pw@cdn.example/sekrit/ep.mp3?token=T0KEN" for now`)
	err := unanswered("netsafe.Do", "https://feeds.example/feed.xml", &url.Error{Op: "Get", URL: "https://feeds.example/feed.xml", Err: inner})
	for _, leak := range []string{"alice", "sekrit", "T0KEN"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("%q names %q", err, leak)
		}
	}
	if !strings.Contains(err.Error(), "https://cdn.example") {
		t.Errorf("%q, want the host it named kept", err)
	}
	bare := unanswered("netsafe.Do", "", &url.Error{Op: "Get", Err: errors.New("http: no Host in request URL")})
	if got := bare.Error(); got != "netsafe.Do: fetching: http: no Host in request URL" {
		t.Errorf("no host = %q", got)
	}
}

// TestAnUnreadableDialAddressIsBlocked: an address the guard cannot read is refused as a
// blocked address, which no retry gets past, not as a transport failure.
func TestAnUnreadableDialAddressIsBlocked(t *testing.T) {
	t.Parallel()
	var blocked *BlockedAddressError
	err := guardDialAddr("not-an-address:80")
	if !errors.As(err, &blocked) {
		t.Fatalf("guard = %v, want a BlockedAddressError", err)
	}
	if got := waxerr.ReasonOf(unanswered("netsafe.Do", "https://feeds.example/", &url.Error{Op: "Get", Err: err})); got != ReasonBlockedAddress {
		t.Errorf("reason = %q, want blocked_address", got)
	}
}
