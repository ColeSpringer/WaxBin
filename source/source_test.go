package source_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

const feedTmpl = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
     xmlns:podcast="https://podcastindex.org/namespace/1.0">
  <channel>
    <title>Cast</title>
    <podcast:guid>guid-1</podcast:guid>
    <item>
      <title>Ep1</title>
      <guid>e1</guid>
      <enclosure url="%s/1.mp3" length="3" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

func feedServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, fmt.Sprintf(feedTmpl, base))
	})
	mux.HandleFunc("/1.mp3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("\xff\xfb\x90"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	return base
}

func httpProvider() *source.HTTPProvider {
	return source.NewHTTP(netsafe.New(netsafe.Policy{}), podcast.ParseFeed)
}

func TestHTTPProviderResolveEnumerateFetch(t *testing.T) {
	ctx := context.Background()
	base := feedServer(t)
	prov := httpProvider()

	if prov.SourceType() != model.SourceRSS {
		t.Fatalf("source type = %q, want rss", prov.SourceType())
	}

	res, err := prov.Resolve(ctx, source.Request{URL: base + "/feed.xml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.IdentityKey != "pguid:guid-1" || res.Title != "Cast" {
		t.Fatalf("resolve = %+v, want identity pguid:guid-1 title Cast", res)
	}

	enum, err := prov.Enumerate(ctx, source.Request{URL: base + "/feed.xml"})
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if enum.NotModified || enum.Feed == nil || len(enum.Feed.Episodes) != 1 {
		t.Fatalf("enumerate = %+v, want 1 episode", enum)
	}
	if enum.ETag != `"v1"` || enum.IdentityKey != "pguid:guid-1" {
		t.Fatalf("enumerate validators/identity = %q / %q", enum.ETag, enum.IdentityKey)
	}

	// A conditional enumerate with the stored ETag is a 304 -> NotModified.
	enum2, err := prov.Enumerate(ctx, source.Request{URL: base + "/feed.xml", ETag: `"v1"`})
	if err != nil {
		t.Fatalf("conditional enumerate: %v", err)
	}
	if !enum2.NotModified {
		t.Fatalf("conditional enumerate not NotModified: %+v", enum2)
	}

	var buf bytes.Buffer
	fr, err := prov.Fetch(ctx, source.FetchRequest{URL: base + "/1.mp3"}, &buf)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if buf.String() != "\xff\xfb\x90" || fr.Bytes != 3 || !strings.HasPrefix(fr.ContentHash, "sha256:") {
		t.Fatalf("fetch = %q bytes=%d hash=%q", buf.String(), fr.Bytes, fr.ContentHash)
	}
}

func TestHTTPProviderFetchRejects304(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/e.mp3", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified) // a spurious 304 to an unconditional GET
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	if _, err := httpProvider().Fetch(ctx, source.FetchRequest{URL: srv.URL + "/e.mp3"}, &buf); err == nil {
		t.Fatal("Fetch should error on a 304 to an unconditional GET, not report a 0-byte download")
	}
	if buf.Len() != 0 {
		t.Fatalf("Fetch wrote %d bytes on a 304", buf.Len())
	}
}

func TestMockProvider(t *testing.T) {
	ctx := context.Background()
	m := &source.Mock{
		Type:        model.SourceYouTube,
		IdentityKey: "youtube:channel:abc",
		SourceID:    "abc",
		Feed: &model.Feed{Title: "Chan", Episodes: []model.FeedEpisode{
			{Title: "V1", GUID: "youtube:video:1", EnclosureURL: "yt://1"},
		}},
		Payload: []byte("hello"),
	}
	if m.SourceType() != model.SourceYouTube {
		t.Fatalf("mock source type = %q", m.SourceType())
	}
	enum, err := m.Enumerate(ctx, source.Request{URL: "yt://chan"})
	if err != nil {
		t.Fatalf("mock enumerate: %v", err)
	}
	if enum.IdentityKey != "youtube:channel:abc" || len(enum.Feed.Episodes) != 1 {
		t.Fatalf("mock enumerate = %+v", enum)
	}
	var buf bytes.Buffer
	fr, err := m.Fetch(ctx, source.FetchRequest{URL: "yt://1"}, &buf)
	if err != nil {
		t.Fatalf("mock fetch: %v", err)
	}
	if buf.String() != "hello" || fr.Bytes != 5 {
		t.Fatalf("mock fetch = %q bytes=%d", buf.String(), fr.Bytes)
	}
}

// TestProviderErrorCarriesReasonAndStatus: a provider's failure keeps the reason its cause
// names and, for a refused status, the status, so a host reads neither from the words.
func TestProviderErrorCarriesReasonAndStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/gone.xml", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) })
	mux.HandleFunc("/page.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>down for maintenance</body></html>")
	})
	mux.HandleFunc("/json.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	prov := httpProvider()

	_, err := prov.Enumerate(ctx, source.Request{URL: srv.URL + "/gone.xml"})
	pe := source.NewProviderError(model.SourceRSS, "enumerate", err)
	var status *source.StatusError
	if pe.Reason != source.ReasonHTTPStatus || pe.Status != http.StatusGone || !errors.As(pe, &status) || status.Status != http.StatusGone {
		t.Errorf("a 410 = %+v, want reason http_status and status 410", pe)
	}
	for _, path := range []string{"/page.xml", "/json.xml"} {
		_, err := prov.Enumerate(ctx, source.Request{URL: srv.URL + path})
		if pe := source.NewProviderError(model.SourceRSS, "enumerate", err); pe.Reason != source.ReasonNotAFeed || pe.Status != 0 ||
			!waxerr.Is(pe, waxerr.CodeInvalid) {
			t.Errorf("%s = %+v (%v), want an invalid not_a_feed", path, pe, err)
		}
	}
	// An injected provider names its own reason, or hands back a bare status.
	if pe := source.NewProviderError(model.SourceYouTube, "fetch",
		waxerr.New(waxerr.CodeIO, "youtube", "quota").WithReason(source.ReasonTooLarge)); pe.Reason != source.ReasonTooLarge || pe.Status != 0 {
		t.Errorf("a reasoned injected failure = %+v, want too_large", pe)
	}
	// A status under another reason is not the refusal; a classed status keeps its class.
	if pe := source.NewProviderError(model.SourceYouTube, "fetch", &waxerr.Error{Code: waxerr.CodeIO, Op: "youtube",
		Err: &source.StatusError{Status: http.StatusNotFound}, Reason: source.ReasonTransport}); pe.Reason != source.ReasonTransport || pe.Status != 0 {
		t.Errorf("a status under a transport reason = %+v, want transport and no status", pe)
	}
	if pe := source.NewProviderError(model.SourceYouTube, "fetch", waxerr.Wrap(waxerr.CodeIO, "youtube", &source.StatusError{Status: http.StatusNotFound})); pe.Reason != source.ReasonHTTPStatus ||
		pe.Status != http.StatusNotFound || !waxerr.Is(pe, waxerr.CodeIO) || waxerr.ReasonOf(pe) != source.ReasonHTTPStatus {
		t.Errorf("a classed status = %+v (%s %q), want an io http_status 404", pe, waxerr.CodeOf(pe), waxerr.ReasonOf(pe))
	}
	if pe := source.NewProviderError(model.SourceYouTube, "fetch", &source.StatusError{Status: http.StatusTooManyRequests}); pe.Reason != source.ReasonHTTPStatus ||
		pe.Status != http.StatusTooManyRequests || waxerr.ReasonOf(pe) != source.ReasonHTTPStatus || !waxerr.Is(pe, waxerr.CodeIO) {
		t.Errorf("a bare status = %+v (%s %q), want an io http_status 429 however it is read", pe, waxerr.CodeOf(pe), waxerr.ReasonOf(pe))
	}
}

