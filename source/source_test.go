package source_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
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
		_, _ = w.Write([]byte("abc"))
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
	if buf.String() != "abc" || fr.Bytes != 3 || !strings.HasPrefix(fr.ContentHash, "sha256:") {
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
