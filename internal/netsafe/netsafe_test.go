package netsafe

import (
	"context"
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