// enclosure serves body as type ctype, or with no type when ctype is "".
func enclosure(t *testing.T, ctype string, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ctype == "" {
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", ctype)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/e"
}

// fetchWhole fetches url and fails unless body lands whole with its count and hash.
func fetchWhole(t *testing.T, name, url string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	fr, err := httpProvider().Fetch(context.Background(), source.FetchRequest{URL: url}, &buf)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	h, sum := identity.StreamHasher()
	h.Write(body)
	if !bytes.Equal(buf.Bytes(), body) || fr.Bytes != int64(len(body)) || fr.ContentHash != sum() {
		t.Errorf("%s: wrote %d bytes, reported %d and %s; want %d whole", name, buf.Len(), fr.Bytes, fr.ContentHash, len(body))
	}
}

// fetchRefused fetches url and fails unless it is refused as the wrong media, nothing
// written.
func fetchRefused(t *testing.T, name, url string) {
	t.Helper()
	var buf bytes.Buffer
	_, err := httpProvider().Fetch(context.Background(), source.FetchRequest{URL: url}, &buf)
	if !waxerr.Is(err, waxerr.CodeInvalid) || waxerr.ReasonOf(err) != source.ReasonMediaType || buf.Len() != 0 {
		t.Errorf("%s = %v (%s %q), %d bytes written; want an invalid media_type and nothing written",
			name, err, waxerr.CodeOf(err), waxerr.ReasonOf(err), buf.Len())
	}
	if err != nil && strings.Contains(err.Error(), "source.http.Fetch: source.http.Fetch") {
		t.Errorf("%s = %q, names its op twice", name, err)
	}
}

// mp3Body is MPEG audio as far as a type gate can tell: an ID3 header, then frames.
func mp3Body(n int) []byte {
	b := bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64}, n/4+1)[:n]
	return append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), b...)
}

