package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/waxerr"
)

// TestAcoustIDLookupPostsFormBody verifies the AcoustID lookup is a POST with the
// parameters in the body (not a giant GET URL), that the compressed fingerprint
// round-trips through form encoding intact, and that meta is space-separated on the
// wire (decoded from the "+" url.Values produces) rather than a literal "+".
func TestAcoustIDLookupPostsFormBody(t *testing.T) {
	var method, meta, fp, client, format string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = r.ParseForm()
		meta = r.PostFormValue("meta")
		fp = r.PostFormValue("fingerprint")
		client = r.PostFormValue("client")
		format = r.PostFormValue("format")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[{"id":"aid","score":0.95,
			"recordings":[{"id":"rec-mbid","releasegroups":[{"id":"rg-mbid"}]}]}]}`))
	}))
	defer srv.Close()

	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "testkey"}
	// A fingerprint containing base64 specials (+ / =) must survive form encoding.
	m, err := a.lookup(context.Background(), "AQADtMk+/=abc", 240)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if meta != "recordingids releasegroupids" {
		t.Errorf("meta = %q, want space-separated %q", meta, "recordingids releasegroupids")
	}
	if fp != "AQADtMk+/=abc" {
		t.Errorf("fingerprint round-trip = %q, want the original", fp)
	}
	if client != "testkey" || format != "json" {
		t.Errorf("client=%q format=%q, want testkey/json", client, format)
	}
	if m == nil || m.ReleaseGroupMBID != "rg-mbid" || m.RecordingMBID != "rec-mbid" {
		t.Fatalf("match = %+v, want rec-mbid/rg-mbid", m)
	}
}

func TestAcoustIDNoKeyIsUnsupported(t *testing.T) {
	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: "http://unused", key: ""}
	if _, err := a.lookup(context.Background(), "fp", 100); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("no-key lookup err = %v, want CodeUnsupported", err)
	}
}

func TestAcoustIDLowScoreYieldsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[{"id":"aid","score":0.4,
			"recordings":[{"id":"rec","releasegroups":[{"id":"rg"}]}]}]}`))
	}))
	defer srv.Close()
	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "k"}
	m, err := a.lookup(context.Background(), "fp", 100)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if m != nil {
		t.Fatalf("low-score match = %+v, want nil (below threshold)", m)
	}
}

// TestAcoustIDPrefersRecordingWithReleaseGroup covers the case where the first
// recording in a result carries no release group but a same-score sibling does: the
// selection must not lock onto the first and drop a resolvable match.
func TestAcoustIDPrefersRecordingWithReleaseGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[{"id":"aid","score":0.95,"recordings":[
			{"id":"rec-no-rg"},
			{"id":"rec-with-rg","releasegroups":[{"id":"the-rg"}]}]}]}`))
	}))
	defer srv.Close()
	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "k"}
	m, err := a.lookup(context.Background(), "fp", 100)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if m == nil || m.ReleaseGroupMBID != "the-rg" {
		t.Fatalf("match = %+v, want the release-group-bearing recording (the-rg)", m)
	}
}

func TestAcoustIDErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "k"}
	if _, err := a.lookup(context.Background(), "fp", 100); err == nil {
		t.Fatal("an AcoustID error response should surface as an error")
	}
}
