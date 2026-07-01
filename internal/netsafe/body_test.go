package netsafe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoSendsPostBody(t *testing.T) {
	var method, body, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		ctype = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(Policy{})
	// A Body with no explicit Method defaults to POST.
	resp, err := c.Do(context.Background(), Request{
		URL:         srv.URL,
		Body:        []byte("a=1&b=2"),
		ContentType: "application/x-www-form-urlencoded",
		AcceptMIME:  []string{"application/json"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if body != "a=1&b=2" {
		t.Errorf("body = %q, want a=1&b=2", body)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", ctype)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d", resp.Status)
	}
}

func TestDoRefusesPostToGetRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer final.Close()
	// A 302 on a POST would downgrade to a body-less GET; netsafe must refuse it.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redir.Close()

	c := New(Policy{})
	_, err := c.Do(context.Background(), Request{
		URL:         redir.URL,
		Body:        []byte("x=1"),
		ContentType: "application/x-www-form-urlencoded",
	})
	if err == nil {
		t.Fatal("expected refusal of a POST->GET redirect for a body-carrying request")
	}
}

func TestDoStillFollowsGetRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("landed"))
	}))
	defer final.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redir.Close()

	c := New(Policy{})
	resp, err := c.Do(context.Background(), Request{URL: redir.URL})
	if err != nil {
		t.Fatalf("a GET redirect must still be followed: %v", err)
	}
	if string(resp.Body) != "landed" {
		t.Errorf("body = %q, want landed", resp.Body)
	}
}