// TestFetchAdmitsApplicationMpegAndUntyped: an enclosure served as application/mpeg, or
// with no type at all, is audio unless its bytes say otherwise.
func TestFetchAdmitsApplicationMpegAndUntyped(t *testing.T) {
	t.Parallel()
	body := mp3Body(64 << 10)
	fetchWhole(t, "application/mpeg", enclosure(t, "application/mpeg", body), body)
	fetchWhole(t, "untyped", enclosure(t, "", body), body)
}

// TestFetchRefusesPlaylistTypes: a playlist or streaming manifest is not an episode,
// whatever bytes come with it.
func TestFetchRefusesPlaylistTypes(t *testing.T) {
	t.Parallel()
	for _, ctype := range []string{"audio/x-mpegurl", "audio/mpegurl", "application/vnd.apple.mpegurl", "application/x-mpegurl",
		"audio/x-scpls", "application/pls+xml", "video/x-ms-asx", "video/x-ms-wvx", "video/x-ms-wax", "audio/x-ms-wax",
		"audio/x-pn-realaudio", "audio/vnd.rn-realaudio", "application/dash+xml", "application/smil+xml",
		"audio/x-mpegurl; charset"} {
		fetchRefused(t, ctype, enclosure(t, ctype, mp3Body(1024)))
	}
}

// TestFetchRefusesAPlaylistBodyUnderAnAudioType: a body that reads as a page or a
// playlist is refused under an audio type, an octet stream, or no type, after a BOM and
// whitespace too.
func TestFetchRefusesAPlaylistBodyUnderAnAudioType(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ ctype, body string }{
		"an m3u":                         {"audio/mpeg", "#EXTM3U\n#EXTINF:-1,Live\nhttp://stream.example/live.mp3\n"},
		"a pls after a BOM":              {"application/octet-stream", "\ufeff  [Playlist]\nFile1=http://stream.example/live.mp3\n"},
		"an asx":                         {"video/x-ms-asf", "<ASX version=\"3.0\"><Entry><Ref href=\"mms://x\"/></Entry></ASX>"},
		"an untyped html page":           {"", "\r\n<!DOCTYPE html><html><body>Not found</body></html>"},
		"a long page":                    {"audio/mpeg", "<html>" + strings.Repeat("<p>sorry</p>", 200) + "</html>"},
		"html after a form feed":         {"audio/mpeg", "\f<html><body>gone</body></html>"},
		"utf-16 html":                    {"", "\xff\xfe<\x00h\x00t\x00m\x00l\x00>\x00"},
		"an untyped json error":          {"", `{"error":"subscription expired"}`},
		"plain text under an audio type": {"audio/mpeg", "Your download link has expired. Please sign in again.\n"},
		"a bare url list":                {"audio/mpeg", "http://stream.example/live.mp3\nhttp://stream.example/backup.mp3\n"},
		"text cut inside a rune":         {"application/octet-stream", strings.Repeat("\u20ac", 200)},
		"an empty body":                  {"audio/mpeg", ""},
	} {
		fetchRefused(t, name, enclosure(t, tc.ctype, []byte(tc.body)))
	}
}

// TestFetchWritesAShortBodyWhole: a body shorter than the sniffed head is judged when the
// stream ends and lands intact.
func TestFetchWritesAShortBodyWhole(t *testing.T) {
	t.Parallel()
	body := mp3Body(300)[:300]
	fetchWhole(t, "300 bytes", enclosure(t, "audio/mpeg", body), body)
}

// TestFetchKeepsASFAudio: ASF is an audio container, refused only when its bytes are an
// ASX playlist.
func TestFetchKeepsASFAudio(t *testing.T) {
	t.Parallel()
	header := []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0x00, 0xaa, 0x00, 0x62, 0xce, 0x6c}
	body := append(header, bytes.Repeat([]byte{0x01}, 4096)...)
	fetchWhole(t, "video/x-ms-asf", enclosure(t, "video/x-ms-asf", body), body)
}

// TestFetchSendsALoginWithNoOriginToItsOwnURL: a login given without AuthOrigin belongs to
// the URL fetched, as a feed's login belongs to the feed.
func TestFetchSendsALoginWithNoOriginToItsOwnURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(mp3Body(64))
	}))
	t.Cleanup(srv.Close)
	var buf bytes.Buffer
	if _, err := httpProvider().Fetch(context.Background(), source.FetchRequest{URL: srv.URL + "/e", User: "alice", Pass: "pw"}, &buf); err != nil {
		t.Fatalf("fetch with a login and no origin: %v", err)
	}
}
