package netsafe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestRequireContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suppress Go's content sniffing so the response carries no Content-Type.
		w.Header()["Content-Type"] = []string{}
		_, _ = w.Write([]byte("<html>error</html>"))
	}))
	defer srv.Close()

	c := New(Policy{})
	// Without the requirement an empty content type is accepted (lenient, for feeds).
	if _, err := c.Do(context.Background(), Request{URL: srv.URL, AcceptMIME: []string{"audio/*"}}); err != nil {
		t.Fatalf("empty CT should be accepted without RequireContentType: %v", err)
	}
	// With the requirement (enclosure downloads) an untyped body is rejected.
	_, err := c.Do(context.Background(), Request{URL: srv.URL, AcceptMIME: []string{"audio/*"}, RequireContentType: true})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for missing content type, got %v", err)
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
	if !TooLarge(err) || !waxerr.Is(err, waxerr.CodeInvalid) || err.Error() != "netsafe.read: response exceeds 16-byte limit" {
		t.Errorf("oversized Do = %v (too large %v), want a CodeInvalid TooLarge with its message", err, TooLarge(err))
	}
	_, _, err = c.Stream(ctx, Request{URL: srv.URL + "/big"}, io.Discard, 16)
	if want := "netsafe.Stream: response from " + srv.URL + "/big exceeds 16-byte limit"; !TooLarge(err) || err.Error() != want {
		t.Errorf("oversized Stream = %v (too large %v), want a TooLarge reading %q", err, TooLarge(err), want)
	}
	_, err = c.Do(ctx, Request{URL: srv.URL + "/page", AcceptMIME: []string{"image/*"}})
	if TooLarge(err) || !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("wrong media type = %v (too large %v), want a CodeInvalid that is not TooLarge", err, TooLarge(err))
	}
	_, err = c.Do(ctx, Request{URL: srv.URL + "/gone"})
	if TooLarge(err) || !waxerr.Is(err, waxerr.CodeIO) || err.Error() != "netsafe.Do: "+srv.URL+"/gone returned HTTP 403" {
		t.Errorf("refused status = %v (too large %v), want a CodeIO status error", err, TooLarge(err))
	}
}
