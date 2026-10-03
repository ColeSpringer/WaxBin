package enrich

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colespringer/waxbin/internal/caps"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/internal/testfpcalc"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestAcoustIDLookupPostsFormBody verifies the AcoustID lookup is a POST with the
// parameters in the body (not a giant GET URL), that the compressed fingerprint
// round-trips through form encoding intact, and that meta is space-separated on the
// wire (decoded from the "+" url.Values produces) rather than a literal "+".
func TestAcoustIDLookupPostsFormBody(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	a := &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: "http://unused", key: ""}
	if _, err := a.lookup(context.Background(), "fp", 100); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("no-key lookup err = %v, want CodeUnsupported", err)
	}
}

func TestAcoustIDLowScoreYieldsNoMatch(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestAcoustIDKeepsAPartialFpcalcRead: a read error fpcalc reports after fingerprinting
// the span it was asked for still makes the lookup, with that fingerprint, while one that
// stopped well short of the file's duration makes none.
func TestAcoustIDKeepsAPartialFpcalcRead(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		asked = append(asked, r.PostFormValue("fingerprint"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[{"id":"aid","score":0.95,
			"recordings":[{"id":"rec-mbid","releasegroups":[{"id":"rg-mbid"}]}]}]}`))
	}))
	defer srv.Close()
	compressed := func(n int) string {
		return base64.RawURLEncoding.EncodeToString([]byte{1, byte(n >> 16), byte(n >> 8), byte(n), 0x5a, 0xc3})
	}
	resolve := func(n int) string {
		t.Helper()
		bin := testfpcalc.Write(t, `{"duration": 300.4, "fingerprint": "`+compressed(n)+`"}`,
			"ERROR: Error decoding audio frame (End of file)", 3)
		s := &Service{
			log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			caps: caps.Caps{Fpcalc: true, FpcalcPath: bin},
			aid:  &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "k"},
		}
		return s.acoustResolveReleaseGroup(context.Background(), &runState{}, model.EnrichTarget{FilePath: "song.flac", DurationSec: 300})
	}

	if got := resolve(948); got != "rg-mbid" || len(asked) != 1 || asked[0] != compressed(948) {
		t.Fatalf("covering partial read resolved %q after asking %q, want rg-mbid from its fingerprint", got, asked)
	}
	if got := resolve(300); got != "" || len(asked) != 1 {
		t.Fatalf("short partial read resolved %q after asking %q, want no lookup", got, asked)
	}
}
