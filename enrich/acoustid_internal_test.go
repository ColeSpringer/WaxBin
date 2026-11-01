package enrich

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/fingerprint"
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

// rawVector is fpcalc -raw -json output reporting duration seconds and n values, with
// the vector it holds.
func rawVector(duration string, n int) (string, []uint32) {
	vec := make([]uint32, n)
	var b strings.Builder
	b.WriteString(`{"duration": ` + duration + `, "fingerprint": [`)
	for i := range vec {
		vec[i] = uint32(i*2654435761) & 0x7fffffff
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.Itoa(int(vec[i])))
	}
	b.WriteString("]}")
	return b.String(), vec
}

// acoustServer answers every AcoustID lookup with one release group and records each
// fingerprint it was asked about.
func acoustServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		asked = append(asked, r.PostFormValue("fingerprint"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","results":[{"id":"aid","score":0.95,
			"recordings":[{"id":"rec-mbid","releasegroups":[{"id":"rg-mbid"}]}]}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// vectorStore is the one store read the AcoustID fallback makes: a file's stored
// fingerprint and the analysis that made it.
type vectorStore struct {
	Store
	vec      []uint32
	analysis int
	err      error
}

func (v vectorStore) LoadFingerprint(context.Context, model.PID) ([]byte, int, error) {
	if v.err != nil {
		return nil, 0, v.err
	}
	return fingerprint.Pack(v.vec), v.analysis, nil
}

func acoustService(srv *httptest.Server, st Store, bin string) *Service {
	return &Service{
		store:  st,
		cfg:    Config{AcoustIDKey: "k"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		fpcalc: bin,
		aid:    &acoustID{client: netsafe.New(netsafe.Policy{}), baseURL: srv.URL, key: "k"},
	}
}

// TestAcoustIDKeepsAPartialFpcalcRead: a read error fpcalc reports after fingerprinting
// the span it was asked for still makes the lookup, with that fingerprint, while one that
// stopped well short of the file's duration makes none.
func TestAcoustIDKeepsAPartialFpcalcRead(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	resolve := func(n int) (string, string) {
		t.Helper()
		out, vec := rawVector("300.4", n)
		bin := testfpcalc.Write(t, out, "ERROR: Error decoding audio frame (End of file)", 3)
		want, err := fingerprint.EncodeCompressed(vec, fingerprint.ChromaprintAlgorithmDefault)
		if err != nil {
			t.Fatal(err)
		}
		return acoustService(srv, nil, bin).acoustResolveReleaseGroup(context.Background(), &runState{},
			model.EnrichTarget{FilePath: "song.flac", DurationSec: 300}), want
	}

	if got, want := resolve(948); got != "rg-mbid" || len(*asked) != 1 || (*asked)[0] != want {
		t.Fatalf("covering partial read resolved %q after asking %q, want rg-mbid from its fingerprint", got, *asked)
	}
	if got, _ := resolve(300); got != "" || len(*asked) != 1 {
		t.Fatalf("short partial read resolved %q after asking %q, want no lookup", got, *asked)
	}
}

// TestAcoustIDUsesTheStoredVectorWithoutFpcalc: a file whose stored analysis is
// Chromaprint's is looked up by that vector, encoded under fpcalc's default algorithm (the
// string opens with its byte, never the catalog's analysis id), and fpcalc never runs.
func TestAcoustIDUsesTheStoredVectorWithoutFpcalc(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	bin, runs := testfpcalc.WriteLogged(t, "", "ERROR: should not run", 2)
	_, stored := rawVector("0", 948)
	st := vectorStore{vec: stored, analysis: fingerprint.ChromaprintAlgoVersion}
	got := acoustService(srv, st, bin).acoustResolveReleaseGroup(context.Background(), &runState{},
		model.EnrichTarget{FilePID: "file", FilePath: "song.flac", DurationSec: 300})
	if got != "rg-mbid" || len(*asked) != 1 {
		t.Fatalf("resolved %q after asking %q, want rg-mbid from the stored vector", got, *asked)
	}
	head, err := base64.RawURLEncoding.DecodeString((*asked)[0][:8])
	if err != nil || head[0] != fingerprint.ChromaprintAlgorithmDefault {
		t.Errorf("asked %q, want it to open with algorithm byte %d (err %v)", (*asked)[0], fingerprint.ChromaprintAlgorithmDefault, err)
	}
	if vec, _, err := fingerprint.DecodeCompressed((*asked)[0]); err != nil || !slices.Equal(vec, stored) {
		t.Errorf("asked about %d values (err %v), want the stored vector", len(vec), err)
	}
	if r := testfpcalc.Runs(t, runs); len(r) != 0 {
		t.Errorf("fpcalc ran %q, want it never run", r)
	}
}

// TestAcoustIDRunsFpcalcOnceForAnUnanalyzedFile: a file with no stored analysis is
// fingerprinted by one fpcalc run, with -raw, whose vector is encoded for the lookup.
func TestAcoustIDRunsFpcalcOnceForAnUnanalyzedFile(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	out, vec := rawVector("300.4", 948)
	bin, runs := testfpcalc.WriteLogged(t, out, "", 0)
	st := vectorStore{err: waxerr.New(waxerr.CodeNotFound, "test", "file not analyzed")}
	got := acoustService(srv, st, bin).acoustResolveReleaseGroup(context.Background(), &runState{},
		model.EnrichTarget{FilePID: "file", FilePath: "song.flac", DurationSec: 300})
	want, err := fingerprint.EncodeCompressed(vec, fingerprint.ChromaprintAlgorithmDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got != "rg-mbid" || len(*asked) != 1 || (*asked)[0] != want {
		t.Fatalf("resolved %q after asking %q, want rg-mbid from fpcalc's vector", got, *asked)
	}
	if r := testfpcalc.Runs(t, runs); len(r) != 1 || !strings.Contains(r[0], "-raw") {
		t.Errorf("fpcalc runs = %q, want one with -raw", r)
	}
}

// TestAcoustIDSkipsAPureGoVector: a stored pure-Go fingerprint is not Chromaprint's, so
// the lookup runs fpcalc instead of encoding it.
func TestAcoustIDSkipsAPureGoVector(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	out, vec := rawVector("300.4", 948)
	bin, runs := testfpcalc.WriteLogged(t, out, "", 0)
	_, pureGo := rawVector("0", 500)
	st := vectorStore{vec: pureGo, analysis: fingerprint.AlgoVersion}
	acoustService(srv, st, bin).acoustResolveReleaseGroup(context.Background(), &runState{},
		model.EnrichTarget{FilePID: "file", FilePath: "song.flac", DurationSec: 300})
	want, err := fingerprint.EncodeCompressed(vec, fingerprint.ChromaprintAlgorithmDefault)
	if err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || (*asked)[0] != want || len(testfpcalc.Runs(t, runs)) != 1 {
		t.Errorf("asked %q after %d fpcalc runs, want fpcalc's vector from one run", *asked, len(testfpcalc.Runs(t, runs)))
	}
}

// TestAcoustIDSkipsAnEmptyFingerprint: a fingerprint of no values identifies nothing, so no
// lookup is made for it, from a stored analysis (fpcalc would print the same, so it is not
// run) or from fpcalc, and the fallback stays on for the rest of the run.
func TestAcoustIDSkipsAnEmptyFingerprint(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	stored, storedRuns := testfpcalc.WriteLogged(t, "", "ERROR: should not run", 2)
	fresh, freshRuns := testfpcalc.WriteLogged(t, `{"duration": 0.4, "fingerprint": []}`, "", 0)
	for _, c := range []struct {
		name string
		st   Store
		bin  string
		runs string
		want int
	}{
		{"stored", vectorStore{vec: []uint32{}, analysis: fingerprint.ChromaprintAlgoVersion}, stored, storedRuns, 0},
		{"fpcalc", vectorStore{err: waxerr.New(waxerr.CodeNotFound, "test", "file not analyzed")}, fresh, freshRuns, 1},
	} {
		st := &runState{}
		got := acoustService(srv, c.st, c.bin).acoustResolveReleaseGroup(context.Background(), st,
			model.EnrichTarget{FilePID: "file", FilePath: "short.flac", DurationSec: 1})
		if got != "" || len(*asked) != 0 || st.acoustOff {
			t.Errorf("%s: resolved %q after asking %q, fallback off %v; want no lookup and the fallback on", c.name, got, *asked, st.acoustOff)
		}
		if r := testfpcalc.Runs(t, c.runs); len(r) != c.want {
			t.Errorf("%s: fpcalc ran %d times, want %d", c.name, len(r), c.want)
		}
	}
}

// TestAcoustIDNeedsNoFpcalcForAStoredVector: a catalog analyzed with fpcalc keeps its
// Chromaprint vectors, so with fpcalc gone the fallback still runs on them, and a file
// with none is skipped rather than handed to a binary that is not there.
func TestAcoustIDNeedsNoFpcalcForAStoredVector(t *testing.T) {
	t.Parallel()
	srv, asked := acoustServer(t)
	_, stored := rawVector("0", 948)
	s := acoustService(srv, vectorStore{vec: stored, analysis: fingerprint.ChromaprintAlgoVersion}, "")
	if !s.acoustEnabled() {
		t.Fatal("the fallback is off without fpcalc, want it on for stored vectors")
	}
	st := &runState{}
	if got := s.acoustResolveReleaseGroup(context.Background(), st,
		model.EnrichTarget{FilePID: "file", FilePath: "song.flac", DurationSec: 300}); got != "rg-mbid" || len(*asked) != 1 {
		t.Fatalf("resolved %q after asking %q, want rg-mbid from the stored vector", got, *asked)
	}
	s = acoustService(srv, vectorStore{err: waxerr.New(waxerr.CodeNotFound, "test", "file not analyzed")}, "")
	if got := s.acoustResolveReleaseGroup(context.Background(), st,
		model.EnrichTarget{FilePID: "other", FilePath: "other.flac", DurationSec: 300}); got != "" || len(*asked) != 1 || st.acoustOff {
		t.Errorf("an unanalyzed file without fpcalc resolved %q after asking %q (fallback off %v), want it skipped", got, *asked, st.acoustOff)
	}
}
